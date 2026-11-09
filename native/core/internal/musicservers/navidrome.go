package musicservers

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ValidateNavidromeURL validates a Navidrome base URL. An empty URL is
// allowed (Navidrome not configured); otherwise it must be a clean origin —
// scheme plus host with no userinfo, query, fragment or path — and plain
// HTTP is only allowed for local-service hosts (loopback, LAN, Docker
// internal); a public host must use HTTPS.
func ValidateNavidromeURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("musicservers: Navidrome URL must be a clean origin: %w", ErrInvalidInput)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("musicservers: Navidrome URL must be a clean origin: %w: %w", ErrInvalidInput, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("musicservers: Navidrome URL must be a clean origin: %w", ErrInvalidInput)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("musicservers: Navidrome URL must be a clean origin without credentials, query, fragment or path: %w", ErrInvalidInput)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("musicservers: Navidrome URL must be a clean origin: %w", ErrInvalidInput)
	}
	if scheme == "http" && !localServiceHost(host) {
		return fmt.Errorf("musicservers: public Navidrome must use HTTPS; HTTP is only allowed for local, LAN or Docker-internal hosts: %w", ErrInvalidInput)
	}
	return nil
}

// NavidromePing mirrors the read-only half of navidrome.scan: a Subsonic
// ping (GET /rest/ping) that only verifies the server accepts the
// credentials. See navidromeRequest for the auth scheme and status rules.
func NavidromePing(ctx context.Context, baseURL, username, password string) error {
	return navidromeRequest(ctx, baseURL, username, password, "/rest/ping")
}

// NavidromeScan mirrors navidrome.scan: after audio has been added to the
// library it requests a rescan with GET /rest/startScan using the same
// Subsonic authentication as the ping.
func NavidromeScan(ctx context.Context, baseURL, username, password string) error {
	return navidromeRequest(ctx, baseURL, username, password, "/rest/startScan")
}

// navidromeRequest performs one authenticated Subsonic GET. The token
// scheme mirrors the Python client: salt is 16 random bytes rendered as
// 32 hex characters, and token is the hex MD5 of password+salt. The
// request carries u=<username>&t=<token>&s=<salt>&v=1.16.1&c=MusicOps&f=json.
// Success requires HTTP 200 and subsonic-response.status == "ok"; anything
// else is ErrAccessBlocked, and a response that is not JSON at all is
// additionally an invalid payload.
func navidromeRequest(ctx context.Context, baseURL, username, password, path string) error {
	if err := ValidateNavidromeURL(baseURL); err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("musicservers: Navidrome password is required: %w", ErrAuthRequired)
	}
	var saltBytes [16]byte
	if _, err := rand.Read(saltBytes[:]); err != nil {
		return fmt.Errorf("musicservers: generate Navidrome salt: %w", err)
	}
	salt := hex.EncodeToString(saltBytes[:])
	sum := md5.Sum([]byte(password + salt))
	token := hex.EncodeToString(sum[:])
	query := url.Values{}
	query.Set("u", username)
	query.Set("t", token)
	query.Set("s", salt)
	query.Set("v", "1.16.1")
	query.Set("c", "MusicOps")
	query.Set("f", "json")
	endpoint := strings.TrimRight(baseURL, "/") + path + "?" + query.Encode()
	body, status, err := httpGet(ctx, endpoint, nil, 0)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("musicservers: Navidrome rejected the request (status %d): %w", status, ErrAccessBlocked)
	}
	var payload struct {
		Response struct {
			Status string `json:"status"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// No parseable confirmation of success: this is both an invalid
		// payload and a failed access check.
		return fmt.Errorf("musicservers: Navidrome returned an invalid response: %w: %w", ErrAccessBlocked, ErrInvalidPayload)
	}
	if payload.Response.Status != "ok" {
		return fmt.Errorf("musicservers: Navidrome did not confirm success: %w", ErrAccessBlocked)
	}
	return nil
}
