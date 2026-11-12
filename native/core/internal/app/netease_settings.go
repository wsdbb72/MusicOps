package app

// The NetEase account cookie is what makes the built-in streaming source
// usable: an anonymous request is answered with a 320k mp3, which this
// library refuses. The cookie lives in the Vault and is also written as the
// netease playlist credential, so one paste serves playlist reading and the
// fallback download.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/cookies"
)

// neteaseCookieView reports whether a cookie is saved. The value never leaves
// the Vault.
func (a *App) neteaseCookieView(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "saved": a.neteaseSaved(),
		"note": "保存后，内置网易云会用它搜索并取无损音源；匿名请求只有 320k mp3，本程序不把有损文件当无损保存。",
	})
}

// saveNeteaseCookie imports a Cookie-Editor export for music.163.com.
func (a *App) saveNeteaseCookie(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CookieJSON string `json:"cookie_json"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if strings.TrimSpace(body.CookieJSON) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": a.neteaseSaved()})
		return
	}
	if a.vault == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	parsed, err := cookies.ParseEditor(body.CookieJSON, neteaseHost)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	encoded, err := cookies.Marshal(parsed)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	if _, err := a.vault.Set(neteaseStreamingRef, encoded, neteaseHost); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	// The playlist reader looks the cookie up by platform, not by this
	// reference, so record it there too rather than asking for a second paste.
	if err := a.casSaveJSONIfAbsentOrPatch([]byte("app/settings/playlist-sources"), "netease", neteaseStreamingRef); err != nil {
		// The streaming path already works; a playlist-ref failure is a
		// degraded convenience, not a reason to fail the save.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": true, "playlist_ref": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": true, "playlist_ref": true})
}

// casSaveJSONIfAbsentOrPatch writes {"<platform>": {"credential_ref": ref}}
// into a settings map, keeping every other platform's entry.
func (a *App) casSaveJSONIfAbsentOrPatch(key []byte, platform, ref string) error {
	current := map[string]struct {
		CredentialRef string `json:"credential_ref"`
	}{}
	found, err := a.loadJSONSetting(key, &current)
	if err != nil {
		return err
	}
	if !found || current == nil {
		current = map[string]struct {
			CredentialRef string `json:"credential_ref"`
		}{}
	}
	current[platform] = struct {
		CredentialRef string `json:"credential_ref"`
	}{CredentialRef: ref}
	encoded, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return a.casSaveJSON(key, encoded)
}
