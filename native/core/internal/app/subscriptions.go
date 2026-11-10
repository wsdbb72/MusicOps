package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// Playlist subscriptions re-read a saved local playlist. They do not call
// a streaming platform. New tracks are queued as playlist-scene jobs; the
// first check can record the current tracks without queueing them.

type playlistSubscription struct {
	PlaylistID      string   `json:"playlist_id"`
	Name            string   `json:"name"`
	IntervalHours   int      `json:"interval_hours"`
	IncludeExisting bool     `json:"include_existing"`
	Paused          bool     `json:"paused"`
	Seen            []string `json:"seen"`
	LastChecked     string   `json:"last_checked,omitempty"`
	NextCheck       string   `json:"next_check,omitempty"`
	LastResult      string   `json:"last_result,omitempty"`
}

func (a *App) subscriptionPath() string {
	return filepath.Join(a.Dir, "subscriptions.json")
}

func (a *App) loadSubscriptions() ([]playlistSubscription, error) {
	data, err := os.ReadFile(a.subscriptionPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []playlistSubscription{}, nil
		}
		return nil, err
	}
	var rows []playlistSubscription
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []playlistSubscription{}
	}
	return rows, nil
}

func (a *App) storeSubscriptions(rows []playlistSubscription) error {
	encoded, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	path := a.subscriptionPath()
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (a *App) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	rows, err := a.loadSubscriptions()
	a.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SUBSCRIPTION_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "subscriptions": rows, "count": len(rows)})
}

func (a *App) saveSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlaylistID      string `json:"playlist_id"`
		IntervalHours   int    `json:"interval_hours"`
		IncludeExisting bool   `json:"include_existing"`
	}
	if err := decode(r, &body); err != nil || !playlistSlugPattern.MatchString(body.PlaylistID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.IntervalHours < 1 || body.IntervalHours > 168 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "检查间隔须为 1–168 小时"})
		return
	}
	tracks, name, err := a.readSavedPlaylist(body.PlaylistID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.loadSubscriptions()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SUBSCRIPTION_UNAVAILABLE"})
		return
	}
	now := a.now()
	next := now.Add(time.Duration(body.IntervalHours) * time.Hour).UTC().Format(time.RFC3339)
	row := playlistSubscription{
		PlaylistID: body.PlaylistID, Name: name, IntervalHours: body.IntervalHours,
		IncludeExisting: body.IncludeExisting, NextCheck: next, LastResult: "已订阅，尚未检查",
	}
	if name == "" {
		row.Name = body.PlaylistID
	}
	_ = tracks
	replaced := false
	for i := range rows {
		if rows[i].PlaylistID == body.PlaylistID {
			row.Seen = rows[i].Seen
			row.Paused = rows[i].Paused
			row.LastChecked = rows[i].LastChecked
			row.LastResult = rows[i].LastResult
			rows[i] = row
			replaced = true
		}
	}
	if !replaced {
		rows = append(rows, row)
	}
	if err := a.storeSubscriptions(rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SUBSCRIPTION_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "subscription": row})
}

func (a *App) pauseSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlaylistID string `json:"playlist_id"`
		Paused     bool   `json:"paused"`
	}
	if err := decode(r, &body); err != nil || !playlistSlugPattern.MatchString(body.PlaylistID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	row, err := a.updateSubscription(body.PlaylistID, func(row *playlistSubscription) {
		row.Paused = body.Paused
		if body.Paused {
			row.LastResult = "已暂停"
		} else {
			row.LastResult = "已恢复"
		}
	})
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "subscription": row})
}

func (a *App) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlaylistID string `json:"playlist_id"`
	}
	if err := decode(r, &body); err != nil || !playlistSlugPattern.MatchString(body.PlaylistID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.loadSubscriptions()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SUBSCRIPTION_UNAVAILABLE"})
		return
	}
	kept := rows[:0]
	found := false
	for _, row := range rows {
		if row.PlaylistID == body.PlaylistID {
			found = true
			continue
		}
		kept = append(kept, row)
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if err := a.storeSubscriptions(kept); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SUBSCRIPTION_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": body.PlaylistID})
}

