// Saved qBittorrent settings.
//
// Python SetupWizard page 4 carries the qBittorrent block (base_url,
// username + password, test-saved-connection) and config/schema.py stores
// it as qbt {base_url, credential_ref, category, tag_prefix,
// poll_seconds}. The Go download lane until now took the login per call
// (POST /v1/jobs/{id}/download) and kept it in memory for one attempt
// only. That per-attempt rule stays: the handler still remembers whatever
// login the download runs with in memory and drops it when the attempt
// ends. What changes is where the login may come from — the saved settings
// below — so a download no longer needs the password re-typed every time.
//
// The password lives in the Vault as JSON {"username","password"} under
// vault://user/qbt/<sha256(host)[:12]>, the ref Python's setup.py secret()
// builds, bound to the qBittorrent host. The Pebble document holds only
// the address, category and tags, plus whether a login is saved.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
)

// qbittorrentSettingsKey is the persisted qBittorrent address and defaults.
var qbittorrentSettingsKey = []byte("app/settings/qbittorrent")

// qbittorrentSettings is the persisted qBittorrent configuration. The
// login itself is in the Vault; LoginSaved only says one is stored.
type qbittorrentSettings struct {
	BaseURL   string `json:"base_url"`
	Category  string `json:"category"`
	Tags      string `json:"tags"`
	UpdatedNS int64  `json:"updated_ns"`
}

func (a *App) loadQBittorrentSettings() (qbittorrentSettings, error) {
	var out qbittorrentSettings
	found, err := a.loadJSONSetting(qbittorrentSettingsKey, &out)
	if err != nil {
		return qbittorrentSettings{}, err
	}
	if !found {
		return qbittorrentSettings{}, nil
	}
	if out.Category == "" {
		out.Category = qbittorrent.DefaultCategory
	}
	if out.Tags == "" {
		out.Tags = qbittorrent.ProjectTag
	}
	return out, nil
}

func (a *App) saveQBittorrentSettings(in qbittorrentSettings) (qbittorrentSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return qbittorrentSettings{}, err
	}
	if err := a.casSaveJSON(qbittorrentSettingsKey, encoded); err != nil {
		return qbittorrentSettings{}, err
	}
	return in, nil
}

// qbtLogin is the Vault JSON for a saved qBittorrent login.
type qbtLogin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// savedQBittorrentLogin reads the saved login for the saved address. The
// login is bound to that address's host: it is never applied to a
// different address.
func (a *App) savedQBittorrentLogin() (qbtLogin, string, bool) {
	if a.vault == nil {
		return qbtLogin{}, "", false
	}
	settings, err := a.loadQBittorrentSettings()
	if err != nil || settings.BaseURL == "" {
		return qbtLogin{}, "", false
	}
	_, host, err := cleanOrigin(settings.BaseURL)
	if err != nil {
		return qbtLogin{}, "", false
	}
	raw, found, err := a.vault.Get(vaultUserRef("qbt", host), host)
	if err != nil || !found || raw == "" {
		return qbtLogin{}, "", false
	}
	var login qbtLogin
	if err := json.Unmarshal([]byte(raw), &login); err != nil || login.Username == "" || login.Password == "" {
		return qbtLogin{}, "", false
	}
	return login, settings.BaseURL, true
}

// qbtLoginSaved reports whether a login is stored for the saved address.
func (a *App) qbtLoginSaved() (bool, int) {
	login, baseURL, ok := a.savedQBittorrentLogin()
	if !ok || login.Username == "" {
		return false, 0
	}
	_, host, err := cleanOrigin(baseURL)
	if err != nil {
		return false, 0
	}
	return true, a.vault.Revision(vaultUserRef("qbt", host))
}

// qbtSettingsView renders the saved qBittorrent settings without secrets.
func qbtSettingsView(settings qbittorrentSettings, loginSaved bool, loginRevision int) map[string]any {
	return map[string]any{
		"base_url": settings.BaseURL, "category": settings.Category, "tags": settings.Tags,
		"login_saved": loginSaved, "login_revision": loginRevision,
	}
}

// qbtSettingsHandler returns the saved qBittorrent settings and login state.
func (a *App) qbtSettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadQBittorrentSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	saved, revision := a.qbtLoginSaved()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "qbittorrent": qbtSettingsView(settings, saved, revision)})
}

