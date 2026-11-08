// Package trackersearch is the read-only Gazelle and NexusPHP search client.
// It performs the same allowlisted GETs as the legacy provider and never
// requests a download, spends a token, or reports a file as imported.
package trackersearch

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// Hosts are the four tracker origins. NetEase is not one of them.
var Hosts = map[string]string{
	"open.cd":  "www.open.cd",
	"dicmusic": "dicmusic.com",
	"orpheus":  "orpheus.network",
	"redacted": "redacted.sh",
}

const maxBody = 10_000_000

var (
	positiveID = regexp.MustCompile(`^[1-9][0-9]*$`)
	logoutLink = regexp.MustCompile(`(?i)<a\b[^>]*\bhref\s*=\s*["'](?:https://www\.open\.cd/|/)?logout\.php(?:[?][^"']*)?["']`)
	emptyPage  = regexp.MustCompile(`(?i)没有种子|没有找到|沒有種子|没有匹配|no torrents|no results|nothing found`)
	sizeText   = regexp.MustCompile(`(?i)([0-9.]+)\s*(TiB|GiB|MiB|KiB|TB|GB|MB|KB)`)
	formatText = regexp.MustCompile(`(?i)\b(FLAC|ALAC|MP3|AAC|WAV|DSD)\b`)
	bitText    = regexp.MustCompile(`(?i)\b(16|24)\s*[- ]?\s*bit\b`)
)

// Query is one read-only search or preflight. Cookie is sent on the request
// and is not part of any returned observation.
type Query struct {
	Site      string
	Text      string
	Page      int
	Cookie    string
	Synthetic bool
	BaseURL   string
	Timeout   time.Duration
	Kind      string // search or preflight; empty means search
}

// Candidate is a search hit. Eligibility stays unknown and download stays blocked.
type Candidate struct {
	SchemaVersion      int                 `json:"schema_version"`
	CandidateID        string              `json:"candidate_id"`
	Source             string              `json:"source"`
	ResourceID         string              `json:"resource_id"`
	TorrentID          string              `json:"torrent_id"`
	Title              string              `json:"title"`
	Artists            []string            `json:"artists"`
	Aliases            []string            `json:"aliases,omitempty"`
	Eligibility        map[string]string   `json:"eligibility"`
	EstimatedCost      *int                `json:"estimated_cost"`
	Evidence           []map[string]string `json:"evidence"`
	EvidenceLevel      string              `json:"evidence_level"`
	RequiresReview     bool                `json:"requires_review"`
	DownloadStatus     string              `json:"download_status"`
	Downloaded         bool                `json:"downloaded"`
	ProductionVerified bool                `json:"production_verified"`
	FileList           []any               `json:"file_list"`
	GroupID            string              `json:"group_id,omitempty"`
	Release            map[string]any      `json:"release,omitempty"`
	Media              map[string]any      `json:"media,omitempty"`
	Seeders            *int                `json:"seeders"`
	SiteFlags          map[string]any      `json:"site_flags,omitempty"`
}

// Observation is the classified read. It is not a download result.
type Observation struct {
	Status             string         `json:"status"`
	Source             string         `json:"source"`
	Candidates         []Candidate    `json:"candidates"`
	Page               int            `json:"page,omitempty"`
	Pages              *int           `json:"pages,omitempty"`
	SearchComplete     bool           `json:"search_complete"`
	ReadOnly           bool           `json:"read_only"`
	DownloadStatus     string         `json:"download_status"`
	Downloaded         bool           `json:"downloaded"`
	ProductionVerified bool           `json:"production_verified"`
	OfficialHost       bool           `json:"official_host"`
	Synthetic          bool           `json:"synthetic"`
	ErrorClass         string         `json:"error_class,omitempty"`
	Reason             string         `json:"reason,omitempty"`
	RetryAfterSec      int            `json:"retry_after_sec,omitempty"`
	Authenticated      bool           `json:"authenticated,omitempty"`
	TokenBalance       *int           `json:"token_balance,omitempty"`
	ObservedAt         string         `json:"observed_at,omitempty"`
	Capabilities       map[string]any `json:"capabilities,omitempty"`
}

