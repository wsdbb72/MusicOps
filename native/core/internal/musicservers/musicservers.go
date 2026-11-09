// Package musicservers is a read-only client for the Plex and Navidrome
// music servers, mirroring src/musicops/clients/plex.py and
// src/musicops/clients/navidrome.py.
//
// It only ever performs two operations against either server: a read-only
// connection test (Plex library sections, Navidrome Subsonic ping) and a
// scan notification after audio has been added to the library. It never
// uploads, downloads or modifies library content, and it only uses the
// standard library.
package musicservers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Package-level errors. Callers should test for them with errors.Is; the
// returned errors wrap the sentinel together with a short description.
var (
	// ErrAuthRequired is returned when a credential (Plex token or
	// Navidrome password) was not supplied.
	ErrAuthRequired = errors.New("musicservers: authentication required")
	// ErrAuthFailed is returned when the server rejects the credential
	// (HTTP 401/403 from Plex).
	ErrAuthFailed = errors.New("musicservers: authentication failed")
	// ErrAccessBlocked is returned when the server does not accept the
	// request or does not confirm success.
	ErrAccessBlocked = errors.New("musicservers: access blocked")
	// ErrInvalidPayload is returned when the server response cannot be
	// parsed or is not the expected shape.
	ErrInvalidPayload = errors.New("musicservers: invalid payload")
	// ErrInvalidInput is returned when a URL or a section ID is malformed
	// or fails the local-service policy.
	ErrInvalidInput = errors.New("musicservers: invalid input")
	// ErrNetwork wraps transport failures (connection refused, DNS
	// failure, timeout, truncated body).
	ErrNetwork = errors.New("musicservers: network error")
)

// requestTimeout mirrors the 15 second timeout used by the Python clients.
const requestTimeout = 15 * time.Second

var localSuffixes = []string{".localhost", ".local", ".lan", ".internal", ".home.arpa"}

var singleLabelName = regexp.MustCompile(`^[a-z0-9_](?:[a-z0-9_-]{0,61}[a-z0-9_])?$`)

// localServiceHost mirrors musicops.clients.http.local_service_host:
// loopback, private-network and Docker/LAN service names may use HTTP.
// Single-label names are what Docker networks and LAN DNS hand out for
// sibling containers (for example "navidrome"); public hosts still need
// HTTPS so credentials never cross the internet in plain text.
func localServiceHost(host string) bool {
	host = strings.ToLower(strings.TrimRight(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	for _, suffix := range localSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	if ip := parseHostIP(host); ip != nil {
		return isLocalIP(ip)
	}
	if !strings.Contains(host, ".") {
		return singleLabelName.MatchString(host)
	}
	return false
}

// parseHostIP parses host as an IP address, dropping any IPv6 zone id
// ("fe80::1%eth0") first, exactly as the Python implementation does.
func parseHostIP(host string) net.IP {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

func isLocalIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	// Python ipaddress.is_private also covers the CGNAT shared address
	// space 100.64.0.0/10; treat it as local too.
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat != nil && cgnat.Contains(ip)
}

// httpGet performs a GET with no redirect following and returns the
// response body and status code. When limit > 0 at most limit+1 bytes are
// read, so the caller can detect an over-limit response without reading an
// unbounded body. Transport failures are wrapped with ErrNetwork.
func httpGet(ctx context.Context, endpoint string, headers map[string]string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("musicservers: build request: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("musicservers: GET request failed: %w: %w", ErrNetwork, err)
	}
	defer resp.Body.Close()
	var bodyReader io.Reader = resp.Body
	if limit > 0 {
		bodyReader = io.LimitReader(resp.Body, limit+1)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("musicservers: read response: %w: %w", ErrNetwork, err)
	}
	return body, resp.StatusCode, nil
}