func (a *App) checkSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlaylistID string `json:"playlist_id"`
	}
	if err := decode(r, &body); err != nil || !playlistSlugPattern.MatchString(body.PlaylistID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	result, err := a.runSubscriptionCheck(body.PlaylistID, true)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *App) runDueSubscriptions() {
	a.mu.Lock()
	rows, err := a.loadSubscriptions()
	a.mu.Unlock()
	if err != nil {
		return
	}
	now := a.now()
	for _, row := range rows {
		if row.Paused {
			continue
		}
		next, err := time.Parse(time.RFC3339, row.NextCheck)
		if err != nil || now.Before(next) {
			continue
		}
		_, _ = a.runSubscriptionCheck(row.PlaylistID, false)
	}
}

func (a *App) runSubscriptionCheck(playlistID string, manual bool) (map[string]any, error) {
	a.mu.Lock()
	rows, err := a.loadSubscriptions()
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	index := -1
	for i := range rows {
		if rows[i].PlaylistID == playlistID {
			index = i
		}
	}
	if index < 0 {
		a.mu.Unlock()
		return nil, errSubscriptionNotFound
	}
	row := rows[index]
	if row.Paused && !manual {
		a.mu.Unlock()
		return map[string]any{"ok": true, "subscription": row, "queued": 0, "job_ids": []string{}, "downloaded": false}, nil
	}
	if row.Paused && manual {
		a.mu.Unlock()
		return nil, errors.New("订阅已暂停")
	}
	a.mu.Unlock()

	tracks, _, err := a.readSavedPlaylist(playlistID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, key := range row.Seen {
		seen[key] = true
	}
	fresh := len(row.Seen) == 0 && row.LastChecked == ""
	if fresh && !row.IncludeExisting {
		keys := trackKeys(tracks)
		row.Seen = keys
		row.LastChecked = a.now().UTC().Format(time.RFC3339)
		row.NextCheck = a.now().Add(time.Duration(row.IntervalHours) * time.Hour).UTC().Format(time.RFC3339)
		row.LastResult = fmt.Sprintf("已记录现有 %d 首，未创建任务", len(keys))
		if err := a.updateStoredSubscription(row); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "subscription": row, "queued": 0, "job_ids": []string{}, "downloaded": false, "message": row.LastResult}, nil
	}
	groups := map[string][]playlistTrack{}
	order := []string{}
	for _, track := range tracks {
		key := trackKey(track)
		if seen[key] {
			continue
		}
		group := track.Album
		if group == "" {
			group = track.Artist + " - " + track.Title
		}
		if _, ok := groups[group]; !ok {
			order = append(order, group)
		}
		groups[group] = append(groups[group], track)
	}
	if len(order) > 20 {
		order = order[:20]
	}
	jobIDs := []string{}
	queuedTracks := 0
	for index, album := range order {
		members := groups[album]
		jobID := subscriptionJobID(playlistID, album, a.now(), index)
		if err := a.queueSubscriptionAlbum(jobID, playlistID, album, members); err != nil {
			row.LastResult = "排队未完成：" + err.Error()
			_ = a.updateStoredSubscription(row)
			return nil, err
		}
		jobIDs = append(jobIDs, jobID)
		queuedTracks += len(members)
		for _, track := range members {
			row.Seen = append(row.Seen, trackKey(track))
		}
		sort.Strings(row.Seen)
		row.LastResult = fmt.Sprintf("已为 %d 首新增曲目创建任务；尚未下载、尚未入库", queuedTracks)
		if err := a.updateStoredSubscription(row); err != nil {
			return nil, err
		}
	}
	row.LastChecked = a.now().UTC().Format(time.RFC3339)
	row.NextCheck = a.now().Add(time.Duration(row.IntervalHours) * time.Hour).UTC().Format(time.RFC3339)
	if queuedTracks == 0 {
		row.LastResult = "没有新增曲目"
	}
	if err := a.updateStoredSubscription(row); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "subscription": row, "queued": queuedTracks, "job_ids": jobIDs, "downloaded": false, "message": row.LastResult}, nil
}