// Doer is the HTTP round trip. Production uses http.Client. Tests can observe the URL.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client performs allowlisted reads. A rate-limit block is process memory only.
type Client struct {
	Doer   Doer
	Now    func() time.Time
	mu     sync.Mutex
	blocks map[string]time.Time
}

func (c *Client) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) doer(timeout time.Duration) Doer {
	if c != nil && c.Doer != nil {
		return c.Doer
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// Search reads one page. It never calls a download endpoint.
func (c *Client) Search(ctx context.Context, q Query) (Observation, error) {
	q.Kind = "search"
	if q.Page == 0 {
		q.Page = 1
	}
	if _, ok := Hosts[q.Site]; !ok || q.Page < 1 || q.Page > 1000 {
		return Observation{}, fmt.Errorf("%w: site %s page %d", dataengine.ErrInvalidRequest, q.Site, q.Page)
	}
	if strings.TrimSpace(q.Text) == "" || len([]rune(q.Text)) > 300 {
		return c.finish(q, Observation{Status: "INVALID_INPUT", Reason: "检索词无效"}), nil
	}
	return c.roundTrip(ctx, q)
}

// Preflight checks the login read endpoint and returns a token count when the
// JSON exposes one. It does not reserve or spend that balance.
func (c *Client) Preflight(ctx context.Context, q Query) (Observation, error) {
	q.Kind = "preflight"
	q.Page = 1
	if _, ok := Hosts[q.Site]; !ok {
		return Observation{}, dataengine.ErrInvalidRequest
	}
	return c.roundTrip(ctx, q)
}

func (c *Client) roundTrip(ctx context.Context, q Query) (Observation, error) {
	if strings.ContainsAny(q.Cookie, "\r\n") {
		return Observation{}, fmt.Errorf("%w: cookie", dataengine.ErrInvalidRequest)
	}
	base := c.base(q)
	if strings.TrimSpace(q.Cookie) == "" {
		return base, nil
	}
	if until, ok := c.blocked(q.Site); ok {
		wait := int(until.Sub(c.now()).Seconds())
		if wait < 1 {
			wait = 1
		}
		out := base
		out.Status = "RATE_LIMITED"
		out.ErrorClass = "RATE_LIMITED"
		out.Reason = "站点仍在 Retry-After 等待期"
		out.RetryAfterSec = wait
		return out, nil
	}
	endpoint, err := endpointURL(q)
	if err != nil {
		return Observation{}, err
	}
	if q.Kind == "preflight" {
		if q.Site == "open.cd" {
			endpoint.Path = "/torrents.php"
		} else {
			endpoint.Path = "/ajax.php"
			endpoint.RawQuery = "action=index"
		}
	} else if q.Site == "open.cd" {
		values := url.Values{"search": {strings.TrimSpace(q.Text)}, "search_area": {"0"}, "search_mode": {"0"}}
		if q.Page > 1 {
			values.Set("page", strconv.Itoa(q.Page-1))
		}
		endpoint.Path = "/torrents.php"
		endpoint.RawQuery = values.Encode()
	} else {
		values := url.Values{"action": {"browse"}, "searchstr": {strings.TrimSpace(q.Text)}, "page": {strconv.Itoa(q.Page)}}
		endpoint.Path = "/ajax.php"
		endpoint.RawQuery = values.Encode()
	}
	if !allowedPath(q.Site, endpoint.Path, endpoint.Query()) {
		return Observation{}, fmt.Errorf("%w: path %s %s", dataengine.ErrInvalidRequest, endpoint.Path, endpoint.RawQuery)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Observation{}, err
	}
	req.Header.Set("Cookie", q.Cookie)
	if endpoint.Path == "/ajax.php" {
		req.Header.Set("Accept", "application/json")
	} else {
		req.Header.Set("Accept", "text/html")
	}
	if q.Synthetic {
		req.Header.Set("X-MusicOps-Site", q.Site)
	}
	timeout := q.Timeout
	response, err := c.doer(timeout).Do(req)
	if err != nil {
		out := base
		out.Status = "NETWORK_ERROR"
		out.ErrorClass = "NETWORK_ERROR"
		out.Reason = "连接失败"
		return out, nil
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		out := base
		out.Status = "NETWORK_ERROR"
		out.ErrorClass = "NETWORK_ERROR"
		out.Reason = "连接失败"
		return out, nil
	}
	if len(body) > maxBody {
		out := base
		out.Status = "PARSE_ERROR"
		out.ErrorClass = "PARSE_ERROR"
		out.Reason = "站点响应超过大小限制"
		return out, nil
	}
	if response.StatusCode == http.StatusTooManyRequests {
		delay := retryAfter(response.Header.Get("Retry-After"))
		c.block(q.Site, delay)
		out := base
		out.Status = "RATE_LIMITED"
		out.ErrorClass = "RATE_LIMITED"
		out.Reason = "站点限流；停止请求"
		out.RetryAfterSec = delay
		return out, nil
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		out := base
		out.Status = "AUTH_FAILED"
		out.ErrorClass = "AUTH_FAILED"
		out.Reason = "站点拒绝鉴权"
		return out, nil
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return c.redirect(base, q, response.Header.Get("Location")), nil
	}
	if response.StatusCode != http.StatusOK {
		out := base
		out.Status = "NETWORK_ERROR"
		out.ErrorClass = "NETWORK_ERROR"
		out.Reason = "站点请求失败"
		return out, nil
	}
	if q.Kind == "preflight" {
		return c.preflightBody(base, q, body), nil
	}
	if q.Site == "open.cd" {
		return c.nexus(base, q, string(body)), nil
	}
	return c.gazelle(base, q, body), nil
}

func (c *Client) base(q Query) Observation {
	return Observation{
		Status: "AUTH_REQUIRED", Source: q.Site, Candidates: []Candidate{}, ReadOnly: true,
		DownloadStatus: "POLICY_BLOCKED", OfficialHost: !q.Synthetic, Synthetic: q.Synthetic,
		ErrorClass: "AUTH_REQUIRED", Reason: "尚未提供该站 Cookie",
	}
}

func (c *Client) finish(q Query, out Observation) Observation {
	out.Source = q.Site
	if out.Candidates == nil {
		out.Candidates = []Candidate{}
	}
	out.ReadOnly = true
	out.DownloadStatus = "POLICY_BLOCKED"
	out.OfficialHost = !q.Synthetic
	out.Synthetic = q.Synthetic
	if out.ErrorClass == "" && out.Status != "OK" && out.Status != "NO_RESOURCE" {
		out.ErrorClass = out.Status
	}
	return out
}

func endpointURL(q Query) (*url.URL, error) {
	if q.Synthetic {
		parsed, err := url.Parse(q.BaseURL)
		if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.User != nil {
			return nil, fmt.Errorf("%w: synthetic base %q", dataengine.ErrInvalidRequest, q.BaseURL)
		}
		return &url.URL{Scheme: "http", Host: parsed.Host}, nil
	}
	host := Hosts[q.Site]
	return &url.URL{Scheme: "https", Host: host}, nil
}

func allowedPath(site, path string, values url.Values) bool {
	keys := map[string]bool{}
	for key := range values {
		keys[key] = true
	}
	if site == "open.cd" && path == "/torrents.php" {
		for key := range keys {
			switch key {
			case "search", "search_area", "search_mode", "page":
			default:
				return false
			}
		}
		return true
	}
	if site != "open.cd" && path == "/ajax.php" {
		action := values.Get("action")
		if action != "index" && action != "browse" {
			return false
		}
		for key := range keys {
			switch key {
			case "action", "searchstr", "page", "artistname":
			default:
				return false
			}
		}
		return true
	}
	return false
}

func retryAfter(raw string) int {
	if raw == "" {
		return 60
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 60
	}
	if value < 1 {
		return 1
	}
	if value > 1800 {
		return 1800
	}
	return int(value)
}

func (c *Client) block(site string, seconds int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocks == nil {
		c.blocks = map[string]time.Time{}
	}
	c.blocks[site] = c.now().Add(time.Duration(seconds) * time.Second)
}

func (c *Client) blocked(site string) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.blocks[site]
	if !ok || !until.After(c.now()) {
		return time.Time{}, false
	}
	return until, true
}

func (c *Client) redirect(base Observation, q Query, location string) Observation {
	out := base
	parsed, err := url.Parse(location)
	login := err == nil && parsed.Path == "/login.php"
	if login {
		out.Status = "AUTH_FAILED"
		out.Reason = "重定向到登录页，未转发 Cookie"
	} else {
		out.Status = "ACCESS_BLOCKED"
		out.Reason = "响应来源或路径改变"
	}
	out.ErrorClass = out.Status
	return out
}

func (c *Client) preflightBody(base Observation, q Query, body []byte) Observation {
	out := c.finish(q, Observation{Status: "OK", Authenticated: true, ObservedAt: c.now().UTC().Format(time.RFC3339Nano)})
	out.Capabilities = capabilities(q.Site)
	if q.Site == "open.cd" {
		if !logoutLink.Match(body) {
			failed := base
			failed.Status = "AUTH_FAILED"
			failed.ErrorClass = "AUTH_FAILED"
			failed.Reason = "未找到已登录证据"
			return failed
		}
		return out
	}
	payload, status := gazelleObject(body)
	if status != "" {
		failed := base
		failed.Status = status
		failed.ErrorClass = status
		failed.Reason = "只读预检未通过"
		return failed
	}
	if stringValue(payload["id"]) == "" || stringValue(payload["username"]) == "" {
		failed := base
		failed.Status = "PARSE_ERROR"
		failed.ErrorClass = "PARSE_ERROR"
		failed.Reason = "缺少登录证据"
		return failed
	}
	if balance, ok := tokenBalance(payload); ok {
		out.TokenBalance = &balance
	}
	return out
}

func capabilities(site string) map[string]any {
	contract := "gazelle-readonly.v1"
	if site == "open.cd" {
		contract = "nexus-readonly.v1"
	}
	return map[string]any{
		"status": "OK", "source": site, "contract": contract, "search": true,
		"check_eligibility": false, "fetch_asset": false, "reconcile": false,
		"reliable_free_confirmation": false, "token_required": site != "open.cd",
		"reason": "只读登录/检索已接入；令牌成本、免费生效与保种规则仍须确认",
	}
}

func tokenBalance(payload map[string]any) (int, bool) {
	for _, item := range []struct{ block, key string }{{"personal", "freeleechTokens"}, {"stats", "tokens"}, {"userstats", "tokens"}} {
		raw, _ := payload[item.block].(map[string]any)
		if raw == nil {
			continue
		}
		if value, ok := nonNegative(raw[item.key]); ok {
			return value, true
		}
	}
	return 0, false
}

func (c *Client) gazelle(base Observation, q Query, body []byte) Observation {
	payload, status := gazelleObject(body)
	if status != "" {
		failed := c.finish(q, Observation{Status: status, Reason: "API失败或响应结构改变"})
		return failed
	}
	results, ok := payload["results"].([]any)
	if !ok {
		return c.finish(q, Observation{Status: "PARSE_ERROR", Reason: "缺少 results 数组"})
	}
	candidates := []Candidate{}
	for _, item := range results {
		group, ok := item.(map[string]any)
		if !ok {
			return c.finish(q, Observation{Status: "PARSE_ERROR", Reason: "分组结构改变"})
		}
		rows, err := groupCandidates(q, group)
		if err != nil {
			return c.finish(q, Observation{Status: "PARSE_ERROR", Reason: "分组结构改变"})
		}
		candidates = append(candidates, rows...)
	}
	page, pages, complete, ok := gazellePages(payload, q.Page, len(results) == 0)
	if !ok {
		return c.finish(q, Observation{Status: "PARSE_ERROR", Reason: "分页结构改变"})
	}
	status = "NO_RESOURCE"
	if len(candidates) > 0 {
		status = "OK"
	}
	return c.finish(q, Observation{Status: status, Candidates: candidates, Page: page, Pages: pages, SearchComplete: complete})
}

func gazelleObject(body []byte) (map[string]any, string) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var data map[string]any
	if err := decoder.Decode(&data); err != nil {
		return nil, "ACCESS_BLOCKED"
	}
	if data["status"] != "success" {
		return nil, "API_CONTRACT_ERROR"
	}
	payload, ok := data["response"].(map[string]any)
	if !ok {
		return nil, "API_CONTRACT_ERROR"
	}
	return payload, ""
}

