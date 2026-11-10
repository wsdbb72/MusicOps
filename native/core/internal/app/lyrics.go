// Lyrics and cover service settings, and the Discogs token.
//
// Python SetupWizard page 6 (providers/lrcapi.py) sets the lyrics
// service: base_url, lyrics_path, candidates_path, cover_path,
// auth_mode 'raw', auth_header, and the authorization value in the Vault
// under vault://lyrics/default/authorization. The Go rules mirror it:
//
//   - the primary service must be HTTPS; paths must start with "/",
//     must not start with "//", and must not contain "? #://..";
//   - auth_header must be a token; the authorization value is the raw
//     original value — never stripped, never given a "Bearer " prefix —
//     and must not contain CR, LF or NUL;
//   - the connection test is Python's connection_tests lyrics kind:
//     candidates("童年", "罗大佑", "之乎者也") — GET
//     {base_url}{candidates_path}?title=...&artist=...&album=..., Accept
//     application/json, redirects refused. The response must be a JSON
//     array whose items carry id/title/artist/lyrics, the lyrics must
//     parse (lrc.Parse), and the identity must match the query the way
//     Python's _identity does (normalized title and artist set equal,
//     album equal when both sides have one). With no saved authorization
//     the test returns NOT_CONFIGURED without sending a request.
//
// Honest limit: these settings are stored and testable, but Go has no
// lyrics-fetch client — the lrc package only reads and writes local LRC
// files — so saving them changes no fetch behaviour yet. The Discogs
// token is the same: stored and testable, but identify still reports
// Discogs unavailable until a real Discogs client exists. Both limits
// are recorded in MIGRATION_PLAN.md.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
)

// lyricsSettingsKey is the persisted lyrics/cover service configuration.
// The authorization value lives in the Vault; this document holds only
// the addresses, paths, header name and whether a value is saved.
var lyricsSettingsKey = []byte("app/settings/lyrics")

// lyricsAuthRef is the Vault reference for the lyrics authorization
// value, host-bound to the lyrics service hostname.
const lyricsAuthRef = "vault://lyrics/default/authorization"

// lyricsSettings mirrors Python's lyrics.primary configuration.
type lyricsSettings struct {
	BaseURL        string `json:"base_url"`
	LyricsPath     string `json:"lyrics_path"`
	CandidatesPath string `json:"candidates_path"`
	CoverPath      string `json:"cover_path"`
	AuthHeader     string `json:"auth_header"`
	WriteMode      string `json:"write_mode,omitempty"`
	UpdatedNS      int64  `json:"updated_ns"`
}

// defaultLyricsSettings is Python's lyrics.primary default. The default
// base_url is a placeholder that refuses real use until the user sets a
// service address.
func defaultLyricsSettings() lyricsSettings {
	return lyricsSettings{
		BaseURL: "https://lyrics.example.invalid", LyricsPath: "/lyrics",
		CandidatesPath: "/jsonapi", CoverPath: "/cover", AuthHeader: "Authorization",
		WriteMode: "sidecar",
	}
}

var tokenPattern = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

func (a *App) loadLyricsSettings() (lyricsSettings, error) {
	var out lyricsSettings
	found, err := a.loadJSONSetting(lyricsSettingsKey, &out)
	if err != nil {
		return lyricsSettings{}, err
	}
	if !found {
		return defaultLyricsSettings(), nil
	}
	return out, nil
}

func (a *App) saveLyricsSettings(in lyricsSettings) (lyricsSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return lyricsSettings{}, err
	}
	if err := a.casSaveJSON(lyricsSettingsKey, encoded); err != nil {
		return lyricsSettings{}, err
	}
	return in, nil
}

// validateLyricsPath checks one lyrics service path the Python way.
func validateLyricsPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("路径须以 / 开头且不得以 // 开头")
	}
	if strings.ContainsAny(path, "?#") || strings.Contains(path, "://") || strings.Contains(path, "..") {
		return errors.New("路径不得包含 ? # :// 或 ..")
	}
	return nil
}

