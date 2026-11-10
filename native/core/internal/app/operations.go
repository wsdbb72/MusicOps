package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
)

// siteSettingsKey is the persisted default site selection. A search that does
// not name any sites uses these; an explicit list, including an empty one,
// always wins over the saved default.
var siteSettingsKey = []byte("app/settings/sites")

// siteSettings is the persisted default: which of the four tracker sites a
// search uses when the request does not name any.
type siteSettings struct {
	EnabledSites []string `json:"enabled_sites"`
	UpdatedNS    int64    `json:"updated_ns"`
}

// knownSites lists the four tracker sites in display order.
func knownSites() []string { return []string{"open.cd", "dicmusic", "orpheus", "redacted"} }

func (a *App) loadSiteSettings() (siteSettings, error) {
	var out siteSettings
	err := a.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(siteSettingsKey)
		if err != nil || !ok {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return siteSettings{}, err
	}
	// Only known sites survive: a stored name from an older list is ignored,
	// and duplicates collapse to one entry.
	known := map[string]bool{}
	for _, name := range knownSites() {
		known[name] = true
	}
	seen := map[string]bool{}
	kept := []string{}
	for _, name := range out.EnabledSites {
		if known[name] && !seen[name] {
			seen[name] = true
			kept = append(kept, name)
		}
	}
	out.EnabledSites = kept
	return out, nil
}

func (a *App) saveSiteSettings(in siteSettings) (siteSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return siteSettings{}, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		var current []byte
		var present bool
		if err := a.engine.View(func(v *dataengine.View) error {
			var err error
			current, present, err = v.Get(siteSettingsKey)
			return err
		}); err != nil {
			return siteSettings{}, err
		}
		conditions := []dataengine.Condition{{Key: siteSettingsKey, Absent: true}}
		if present {
			conditions = []dataengine.Condition{{Key: siteSettingsKey, Value: current}}
		}
		ok, err := a.engine.CompareAndSwap(conditions, []dataengine.Mutation{{Key: siteSettingsKey, Value: encoded}})
		if err != nil {
			return siteSettings{}, err
		}
		if ok {
			return in, nil
		}
	}
	return siteSettings{}, errors.New("site settings changed under the write")
}

func siteViews(settings siteSettings) []map[string]any {
	enabled := map[string]bool{}
	for _, name := range settings.EnabledSites {
		enabled[name] = true
	}
	views := make([]map[string]any, 0, len(knownSites()))
	for _, name := range knownSites() {
		views = append(views, map[string]any{"name": name, "host": trackersearch.Hosts[name], "enabled": enabled[name]})
	}
	return views
}

// sites returns the four tracker sites with the saved default selection. No
// settings saved yet means none enabled, which matches what a search does
// when the request names no sites.
func (a *App) sites(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadSiteSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SITES_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sites": siteViews(settings), "count": len(settings.EnabledSites)})
}

