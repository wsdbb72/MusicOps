// Saved tracker cookies.
//
// This reverses the migration plan's "Cookie 永不保存" decision
// (MIGRATION_PLAN.md §2: "每站 Cookie 永不保存"), which the 0.4.33 release
// proved wrong in practice: without a saved login the user had to paste a
// cookie into every search, preflight and download, and the settings page
// the user asked for on 2026-11-09 lists per-site cookies as a first-class
// setup item, exactly like the Python SetupWizard page 2. The reversal
// keeps the rest of the original decision intact:
//
//   - Cookie values live only in the Vault, encrypted, bound to the site's
//     host (vault://pt/<site>/cookies, the same ref Python's setup.py
//     writes), never in the Pebble settings documents, never in logs, and
//     never returned to the frontend. The settings document below records
//     only saved/not-saved and the vault revision, so the UI can show
//     "已保存" without ever echoing content.
//   - A per-call cookie still wins: an explicit cookie on a search,
//     preflight or token recheck is used for that call only and is never
//     written to the Vault. A saved cookie is only the fallback when no
//     per-call value was supplied.
//   - The import format is the Python one: Cookie-Editor export JSON,
//     validated by internal/cookies (domain match, token names, no CR/LF,
//     only the ten fields Python keeps).
package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/cookies"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
)

// cookieSettingsKey is the persisted saved-cookie state. Values stay in the
// Vault; this document only says which sites have one.
var cookieSettingsKey = []byte("app/settings/cookies")

// cookieSiteState is one site's saved-cookie state, without the value.
type cookieSiteState struct {
	Saved     bool  `json:"saved"`
	Revision  int   `json:"revision"`
	UpdatedNS int64 `json:"updated_ns"`
}

// cookieSettings is the saved-cookie state for the four tracker sites.
type cookieSettings struct {
	Sites     map[string]cookieSiteState `json:"sites"`
	UpdatedNS int64                      `json:"updated_ns"`
}

// cookieRef is the Vault reference for one site's saved cookie, the same
// ref Python's setup.py writes (vault://pt/<site>/cookies).
func cookieRef(site string) string { return "vault://pt/" + site + "/cookies" }

func (a *App) loadCookieSettings() (cookieSettings, error) {
	var out cookieSettings
	found, err := a.loadJSONSetting(cookieSettingsKey, &out)
	if err != nil {
		return cookieSettings{}, err
	}
	if !found {
		return cookieSettings{}, nil
	}
	if out.Sites == nil {
		out.Sites = map[string]cookieSiteState{}
	}
	return out, nil
}

func (a *App) saveCookieSettings(in cookieSettings) (cookieSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return cookieSettings{}, err
	}
	if err := a.casSaveJSON(cookieSettingsKey, encoded); err != nil {
		return cookieSettings{}, err
	}
	return in, nil
}

// savedCookie returns the Cookie header for one site's saved cookie, built
// the way Python's cookie_header does it: only the cookies that apply to
// this one request (unexpired, path covers "/"), longest path first. A
// per-call cookie is never involved here; callers check that first.
func (a *App) savedCookie(site string) (string, bool) {
	host, ok := trackersearch.Hosts[site]
	if !ok || a.vault == nil {
		return "", false
	}
	raw, found, err := a.vault.Get(cookieRef(site), host)
	if err != nil || !found || raw == "" {
		return "", false
	}
	parsed, err := cookies.Unmarshal(raw)
	if err != nil {
		return "", false
	}
	header, err := cookies.Header(parsed, "/", a.now())
	if err != nil {
		return "", false
	}
	return header, true
}

// cookieFor resolves the cookie for one site in one job: an explicit
// per-call cookie wins over the saved one, exactly like the search form's
// "本次检索的 Cookie" always did. The saved cookie is only the fallback.
func (a *App) cookieFor(rootJobID, site string) string {
	if cookie := a.secret(rootJobID); cookie != "" {
		return cookie
	}
	if header, ok := a.savedCookie(site); ok {
		return header
	}
	return ""
}

// siteCookieViews renders the saved state for the settings page: saved or
// not, and the vault revision, never the value.
func siteCookieViews(settings cookieSettings) []map[string]any {
	views := make([]map[string]any, 0, len(knownSites()))
	for _, name := range knownSites() {
		state := settings.Sites[name]
		views = append(views, map[string]any{
			"name": name, "host": trackersearch.Hosts[name],
			"saved": state.Saved, "revision": state.Revision,
		})
	}
	return views
}

// cookieSettingsHandler returns the saved-cookie state for the four sites.
func (a *App) cookieSettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadCookieSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sites": siteCookieViews(settings)})
}

// saveCookie imports one site's Cookie-Editor JSON into the Vault. An empty
// cookie_json keeps the saved value, the way the Python wizard's "留空保留"
// does: only a non-empty value overwrites. The response never contains the
// cookie.
func (a *App) saveCookie(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Site       string `json:"site"`
		CookieJSON string `json:"cookie_json"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	host, ok := trackersearch.Hosts[body.Site]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "unknown site: " + body.Site})
		return
	}
	settings, err := a.loadCookieSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if settings.Sites == nil {
		settings.Sites = map[string]cookieSiteState{}
	}
	if strings.TrimSpace(body.CookieJSON) == "" {
		// Blank keeps the saved value; there is nothing to store.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sites": siteCookieViews(settings)})
		return
	}
	if a.vault == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	parsed, err := cookies.ParseEditor(body.CookieJSON, host)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	encoded, err := cookies.Marshal(parsed)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	revision, err := a.vault.Set(cookieRef(body.Site), encoded, host)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	settings.Sites[body.Site] = cookieSiteState{Saved: true, Revision: revision, UpdatedNS: a.now().UnixNano()}
	saved, err := a.saveCookieSettings(settings)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sites": siteCookieViews(saved)})
}

// errNoSavedCookie marks a request that supplied no cookie and has none saved.
var errNoSavedCookie = errors.New("no saved cookie for this site")

// resolvePreflightCookie returns the cookie a login check should use: the
// caller's value when given, else the saved cookie for the site.
func (a *App) resolvePreflightCookie(site, cookie string) (string, error) {
	if cookie != "" {
		return cookie, nil
	}
	if header, ok := a.savedCookie(site); ok {
		return header, nil
	}
	return "", errNoSavedCookie
}
