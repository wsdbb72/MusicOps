// Music library directory and music-server settings.
//
// Python SetupWizard page 1 sets the library directory and page 7 the
// music servers (Plex / Navidrome). The Python rules mirrored here:
//
//   - Library: an absolute path, never the filesystem root, and never
//     overlapping the state or backup directories (config/schema.py
//     validate_roots). Symlinks are resolved first rather than rejected
//     the way Python's reject_reparse_chain does: on macOS /var is a
//     symlink to /private/var, so rejecting any symlink would reject
//     every path under /var/folders. The resolved target is what the
//     overlap checks validate. The directory is created at save time
//     (setup.py library.mkdir). There is no whole-library migration: Python's storage_migration only moves
//     in-flight downloads, never published audio (see the spec file). Go
//     does the same — changing the directory only moves where NEW audio
//     is published; existing records keep their absolute paths and keep
//     verifying, and each record remembers the library root it was
//     published under (audiorelease record.Library), so library
//     maintenance resolves every record against its own root.
//   - Plex: enabled, url, section_id, token in the Vault under
//     vault://user/plex/<sha256(host)[:12]>. section_id comes from the
//     connection test, which lists the music libraries
//     (clients/plex.py sections). URL rule: plain HTTP only for a local
//     service, public hosts must be HTTPS.
//   - Navidrome: enabled, url, username, password in the Vault under
//     vault://user/navidrome/<sha256(host)[:12]>. There is no separate
//     Navidrome test in Python (connection_tests only has plex/ai/lyrics);
//     the Go test is a Subsonic ping, the read-only auth check, so testing
//     never triggers a library scan.
//   - Notify: after a publish completes, Python's pipeline._notifying
//     calls Navidrome scan() then Plex scan(); a failure only appends to
//     the job message and never blocks completion. Roon gets no call:
//     Python never notifies Roon, it discovers new files by watching the
//     library directory. Go has no Roon client and adds none.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicservers"
)

// librarySettingsKey is the persisted library directory.
var librarySettingsKey = []byte("app/settings/library")

// librarySettings is the persisted library configuration. An empty
// LibraryDir means the default (<state>/library-audio).
type librarySettings struct {
	LibraryDir string `json:"library_dir"`
	UpdatedNS  int64  `json:"updated_ns"`
}

// defaultLibraryDir is the library location before any custom directory is
// saved: inside the state directory, where every release before this change
// published.
func (a *App) defaultLibraryDir() string {
	return filepath.Join(a.Dir, "library-audio")
}

// libraryDir returns the effective library directory: the saved one, or
// the default when nothing was saved or the saved document cannot be read.
func (a *App) libraryDir() string {
	settings, err := a.loadLibrarySettings()
	if err != nil || settings.LibraryDir == "" {
		return a.defaultLibraryDir()
	}
	return settings.LibraryDir
}

// entryLibraryDir returns the library root one record was published under,
// or the default for records written before the root was stored.
func (a *App) entryLibraryDir(entry audiorelease.Entry) string {
	if entry.LibraryDir != "" {
		return entry.LibraryDir
	}
	return a.defaultLibraryDir()
}

// entryFilePath resolves one library entry to its on-disk file, against
// the library root that entry was published under.
func (a *App) entryFilePath(entry audiorelease.Entry) string {
	return a.libraryFilePath(entry.LibraryDir, entry.File)
}

// libraryFilePath joins a library-relative file under a library root,
// falling back to the default library when the root was not stored.
func (a *App) libraryFilePath(libraryDir, file string) string {
	if libraryDir == "" {
		libraryDir = a.defaultLibraryDir()
	}
	return filepath.Join(libraryDir, file)
}

func (a *App) loadLibrarySettings() (librarySettings, error) {
	var out librarySettings
	found, err := a.loadJSONSetting(librarySettingsKey, &out)
	if err != nil {
		return librarySettings{}, err
	}
	if !found {
		return librarySettings{}, nil
	}
	return out, nil
}