func validateLyricsSettings(in lyricsSettings) error {
	cleaned, _, err := cleanURL(in.BaseURL, true)
	if err != nil || cleaned == "" {
		return errors.New("歌词服务地址须为干净的 HTTPS 地址")
	}
	for _, path := range []string{in.LyricsPath, in.CandidatesPath, in.CoverPath} {
		if err := validateLyricsPath(path); err != nil {
			return err
		}
	}
	if !tokenPattern.MatchString(in.AuthHeader) {
		return errors.New("鉴权请求头名无效")
	}
	if in.WriteMode != "" && in.WriteMode != "sidecar" && in.WriteMode != "embedded" {
		return errors.New("资源写入模式仅支持 sidecar 或 embedded")
	}
	return nil
}

// lyricsAuthSaved reports whether a lyrics authorization value is stored
// for the saved service host, and its vault revision.
func (a *App) lyricsAuthSaved(settings lyricsSettings) (bool, int) {
	if a.vault == nil || settings.BaseURL == "" {
		return false, 0
	}
	_, host, err := cleanURL(settings.BaseURL, true)
	if err != nil {
		return false, 0
	}
	_, found, err := a.vault.Get(lyricsAuthRef, host)
	if err != nil || !found {
		return false, 0
	}
	return true, a.vault.Revision(lyricsAuthRef)
}

// lyricsSettingsHandler returns the saved lyrics configuration and auth state.
func (a *App) lyricsSettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadLyricsSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	saved, revision := a.lyricsAuthSaved(settings)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lyrics": map[string]any{
		"base_url": settings.BaseURL, "lyrics_path": settings.LyricsPath,
		"candidates_path": settings.CandidatesPath, "cover_path": settings.CoverPath,
		"auth_header": settings.AuthHeader, "write_mode": coverWriteMode(settings.WriteMode, settings),
		"auth_saved": saved, "auth_revision": revision,
	}})
}