func gazellePages(payload map[string]any, requested int, empty bool) (int, *int, bool, bool) {
	page := 1
	if raw, ok := payload["currentPage"]; ok && raw != nil {
		parsed, good := integer(raw)
		if !good {
			return 0, nil, false, false
		}
		page = parsed
	}
	var pages *int
	if raw, ok := payload["pages"]; ok && raw != nil {
		parsed, good := integer(raw)
		if !good {
			return 0, nil, false, false
		}
		pages = &parsed
	}
	if empty && page == 1 && (pages == nil || *pages == 0) {
		one := 1
		return 1, &one, true, true
	}
	if page != requested || pages != nil && *pages < page {
		return 0, nil, false, false
	}
	complete := pages != nil && *pages == page
	return page, pages, complete, true
}

func groupCandidates(q Query, group map[string]any) ([]Candidate, error) {
	if _, ok := group["torrents"]; !ok {
		if stringValue(group["torrentId"]) != "" && stringValue(group["groupId"]) != "" {
			category, _ := group["category"].(string)
			if category != "" && !strings.EqualFold(category, "music") {
				if _, err := requireID(group["torrentId"]); err != nil {
					return nil, err
				}
				if _, err := requireID(group["groupId"]); err != nil {
					return nil, err
				}
				return nil, nil
			}
		}
		return nil, errShape
	}
	torrents, ok := group["torrents"].([]any)
	if !ok {
		return nil, errShape
	}
	groupID, err := requireID(group["groupId"])
	if err != nil {
		return nil, err
	}
	artist, ok := group["artist"].(string)
	if !ok && group["artist"] != nil {
		return nil, errShape
	}
	title, ok := group["groupName"].(string)
	if !ok {
		return nil, errShape
	}
	out := []Candidate{}
	for _, item := range torrents {
		torrent, ok := item.(map[string]any)
		if !ok {
			return nil, errShape
		}
		artists, err := artistNames(torrent["artists"], artist)
		if err != nil {
			return nil, err
		}
		row, err := baseCandidate(q, torrent["torrentId"], title, artists)
		if err != nil {
			return nil, err
		}
		row.GroupID = groupID
		row.Aliases = optionalStrings(group["aliases"])
		seeders := 0
		if raw, ok := torrent["seeders"]; ok {
			parsed, good := integer(raw)
			if !good {
				return nil, errShape
			}
			seeders = parsed
		}
		row.Seeders = &seeders
		row.Release = map[string]any{
			"album": row.Title, "year": numberOrRaw(group["groupYear"]),
			"edition":            unescape(stringOr(torrent["remasterTitle"], "unknown")),
			"complete_tracklist": false, "release_type": numberOrRaw(group["releaseType"]),
		}
		row.Media = map[string]any{}
		for _, key := range []string{"format", "encoding", "media", "fileCount", "size"} {
			if value, ok := torrent[key]; ok {
				row.Media[key] = numberOrRaw(value)
			}
		}
		row.SiteFlags = map[string]any{}
		for _, key := range []string{"isFreeleech", "isPersonalFreeleech", "isNeutralLeech", "canUseToken"} {
			if value, ok := torrent[key]; ok {
				row.SiteFlags[key] = value
			}
		}
		out = append(out, row)
	}
	return out, nil
}

