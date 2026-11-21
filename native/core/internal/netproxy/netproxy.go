// Package netproxy owns the one outbound HTTP proxy setting.
//
// Requests are evaluated when they are sent, so saving a proxy takes effect
// without rebuilding the many clients in this program. Loopback destinations
// stay direct: qBittorrent, FlareSolverr, the bundled engines and the desktop
// reverse proxy are local services, never internet requests.
package netproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const ConnectivityURL = "https://api.github.com/meta"

var connectivityURLForTest = ConnectivityURL

var current atomic.Pointer[url.URL]

var installMu sync.Mutex

// Normalize accepts either a full http(s) proxy URL or a bare host:port.
// Credentials, paths, queries and fragments are refused rather than stored.
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return "", errors.New("proxy address contains control characters")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("proxy address is invalid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("proxy address must use http or https")
	}
	if parsed.User != nil {
		return "", errors.New("proxy credentials are not supported")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("proxy address must be an origin without a path, query or fragment")
	}
	if parsed.Hostname() == "" {
		return "", errors.New("proxy host is required")
	}
	if port := parsed.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return "", errors.New("proxy port must be between 1 and 65535")
		}
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

// Configure validates and atomically replaces the runtime proxy. An empty
// string disables proxying, including any inherited environment proxy, so the
// settings switch has the same meaning on every machine. Existing clients use
// the default transport and see the switch on their next request.
func Configure(raw string) (string, error) {
	normalized, err := Normalize(raw)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		current.Store(nil)
	} else {
		parsed, err := url.Parse(normalized)
		if err != nil {
			return "", err
		}
		current.Store(parsed)
	}
	closeDefaultIdleConnections()
	return normalized, nil
}

// Current returns the explicit runtime proxy. An empty value means direct
// connections; environment proxy variables are intentionally not inherited.
func Current() (string, bool) {
	parsed := current.Load()
	if parsed == nil {
		return "", false
	}
	return parsed.String(), true
}

// Install replaces http.DefaultTransport with a proxy-aware clone. It is safe
// to call on every application start and also repairs a test or embedder that
// replaced the default transport with another *http.Transport.
func Install() {
	installMu.Lock()
	defer installMu.Unlock()
	base := defaultBaseTransport()
	var old *http.Transport
	if prior, ok := http.DefaultTransport.(*http.Transport); ok {
		old = prior
	}
	next := base.Clone()
	next.Proxy = dynamicProxy
	http.DefaultTransport = next
	if old != nil && old != next {
		old.CloseIdleConnections()
	}
}

// Transport returns a cloned transport with the same dynamic proxy rule.
// Callers that need custom connection settings (for example disabling
// keep-alive for token downloads) should use this instead of a bare Transport.
func Transport(base *http.Transport) *http.Transport {
	if base == nil {
		base = defaultBaseTransport()
	}
	cloned := base.Clone()
	cloned.Proxy = dynamicProxy
	return cloned
}

// IsLoopback reports whether a destination must bypass the configured proxy.
func IsLoopback(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func dynamicProxy(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	if IsLoopback(req.URL.Hostname()) {
		return nil, nil
	}
	if configured := current.Load(); configured != nil {
		return configured, nil
	}
	return nil, nil
}

func defaultBaseTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base
	}
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func closeDefaultIdleConnections() {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

// ConnectivityResult is the independent connectivity check exposed by settings.
type ConnectivityResult struct {
	OK        bool   `json:"ok"`
	Proxy     string `json:"proxy"`
	URL       string `json:"url"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// CheckConnectivity checks an explicit proxy URL. It uses the proxy for this request even
// when the runtime setting is empty, so the settings page can validate before
// saving. The response body is hashed only by success, never logged.
func CheckConnectivity(ctx context.Context, raw string) ConnectivityResult {
	normalized, err := Normalize(raw)
	if err != nil {
		return ConnectivityResult{Proxy: strings.TrimSpace(raw), URL: connectivityURLForTest, Error: err.Error()}
	}
	if normalized == "" {
		return ConnectivityResult{Proxy: "", URL: connectivityURLForTest, Error: "proxy is not configured"}
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return ConnectivityResult{Proxy: normalized, URL: connectivityURLForTest, Error: err.Error()}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	// Check with the same default transport shape as normal traffic. A
	// zero-value transport can negotiate differently with some HTTP proxies
	// (notably when CONNECT is upgraded to HTTP/2), producing an EOF even
	// though the saved proxy is usable by the tracker clients.
	transport := Transport(nil)
	transport.Proxy = http.ProxyURL(parsed)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second}
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connectivityURLForTest, nil)
	if err != nil {
		return ConnectivityResult{Proxy: normalized, URL: connectivityURLForTest, Error: err.Error()}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "MusicOps/settings-network-check")
	resp, err := client.Do(req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return ConnectivityResult{Proxy: normalized, URL: connectivityURLForTest, LatencyMS: latency, Error: sanitizeConnectivityError(err)}
	}
	defer resp.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return ConnectivityResult{Proxy: normalized, URL: connectivityURLForTest, Status: resp.StatusCode, LatencyMS: latency, Error: "proxy response could not be read"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return ConnectivityResult{Proxy: normalized, URL: connectivityURLForTest, Status: resp.StatusCode, LatencyMS: latency, Error: fmt.Sprintf("network check returned HTTP %d", resp.StatusCode)}
	}
	return ConnectivityResult{OK: true, Proxy: normalized, URL: connectivityURLForTest, Status: resp.StatusCode, LatencyMS: latency}
}

func sanitizeConnectivityError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 240 {
		text = text[:240]
	}
	return text
}

// SanitizedEnvironment removes every proxy variable inherited from the host.
// Services whose target request carries its own proxy field must start without
// an environment-level fallback that could survive a later settings change.
func SanitizedEnvironment(base []string) []string {
	proxyKeys := map[string]bool{
		"HTTP_PROXY": true, "http_proxy": true, "HTTPS_PROXY": true, "https_proxy": true,
		"ALL_PROXY": true, "all_proxy": true, "NO_PROXY": true, "no_proxy": true,
		"PROXY_URL": true, "PROXY_USERNAME": true, "PROXY_PASSWORD": true,
	}
	out := make([]string, 0, len(base))
	for _, entry := range base {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if proxyKeys[key] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ChildEnvironment applies the configured proxy to a managed engine process.
// A local proxy can itself be on loopback; the bypass list names destinations,
// not the proxy endpoint.
func ChildEnvironment(base []string) []string {
	out := SanitizedEnvironment(base)
	proxy, enabled := Current()
	if !enabled {
		return out
	}
	values := map[string]string{
		"HTTP_PROXY": proxy, "http_proxy": proxy,
		"HTTPS_PROXY": proxy, "https_proxy": proxy,
		"NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1",
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}