// saveLyrics stores the lyrics settings. The authorization value follows
// the Python wizard's blank-keeps rule; it is the raw original value,
// never stripped and never given a "Bearer " prefix.
func (a *App) saveLyrics(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL        string `json:"base_url"`
		LyricsPath     string `json:"lyrics_path"`
		CandidatesPath string `json:"candidates_path"`
		CoverPath      string `json:"cover_path"`
		AuthHeader     string `json:"auth_header"`
		WriteMode      string `json:"write_mode"`
		Authorization  string `json:"authorization"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Authorization, "\r\n\x00") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	previous, err := a.loadLyricsSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	next := previous
	if body.BaseURL != "" {
		next.BaseURL = strings.TrimSpace(body.BaseURL)
	}
	if body.LyricsPath != "" {
		next.LyricsPath = body.LyricsPath
	}
	if body.CandidatesPath != "" {
		next.CandidatesPath = body.CandidatesPath
	}
	if body.CoverPath != "" {
		next.CoverPath = body.CoverPath
	}
	if body.AuthHeader != "" {
		next.AuthHeader = body.AuthHeader
	}
	if body.WriteMode == "sidecar" || body.WriteMode == "embedded" {
		next.WriteMode = body.WriteMode
	}
	if err := validateLyricsSettings(next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	if a.vault == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if body.Authorization != "" {
		_, host, err := cleanURL(next.BaseURL, true)
		if err != nil || host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "保存鉴权值需要先填歌词服务地址"})
			return
		}
		if _, err := a.vault.Set(lyricsAuthRef, body.Authorization, host); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
	}
	saved, err := a.saveLyricsSettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	authSaved, revision := a.lyricsAuthSaved(saved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lyrics": map[string]any{
		"base_url": saved.BaseURL, "lyrics_path": saved.LyricsPath,
		"candidates_path": saved.CandidatesPath, "cover_path": saved.CoverPath,
		"auth_header": saved.AuthHeader, "write_mode": coverWriteMode(saved.WriteMode, saved),
		"auth_saved": authSaved, "auth_revision": revision,
	}})
}

// normalizeIdentity folds a title or artist the way Python's _normalized
// does: trim, collapse whitespace to one space, casefold.
func normalizeIdentity(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// normalizeLRCText drops the metadata lines Python's normalize_lrc
// removes ([hash|sign|qq|total]: ...), so the parser sees the timed lines.
func normalizeLRCText(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "[hash:") || strings.HasPrefix(lower, "[sign:") ||
			strings.HasPrefix(lower, "[qq:") || strings.HasPrefix(lower, "[total:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// lyricCandidate is one item of a lyrics candidates response, the shape
// Python's _parse requires for kind=candidates.
type lyricCandidate struct {
	ID      any    `json:"id"`
	Title   string `json:"title"`
	Artist  any    `json:"artist"`
	Lyrics  string `json:"lyrics"`
	Album   string `json:"album"`
	Version string `json:"version"`
}

// candidateArtists returns the artist names of a candidate item.
func (c lyricCandidate) candidateArtists() []string {
	switch v := c.Artist.(type) {
	case string:
		return []string{v}
	case []any:
		out := []string{}
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

// identityMatches reports whether a candidate matches the query the way
// Python's _identity does: normalized title and artist set equal, and
// the album equal when both sides have one.
func (c lyricCandidate) identityMatches(title, artist, album string) bool {
	if normalizeIdentity(c.Title) != normalizeIdentity(title) {
		return false
	}
	artists := c.candidateArtists()
	if artists == nil {
		return false
	}
	want := map[string]struct{}{normalizeIdentity(artist): {}}
	got := map[string]struct{}{}
	for _, name := range artists {
		got[normalizeIdentity(name)] = struct{}{}
	}
	if len(got) != len(want) {
		return false
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			return false
		}
	}
	if c.Album != "" && album != "" && normalizeIdentity(c.Album) != normalizeIdentity(album) {
		return false
	}
	return true
}

// testLyrics runs the lyrics connection test: candidates("童年", "罗大佑",
// "之乎者也") against the saved or overridden service. Body fields
// override the saved settings for this one test only; an address change
// without a fresh authorization is refused. With no saved authorization
// the test returns NOT_CONFIGURED without sending a request.
func (a *App) testLyrics(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL       string `json:"base_url"`
		Authorization string `json:"authorization"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Authorization, "\r\n\x00") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	saved, err := a.loadLyricsSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	baseURL := saved.BaseURL
	if body.BaseURL != "" {
		baseURL = strings.TrimSpace(body.BaseURL)
	}
	if _, _, err := cleanURL(baseURL, true); err != nil || baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "歌词服务地址须为干净的 HTTPS 地址"})
		return
	}
	if body.BaseURL != "" && body.BaseURL != saved.BaseURL && body.Authorization == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "地址已改变，请填写鉴权值"})
		return
	}
	authorization := body.Authorization
	if authorization == "" {
		if a.vault == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		_, host, _ := cleanURL(baseURL, true)
		stored, found, err := a.vault.Get(lyricsAuthRef, host)
		if err != nil || !found || stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "NOT_CONFIGURED", "error": "尚未保存该地址的鉴权值"})
			return
		}
		authorization = stored
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + saved.CandidatesPath
	query := url.Values{}
	query.Set("title", "童年")
	query.Set("artist", "罗大佑")
	query.Set("album", "之乎者也")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "LYRICS_UNAVAILABLE", "error": err.Error()})
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(saved.AuthHeader, authorization)
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "LYRICS_UNAVAILABLE", "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "AUTH_FAILED", "error": "歌词服务拒绝了鉴权值"})
		return
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "RATE_LIMITED", "error": "歌词服务限流"})
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "LYRICS_UNAVAILABLE", "error": fmt.Sprintf("歌词服务返回 %d", resp.StatusCode)})
		return
	}
	limited := io.LimitReader(resp.Body, 10_000_000)
	var items []lyricCandidate
	if err := json.NewDecoder(limited).Decode(&items); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "LYRICS_UNAVAILABLE", "error": "歌词服务返回无法解析"})
		return
	}
	valid := 0
	for _, item := range items {
		id, ok := item.ID.(string)
		if !ok {
			if num, isNum := item.ID.(float64); isNum {
				id = fmt.Sprintf("%v", num)
			}
		}
		if strings.TrimSpace(id) == "" || item.Title == "" || item.Lyrics == "" {
			continue
		}
		if _, err := lrc.Parse(normalizeLRCText(item.Lyrics)); err != nil {
			continue
		}
		if !item.identityMatches("童年", "罗大佑", "之乎者也") {
			continue
		}
		valid++
	}
	if valid == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "LYRICS_UNAVAILABLE", "error": "歌词服务未返回可用的候选歌词"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "歌词服务连接成功", "candidates": len(items), "valid": valid})
}