// saveSites replaces the saved default site selection. An empty list is a
// real choice: it disables every site by default.
func (a *App) saveSites(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EnabledSites []string `json:"enabled_sites"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	known := map[string]bool{}
	for _, name := range knownSites() {
		known[name] = true
	}
	seen := map[string]bool{}
	kept := []string{}
	for _, name := range body.EnabledSites {
		if !known[name] || seen[name] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "unknown or duplicate site: " + name})
			return
		}
		seen[name] = true
		kept = append(kept, name)
	}
	saved, err := a.saveSiteSettings(siteSettings{EnabledSites: kept})
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SITES_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sites": siteViews(saved), "count": len(saved.EnabledSites)})
}

// downloadSettingsKey is the persisted download default. A search that does
// not say whether streaming is on uses this; an explicit value always wins.
var downloadSettingsKey = []byte("app/settings/download")

// downloadSettings is the persisted download default: whether a search falls
// back to streaming when the trackers have no exact match, and in which
// order the fallback tries the sources. An empty order means the canonical
// order; a saved order is a permutation of the canonical source names.
type downloadSettings struct {
	StreamingEnabled bool     `json:"streaming_enabled"`
	StreamingOrder   []string `json:"streaming_order,omitempty"`
	UpdatedNS        int64    `json:"updated_ns"`
}

func (a *App) loadDownloadSettings() (downloadSettings, error) {
	var out downloadSettings
	err := a.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(downloadSettingsKey)
		if err != nil || !ok {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return downloadSettings{}, err
	}
	if len(out.StreamingOrder) == 0 {
		out.StreamingOrder = downloadpolicy.CanonicalOrder()
	}
	return out, nil
}

// streamingOrder returns the saved fallback order, or the canonical order
// when nothing was saved or the saved order is invalid.
func (a *App) streamingOrder() []string {
	settings, err := a.loadDownloadSettings()
	if err != nil || len(settings.StreamingOrder) == 0 {
		return downloadpolicy.CanonicalOrder()
	}
	if err := downloadpolicy.ValidateOrder(settings.StreamingOrder); err != nil {
		return downloadpolicy.CanonicalOrder()
	}
	return settings.StreamingOrder
}

func (a *App) saveDownloadSettings(in downloadSettings) (downloadSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return downloadSettings{}, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		var current []byte
		var present bool
		if err := a.engine.View(func(v *dataengine.View) error {
			var err error
			current, present, err = v.Get(downloadSettingsKey)
			return err
		}); err != nil {
			return downloadSettings{}, err
		}
		conditions := []dataengine.Condition{{Key: downloadSettingsKey, Absent: true}}
		if present {
			conditions = []dataengine.Condition{{Key: downloadSettingsKey, Value: current}}
		}
		ok, err := a.engine.CompareAndSwap(conditions, []dataengine.Mutation{{Key: downloadSettingsKey, Value: encoded}})
		if err != nil {
			return downloadSettings{}, err
		}
		if ok {
			return in, nil
		}
	}
	return downloadSettings{}, errors.New("download settings changed under the write")
}

// downloadSettingsView returns the saved streaming default plus the saved
// streaming fallback order and each source's wiring state. The order is the
// operator's saved order (canonical when nothing was saved); the fallback
// walk follows it (see acquireFallback), and the status list says honestly
// which sources have an engine behind them.
func (a *App) downloadSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadDownloadSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	steps, err := downloadpolicy.PlanOrdered(downloadpolicy.DefaultSources(), settings.StreamingOrder, downloadpolicy.Track{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "streaming_enabled": settings.StreamingEnabled, "streaming_order": settings.StreamingOrder, "streaming_sources": steps, "streaming_source_status": a.streamingSourceStatus(), "streaming_fallback_wired": true})
}

// saveDownload replaces the saved streaming default and/or the saved
// fallback order. Either field may be omitted; an omitted field keeps its
// saved value, so saving the checkbox never wipes the order.
func (a *App) saveDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StreamingEnabled *bool    `json:"streaming_enabled"`
		StreamingOrder   []string `json:"streaming_order"`
	}
	if err := decode(r, &body); err != nil || (body.StreamingEnabled == nil && body.StreamingOrder == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.StreamingOrder != nil {
		if err := downloadpolicy.ValidateOrder(body.StreamingOrder); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "streaming order must name each source exactly once"})
			return
		}
	}
	current, err := a.loadDownloadSettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	next := current
	if body.StreamingEnabled != nil {
		next.StreamingEnabled = *body.StreamingEnabled
	}
	if body.StreamingOrder != nil {
		next.StreamingOrder = body.StreamingOrder
	}
	saved, err := a.saveDownloadSettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "streaming_enabled": saved.StreamingEnabled, "streaming_order": saved.StreamingOrder})
}

// library lists every verified library record. A record whose file is gone
// stays listed with file_present false, so the gap is visible.
func (a *App) library(w http.ResponseWriter, r *http.Request) {
	entries, err := audiorelease.List(a.engine)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entries": entries, "count": len(entries)})
}

// logs lists recent task activity: the real run record, newest first in
// taskrepo order. It shows what ran and how it ended; it is not a
// reconstructed narrative.
func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	page, err := a.tasks.Recent(nil, 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LOGS_UNAVAILABLE"})
		return
	}
	tasks := publicTasks(page.Tasks)
	if tasks == nil {
		tasks = []taskrepo.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks, "count": len(tasks)})
}