var errShape = errString("shape")

type errString string

func (e errString) Error() string { return string(e) }

func optionalStrings(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	names := []string{}
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			continue
		}
		text = strings.TrimSpace(unescape(text))
		if text != "" {
			names = append(names, text)
		}
	}
	return names
}

func artistNames(raw any, fallback string) ([]string, error) {
	list, ok := raw.([]any)
	if ok && len(list) > 0 {
		names := []string{}
		seen := map[string]bool{}
		for _, item := range list {
			credit, ok := item.(map[string]any)
			name, nameOK := credit["name"].(string)
			if !ok || !nameOK || strings.TrimSpace(name) == "" {
				return nil, errShape
			}
			name = unescape(name)
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		return names, nil
	}
	if strings.TrimSpace(fallback) == "" {
		return []string{}, nil
	}
	return []string{unescape(fallback)}, nil
}

func baseCandidate(q Query, id any, title string, artists []string) (Candidate, error) {
	resource, err := requireID(id)
	if err != nil {
		return Candidate{}, err
	}
	title = strings.TrimSpace(unescape(title))
	if title == "" {
		return Candidate{}, errShape
	}
	level := "live_readonly"
	if q.Synthetic {
		level = "synthetic_fixture"
	}
	return Candidate{
		SchemaVersion: 1, CandidateID: q.Site + ":" + resource, Source: q.Site, ResourceID: resource, TorrentID: resource,
		Title: title, Artists: artists, Eligibility: map[string]string{"state": "UNKNOWN"},
		Evidence: []map[string]string{{"kind": "authenticated_search"}}, EvidenceLevel: level,
		RequiresReview: true, DownloadStatus: "POLICY_BLOCKED", FileList: []any{},
	}, nil
}

func (c *Client) nexus(base Observation, q Query, text string) Observation {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "cf-chl-") || strings.Contains(lower, "just a moment") {
		return c.finish(q, Observation{Status: "ACCESS_BLOCKED", Reason: "站点风控页面"})
	}
	if !logoutLink.MatchString(text) {
		return c.finish(q, Observation{Status: "AUTH_FAILED", Reason: "未找到已登录证据"})
	}
	rows, err := nexusRows(text)
	if err != nil {
		return c.finish(q, Observation{Status: "PARSE_ERROR", Reason: "站点页面暂时无法解析"})
	}
	if len(rows) == 0 && !emptyPage.MatchString(text) {
		return c.finish(q, Observation{Status: "PARSE_ERROR", Reason: "无法确认空结果结构"})
	}
	candidates := []Candidate{}
	index := map[string]int{}
	for _, row := range rows {
		item, err := baseCandidate(q, row.id, row.title, []string{})
		if err != nil {
			return c.finish(q, Observation{Status: "PARSE_ERROR", Reason: "站点页面暂时无法解析"})
		}
		item.Release = map[string]any{"complete_tracklist": false, "edition": "unknown"}
		item.Media = map[string]any{}
		if size := sizeText.FindString(row.text); size != "" {
			item.Media["size_display"] = size
		}
		if format := formatText.FindStringSubmatch(item.Title); format != nil {
			item.Media["format"] = strings.ToUpper(format[1])
		}
		if bits := bitText.FindStringSubmatch(item.Title); bits != nil {
			item.Media["encoding"] = bits[1] + "bit Lossless"
		}
		if pos, ok := index[item.CandidateID]; ok {
			candidates[pos] = item
			continue
		}
		index[item.CandidateID] = len(candidates)
		candidates = append(candidates, item)
	}
	status := "NO_RESOURCE"
	if len(candidates) > 0 {
		status = "OK"
	}
	page := q.Page
	return c.finish(q, Observation{Status: status, Candidates: candidates, Page: page, Pages: &page, SearchComplete: true})
}