func (a *App) saveLibrarySettings(in librarySettings) (librarySettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return librarySettings{}, err
	}
	if err := a.casSaveJSON(librarySettingsKey, encoded); err != nil {
		return librarySettings{}, err
	}
	return in, nil
}

// resolveExisting resolves the longest existing ancestor of path with
// EvalSymlinks and appends the non-existent remainder lexically.
// System prefixes are symlinks by design — macOS /var points at
// /private/var, so every path under the CI temp dir passes through one —
// and must be resolved, not rejected. This is the same lesson as
// resolveInDir (Phase 3): a symlinked parent is a spelling, not a scope
// escape. A user symlink is followed the same way, and the resolved
// target is what the overlap checks below validate.
func resolveExisting(path string) string {
	for current := path; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			if rel, err := filepath.Rel(current, path); err == nil {
				return filepath.Join(resolved, rel)
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
	}
}

// validateLibraryDir checks a library directory: absolute, not the
// filesystem root, and never equal to, containing, or inside the state
// or backup directories — all compared on resolved paths. (Being a new
// subdirectory of the state directory is allowed: the default library
// lives there.) This deliberately differs from Python's
// reject_reparse_chain, which rejects any symlink in the chain: on
// macOS that would reject every path under /var/folders, including the
// test temp dir. Resolving first keeps the safety property — the real
// location is what gets validated — without rejecting system prefixes.
func (a *App) validateLibraryDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if abs == filepath.Dir(abs) {
		return errors.New("请选择音乐库子目录，不使用整个磁盘根目录")
	}
	resolved := resolveExisting(abs)
	state, err := filepath.Abs(a.Dir)
	if err != nil {
		return err
	}
	backup, err := filepath.Abs(a.Backup)
	if err != nil {
		return err
	}
	resolvedState := resolveExisting(state)
	resolvedBackup := resolveExisting(backup)
	if resolved == resolvedState || resolved == resolvedBackup {
		return errors.New("音乐库目录不能与状态目录或备份目录相同")
	}
	if insideDir(resolved, resolvedState) || insideDir(resolved, resolvedBackup) {
		return errors.New("音乐库目录不能包含状态目录或备份目录")
	}
	if insideDir(resolvedBackup, resolved) {
		return errors.New("音乐库目录不能位于备份目录内")
	}
	return nil
}

// librarySettingsHandler returns the saved library directory and the
// default it falls back to.
func (a *App) librarySettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadLibrarySettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "library": map[string]any{
		"library_dir": settings.LibraryDir, "default_library_dir": a.defaultLibraryDir(),
		"effective_library_dir": a.libraryDir(),
	}})
}

// saveLibrary stores the library directory. An empty value resets to the
// default. The directory is created when it does not exist yet, the way
// Python's save_setup creates it.
func (a *App) saveLibrary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LibraryDir string `json:"library_dir"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	dir := strings.TrimSpace(body.LibraryDir)
	next := librarySettings{}
	if dir != "" {
		if !filepath.IsAbs(dir) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "音乐库目录须为绝对路径"})
			return
		}
		if err := a.validateLibraryDir(dir); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "音乐库目录无法创建"})
			return
		}
		next.LibraryDir = filepath.Clean(dir)
	}
	saved, err := a.saveLibrarySettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "library": map[string]any{
		"library_dir": saved.LibraryDir, "default_library_dir": a.defaultLibraryDir(),
		"effective_library_dir": a.libraryDir(),
	}})
}

// serverSettingsKey is the persisted Plex / Navidrome configuration.
// Tokens and passwords live in the Vault; this document holds only the
// addresses, the Plex section id and whether a secret is saved.
var serverSettingsKey = []byte("app/settings/servers")

// plexSettings mirrors Python's music_servers.plex: enabled, url,
// section_id, and the Vault token state (never the token).
type plexSettings struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url"`
	SectionID string `json:"section_id"`
}

