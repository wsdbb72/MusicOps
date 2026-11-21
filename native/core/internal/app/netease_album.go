package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

type neteaseAlbumArtist struct{ Name string }
type neteaseAlbumRow struct {
	ID           int64
	Name         string
	Artists, AR  []neteaseAlbumArtist
	Duration, DT int
}

func decodeNeteaseAlbum(raw []byte, anchor *neteaseSong) ([]neteaseSong, error) {
	var data struct {
		Code  int
		Album struct {
			ID     int64
			Name   string
			Size   int
			PicURL string `json:"picUrl"`
			Songs  []neteaseAlbumRow
		}
		Songs []neteaseAlbumRow
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Code != 200 || data.Album.ID != anchor.AlbumID || normalizeKey(data.Album.Name) != normalizeKey(anchor.Album) {
		return nil, errors.New("网易云返回的发行版与已匹配曲目不一致")
	}
	rows := data.Songs
	if len(rows) == 0 {
		rows = data.Album.Songs
	}
	if len(rows) < 1 || len(rows) > 100 || len(rows) != data.Album.Size {
		return nil, errors.New("网易云专辑轨道清单不完整或超过 100 首")
	}
	seen := map[int64]bool{}
	found := false
	out := make([]neteaseSong, 0, len(rows))
	for _, row := range rows {
		if row.ID <= 0 || seen[row.ID] || strings.TrimSpace(row.Name) == "" {
			return nil, errors.New("网易云专辑含无效曲目")
		}
		seen[row.ID] = true
		artists := row.Artists
		if len(artists) == 0 {
			artists = row.AR
		}
		credited := make([]string, 0, len(artists))
		for _, artist := range artists {
			if strings.TrimSpace(artist.Name) != "" {
				credited = append(credited, artist.Name)
			}
		}
		if row.ID == anchor.ID {
			if anchor.Name == "" || normalizeKey(row.Name) == normalizeKey(anchor.Name) {
				found = true
				if anchor.Name == "" {
					anchor.Name = row.Name
				}
				if len(anchor.Artists) == 0 {
					anchor.Artists = append([]string(nil), credited...)
				} else if !playlistArtistsMatch(anchor.Artists, credited) {
					found = false
				}
			}
		}
		duration := row.Duration
		if duration == 0 {
			duration = row.DT
		}
		if len(credited) == 0 || duration <= 0 {
			return nil, errors.New("网易云专辑曲目缺少艺人或时长")
		}
		out = append(out, neteaseSong{ID: row.ID, Name: row.Name, Artists: credited, Album: data.Album.Name, AlbumID: data.Album.ID, DurationMS: duration, CoverURL: data.Album.PicURL, CatalogVerified: true})
	}
	if !found {
		return nil, errors.New("网易云专辑清单没有包含匹配的目标曲目")
	}
	return out, nil
}

// recoverNeteasePlaylistIDs rehydrates durable IDs for jobs created before the
// search plan carried them. It first uses the job's persisted M3U. Older M3Us
// predate the ID headers, so it refreshes that same ordered playlist once from
// the user's saved NetEase credential and merges IDs by title, artist and
// album without changing the selected track order.
func (a *App) recoverNeteasePlaylistIDs(ctx context.Context, batchID string, plan *SearchPlan) bool {
	if plan == nil || !strings.HasPrefix(batchID, "pl-netease-") {
		return false
	}
	if plan.NeteaseSongID > 0 {
		return true
	}
	a.neteaseRecoveryMu.Lock()
	defer a.neteaseRecoveryMu.Unlock()
	if plan.NeteaseSongID > 0 {
		return true
	}
	rows, name, err := a.playlistTrackRows(batchID)
	if err == nil && recoverIDsFromPlaylistRows(rows, plan) {
		return true
	}
	remoteID := neteasePlaylistIDFromBatch(batchID)
	if remoteID == "" {
		return false
	}
	remote, err := a.fetchRemotePlaylist(ctx, "netease", remoteID, "")
	if err != nil || len(remote.Tracks) == 0 {
		return false
	}
	if len(rows) == 0 {
		rows = remote.Tracks
		name = remote.Name
	} else {
		updated := mergeNeteasePlaylistIDs(rows, remote.Tracks)
		if updated == 0 && !recoverIDsFromPlaylistRows(rows, plan) {
			return false
		}
		if name == "" {
			name = remote.Name
		}
		data := encodeM3U(name, rows)
		path := a.workPath("incoming", playlistFileName(batchID))
		temp, createErr := os.CreateTemp(filepath.Dir(path), ".playlist-ids-*.tmp")
		if createErr != nil {
			return false
		}
		tempName := temp.Name()
		_, writeErr := temp.Write(data)
		closeErr := temp.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(tempName)
			return false
		}
		if renameErr := os.Rename(tempName, path); renameErr != nil {
			_ = os.Remove(tempName)
			return false
		}
	}
	return recoverIDsFromPlaylistRows(rows, plan)
}

