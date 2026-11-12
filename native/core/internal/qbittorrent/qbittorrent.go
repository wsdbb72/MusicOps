// Package qbittorrent is a real client for the qBittorrent Web API v2.
//
// It mirrors the Python client in src/musicops/clients/qbt.py and the
// metainfo rules in src/musicops/clients/metainfo.py: clean-origin base URLs
// only (plain HTTP only for loopback / private / Docker-internal hosts),
// cookie authentication, version-gated behaviour, stopped adds, persisted
// ownership checks, free-evidence matching and poll classification.
//
// Credentials are never included in errors or logs. Every failure path
// returns a real error or a typed ownership/poll outcome, never a fake
// success.
package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProjectTag is the tag every MusicOps-owned torrent carries.
const ProjectTag = "MusicOps"

// DefaultCategory is the qBittorrent category for MusicOps torrents.
const DefaultCategory = "musicops"

const apiPrefix = "/api/v2/"
const maxResponseBytes = 20 << 20
const httpTimeout = 15 * time.Second

var (
	// ErrInvalidBaseURL is returned when a base URL is not a clean origin or
	// uses plain HTTP for a public host.
	ErrInvalidBaseURL = errors.New("qbittorrent: invalid base URL")
	// ErrInvalidConfig is returned for an unusable client configuration.
	ErrInvalidConfig = errors.New("qbittorrent: invalid configuration")
	// ErrInvalidRequest is returned for an invalid client call argument.
	ErrInvalidRequest = errors.New("qbittorrent: invalid request")
	// ErrAuthFailed is returned when login is rejected or no SID cookie is issued.
	ErrAuthFailed = errors.New("qbittorrent: authentication failed")
	// ErrNotAuthenticated is returned when an API call is made before Login.
	ErrNotAuthenticated = errors.New("qbittorrent: not logged in")
	// ErrUnsupportedVersion is returned when the server version is outside
	// the supported 4.6.0-5.2.2 range.
	ErrUnsupportedVersion = errors.New("qbittorrent: unsupported version")
	// ErrUnexpectedSchema is returned when a response is not the JSON shape
	// the API documents.
	ErrUnexpectedSchema = errors.New("qbittorrent: unexpected response schema")
	// ErrFreeEvidenceMismatch is returned when free evidence does not bind to
	// the metainfo being added or started.
	ErrFreeEvidenceMismatch = errors.New("qbittorrent: free evidence does not match metainfo")
	// ErrRecordConflict is returned when a task id is already bound to a
	// different resource.
	ErrRecordConflict = errors.New("qbittorrent: task id already bound to a different resource")
	// ErrAlreadyExists is returned when the client already holds a resource
	// with no creation record for this task.
	ErrAlreadyExists = errors.New("qbittorrent: client already has this resource without a creation record")
	// ErrStartAttemptsExhausted is returned after three failed start attempts.
	ErrStartAttemptsExhausted = errors.New("qbittorrent: start failed three times")
	// ErrStartCooldown is returned when a start was requested less than ten
	// seconds ago and the client has not acted on it yet.
	ErrStartCooldown = errors.New("qbittorrent: waiting for client to execute start request")
)

// ResponseError is a typed error for an unexpected HTTP status. It carries
// the method, endpoint and status only; bodies and credentials are never
// included.
type ResponseError struct {
	Method     string
	Endpoint   string
	StatusCode int
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("qbittorrent: %s %s returned status %d", e.Method, e.Endpoint, e.StatusCode)
}

// OwnershipError carries the plain reason a torrent failed an ownership
// check. The reason is safe to surface to the operator.
type OwnershipError struct {
	Reason string
}

func (e *OwnershipError) Error() string {
	return "qbittorrent: ownership check failed: " + e.Reason
}

// Version is a parsed qBittorrent application version.
type Version struct {
	Major int
	Minor int
	Patch int
}

