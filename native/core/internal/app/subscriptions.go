package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/textnorm"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const (
	playlistSubscriptionMissingCooldown = 7 * 24 * time.Hour
	playlistSubscriptionIncompleteRetry = 30 * time.Minute
	playlistSubscriptionAlbumBatch      = 100
)

// Playlist subscriptions re-read a saved local playlist. A row that records a
// public streaming-platform id refreshes that playlist first. New tracks and
// previously seen tracks that are still absent from the verified library are
// queued as playlist-scene jobs; a cooldown prevents an immediate duplicate
// batch. The first check can record current tracks without queueing them.
// Nothing here downloads audio.

type playlistSubscription struct {
	PlaylistID      string            `json:"playlist_id"`
	Name            string            `json:"name"`
	IntervalHours   int               `json:"interval_hours"`
	IncludeExisting bool              `json:"include_existing"`
	Paused          bool              `json:"paused"`
	Seen            []string          `json:"seen"`
	RetryMissing    map[string]string `json:"retry_missing,omitempty"`
	AlbumRetry      map[string]string `json:"album_completion_retry,omitempty"`
	Platform        string            `json:"platform,omitempty"`
	RemoteID        string            `json:"remote_id,omitempty"`
	LastChecked     string            `json:"last_checked,omitempty"`
	NextCheck       string            `json:"next_check,omitempty"`
	LastResult      string            `json:"last_result,omitempty"`
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
	return replaceFileAtomically(a.subscriptionPath(), encoded, 0o644)
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
		Platform        string `json:"platform"`
		RemoteID        string `json:"remote_id"`
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
	if body.Platform != "" && (remotePlatforms[body.Platform].Host == "" || !remoteIDOK(body.Platform, body.RemoteID)) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "只订阅已接入的公开歌单"})
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
		Platform: body.Platform, RemoteID: body.RemoteID,
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
			if body.Platform == "" {
				row.Platform = rows[i].Platform
				row.RemoteID = rows[i].RemoteID
			}
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

	var tracks []playlistTrack
	preservedFromLocal := false
	remoteTruncated, remotePreview := false, false
	remoteTrackCount := 0
	incompleteNotice := ""
	if row.Platform != "" && row.RemoteID != "" {
		remote, remoteErr := a.fetchRemotePlaylist(context.Background(), row.Platform, row.RemoteID, a.playlistBase(row.Platform))
		if remoteErr != nil {
			return nil, remoteErr
		}
		remoteTruncated, remotePreview = remote.Truncated, remote.Preview
		remoteTrackCount = len(remote.Tracks)
		if remote.Truncated || remote.Preview {
			// Preserve a canonical saved list while continuing the useful
			// subscription work. The truncated response is only a refresh
			// failure; it must not block every later queue pass forever.
			localTracks, _, readErr := a.readSavedPlaylist(playlistID)
			if readErr != nil || len(localTracks) == 0 || len(localTracks) <= len(remote.Tracks) {
				kind := "截断"
				if remote.Preview {
					kind = "预览"
				}
				now := a.now()
				row.LastResult = fmt.Sprintf("远端刷新只返回%s名单（%d 首），且没有可复用的完整本地名单；稍后重试", kind, len(remote.Tracks))
				row.NextCheck = now.Add(playlistSubscriptionIncompleteRetry).UTC().Format(time.RFC3339)
				if updateErr := a.updateStoredSubscription(row); updateErr != nil {
					return nil, updateErr
				}
				return map[string]any{
					"ok": true, "subscription": row, "queued": 0, "job_ids": []string{},
					"downloaded": false, "preserved": false, "truncated": remote.Truncated,
					"preview": remote.Preview, "remote_track_count": len(remote.Tracks), "local_track_count": 0,
					"message": row.LastResult,
				}, nil
			}
			tracks = localTracks
			preservedFromLocal = true
			kind := "截断"
			if remote.Preview {
				kind = "预览"
			}
			incompleteNotice = fmt.Sprintf("远端只返回%s名单（%d 首），已沿用本地完整名单（%d 首）核对", kind, remoteTrackCount, len(tracks))
		} else {
			tracks = remote.Tracks
			if writeErr := a.writeSavedPlaylist(playlistID, remote.Name, tracks); writeErr != nil {
				return nil, writeErr
			}
		}
	} else {
		var readErr error
		tracks, _, readErr = a.readSavedPlaylist(playlistID)
		if readErr != nil {
			return nil, readErr
		}
	}
	seen := map[string]bool{}
	for _, key := range row.Seen {
		seen[key] = true
	}
	now := a.now()
	fresh := len(row.Seen) == 0 && row.LastChecked == ""
	if fresh && !row.IncludeExisting {
		keys := trackKeys(tracks)
		row.Seen = keys
		row.RetryMissing = make(map[string]string, len(keys))
		retryAt := now.Add(playlistSubscriptionMissingCooldown).UTC().Format(time.RFC3339)
		for _, key := range keys {
			row.RetryMissing[key] = retryAt
		}
		row.LastChecked = now.UTC().Format(time.RFC3339)
		row.NextCheck = now.Add(time.Duration(row.IntervalHours) * time.Hour).UTC().Format(time.RFC3339)
		row.LastResult = fmt.Sprintf("已记录现有 %d 首，未创建任务", len(keys))
		if err := a.updateStoredSubscription(row); err != nil {
			return nil, err
		}
		if incompleteNotice != "" {
			row.LastResult = incompleteNotice + "；" + row.LastResult
			if err := a.updateStoredSubscription(row); err != nil {
				return nil, err
			}
		}
		return map[string]any{
			"ok": true, "subscription": row, "queued": 0, "job_ids": []string{}, "downloaded": false,
			"preserved": preservedFromLocal, "truncated": remoteTruncated, "preview": remotePreview,
			"remote_track_count": remoteTrackCount, "local_track_count": len(tracks), "message": row.LastResult,
		}, nil
	}
	retrySeenMissing := manual
	if !manual {
		if last, err := time.Parse(time.RFC3339, row.LastChecked); err == nil {
			retrySeenMissing = !now.Before(last.Add(time.Duration(row.IntervalHours) * time.Hour))
		}
	}
	if row.RetryMissing == nil {
		row.RetryMissing = map[string]string{}
	}
	if row.AlbumRetry == nil {
		row.AlbumRetry = map[string]string{}
	}
	library := a.playlistLibrary()
	groups := map[string][]playlistTrack{}
	groupLabels := map[string]string{}
	order := []string{}
	queuedKeys := map[string]bool{}
	albumPending := map[string]bool{}
	newTracks, missingTracks := 0, 0
	for _, track := range tracks {
		key := trackKey(track)
		present := library.hasPlaylistRow(track)
		needsSubscription := !present || library.needsAlbumCompletion(track.Artist, normalizePlaylistAlbumTitle(track.Album), track.Title)
		if !needsSubscription {
			delete(row.RetryMissing, key)
			delete(row.AlbumRetry, key)
			continue
		}
		albumNeedsCompletion := present
		seenTrack := seen[key]
		if seenTrack && !albumNeedsCompletion {
			if !retrySeenMissing {
				continue
			}
			if retryAt, err := time.Parse(time.RFC3339, row.RetryMissing[key]); err == nil && now.Before(retryAt) {
				continue
			}
		}
		if albumNeedsCompletion {
			albumPending[key] = true
			if !manual {
				if retryAt, err := time.Parse(time.RFC3339, row.AlbumRetry[key]); err == nil && now.Before(retryAt) {
					continue
				}
			}
		}
		if queuedKeys[key] {
			continue
		}
		queuedKeys[key] = true
		groupKey, groupLabel := subscriptionTrackGroup(track)
		if _, ok := groups[groupKey]; !ok {
			if len(order) >= playlistSubscriptionAlbumBatch {
				continue
			}
			order = append(order, groupKey)
			groupLabels[groupKey] = groupLabel
		}
		if seenTrack {
			missingTracks++
		} else {
			newTracks++
		}
		groups[groupKey] = append(groups[groupKey], track)
	}
	library.commitPresenceSnapshots()
	jobIDs := []string{}
	queuedTracks := 0
	for index, groupKey := range order {
		members := groups[groupKey]
		album := groupLabels[groupKey]
		jobID := subscriptionJobID(playlistID, album+"\x00"+groupKey, a.now(), index)
		completeAlbum := false
		for _, member := range members {
			if albumPending[trackKey(member)] {
				completeAlbum = true
				break
			}
		}
		if err := a.queueSubscriptionAlbum(jobID, playlistID, album, members, completeAlbum); err != nil {
			row.LastResult = "排队未完成：" + err.Error()
			_ = a.updateStoredSubscription(row)
			return nil, err
		}
		jobIDs = append(jobIDs, jobID)
		queuedTracks += len(members)
		for _, track := range members {
			key := trackKey(track)
			if !seen[key] {
				row.Seen = append(row.Seen, key)
			}
			seen[key] = true
			row.RetryMissing[key] = now.Add(playlistSubscriptionMissingCooldown).UTC().Format(time.RFC3339)
			if albumPending[key] {
				row.AlbumRetry[key] = now.Add(playlistSubscriptionMissingCooldown).UTC().Format(time.RFC3339)
			} else {
				delete(row.AlbumRetry, key)
			}
		}
		sort.Strings(row.Seen)
		row.LastResult = fmt.Sprintf("已为 %d 首新增曲目和 %d 首仍未入库的已见曲目创建任务；尚未下载、尚未入库", newTracks, missingTracks)
		if err := a.updateStoredSubscription(row); err != nil {
			return nil, err
		}
	}
	row.LastChecked = a.now().UTC().Format(time.RFC3339)
	row.NextCheck = a.now().Add(time.Duration(row.IntervalHours) * time.Hour).UTC().Format(time.RFC3339)
	if queuedTracks == 0 {
		row.LastResult = "没有需要重新排队的曲目"
	}
	if incompleteNotice != "" {
		row.LastResult = incompleteNotice + "；" + row.LastResult
	}
	if err := a.updateStoredSubscription(row); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok": true, "subscription": row, "queued": queuedTracks, "job_ids": jobIDs, "downloaded": false,
		"preserved": preservedFromLocal, "truncated": remoteTruncated, "preview": remotePreview,
		"remote_track_count": remoteTrackCount, "local_track_count": len(tracks), "message": row.LastResult,
	}, nil
}