// discogs.go: the Discogs token. Python's catalog.discogs stores the
// token in the Vault under vault://catalog/discogs/token, host-bound to
// api.discogs.com, and searches /database/search with
// "Authorization: Discogs token=<token>". The token must match
// [A-Za-z0-9_-]{8,256}; without one, identify reports AUTH_REQUIRED.

// discogsTokenRef is the Vault reference for the Discogs token.
const discogsTokenRef = "vault://catalog/discogs/token"

var discogsTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,256}$`)

// discogsTokenSaved reports whether a Discogs token is stored, and its
// vault revision.
func (a *App) discogsTokenSaved() (bool, int) {
	if a.vault == nil {
		return false, 0
	}
	_, found, err := a.vault.Get(discogsTokenRef, "api.discogs.com")
	if err != nil || !found {
		return false, 0
	}
	return true, a.vault.Revision(discogsTokenRef)
}

// discogsSettingsHandler returns the saved Discogs token state.
func (a *App) discogsSettingsHandler(w http.ResponseWriter, r *http.Request) {
	saved, revision := a.discogsTokenSaved()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "discogs": map[string]any{
		"token_saved": saved, "token_revision": revision,
	}})
}

// saveDiscogs stores the Discogs token. Blank keeps the saved token.
func (a *App) saveDiscogs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Token, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.Token != "" {
		if !discogsTokenPattern.MatchString(body.Token) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "Discogs Token 须为 8-256 位字母、数字、下划线或连字符"})
			return
		}
		if a.vault == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		if _, err := a.vault.Set(discogsTokenRef, body.Token, "api.discogs.com"); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
	}
	saved, revision := a.discogsTokenSaved()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "discogs": map[string]any{
		"token_saved": saved, "token_revision": revision,
	}})
}

// testDiscogs checks a Discogs token against the real search endpoint,
// GET https://api.discogs.com/database/search with
// "Authorization: Discogs token=<token>". A body token overrides the
// saved one for this one test only. The base_url override exists only so
// tests can point at a local server; it is never stored.
func (a *App) testDiscogs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Token, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	token := body.Token
	if token == "" {
		if a.vault == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		stored, found, err := a.vault.Get(discogsTokenRef, "api.discogs.com")
		if err != nil || !found || stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "DISCOGS_NOT_CONFIGURED", "error": "尚未保存 Discogs Token"})
			return
		}
		token = stored
	}
	baseURL := "https://api.discogs.com"
	if body.BaseURL != "" {
		baseURL = strings.TrimSpace(body.BaseURL)
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/database/search"
	query := url.Values{}
	query.Set("q", "test")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "DISCOGS_UNAVAILABLE", "error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Discogs token="+token)
	req.Header.Set("Accept", "application/json")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "DISCOGS_UNAVAILABLE", "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "AUTH_FAILED", "error": "Discogs 拒绝了该 Token"})
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "DISCOGS_UNAVAILABLE", "error": fmt.Sprintf("Discogs 返回 %d", resp.StatusCode)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Discogs Token 可用"})
}