// ParseVersion parses "v5.0.4" or "5.0" (patch defaults to 0).
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: %q is not a version", ErrUnexpectedSchema, s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" {
			return Version{}, fmt.Errorf("%w: %q is not a version", ErrUnexpectedSchema, s)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("%w: %q is not a version", ErrUnexpectedSchema, s)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, nil
}

// IsZero reports whether no version has been recorded yet.
func (v Version) IsZero() bool { return v.Major == 0 && v.Minor == 0 && v.Patch == 0 }

// Compare orders two versions: -1, 0 or 1.
func (v Version) Compare(o Version) int {
	if v.Major != o.Major {
		return compareInt(v.Major, o.Major)
	}
	if v.Minor != o.Minor {
		return compareInt(v.Minor, o.Minor)
	}
	return compareInt(v.Patch, o.Patch)
}

// AtLeast reports whether v is at least major.minor.patch.
func (v Version) AtLeast(major, minor, patch int) bool {
	return v.Compare(Version{Major: major, Minor: minor, Patch: patch}) >= 0
}

func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

func compareInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// Capabilities describes what the connected server supports.
type Capabilities struct {
	Supported bool
	// InitialStopParameter is "stopped" for >= 5.0.0, else "paused".
	InitialStopParameter string
	// AutoTMM is always false: this client never enables automatic torrent
	// management.
	AutoTMM       bool
	Version       Version
	WebAPIVersion string
}

// Supported reports whether v is inside the supported 4.6.0-5.2.2 range.
func (v Version) Supported() bool {
	return !v.IsZero() && v.Compare(Version{Major: 4, Minor: 6, Patch: 0}) >= 0 &&
		v.Compare(Version{Major: 5, Minor: 2, Patch: 2}) <= 0
}

// Config configures a Client. Password is held in memory only and never
// appears in an error, log line or returned struct.
type Config struct {
	BaseURL  string
	Username string
	Password string
}

var localSuffixes = []string{".localhost", ".local", ".lan", ".internal", ".home.arpa"}

var singleLabelName = regexp.MustCompile(`^[a-z0-9_](?:[a-z0-9_-]{0,61}[a-z0-9_])?$`)

// LocalServiceHost mirrors musicops.clients.http.local_service_host: loopback,
// private-network and Docker/LAN service names may use HTTP. Single-label
// names are what Docker networks and LAN DNS hand out for sibling
// containers; public hosts still need HTTPS.
func LocalServiceHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
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

// ValidateBaseURL validates a qBittorrent base URL and returns the cleaned
// origin (scheme://host[:port]). It rejects any userinfo, query, fragment or
// path, and plain HTTP for public hosts. It never returns a silent default
// and never echoes the input in the error.
func ValidateBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || strings.ContainsAny(raw, " \t\r\n") {
		return "", ErrInvalidBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidBaseURL
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrInvalidBaseURL
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", ErrInvalidBaseURL
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return "", ErrInvalidBaseURL
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrInvalidBaseURL
		}
	}
	if scheme == "http" && !LocalServiceHost(host) {
		return "", ErrInvalidBaseURL
	}
	originHost := host
	if strings.Contains(host, ":") {
		originHost = "[" + host + "]"
	}
	origin := scheme + "://" + originHost
	if port != "" {
		origin += ":" + port
	}
	return origin, nil
}

// Client is a qBittorrent Web API v2 client. The zero value is unusable;
// use NewClient.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client

	mu            sync.Mutex
	sid           string
	version       Version
	webAPIVersion string
	versionLoaded bool
}