func recoverIDsFromPlaylistRows(rows []playlistTrack, plan *SearchPlan) bool {
	if plan == nil {
		return false
	}
	if plan.NeteaseSongID > 0 {
		return true
	}
	title := strings.TrimSpace(neteasePlaylistTitle(*plan))
	if title == "" {
		return false
	}
	wantArtists := splitPlaylistArtists(plan.ExpectedArtist)
	wantAlbum := normalizePlaylistAlbumTitle(plan.ExpectedTitle)
	matches := make([]playlistTrack, 0, 1)
	for _, row := range rows {
		if row.NeteaseSongID <= 0 || normalizeKey(row.Title) != normalizeKey(title) {
			continue
		}
		if len(wantArtists) == 0 || playlistArtistsMatch(wantArtists, splitPlaylistArtists(row.Artist)) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 && len(wantArtists) == 0 && wantAlbum != "" {
		for _, row := range rows {
			if row.NeteaseSongID > 0 && normalizeKey(row.Title) == normalizeKey(title) && normalizePlaylistAlbumTitle(row.Album) == wantAlbum {
				matches = append(matches, row)
			}
		}
	}
	if len(matches) > 1 && wantAlbum != "" {
		filtered := matches[:0]
		for _, row := range matches {
			if normalizePlaylistAlbumTitle(row.Album) == wantAlbum {
				filtered = append(filtered, row)
			}
		}
		matches = filtered
	}
	if len(matches) == 0 {
		return false
	}
	chosen := matches[0]
	for _, row := range matches[1:] {
		if row.NeteaseSongID != chosen.NeteaseSongID || row.NeteaseAlbumID != chosen.NeteaseAlbumID {
			return false
		}
	}
	plan.PlaylistTrack = firstNonEmpty(plan.PlaylistTrack, chosen.Title)
	plan.NeteaseSongID = chosen.NeteaseSongID
	if chosen.NeteaseAlbumID > 0 {
		plan.NeteaseAlbumID = chosen.NeteaseAlbumID
	}
	return plan.NeteaseSongID > 0
}

func mergeNeteasePlaylistIDs(local, remote []playlistTrack) int {
	updated := 0
	for i := range local {
		if local[i].NeteaseSongID > 0 && local[i].NeteaseAlbumID > 0 {
			continue
		}
		var match *playlistTrack
		for j := range remote {
			candidate := &remote[j]
			if candidate.NeteaseSongID <= 0 || normalizeKey(candidate.Title) != normalizeKey(local[i].Title) ||
				!playlistArtistsMatch(splitPlaylistArtists(local[i].Artist), splitPlaylistArtists(candidate.Artist)) {
				continue
			}
			if normalizePlaylistAlbumTitle(candidate.Album) != normalizePlaylistAlbumTitle(local[i].Album) {
				continue
			}
			if match != nil && (match.NeteaseSongID != candidate.NeteaseSongID || match.NeteaseAlbumID != candidate.NeteaseAlbumID) {
				match = nil
				break
			}
			match = candidate
		}
		if match == nil {
			continue
		}
		local[i].NeteaseSongID = match.NeteaseSongID
		local[i].NeteaseAlbumID = match.NeteaseAlbumID
		updated++
	}
	return updated
}

func neteasePlaylistIDFromBatch(batchID string) string {
	slug := strings.TrimPrefix(batchID, "pl-netease-")
	if end := strings.IndexByte(slug, '-'); end > 0 {
		slug = slug[:end]
	}
	if remoteIDPatterns["netease"].MatchString(slug) {
		return slug
	}
	return ""
}

func splitPlaylistArtists(raw string) []string {
	normalized := strings.NewReplacer("／", "/", "、", "/", ",", "/", ";", "/", "；", "/").Replace(raw)
	out := []string{}
	for _, part := range strings.Split(normalized, "/") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (a *App) acquireNeteaseAlbum(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan) (scheduler.Outcome, error) {
	a.recoverNeteasePlaylistIDs(ctx, job.BatchID, &plan)
	review := func(message, class string) (scheduler.Outcome, error) {
		return a.finishAcquire(held, job, "candidate", message, class, nil)
	}
	artist := publishComponent(firstNonEmpty(plan.AlbumArtist, plan.ExpectedArtist))
	album := publishComponent(plan.ExpectedTitle)
	directTrack := func(song *neteaseSong, fallbackReason string) (scheduler.Outcome, error) {
		if plan.PlaylistTrack == "" {
			plan.PlaylistTrack = neteasePlaylistTitle(plan)
		}
		if song == nil {
			var err error
			song, err = a.neteaseSearchSong(ctx, plan)
			if err != nil || song == nil {
				return review(fallbackReason, "NETEASE_ALBUM_UNCONFIRMED")
			}
		}
		path, note, class, _ := a.stageNeteaseSong(ctx, plan, song)
		if path == "" {
			if isTemporaryStreamFailure(class) {
				message := "网易云音频暂时不可取：" + note
				if job.SearchRetryCount >= 3 {
					return a.finishSkipAcquire(held, job, message+"；已有限重试三轮")
				}
				return review(message+"；将由自动核对重试", "STREAM_RETRY")
			}
			return review(note, class)
		}
		if song.CatalogVerified {
			// When the album endpoint was unavailable, the exact source
			// recording we just retrieved provides the comparison duration.
			if tags, err := readFLACTagsPrefix(path); err == nil && tags.StreamInfo.SampleRate != 0 {
				source := *song
				source.DurationMS = int(tags.StreamInfo.TotalSamples * 1000 / uint64(tags.StreamInfo.SampleRate))
				local := a.verifiedPlaylistAlbumTracks(a.libraryDir(), artist, album, plan.ExpectedTitle)
				if verifiedNeteaseTrackPresent(local, &source) {
					removeNeteaseStaging(a.workPath("incoming"), path, "scraped-"+filepath.Base(path))
					return a.finishSkipAcquire(held, job, "已核实目标曲目在库；网易云暂未提供可验证的专辑清单")
				}
			}
		}
		item, err := a.ingestPlaylistStream(ctx, job, plan, path, 1)
		if err != nil {
			return review(err.Error(), "STREAM_INGEST_FAILED")
		}
		return a.finishPlaylistStreams(held, job, []publishReceipt{item}, "网易云发行版未确认，已按原曲兜底取到可用音频；等待发布校验")
	}
	if plan.PlaylistTrack == "" {
		plan.PlaylistTrack = neteasePlaylistTitle(plan)
	}
	cookie := a.neteaseCookie()
	if cookie == "" {
		return review("网易云专辑补齐需要登录", "NETEASE_AUTH_REQUIRED")
	}
	var anchor *neteaseSong
	var err error
	if plan.NeteaseSongID > 0 {
		anchor = &neteaseSong{ID: plan.NeteaseSongID, AlbumID: plan.NeteaseAlbumID, Name: plan.PlaylistTrack, Album: plan.ExpectedTitle, Artists: strings.Split(plan.ExpectedArtist, " / "), CatalogVerified: true}
	} else {
		anchor, err = a.neteaseSearchSong(ctx, plan)
	}
	if err != nil || anchor == nil || anchor.AlbumID <= 0 {
		return directTrack(anchor, "未核实到包含目标曲目的网易云发行版")
	}
	raw, err := a.neteaseGet(ctx, a.neteaseBase()+"/api/v1/album/"+strconv.FormatInt(anchor.AlbumID, 10), cookie)
	if err != nil {
		return directTrack(anchor, "无法读取网易云专辑清单")
	}
	tracks, err := decodeNeteaseAlbum(raw, anchor)
	if err != nil {
		return directTrack(anchor, err.Error())
	}
	local := a.verifiedPlaylistAlbumTracks(a.libraryDir(), artist, album, plan.ExpectedTitle)
	receipts := make([]publishReceipt, 0, len(tracks))
	missing := []string{}
	var ingestFailure error
	temporaryFailure := false
	order := make([]int, len(tracks))
	for i := range order {
		order[i] = i
	}
	// The playlist row that created this job must be processed first. A large
	// compilation can exceed the bounded staging cache; completing every extra
	// album track before the requested one makes the whole job look stuck.
	if plan.NeteaseSongID > 0 {
		for pos, index := range order {
			if tracks[index].ID == plan.NeteaseSongID {
				copy(order[1:pos+1], order[:pos])
				order[0] = index
				break
			}
		}
	}
	for processed, index := range order {
		song := &tracks[index]
		if plan.NeteaseTrackOnly && song.ID != anchor.ID {
			continue
		}
		if err := execution.Checkpoint(ctx, "netease_album", float64(processed)*100/float64(len(order)), song.Name); err != nil {
			return scheduler.Outcome{}, err
		}
		if verifiedNeteaseTrackPresent(local, song) {
			continue
		}
		trackPlan := plan
		trackPlan.PlaylistTrack = song.Name
		trackPlan.NeteaseSongID = song.ID
		path, note, class, _ := a.stageNeteaseSong(ctx, trackPlan, song)
		if path == "" {
			if isTemporaryStreamFailure(class) {
				temporaryFailure = true
			}
			missing = append(missing, song.Name+"："+note)
			continue
		}
		// Ingest each staged track immediately. Holding a whole 100-track album
		// in incoming/ exhausted the SSD cache before publication could begin.
		item, err := a.ingestPlaylistStream(ctx, job, trackPlan, path, index+1)
		if err != nil {
			if !errors.Is(err, audiorelease.ErrConflict) && !errors.Is(err, audiorelease.ErrRejected) {
				return review(err.Error(), "STREAM_INGEST_FAILED")
			}
			ingestFailure = err
			// One rejected track must not discard the verified tracks that
			// already published. Record it as missing and let the release advance;
			// retain the publication error if no track was published.
			missing = append(missing, song.Name+"："+err.Error())
			continue
		}
		receipts = append(receipts, item)
	}
	if temporaryFailure {
		message := fmt.Sprintf("网易云音频暂时不可取；本轮已入库 %d 首，仍缺 %d 首：%s", len(receipts), len(missing), firstMissing(missing))
		if job.SearchRetryCount < 3 {
			return review(message+"；将由自动核对重试", "STREAM_RETRY")
		}
		if len(receipts) == 0 {
			return a.finishSkipAcquire(held, job, message+"；已有限重试三轮")
		}
		return a.finishPlaylistStreams(held, job, receipts, message+"；已有限重试三轮，先校验本轮已入库曲目", missing...)
	}
	if len(receipts) == 0 {
		if ingestFailure != nil {
			return review(ingestFailure.Error(), "STREAM_INGEST_FAILED")
		}
		if len(missing) > 0 {
			// The NetEase album and selected song ids are durable catalogue
			// evidence. Once every available quality has been tried, keep the
			// row terminal instead of returning the same unavailable track to
			// human review on every scheduler pass.
			if plan.NeteaseSongID > 0 && plan.NeteaseAlbumID > 0 {
				return a.finishSkipAcquire(held, job, "网易云已核实该发行版但未提供可用音频，已自动结束："+missing[0])
			}
			return review("没有可入库的可用缺轨："+missing[0], "NETEASE_ALBUM_INCOMPLETE")
		}
		if plan.NeteaseTrackOnly {
			return a.finishSkipAcquire(held, job, "网易云歌单目标曲目已在库")
		}
		return a.finishSkipAcquire(held, job, "网易云专辑的全部曲目已在库")
	}
	return a.finishPlaylistStreams(held, job, receipts, fmt.Sprintf("网易云发行版共 %d 首，取到 %d 首可用音频，%d 首未取得；等待发布校验", len(tracks), len(receipts), len(missing)), missing...)
}

func verifiedNeteaseTrackPresent(local map[string][]string, song *neteaseSong) bool {
	if song.DurationMS <= 0 {
		return false
	}
	tolerance := int64(1500)
	if song.CatalogVerified {
		// Match padding differences only for an identified catalogue track.
		// The folder, title and credited artists must still match.
		tolerance = 3000
	}
	for _, file := range local[normalizeKey(song.Name)] {
		tags, err := readFLACTagsPrefix(file)
		if err != nil || tags.StreamInfo.SampleRate == 0 || !playlistArtistsMatch(song.Artists, tags.Artists) {
			continue
		}
		duration := int64(tags.StreamInfo.TotalSamples) * 1000 / int64(tags.StreamInfo.SampleRate)
		delta := duration - int64(song.DurationMS)
		if delta >= -tolerance && delta <= tolerance {
			return true
		}
	}
	return false
}
func isTemporaryStreamFailure(class string) bool {
	switch class {
	case downloadpolicy.ClassRateLimited, downloadpolicy.ClassNetwork:
		return true
	default:
		return false
	}
}

func firstMissing(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[0]
}
