package app

// Library guards are the per-album switches the dedup/merge work left open:
// a field lock stops automatic writes from changing a tag the user curated,
// and a suppression keeps an album out of duplicate scanning and merging.
//
// The key is the album directory inside the library. It is what both the
// single-file completion and the batch scrape already have in hand (the file's
// parent), it is what a user sees in a file manager, and it does not require a
// root id lookup per request. Moving an album folder therefore drops its lock;
// that is stated in the UI rather than silently guessed.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// guardDocument is the persisted form: one entry per album directory.
type guardDocument struct {
	Albums map[string]albumGuard `json:"albums"`
}

// albumGuard is one album's locks. Dir keeps the original spelling so the UI
// can show the path the user typed; the map key is the folded form.
type albumGuard struct {
	Dir        string   `json:"dir"`
	Fields     []string `json:"fields,omitempty"`
	Suppressed bool     `json:"suppressed,omitempty"`
	UpdatedNS  int64    `json:"updated_ns"`
}

// guardFields are the tag fields a lock can protect. Names match the FLAC
// keys the writers use, lower-cased for the API and the UI.
var guardFields = []string{
	"title", "artist", "album", "albumartist", "date", "genre", "tracknumber", "discnumber", "lyrics", "composer",
}

var guardFieldSet = func() map[string]bool {
	out := map[string]bool{}
	for _, field := range guardFields {
		out[field] = true
	}
	return out
}()

// guardsKey is the persisted guard document.
var guardsKey = []byte("app/settings/library-guards")

func (a *App) loadGuards() (guardDocument, error) {
	doc := guardDocument{Albums: map[string]albumGuard{}}
	found, err := a.loadJSONSetting(guardsKey, &doc)
	if err != nil {
		return guardDocument{Albums: map[string]albumGuard{}}, err
	}
	if !found || doc.Albums == nil {
		doc.Albums = map[string]albumGuard{}
	}
	return doc, nil
}

func (a *App) saveGuards(doc guardDocument) error {
	encoded, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return a.casSaveJSON(guardsKey, encoded)
}

// guardKey folds one album path for map lookups. Windows paths compare
// case-insensitively, and users type them with whatever case Explorer showed.
func guardKey(dir string) string {
	return strings.ToLower(filepath.Clean(strings.TrimSpace(dir)))
}

// guardFor returns the saved guard for one album directory.
func (a *App) guardFor(dir string) albumGuard {
	if strings.TrimSpace(dir) == "" {
		return albumGuard{}
	}
	doc, err := a.loadGuards()
	if err != nil {
		return albumGuard{}
	}
	return doc.Albums[guardKey(dir)]
}

// applyFieldLocks removes the locked fields from one planned write and returns
// their names, so the response can say why a field was not touched.
func (a *App) applyFieldLocks(dir string, updates map[string][]string) (map[string][]string, []string) {
	guard := a.guardFor(dir)
	if len(guard.Fields) == 0 || len(updates) == 0 {
		return updates, nil
	}
	locked := []string{}
	for _, field := range guard.Fields {
		key := strings.ToUpper(field)
		if _, present := updates[key]; present {
			delete(updates, key)
			locked = append(locked, field)
		}
	}
	sort.Strings(locked)
	return updates, locked
}

// albumSuppressed reports whether one album directory opts out of dedup.
func (a *App) albumSuppressed(dir string) bool {
	return a.guardFor(dir).Suppressed
}

// groupSuppressed reports whether any file of a duplicate group sits in a
// suppressed album. A group is the unit that would be merged, so one opt-out
// protects the whole group.
func (a *App) groupSuppressed(group *duplicateGroup) bool {
	if group == nil {
		return false
	}
	for _, file := range group.Files {
		dir := filepath.Dir(a.libraryFilePath(file.LibraryDir, file.File))
		if a.albumSuppressed(dir) {
			return true
		}
	}
	return false
}

// guardsHandler is GET /v1/library/guards.
func (a *App) guardsHandler(w http.ResponseWriter, _ *http.Request) {
	doc, err := a.loadGuards()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	albums := make([]albumGuard, 0, len(doc.Albums))
	for _, guard := range doc.Albums {
		albums = append(albums, guard)
	}
	sort.Slice(albums, func(i, j int) bool { return albums[i].Dir < albums[j].Dir })
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "albums": albums, "fields": guardFields,
		"note": "按专辑目录保存：字段锁让自动写入跳过这些字段，抑制让该专辑不参与重复扫描与合并。目录改名或移动后锁不再生效。",
	})
}

// saveGuardsHandler is POST /v1/library/guards. An entry with no fields and no
// suppression is removed, which is how the UI clears one.
func (a *App) saveGuardsHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir        string   `json:"dir"`
		Path       string   `json:"path"`
		Fields     []string `json:"fields"`
		Suppressed *bool    `json:"suppressed"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	raw := strings.TrimSpace(body.Dir)
	if raw == "" && strings.TrimSpace(body.Path) != "" {
		raw = filepath.Dir(strings.TrimSpace(body.Path))
	}
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "需要专辑目录"})
		return
	}
	resolved, err := a.resolveLibraryPath(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "目录须在音乐库内"})
		return
	}
	dir := filepath.Dir(resolved)
	if info, statErr := os.Stat(resolved); statErr == nil && info.IsDir() {
		dir = resolved
	}
	fields := []string{}
	seen := map[string]bool{}
	for _, field := range body.Fields {
		name := strings.ToLower(strings.TrimSpace(field))
		if !guardFieldSet[name] || seen[name] {
			continue
		}
		seen[name] = true
		fields = append(fields, name)
	}
	sort.Strings(fields)
	suppressed := body.Suppressed != nil && *body.Suppressed
	doc, err := a.loadGuards()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	key := guardKey(dir)
	if len(fields) == 0 && !suppressed {
		delete(doc.Albums, key)
	} else {
		doc.Albums[key] = albumGuard{Dir: dir, Fields: fields, Suppressed: suppressed, UpdatedNS: a.now().UnixNano()}
	}
	if err := a.saveGuards(doc); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guard": doc.Albums[key], "dir": dir})
}