type nexusRow struct{ id, title, text string }

func nexusRows(text string) ([]nexusRow, error) {
	type frame struct {
		titles []nexusRow
		parts  []string
	}
	var frames []frame
	var anchor *struct {
		id, title string
		text      []string
	}
	rows := []nexusRow{}
	for i := 0; i < len(text); {
		if text[i] != '<' {
			end := strings.IndexByte(text[i:], '<')
			if end < 0 {
				end = len(text) - i
			}
			chunk := html.UnescapeString(text[i : i+end])
			for n := range frames {
				frames[n].parts = append(frames[n].parts, chunk)
			}
			if anchor != nil {
				anchor.text = append(anchor.text, chunk)
			}
			i += end
			continue
		}
		end := quotedEnd(text, i)
		if end < 0 {
			return nil, errShape
		}
		tag := text[i : end+1]
		i = end + 1
		name, kind, attrs := splitTag(tag)
		switch kind {
		case "start":
			if name == "tr" {
				if len(frames) >= 64 {
					return nil, errShape
				}
				frames = append(frames, frame{})
			}
			if name == "a" && len(frames) > 0 {
				if id, title, ok := detailLink(attrs); ok {
					anchor = &struct {
						id, title string
						text      []string
					}{id: id, title: title}
				}
			}
		case "end":
			if name == "a" && anchor != nil {
				visible := strings.Join(strings.Fields(strings.Join(anchor.text, " ")), " ")
				title := anchor.title
				if title == "" {
					title = visible
				}
				if title != "" || visible != "" {
					for n := range frames {
						frames[n].titles = append(frames[n].titles, nexusRow{id: anchor.id, title: title})
					}
				}
				anchor = nil
			}
			if name == "tr" && len(frames) > 0 {
				frame := frames[len(frames)-1]
				frames = frames[:len(frames)-1]
				ids := map[string]bool{}
				for _, title := range frame.titles {
					ids[title.id] = true
				}
				if len(frame.titles) > 0 && len(ids) == 1 {
					rows = append(rows, nexusRow{id: frame.titles[0].id, title: frame.titles[0].title, text: strings.Join(strings.Fields(strings.Join(frame.parts, " ")), " ")})
				}
			}
		}
	}
	return rows, nil
}