func (a *App) queueSubscriptionAlbum(jobID, playlistID, album string, tracks []playlistTrack) error {
	encoded := encodeM3U(album, tracks)
	incoming := filepath.Join(a.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(incoming, playlistFileName(jobID)), encoded, 0o644); err != nil {
		return err
	}
	sites, err := a.loadSiteSettings()
	if err != nil {
		return err
	}
	streaming := false
	if settings, err := a.loadDownloadSettings(); err == nil {
		streaming = settings.StreamingEnabled
	}
	artist := ""
	if len(tracks) == 1 {
		artist = tracks[0].Artist
	}
	plan := SearchPlan{
		RootJobID: jobID, Query: album, ExpectedTitle: album, ExpectedArtist: artist,
		EnabledSites: sites.EnabledSites, StreamingEnabled: streaming,
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	ids := make([]string, len(tracks))
	for i := range tracks {
		ids[i] = fmt.Sprintf("t%d", i+1)
	}
	label := album
	if len(label) > 500 {
		label = label[:500]
	}
	_, err = a.flows.Start("start-"+jobID, workflow.Start{
		JobID: jobID, BatchID: jobID, Scene: "playlist", Stage: "identifying", Label: label,
		SourceTrackIDs: ids, MatchedCount: uint64(len(ids)), Payload: raw,
		NowNS: a.now().UnixNano(),
	})
	if err != nil {
		_ = os.Remove(filepath.Join(incoming, playlistFileName(jobID)))
		return err
	}
	return nil
}

func (a *App) readSavedPlaylist(id string) ([]playlistTrack, string, error) {
	if !playlistSlugPattern.MatchString(id) {
		return nil, "", errors.New("歌单 ID 无效")
	}
	data, err := os.ReadFile(filepath.Join(a.savedPlaylistsDir(), id+".m3u"))
	if err != nil {
		return nil, "", errors.New("找不到该歌单")
	}
	tracks, name, err := parseM3U(data)
	if err != nil {
		return nil, "", err
	}
	return tracks, name, nil
}

func (a *App) updateStoredSubscription(row playlistSubscription) error {
	return a.rewriteSubscription(row.PlaylistID, func(rows []playlistSubscription) ([]playlistSubscription, error) {
		for i := range rows {
			if rows[i].PlaylistID == row.PlaylistID {
				rows[i] = row
				return rows, nil
			}
		}
		return nil, errSubscriptionNotFound
	})
}

func (a *App) updateSubscription(id string, edit func(*playlistSubscription)) (playlistSubscription, error) {
	var updated playlistSubscription
	err := a.rewriteSubscription(id, func(rows []playlistSubscription) ([]playlistSubscription, error) {
		for i := range rows {
			if rows[i].PlaylistID == id {
				edit(&rows[i])
				updated = rows[i]
				return rows, nil
			}
		}
		return nil, errSubscriptionNotFound
	})
	return updated, err
}

func (a *App) rewriteSubscription(id string, edit func([]playlistSubscription) ([]playlistSubscription, error)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.loadSubscriptions()
	if err != nil {
		return err
	}
	rows, err = edit(rows)
	if err != nil {
		return err
	}
	return a.storeSubscriptions(rows)
}

var errSubscriptionNotFound = errors.New("找不到该订阅")

func writeSubscriptionError(w http.ResponseWriter, err error) {
	if errors.Is(err, errSubscriptionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
}

func trackKey(track playlistTrack) string {
	return track.Artist + "\x00" + track.Album + "\x00" + track.Title
}

func trackKeys(tracks []playlistTrack) []string {
	keys := make([]string, len(tracks))
	for i, track := range tracks {
		keys[i] = trackKey(track)
	}
	sort.Strings(keys)
	return keys
}

func subscriptionJobID(playlistID, album string, now time.Time, index int) string {
	sum := 0
	for _, r := range album {
		sum = (sum*33 + int(r)) % 100000000
	}
	return fmt.Sprintf("sub-%s-%d-%d-%08d", playlistID, now.UnixNano(), index, sum)
}

func (a *App) runSubscriptions(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			a.runDueSubscriptions()
		}
	}
}