// saveQBittorrent stores the qBittorrent address and defaults. The login
// follows the Python wizard's rule: username and password are required
// together, and a blank password keeps the saved login. Changing the
// address does not carry the old host's login over.
func (a *App) saveQBittorrent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL  string `json:"base_url"`
		Username string `json:"username"`
		Password string `json:"password"`
		Category string `json:"category"`
		Tags     string `json:"tags"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Username+body.Password+body.Category+body.Tags, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	previous, err := a.loadQBittorrentSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	next := previous
	if body.BaseURL != "" {
		origin, err := qbittorrent.ValidateBaseURL(body.BaseURL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "qBittorrent 地址无效"})
			return
		}
		next.BaseURL = origin
	}
	if body.Category != "" {
		next.Category = strings.TrimSpace(body.Category)
	}
	if body.Tags != "" {
		next.Tags = strings.TrimSpace(body.Tags)
	}
	if next.Category == "" {
		next.Category = qbittorrent.DefaultCategory
	}
	if next.Tags == "" {
		next.Tags = qbittorrent.ProjectTag
	}
	if (body.Username == "") != (body.Password == "") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "qBittorrent 用户名与密码须同时填写"})
		return
	}
	for _, tag := range strings.Split(next.Tags, ",") {
		if strings.TrimSpace(tag) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "tags 不能含空项"})
			return
		}
	}
	if a.vault == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if body.Username != "" {
		_, host, err := cleanOrigin(next.BaseURL)
		if err != nil || host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "保存登录需要先填 qBittorrent 地址"})
			return
		}
		encoded, err := json.Marshal(qbtLogin{Username: body.Username, Password: body.Password})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		if _, err := a.vault.Set(vaultUserRef("qbt", host), string(encoded), host); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
	}
	saved, err := a.saveQBittorrentSettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	loginSaved, revision := a.qbtLoginSaved()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "qbittorrent": qbtSettingsView(saved, loginSaved, revision)})
}

// testQBittorrent checks the qBittorrent connection read-only, mirroring
// Python's qbt_preflight: login, then app/version, app/webapiVersion,
// app/preferences and torrents/info, all read without changing anything.
// Body fields override the saved settings for this one test only;
// changing the address without supplying a login is refused.
func (a *App) testQBittorrent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL  string `json:"base_url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Username+body.Password, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	saved, err := a.loadQBittorrentSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	baseURL := saved.BaseURL
	if body.BaseURL != "" {
		origin, err := qbittorrent.ValidateBaseURL(body.BaseURL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "qBittorrent 地址无效"})
			return
		}
		baseURL = origin
	}
	username, password := body.Username, body.Password
	if (username == "") != (password == "") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "qBittorrent 用户名与密码须同时填写"})
		return
	}
	if username == "" {
		if body.BaseURL != "" && body.BaseURL != saved.BaseURL {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "地址已改变，请填写登录"})
			return
		}
		login, _, ok := a.savedQBittorrentLogin()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "QBITTORRENT_NOT_CONFIGURED", "error": "尚未保存该地址的登录"})
			return
		}
		username, password = login.Username, login.Password
	}
	if baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "qBittorrent 地址不能为空"})
		return
	}
	client, err := qbittorrent.NewClient(qbittorrent.Config{BaseURL: baseURL, Username: username, Password: password})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "qBittorrent 地址无效"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := qbtPreflight(ctx, client)
	if err != nil {
		code := "QBITTORRENT_UNAVAILABLE"
		message := "连不上 qBittorrent：确认它正在运行，且地址就是它的 WebUI 端口"
		switch {
		case errors.Is(err, qbittorrent.ErrAuthFailed):
			code = "AUTH_FAILED"
			message = "已经连上 qBittorrent，但用户名或密码不对（qB 默认用户名是 admin）"
		case errors.Is(err, qbittorrent.ErrBanned):
			code = "BANNED"
			message = "qBittorrent 暂时封禁了本机 IP，等封禁时间过去再试（" + err.Error() + "）"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": code, "error": message})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "qbittorrent": result})
}

// qbtPreflightResult is the read-only qBittorrent check result.
type qbtPreflightResult struct {
	Version       string         `json:"version"`
	WebAPIVersion string         `json:"webapi_version"`
	Paths         map[string]any `json:"paths"`
	TorrentCount  int            `json:"torrent_count"`
	StateCounts   map[string]int `json:"state_counts"`
}

// qbtPreflight runs the read-only check: login, versions, preferences and
// the torrent list. It changes nothing on the server.
func qbtPreflight(ctx context.Context, client *qbittorrent.Client) (qbtPreflightResult, error) {
	if err := client.Login(ctx); err != nil {
		return qbtPreflightResult{}, err
	}
	version, webAPI, err := client.Version(ctx)
	if err != nil {
		return qbtPreflightResult{}, err
	}
	if _, err := client.Capabilities(ctx); err != nil {
		return qbtPreflightResult{}, err
	}
	preferences, err := client.Preferences(ctx)
	if err != nil {
		return qbtPreflightResult{}, err
	}
	rows, err := client.Info(ctx)
	if err != nil {
		return qbtPreflightResult{}, err
	}
	paths := map[string]any{}
	for _, key := range []string{"save_path", "temp_path", "temp_path_enabled", "auto_tmm_enabled"} {
		if value, ok := preferences[key]; ok {
			paths[key] = value
		}
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.State]++
	}
	return qbtPreflightResult{
		Version: version.String(), WebAPIVersion: webAPI,
		Paths: paths, TorrentCount: len(rows), StateCounts: counts,
	}, nil
}

// effectiveDownloadRequest fills a download request from the saved
// qBittorrent settings wherever the call left a field blank. A half-filled
// login stays invalid, and a saved login is only applied when the call
// uses the saved address (or names no address at all): a login bound to
// one host is never sent to another.
func (a *App) effectiveDownloadRequest(body downloadRequest) (downloadRequest, bool) {
	if (body.Username == "") != (body.Password == "") {
		return body, false
	}
	saved, err := a.loadQBittorrentSettings()
	if err != nil {
		return body, false
	}
	if body.BaseURL == "" {
		body.BaseURL = saved.BaseURL
	}
	if body.Username == "" && body.Password == "" {
		if body.BaseURL != "" && body.BaseURL != saved.BaseURL {
			return body, false
		}
		login, _, ok := a.savedQBittorrentLogin()
		if !ok {
			return body, false
		}
		body.Username, body.Password = login.Username, login.Password
	}
	if body.Category == "" {
		body.Category = saved.Category
	}
	if body.Tags == "" {
		body.Tags = saved.Tags
	}
	return body, true
}

func (a *App) unifyQBittorrentTags(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL  string `json:"base_url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil || body.BaseURL == "" || body.Username == "" || body.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	client, err := qbittorrent.NewClient(qbittorrent.Config{BaseURL: body.BaseURL, Username: body.Username, Password: body.Password})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	changed, err := client.UnifyLegacyTags(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "QBITTORRENT_UNAVAILABLE", "error": "旧标签没有改完"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed, "note": "旧的 musicops-task 标签已并入 MusicOps。没有开始下载。"})
}