// navidromeSettings mirrors Python's music_servers.navidrome: enabled,
// url, username, and the Vault password state (never the password).
type navidromeSettings struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	Username string `json:"username"`
}

// serverSettings is the persisted music-server configuration.
type serverSettings struct {
	Plex      plexSettings      `json:"plex"`
	Navidrome navidromeSettings `json:"navidrome"`
	UpdatedNS int64             `json:"updated_ns"`
}

var plexSectionIDPattern = regexp.MustCompile(`^[0-9]{1,12}$`)

func (a *App) loadServerSettings() (serverSettings, error) {
	var out serverSettings
	found, err := a.loadJSONSetting(serverSettingsKey, &out)
	if err != nil {
		return serverSettings{}, err
	}
	if !found {
		return serverSettings{}, nil
	}
	return out, nil
}

func (a *App) saveServerSettings(in serverSettings) (serverSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return serverSettings{}, err
	}
	if err := a.casSaveJSON(serverSettingsKey, encoded); err != nil {
		return serverSettings{}, err
	}
	return in, nil
}

// plexTokenSaved reports whether a Plex token is stored for the saved Plex
// host, and its vault revision.
func (a *App) plexTokenSaved(settings serverSettings) (bool, int) {
	if a.vault == nil || settings.Plex.URL == "" {
		return false, 0
	}
	_, host, err := cleanURL(settings.Plex.URL, false)
	if err != nil {
		return false, 0
	}
	_, found, err := a.vault.Get(vaultUserRef("plex", host), host)
	if err != nil || !found {
		return false, 0
	}
	return true, a.vault.Revision(vaultUserRef("plex", host))
}

// navidromePasswordSaved reports whether a Navidrome password is stored
// for the saved Navidrome host, and its vault revision.
func (a *App) navidromePasswordSaved(settings serverSettings) (bool, int) {
	if a.vault == nil || settings.Navidrome.URL == "" {
		return false, 0
	}
	_, host, err := cleanURL(settings.Navidrome.URL, false)
	if err != nil {
		return false, 0
	}
	_, found, err := a.vault.Get(vaultUserRef("navidrome", host), host)
	if err != nil || !found {
		return false, 0
	}
	return true, a.vault.Revision(vaultUserRef("navidrome", host))
}

func serverView(settings serverSettings, plexSaved bool, plexRevision int, navidromeSaved bool, navidromeRevision int) map[string]any {
	return map[string]any{
		"plex": map[string]any{
			"enabled": settings.Plex.Enabled, "url": settings.Plex.URL,
			"section_id":  settings.Plex.SectionID,
			"token_saved": plexSaved, "token_revision": plexRevision,
		},
		"navidrome": map[string]any{
			"enabled": settings.Navidrome.Enabled, "url": settings.Navidrome.URL,
			"username":       settings.Navidrome.Username,
			"password_saved": navidromeSaved, "password_revision": navidromeRevision,
		},
	}
}

// serverSettingsHandler returns the saved music-server configuration and
// secret state.
func (a *App) serverSettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadServerSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	plexSaved, plexRevision := a.plexTokenSaved(settings)
	navidromeSaved, navidromeRevision := a.navidromePasswordSaved(settings)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "servers": serverView(settings, plexSaved, plexRevision, navidromeSaved, navidromeRevision)})
}