func quotedEnd(text string, start int) int {
	quote := byte(0)
	for i := start + 1; i < len(text); i++ {
		if quote != 0 {
			if text[i] == quote {
				quote = 0
			}
			continue
		}
		if text[i] == '"' || text[i] == '\'' {
			quote = text[i]
			continue
		}
		if text[i] == '>' {
			return i
		}
	}
	return -1
}

func splitTag(tag string) (string, string, map[string]string) {
	body := strings.Trim(tag, "<>")
	kind := "start"
	if strings.HasPrefix(body, "/") {
		kind = "end"
		body = body[1:]
	}
	if strings.HasSuffix(body, "/") {
		body = strings.TrimSuffix(body, "/")
	}
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return "", kind, nil
	}
	attrs := map[string]string{}
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		attrs[strings.ToLower(key)] = strings.Trim(html.UnescapeString(value), `"'`)
	}
	return strings.ToLower(fields[0]), kind, attrs
}

func detailLink(attrs map[string]string) (string, string, bool) {
	parsed, err := url.Parse(attrs["href"])
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "", "", false
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	if path != "details.php" && path != "plugin_details.php" {
		return "", "", false
	}
	ids := parsed.Query()["id"]
	if len(ids) != 1 || !positiveID.MatchString(ids[0]) {
		return "", "", false
	}
	return ids[0], attrs["title"], true
}

func requireID(value any) (string, error) {
	text := stringValue(value)
	if !positiveID.MatchString(text) {
		return "", errShape
	}
	return text, nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}

func stringOr(value any, fallback string) string {
	text, _ := value.(string)
	if strings.TrimSpace(text) == "" {
		return fallback
	}
	return text
}

func unescape(value string) string { return html.UnescapeString(value) }

func integer(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil || parsed > int64(^uint(0)>>1) {
			return 0, false
		}
		return int(parsed), true
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(typed)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func nonNegative(value any) (int, bool) {
	parsed, ok := integer(value)
	return parsed, ok && parsed >= 0
}

func numberOrRaw(value any) any {
	if parsed, ok := integer(value); ok {
		return parsed
	}
	return value
}
