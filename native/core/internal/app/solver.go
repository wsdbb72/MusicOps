// FlareSolverr settings.
//
// Python SetupWizard page 3 (clients/browser_solver.py validate_solver)
// sets the browser solver: managed, enabled, base_url, timeout,
// executable, auto_start. The Go rules mirror it:
//
//   - base_url must be a loopback origin (127.0.0.1, localhost or ::1),
//     with a non-zero port, an empty or "/" path, and no credentials,
//     query or fragment;
//   - timeout must be an integer in 10-120 seconds;
//   - executable, when set, must be an absolute .exe path (only used on
//     Windows, the way Python's start_local requires it).
//
// The solver is a fallback for read-only GETs: a search page, a torrent
// detail page or an account page that answers with a Cloudflare interstitial
// is retried once through the loopback solver, with the site cookie scoped to
// that host. Direct reads stay the default, and 429, plain auth failures and
// download links are never replayed through a browser.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// solverSettingsKey is the persisted FlareSolverr configuration.
var solverSettingsKey = []byte("app/settings/solver")

// solverSettings mirrors Python's browser_solver configuration.
type solverSettings struct {
	Managed    bool   `json:"managed"`
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"base_url"`
	Timeout    int    `json:"timeout"`
	Executable string `json:"executable"`
	AutoStart  bool   `json:"auto_start"`
	UpdatedNS  int64  `json:"updated_ns"`
}

// defaultSolverSettings is Python's browser_solver default.
func defaultSolverSettings() solverSettings {
	return solverSettings{Managed: true, Enabled: false, BaseURL: "http://127.0.0.1:8191", Timeout: 60, Executable: "", AutoStart: false}
}

func (a *App) loadSolverSettings() (solverSettings, error) {
	var out solverSettings
	found, err := a.loadJSONSetting(solverSettingsKey, &out)
	if err != nil {
		return solverSettings{}, err
	}
	if !found {
		return defaultSolverSettings(), nil
	}
	return out, nil
}

func (a *App) saveSolverSettings(in solverSettings) (solverSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return solverSettings{}, err
	}
	if err := a.casSaveJSON(solverSettingsKey, encoded); err != nil {
		return solverSettings{}, err
	}
	return in, nil
}

// solverURL is the loopback solver address when the feature is enabled, or
// "" when it is off. It is read per request so enabling the solver takes
// effect without a restart.
func (a *App) solverURL() string {
	settings, err := a.loadSolverSettings()
	if err != nil || !settings.Enabled {
		return ""
	}
	return settings.BaseURL
}

// validateSolverURL checks a FlareSolverr base URL the Python way:
// a loopback origin with a non-zero port, empty or "/" path, and no
// credentials, query or fragment.
func validateSolverURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("地址须为完整 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("地址须为 http 或 https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("地址不得带凭据、查询或 fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("地址路径须为空或 /")
	}
	host := u.Hostname()
	if !isLoopbackHost(host) {
		return errors.New("FlareSolverr 地址须为本机回环地址")
	}
	if u.Port() == "" || u.Port() == "0" {
		return errors.New("地址须带非 0 端口")
	}
	return nil
}

func validateSolverSettings(in solverSettings) error {
	if err := validateSolverURL(in.BaseURL); err != nil {
		return err
	}
	if in.Timeout < 10 || in.Timeout > 120 {
		return errors.New("超时须为 10-120 秒")
	}
	if in.Executable != "" {
		if !filepath.IsAbs(in.Executable) {
			return errors.New("FlareSolverr 可执行程序须为绝对路径")
		}
		if !strings.EqualFold(filepath.Ext(in.Executable), ".exe") {
			return errors.New("FlareSolverr 可执行程序须为 .exe 文件")
		}
	}
	return nil
}

// solverSettingsHandler returns the saved FlareSolverr configuration.
func (a *App) solverSettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadSolverSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "solver": settings, "runtime": a.solverRuntimeState()})
}

// saveSolver stores the FlareSolverr settings. Any field may be omitted
// to keep its saved value; the whole document is validated before saving.
func (a *App) saveSolver(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Managed    *bool  `json:"managed"`
		Enabled    *bool  `json:"enabled"`
		BaseURL    string `json:"base_url"`
		Timeout    *int   `json:"timeout"`
		Executable string `json:"executable"`
		AutoStart  *bool  `json:"auto_start"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	previous, err := a.loadSolverSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	next := previous
	if body.Managed != nil {
		next.Managed = *body.Managed
	}
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.BaseURL != "" {
		next.BaseURL = strings.TrimSpace(body.BaseURL)
	}
	if body.Timeout != nil {
		next.Timeout = *body.Timeout
	}
	if body.Executable != "" {
		next.Executable = strings.TrimSpace(body.Executable)
	}
	if body.AutoStart != nil {
		next.AutoStart = *body.AutoStart
	}
	if err := validateSolverSettings(next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	saved, err := a.saveSolverSettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "solver": saved})
}

// solverSessions asks a solver which browser sessions it holds. It is the
// one call both the connection test and the process supervisor use, so a
// solver reported as ready is one the settings page would also accept.
func solverSessions(ctx context.Context, baseURL string) (int, string, bool) {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v1"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(`{"cmd":"sessions.list"}`)))
	if err != nil {
		return 0, "", false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	var payload struct {
		Status   string `json:"status"`
		Sessions []any  `json:"sessions"`
		Version  string `json:"version"`
	}
	if json.NewDecoder(resp.Body).Decode(&payload) != nil {
		return 0, "", false
	}
	if payload.Status != "ok" || payload.Sessions == nil {
		return 0, "", false
	}
	version := payload.Version
	if len(version) > 80 {
		version = version[:80]
	}
	return len(payload.Sessions), version, true
}

// testSolver checks a FlareSolverr endpoint, mirroring Python's
// test_browser_solver: POST {base_url}/v1 {"cmd": "sessions.list"} and
// require a dict with status "ok" and a sessions list. The request
// timeout is the configured timeout plus 5 seconds, the way Python does
// it. A body field overrides the saved address for this one test only.
func (a *App) testSolver(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL string `json:"base_url"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	saved, err := a.loadSolverSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	baseURL := saved.BaseURL
	if body.BaseURL != "" {
		baseURL = strings.TrimSpace(body.BaseURL)
	}
	if err := validateSolverURL(baseURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	timeout := time.Duration(saved.Timeout+5) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v1"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(`{"cmd":"sessions.list"}`)))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "SOLVER_UNAVAILABLE", "error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "SOLVER_UNAVAILABLE", "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var payload struct {
		Status   string `json:"status"`
		Sessions []any  `json:"sessions"`
		Version  string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "SOLVER_UNAVAILABLE", "error": "FlareSolverr 返回无法解析"})
		return
	}
	if payload.Status != "ok" || payload.Sessions == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "SOLVER_UNAVAILABLE", "error": "FlareSolverr 未返回可用的会话列表"})
		return
	}
	version := payload.Version
	if len(version) > 80 {
		version = version[:80]
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "FlareSolverr 连接成功", "sessions": len(payload.Sessions), "version": version})
}

// noRedirectClient is an HTTP client that refuses redirects, the way
// Python's isolated transports do for lyrics and solver requests.
var noRedirectClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}
