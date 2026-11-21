package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/netproxy"
)

var outboundProxySettingsKey = []byte("app/settings/proxy")

type outboundProxySettings struct {
	URL       string `json:"url"`
	UpdatedNS int64  `json:"updated_ns"`
}

func (a *App) loadOutboundProxySettings() (outboundProxySettings, bool, error) {
	var out outboundProxySettings
	found, err := a.loadJSONSetting(outboundProxySettingsKey, &out)
	if err != nil {
		return outboundProxySettings{}, false, err
	}
	return out, found, nil
}

func (a *App) saveOutboundProxySettings(in outboundProxySettings) (outboundProxySettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return outboundProxySettings{}, err
	}
	if err := a.casSaveJSON(outboundProxySettingsKey, encoded); err != nil {
		return outboundProxySettings{}, err
	}
	return in, nil
}

// restoreOutboundProxy runs before the scheduler or any outbound client starts.
// A bad persisted document is reported in settings, but never prevents the
// native store, UI, or local services from starting.
func (a *App) restoreOutboundProxy() error {
	netproxy.Install()
	settings, found, err := a.loadOutboundProxySettings()
	if err != nil {
		a.setProxyConfigError(err)
		return err
	}
	raw := ""
	if found {
		raw = settings.URL
	}
	if _, err = netproxy.Configure(raw); err != nil {
		a.setProxyConfigError(err)
		return err
	}
	a.setProxyConfigError(nil)
	return nil
}

func (a *App) setProxyConfigError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.proxyConfigErr = ""
		return
	}
	a.proxyConfigErr = strings.TrimSpace(err.Error())
}

func (a *App) proxyConfigError() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxyConfigErr
}

func (a *App) outboundProxyView(settings outboundProxySettings, found bool) map[string]any {
	active, enabled := netproxy.Current()
	return map[string]any{
		"url": settings.URL, "enabled": found && settings.URL != "",
		"updated_ns": settings.UpdatedNS, "active_url": active,
		"runtime_enabled": enabled, "bypass": []string{"localhost", "127.0.0.0/8", "::1"},
		"config_error": a.proxyConfigError(),
	}
}

// outboundProxySettingsHandler returns the persisted address and the runtime
// address actually used by existing clients.
func (a *App) outboundProxySettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, found, err := a.loadOutboundProxySettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE", "error": "出站代理设置无法读取"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy": a.outboundProxyView(settings, found)})
}

// saveOutboundProxy persists first, then atomically switches the runtime. A
// failed connectivity probe is returned as a warning while keeping the saved
// choice visible; the user can correct the address explicitly.
func (a *App) saveOutboundProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL *string `json:"url"`
	}
	if err := decode(r, &body); err != nil || body.URL == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "proxy url is required"})
		return
	}
	normalized, err := netproxy.Normalize(*body.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "代理地址无效：" + err.Error()})
		return
	}
	settings, err := a.saveOutboundProxySettings(outboundProxySettings{URL: normalized})
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE", "error": "出站代理设置无法保存"})
		return
	}
	if _, err = netproxy.Configure(normalized); err != nil {
		a.setProxyConfigError(err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "PROXY_APPLY_FAILED", "error": err.Error()})
		return
	}
	a.setProxyConfigError(nil)
	// The downloader inherited its environment when it started. Retire it only
	// when idle; an in-flight request completes before the child is rebuilt.
	// FlareSolverr receives the proxy per target request, so switching it does
	// not require killing the browser service.
	a.retireStreamingEngineForProxyChange()
	var check *netproxy.ConnectivityResult
	if normalized != "" {
		result := netproxy.CheckConnectivity(r.Context(), normalized)
		check = &result
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "proxy": a.outboundProxyView(settings, true),
		"reachable": check == nil || check.OK, "check": check,
	})
}

// testOutboundProxy tests the saved address, or an optional unsaved URL in the
// request. It never changes persistence or the active runtime.
func (a *App) testOutboundProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	_ = decode(r, &body)
	raw := strings.TrimSpace(body.URL)
	if raw == "" {
		settings, found, err := a.loadOutboundProxySettings()
		if err != nil || !found {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "PROXY_NOT_CONFIGURED", "error": "尚未保存出站代理"})
			return
		}
		raw = settings.URL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result := netproxy.CheckConnectivity(ctx, raw)
	if !result.OK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "PROXY_UNREACHABLE", "error": result.Error, "check": result})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "check": result, "message": "代理连接成功。"})
}