// savePlex stores the Plex settings. The token follows the Python
// wizard's "已保存，输入即可替换": blank keeps the saved token, a value
// replaces it. Changing the URL clears the token binding, the way
// Python's host-bound secret() does.
func (a *App) savePlex(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled   *bool  `json:"enabled"`
		URL       string `json:"url"`
		SectionID string `json:"section_id"`
		Token     string `json:"token"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Token, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	previous, err := a.loadServerSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	next := previous
	if body.Enabled != nil {
		next.Plex.Enabled = *body.Enabled
	}
	if body.URL != "" {
		if err := musicservers.ValidatePlexURL(body.URL); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "Plex 地址无效：" + err.Error()})
			return
		}
		next.Plex.URL = strings.TrimSpace(body.URL)
	}
	if body.SectionID != "" {
		if !plexSectionIDPattern.MatchString(body.SectionID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "Plex 音乐库 ID 须为 1-12 位数字，可先测试读取音乐库列表"})
			return
		}
		next.Plex.SectionID = body.SectionID
	}
	if a.vault == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if body.Token != "" {
		_, host, err := cleanURL(next.Plex.URL, false)
		if err != nil || host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "保存 Token 需要先填 Plex 地址"})
			return
		}
		if _, err := a.vault.Set(vaultUserRef("plex", host), body.Token, host); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
	}
	saved, err := a.saveServerSettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	plexSaved, plexRevision := a.plexTokenSaved(saved)
	navidromeSaved, navidromeRevision := a.navidromePasswordSaved(saved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "servers": serverView(saved, plexSaved, plexRevision, navidromeSaved, navidromeRevision)})
}

// testPlex lists the Plex music libraries, mirroring Python's
// connection_tests plex kind. Body fields override the saved settings for
// this one test only; changing the address without supplying a token is
// refused. The returned sections are what the section_id is picked from.
func (a *App) testPlex(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Token, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	saved, err := a.loadServerSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	baseURL := saved.Plex.URL
	if body.URL != "" {
		baseURL = strings.TrimSpace(body.URL)
	}
	if err := musicservers.ValidatePlexURL(baseURL); err != nil || baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "Plex 地址无效"})
		return
	}
	if body.URL != "" && body.URL != saved.Plex.URL && body.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "地址已改变，请填写 Token"})
		return
	}
	token := body.Token
	if token == "" {
		if a.vault == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		_, host, _ := cleanURL(baseURL, false)
		stored, found, err := a.vault.Get(vaultUserRef("plex", host), host)
		if err != nil || !found || stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "PLEX_NOT_CONFIGURED", "error": "尚未保存该地址的 Token"})
			return
		}
		token = stored
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	sections, err := musicservers.PlexSections(ctx, baseURL, token)
	if err != nil {
		code := "PLEX_UNAVAILABLE"
		if errors.Is(err, musicservers.ErrAuthFailed) {
			code = "AUTH_FAILED"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": code, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Plex 连接成功", "sections": sections, "count": len(sections)})
}

// saveNavidrome stores the Navidrome settings. The password follows the
// same blank-keeps rule as the other secrets; changing the URL clears
// the password binding.
func (a *App) saveNavidrome(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  *bool  `json:"enabled"`
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Password, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	previous, err := a.loadServerSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	next := previous
	if body.Enabled != nil {
		next.Navidrome.Enabled = *body.Enabled
	}
	if body.URL != "" {
		if err := musicservers.ValidateNavidromeURL(body.URL); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "Navidrome 地址无效：" + err.Error()})
			return
		}
		next.Navidrome.URL = strings.TrimSpace(body.URL)
	}
	if body.Username != "" {
		next.Navidrome.Username = strings.TrimSpace(body.Username)
	}
	if a.vault == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if body.Password != "" {
		_, host, err := cleanURL(next.Navidrome.URL, false)
		if err != nil || host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "保存密码需要先填 Navidrome 地址"})
			return
		}
		if _, err := a.vault.Set(vaultUserRef("navidrome", host), body.Password, host); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
	}
	saved, err := a.saveServerSettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	plexSaved, plexRevision := a.plexTokenSaved(saved)
	navidromeSaved, navidromeRevision := a.navidromePasswordSaved(saved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "servers": serverView(saved, plexSaved, plexRevision, navidromeSaved, navidromeRevision)})
}

// testNavidrome pings the Navidrome server with the Subsonic API, the
// read-only auth check. Python has no Navidrome test kind; a ping proves
// the address and password without triggering a library scan.
func (a *App) testNavidrome(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Password, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	saved, err := a.loadServerSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	baseURL := saved.Navidrome.URL
	if body.URL != "" {
		baseURL = strings.TrimSpace(body.URL)
	}
	username := saved.Navidrome.Username
	if body.Username != "" {
		username = strings.TrimSpace(body.Username)
	}
	if err := musicservers.ValidateNavidromeURL(baseURL); err != nil || baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "Navidrome 地址无效"})
		return
	}
	if body.URL != "" && body.URL != saved.Navidrome.URL && body.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "地址已改变，请填写密码"})
		return
	}
	password := body.Password
	if password == "" {
		if a.vault == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		_, host, _ := cleanURL(baseURL, false)
		stored, found, err := a.vault.Get(vaultUserRef("navidrome", host), host)
		if err != nil || !found || stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "NAVIDROME_NOT_CONFIGURED", "error": "尚未保存该地址的密码"})
			return
		}
		password = stored
	}
	if username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "Navidrome 用户名不能为空"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := musicservers.NavidromePing(ctx, baseURL, username, password); err != nil {
		code := "NAVIDROME_UNAVAILABLE"
		if errors.Is(err, musicservers.ErrAuthFailed) {
			code = "AUTH_FAILED"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": code, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Navidrome 连接成功"})
}

// notifyServers asks the enabled music servers to rescan after a publish,
// mirroring Python's pipeline._notifying: Navidrome first, then Plex, and
// a failure only appends a note to the job message — it never fails the
// publish. The returned note is appended to the publish completion
// message, or empty when nothing is enabled or every scan was requested.
func (a *App) notifyServers(ctx context.Context) string {
	settings, err := a.loadServerSettings()
	if err != nil {
		return ""
	}
	note := ""
	if settings.Navidrome.Enabled && settings.Navidrome.URL != "" {
		if err := a.notifyNavidrome(ctx, settings); err != nil {
			note += "；Navidrome 扫描未成功，请检查连接设置"
		}
	}
	if settings.Plex.Enabled && settings.Plex.URL != "" {
		if err := a.notifyPlex(ctx, settings); err != nil {
			note += "；Plex 通知失败，请检查连接设置"
		}
	}
	return note
}

func (a *App) notifyNavidrome(ctx context.Context, settings serverSettings) error {
	if a.vault == nil || settings.Navidrome.Username == "" {
		return errors.New("navidrome not configured")
	}
	_, host, err := cleanURL(settings.Navidrome.URL, false)
	if err != nil {
		return err
	}
	password, found, err := a.vault.Get(vaultUserRef("navidrome", host), host)
	if err != nil || !found || password == "" {
		return errors.New("navidrome password not saved")
	}
	return musicservers.NavidromeScan(ctx, settings.Navidrome.URL, settings.Navidrome.Username, password)
}

func (a *App) notifyPlex(ctx context.Context, settings serverSettings) error {
	if a.vault == nil || settings.Plex.SectionID == "" {
		return errors.New("plex not configured")
	}
	_, host, err := cleanURL(settings.Plex.URL, false)
	if err != nil {
		return err
	}
	token, found, err := a.vault.Get(vaultUserRef("plex", host), host)
	if err != nil || !found || token == "" {
		return errors.New("plex token not saved")
	}
	return musicservers.PlexScan(ctx, settings.Plex.URL, token, settings.Plex.SectionID)
}

// resolveLibraryPath resolves a user-supplied path for library file
// operations (transcode source, NCM source, tag and lyric paths). The
// state directory is tried first, so every path that worked before keeps
// working exactly as before; a path inside the configured library
// directory is tried second, so a custom library outside the state
// directory is reachable too.
func (a *App) resolveLibraryPath(raw string) (string, error) {
	if resolved, err := resolveInDir(a.Dir, raw); err == nil {
		return resolved, nil
	}
	library := a.libraryDir()
	if library != a.defaultLibraryDir() {
		if resolved, err := resolveInDir(library, raw); err == nil {
			return resolved, nil
		}
	}
	return "", errors.New("path is outside the state and library directories")
}