func playlistTrackNeedsSubscription(library playlistLibraryIndex, track playlistTrack) bool {
	if !library.hasPlaylistRow(track) {
		return true
	}
	album := normalizePlaylistAlbumTitle(track.Album)
	return library.needsAlbumCompletion(track.Artist, album, track.Title)
}

func (a *App) queueSubscriptionAlbum(jobID, playlistID, album string, tracks []playlistTrack, completeAlbum bool) error {
	encoded := encodeM3U(album, tracks)
	incoming := a.workPath("incoming")
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
		RootJobID: jobID, Scene: "playlist", Query: album, ExpectedTitle: album, ExpectedArtist: artist,
		EnabledSites: sites.EnabledSites, StreamingEnabled: streaming,
	}
	if strings.HasPrefix(playlistID, "netease-") {
		albumID := int64(0)
		allSameAlbum, allHaveSongIDs := len(tracks) > 0, len(tracks) > 0
		for _, track := range tracks {
			if track.NeteaseSongID <= 0 {
				allHaveSongIDs = false
			}
			if albumID == 0 {
				albumID = track.NeteaseAlbumID
			} else if track.NeteaseAlbumID != albumID {
				allSameAlbum = false
			}
		}
		plan.NeteaseOnly = true
		// A single playlist row can still represent an incomplete album. In
		// that case keep the album id and fetch the full tracklist; treating
		// it as track-only would skip as soon as the one requested track is
		// present and leave the album permanently incomplete.
		plan.NeteaseTrackOnly = len(tracks) == 1 && !completeAlbum
		if len(tracks) == 1 {
			plan.PlaylistTrack = tracks[0].Title
			if seconds, parseErr := strconv.ParseFloat(strings.TrimSpace(tracks[0].Duration), 64); parseErr == nil && seconds > 0 {
				plan.ExpectedDuration = int(seconds + 0.5)
			}
			plan.NeteaseSongID = tracks[0].NeteaseSongID
			plan.NeteaseAlbumID = tracks[0].NeteaseAlbumID
		} else if allSameAlbum {
			plan.NeteaseAlbumID = albumID
		}
		if allHaveSongIDs && (len(tracks) == 1 || allSameAlbum && albumID > 0) {
			identityPayload, _ := json.Marshal(tracks)
			plan.Identity = Identity{
				ID: evidenceID(jobID, 0, "playlist-identity"), Kind: "api", Source: "netease",
				SubjectID: fmt.Sprintf("netease:album:%d:%d", albumID, len(tracks)), TracklistVerified: true,
				IdentityConfirmed: true, Payload: string(identityPayload),
			}
		}
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

func subscriptionTrackGroup(track playlistTrack) (key, label string) {
	album := strings.TrimSpace(track.Album)
	artist := strings.TrimSpace(track.Artist)
	title := strings.TrimSpace(track.Title)
	if track.NeteaseAlbumID > 0 {
		label = album
		if label == "" {
			label = artist + " - " + title
		}
		return fmt.Sprintf("netease-album:%d", track.NeteaseAlbumID), label
	}
	if album != "" && artist != "" {
		return "album\x00" + textnorm.Fold(album) + "\x00" + textnorm.Fold(artist), artist + " — " + album
	}
	label = album
	if label == "" {
		label = artist + " - " + title
	}
	return "track\x00" + textnorm.Fold(artist) + "\x00" + textnorm.Fold(album) + "\x00" + textnorm.Fold(title), label
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
			if !a.pipelinePaused() {
				a.runDueSubscriptions()
			}
		}
	}
}