// NewClient validates the base URL and returns a client. No network call is
// made; call Login before any other method.
func NewClient(cfg Config) (*Client, error) {
	origin, err := ValidateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(cfg.Username, "\r\n") || strings.ContainsAny(cfg.Password, "\r\n") {
		return nil, ErrInvalidConfig
	}
	return &Client{
		baseURL:  origin,
		username: cfg.Username,
		password: cfg.Password,
		http: &http.Client{
			Timeout: httpTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// BaseURL returns the validated origin this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) requireLogin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sid == "" {
		return ErrNotAuthenticated
	}
	return nil
}

func (c *Client) versionSnapshot() Version {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

func (c *Client) newRequest(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+apiPrefix+endpoint, body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	sid := c.sid
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("Cookie", "SID="+sid)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// do performs one request and returns the status, body and headers. The body
// is capped at 20 MiB. Network failures are reported as plain errors with no
// credentials or URL details.
func (c *Client) do(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (int, []byte, http.Header, error) {
	req, err := c.newRequest(ctx, method, endpoint, body, contentType)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, ctx.Err()
		}
		return 0, nil, nil, errors.New("qbittorrent: request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(data) > maxResponseBytes {
		return 0, nil, nil, errors.New("qbittorrent: response too large")
	}
	return resp.StatusCode, data, resp.Header, nil
}

func (c *Client) getText(ctx context.Context, endpoint string) (string, error) {
	status, body, _, err := c.do(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", &ResponseError{Method: http.MethodGet, Endpoint: endpoint, StatusCode: status}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "", ErrUnexpectedSchema
	}
	return text, nil
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) error {
	if err := c.requireLogin(); err != nil {
		return err
	}
	status, _, _, err := c.do(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &ResponseError{Method: http.MethodPost, Endpoint: endpoint, StatusCode: status}
	}
	return nil
}

func decodeJSON[T any](body []byte) (T, error) {
	var value T
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&value); err != nil {
		return value, fmt.Errorf("%w: invalid JSON", ErrUnexpectedSchema)
	}
	if dec.More() {
		return value, fmt.Errorf("%w: trailing data", ErrUnexpectedSchema)
	}
	return value, nil
}

// Login authenticates with POST /api/v2/auth/login. Success is 200 with body
// "Ok." or 204. The SID cookie is kept in memory only. A wrong password
// returns ErrAuthFailed; any other unexpected status returns ResponseError.
func (c *Client) Login(ctx context.Context) error {
	if c.username == "" || c.password == "" {
		return ErrAuthFailed
	}
	form := url.Values{
		"username": {c.username},
		"password": {c.password},
	}
	req, err := c.newRequest(ctx, http.MethodPost, "auth/login", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	req.Header.Set("Referer", c.baseURL)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("qbittorrent: request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "Ok.":
	case resp.StatusCode == http.StatusNoContent:
	case resp.StatusCode == http.StatusForbidden:
		return ErrAuthFailed
	default:
		return &ResponseError{Method: http.MethodPost, Endpoint: "auth/login", StatusCode: resp.StatusCode}
	}
	sid := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "SID" {
			sid = cookie.Value
		}
	}
	if sid == "" {
		return ErrAuthFailed
	}
	c.mu.Lock()
	c.sid = sid
	c.mu.Unlock()
	return nil
}

// Version fetches /api/v2/app/version and /api/v2/app/webapiVersion and
// records them for Capabilities and version-gated endpoints.
func (c *Client) Version(ctx context.Context) (Version, string, error) {
	if err := c.requireLogin(); err != nil {
		return Version{}, "", err
	}
	raw, err := c.getText(ctx, "app/version")
	if err != nil {
		return Version{}, "", err
	}
	version, err := ParseVersion(raw)
	if err != nil {
		return Version{}, "", err
	}
	webAPI, err := c.getText(ctx, "app/webapiVersion")
	if err != nil {
		return Version{}, "", err
	}
	if _, err := ParseVersion(webAPI); err != nil {
		return Version{}, "", err
	}
	c.mu.Lock()
	c.version = version
	c.webAPIVersion = webAPI
	c.versionLoaded = true
	c.mu.Unlock()
	return version, webAPI, nil
}

func (c *Client) ensureVersion(ctx context.Context) (Version, string, error) {
	c.mu.Lock()
	loaded := c.versionLoaded
	version := c.version
	webAPI := c.webAPIVersion
	c.mu.Unlock()
	if loaded {
		return version, webAPI, nil
	}
	return c.Version(ctx)
}

// Capabilities returns the supported range, the initial stop parameter and
// the autoTMM requirement for the connected server. It returns
// ErrUnsupportedVersion when the server version is outside 4.6.0-5.2.2.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	version, webAPI, err := c.ensureVersion(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	caps := Capabilities{
		Supported:            version.Supported(),
		InitialStopParameter: "paused",
		AutoTMM:              false,
		Version:              version,
		WebAPIVersion:        webAPI,
	}
	if version.AtLeast(5, 0, 0) {
		caps.InitialStopParameter = "stopped"
	}
	if !caps.Supported {
		return caps, ErrUnsupportedVersion
	}
	return caps, nil
}

// Preferences fetches /api/v2/app/preferences and requires a JSON object.
// It is the read-only half of the Python qbt_preflight: login, version,
// preferences and the torrent list, all read without changing anything.
func (c *Client) Preferences(ctx context.Context) (map[string]any, error) {
	if err := c.requireLogin(); err != nil {
		return nil, err
	}
	status, body, _, err := c.do(ctx, http.MethodGet, "app/preferences", nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &ResponseError{Method: http.MethodGet, Endpoint: "app/preferences", StatusCode: status}
	}
	return decodeJSON[map[string]any](body)
}

// AddRequest is one stopped torrent add.
type AddRequest struct {
	// Torrent is the raw .torrent bytes.
	Torrent []byte
	// SavePath is the final save directory (the task source path).
	SavePath string
	// Category is the qBittorrent category, normally DefaultCategory.
	Category string
	// Tags is the comma-separated tag list, normally ProjectTag.
	Tags string
}

// Add adds a torrent in the stopped state via POST /api/v2/torrents/add
// (multipart). Success is 200 or 202; the caller must still confirm the
// add with Query/ReconcileAdd because the endpoint itself does not confirm
// the torrent exists.
func (c *Client) Add(ctx context.Context, req AddRequest) error {
	if len(req.Torrent) == 0 || req.SavePath == "" || !filepath.IsAbs(req.SavePath) {
		return ErrInvalidRequest
	}
	if req.Category == "" || strings.ContainsAny(req.Category, "\r\n") {
		return ErrInvalidRequest
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	fields := []struct{ key, value string }{
		{"savepath", req.SavePath},
		{"category", req.Category},
		{"tags", req.Tags},
		{"autoTMM", "false"},
		{"skip_checking", "false"},
		{"contentLayout", "Original"},
		{caps.InitialStopParameter, "true"},
		{"ratioLimit", "-1"},
		{"seedingTimeLimit", "-1"},
		{"inactiveSeedingTimeLimit", "-1"},
		{"downloadPath", filepath.Join(req.SavePath, ".incomplete")},
		{"useDownloadPath", "true"},
	}
	for _, field := range fields {
		if err := writer.WriteField(field.key, field.value); err != nil {
			return err
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="torrents"; filename="asset.torrent"`)
	header.Set("Content-Type", "application/x-bittorrent")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(req.Torrent); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	status, _, _, err := c.do(ctx, http.MethodPost, "torrents/add", &buf, writer.FormDataContentType())
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return &ResponseError{Method: http.MethodPost, Endpoint: "torrents/add", StatusCode: status}
	}
	return nil
}

// TorrentRow is one row of GET /api/v2/torrents/info. AutoTMM is a pointer
// so a missing field is distinguishable from false: ownership requires an
// explicit false.
type TorrentRow struct {
	Hash         string  `json:"hash"`
	InfoHashV1   string  `json:"infohash_v1"`
	InfoHashV2   string  `json:"infohash_v2"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	Category     string  `json:"category"`
	Tags         string  `json:"tags"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	DownloadPath string  `json:"download_path"`
	AutoTMM      *bool   `json:"auto_tmm"`
	Size         int64   `json:"size"`
}

// TorrentFile is one row of GET /api/v2/torrents/files.
type TorrentFile struct {
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Priority int     `json:"priority"`
}

func (c *Client) infoQuery(ctx context.Context, values url.Values) ([]TorrentRow, error) {
	if err := c.requireLogin(); err != nil {
		return nil, err
	}
	endpoint := "torrents/info"
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	status, body, _, err := c.do(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &ResponseError{Method: http.MethodGet, Endpoint: "torrents/info", StatusCode: status}
	}
	rows, err := decodeJSON[[]TorrentRow](body)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, ErrUnexpectedSchema
	}
	for _, row := range rows {
		if row.Hash == "" || row.State == "" || row.Progress < 0 || row.Progress > 1 {
			return nil, ErrUnexpectedSchema
		}
	}
	return rows, nil
}

// Info returns GET /api/v2/torrents/info, optionally filtered to the given
// hashes (joined with "|").
func (c *Client) Info(ctx context.Context, hashes ...string) ([]TorrentRow, error) {
	values := url.Values{}
	if len(hashes) > 0 {
		values.Set("hashes", strings.Join(hashes, "|"))
	}
	return c.infoQuery(ctx, values)
}

// InfoByTag returns GET /api/v2/torrents/info?tag=<tag>.
func (c *Client) InfoByTag(ctx context.Context, tag string) ([]TorrentRow, error) {
	values := url.Values{}
	values.Set("tag", tag)
	return c.infoQuery(ctx, values)
}

// Files returns GET /api/v2/torrents/files?hash=<hash>.
func (c *Client) Files(ctx context.Context, hash string) ([]TorrentFile, error) {
	if hash == "" {
		return nil, ErrInvalidRequest
	}
	if err := c.requireLogin(); err != nil {
		return nil, err
	}
	values := url.Values{}
	values.Set("hash", hash)
	status, body, _, err := c.do(ctx, http.MethodGet, "torrents/files?"+values.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &ResponseError{Method: http.MethodGet, Endpoint: "torrents/files", StatusCode: status}
	}
	files, err := decodeJSON[[]TorrentFile](body)
	if err != nil {
		return nil, err
	}
	if files == nil {
		return nil, ErrUnexpectedSchema
	}
	for _, file := range files {
		if file.Name == "" || file.Size < 0 || file.Progress < 0 || file.Progress > 1 {
			return nil, ErrUnexpectedSchema
		}
	}
	return files, nil
}

// Query returns the single row for a record, matched by client id or either
// infohash. It returns (nil, nil) when the client has no such torrent, an
// error when the server returns a row that does not match the record
// (unexpected identity) or more than one match (ambiguous identity).
func (c *Client) Query(ctx context.Context, rec Record) (*TorrentRow, error) {
	if rec.ClientID == "" {
		return nil, ErrInvalidRequest
	}
	rows, err := c.Info(ctx, rec.ClientID)
	if err != nil {
		return nil, err
	}
	hashes := map[string]bool{}
	for _, h := range []string{rec.InfoHashV1, rec.InfoHashV2, rec.ClientID} {
		if h != "" {
			hashes[strings.ToLower(h)] = true
		}
	}
	var matching []TorrentRow
	for _, row := range rows {
		if hashes[strings.ToLower(row.Hash)] || hashes[strings.ToLower(row.InfoHashV1)] || hashes[strings.ToLower(row.InfoHashV2)] {
			matching = append(matching, row)
		}
	}
	if len(matching) != len(rows) {
		return nil, fmt.Errorf("%w: client returned a torrent that does not match the record", ErrUnexpectedSchema)
	}
	if len(matching) > 1 {
		return nil, errors.New("qbittorrent: ambiguous client identity")
	}
	if len(matching) == 0 {
		return nil, nil
	}
	return &matching[0], nil
}

// Start starts a stopped torrent: POST /api/v2/torrents/start for >= 5.0.0,
// else POST /api/v2/torrents/resume.
func (c *Client) Start(ctx context.Context, hash string) error {
	if hash == "" {
		return ErrInvalidRequest
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return err
	}
	endpoint := "torrents/resume"
	if caps.InitialStopParameter == "stopped" {
		endpoint = "torrents/start"
	}
	return c.postForm(ctx, endpoint, url.Values{"hashes": {hash}})
}

// Pause stops one torrent without deleting it. qBittorrent 5.0 renamed the
// endpoint from torrents/pause to torrents/stop, so the version decides which
// one is called; the same probe Start uses.
func (c *Client) Pause(ctx context.Context, hash string) error {
	if hash == "" {
		return ErrInvalidRequest
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return err
	}
	endpoint := "torrents/pause"
	if caps.InitialStopParameter == "stopped" {
		endpoint = "torrents/stop"
	}
	return c.postForm(ctx, endpoint, url.Values{"hashes": {hash}})
}

// Delete removes the torrent from the client. deleteFiles is explicit: the
// library copy is never involved, and the download executor keeps its own
// receipts, so a deleted torrent can still be added again from the saved
// metainfo. Callers pass false to keep partial files for inspection.
func (c *Client) Delete(ctx context.Context, hash string, deleteFiles bool) error {
	if hash == "" {
		return ErrInvalidRequest
	}
	files := "false"
	if deleteFiles {
		files = "true"
	}
	return c.postForm(ctx, "torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {files}})
}

// Recheck asks the client to re-verify the torrent data.
func (c *Client) Recheck(ctx context.Context, hash string) error {
	if hash == "" {
		return ErrInvalidRequest
	}
	return c.postForm(ctx, "torrents/recheck", url.Values{"hashes": {hash}})
}

// SetLocation moves a torrent with POST /api/v2/torrents/setLocation.
func (c *Client) SetLocation(ctx context.Context, hash, location string) error {
	if hash == "" || location == "" {
		return ErrInvalidRequest
	}
	return c.postForm(ctx, "torrents/setLocation", url.Values{
		"hashes":   {hash},
		"location": {location},
	})
}

// SetDownloadPath moves a torrent's incomplete directory with
// POST /api/v2/torrents/setDownloadPath.
func (c *Client) SetDownloadPath(ctx context.Context, id, path string) error {
	if id == "" || path == "" {
		return ErrInvalidRequest
	}
	return c.postForm(ctx, "torrents/setDownloadPath", url.Values{
		"id":   {id},
		"path": {path},
	})
}

// AddTags adds tags with POST /api/v2/torrents/addTags.
func (c *Client) AddTags(ctx context.Context, hashes, tags string) error {
	if hashes == "" || tags == "" {
		return ErrInvalidRequest
	}
	return c.postForm(ctx, "torrents/addTags", url.Values{
		"hashes": {hashes},
		"tags":   {tags},
	})
}

// RemoveTags removes tags with POST /api/v2/torrents/removeTags.
func (c *Client) RemoveTags(ctx context.Context, hashes, tags string) error {
	if hashes == "" || tags == "" {
		return ErrInvalidRequest
	}
	return c.postForm(ctx, "torrents/removeTags", url.Values{
		"hashes": {hashes},
		"tags":   {tags},
	})
}

// DeleteTags deletes tags globally with POST /api/v2/torrents/deleteTags.
// Callers must confirm no torrent still uses the tag first; see
// NormalizeTags.
func (c *Client) DeleteTags(ctx context.Context, tags string) error {
	if tags == "" {
		return ErrInvalidRequest
	}
	return c.postForm(ctx, "torrents/deleteTags", url.Values{"tags": {tags}})
}
