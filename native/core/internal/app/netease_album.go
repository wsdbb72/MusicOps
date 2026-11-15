package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

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
			ID    int64
			Name  string
			Size  int
			Songs []neteaseAlbumRow
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
		if row.ID == anchor.ID {
			found = normalizeKey(row.Name) == normalizeKey(anchor.Name)
		}
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
		if row.ID == anchor.ID && len(anchor.Artists) > 0 && !playlistArtistsMatch(anchor.Artists, credited) {
			found = false
		}
		duration := row.Duration
		if duration == 0 {
			duration = row.DT
		}
		if len(credited) == 0 || duration <= 0 {
			return nil, errors.New("网易云专辑曲目缺少艺人或时长")
		}
		out = append(out, neteaseSong{ID: row.ID, Name: row.Name, Artists: credited, Album: data.Album.Name, AlbumID: data.Album.ID, DurationMS: duration})
	}
	if !found {
		return nil, errors.New("网易云专辑清单没有包含匹配的目标曲目")
	}
	return out, nil
}

func (a *App) acquireNeteaseAlbum(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan) (scheduler.Outcome, error) {
	review := func(message, class string) (scheduler.Outcome, error) {
		return a.finishAcquire(held, job, "candidate", message, class, nil)
	}
	cookie := a.neteaseCookie()
	if cookie == "" {
		return review("网易云专辑补齐需要登录", "NETEASE_AUTH_REQUIRED")
	}
	var anchor *neteaseSong
	var err error
	if plan.NeteaseSongID > 0 && plan.NeteaseAlbumID > 0 {
		anchor = &neteaseSong{ID: plan.NeteaseSongID, AlbumID: plan.NeteaseAlbumID, Name: plan.PlaylistTrack, Album: plan.ExpectedTitle, Artists: strings.Split(plan.ExpectedArtist, " / ")}
	} else {
		anchor, err = a.neteaseSearchSong(ctx, plan)
	}
	if err != nil || anchor == nil || anchor.AlbumID <= 0 {
		return review("未核实到包含目标曲目的网易云发行版", "NETEASE_ALBUM_UNCONFIRMED")
	}
	raw, err := a.neteaseGet(ctx, a.neteaseBase()+"/api/v1/album/"+strconv.FormatInt(anchor.AlbumID, 10), cookie)
	if err != nil {
		return review("无法读取网易云专辑清单", "NETEASE_ALBUM_UNAVAILABLE")
	}
	tracks, err := decodeNeteaseAlbum(raw, anchor)
	if err != nil {
		return review(err.Error(), "NETEASE_ALBUM_UNCONFIRMED")
	}
	artist, album := publishComponent(plan.ExpectedArtist), publishComponent(plan.ExpectedTitle)
	local := a.verifiedPlaylistAlbumTracks(a.libraryDir(), artist, album, plan.ExpectedTitle)
	type staged struct {
		path     string
		plan     SearchPlan
		position int
	}
	stagedFiles := make([]staged, 0, len(tracks))
	missing := []string{}
	for i := range tracks {
		song := &tracks[i]
		if plan.NeteaseTrackOnly && song.ID != anchor.ID {
			continue
		}
		if err := execution.Checkpoint(ctx, "netease_album", float64(i)*100/float64(len(tracks)), song.Name); err != nil {
			return scheduler.Outcome{}, err
		}
		present := false
		for _, file := range local[normalizeKey(song.Name)] {
			tags, err := readFLACTagsPrefix(file)
			if err != nil || tags.StreamInfo.SampleRate == 0 || !playlistArtistsMatch(song.Artists, tags.Artists) {
				continue
			}
			ms := int64(tags.StreamInfo.TotalSamples) * 1000 / int64(tags.StreamInfo.SampleRate)
			delta := ms - int64(song.DurationMS)
			if delta >= -1500 && delta <= 1500 {
				present = true
				break
			}
		}
		if present {
			continue
		}
		trackPlan := plan
		trackPlan.PlaylistTrack = song.Name
		path, note, _, _ := a.stageNeteaseSong(ctx, trackPlan, song)
		if path == "" {
			missing = append(missing, song.Name+"："+note)
			continue
		}
		stagedFiles = append(stagedFiles, staged{path, trackPlan, i + 1})
	}
	if len(stagedFiles) == 0 {
		if len(missing) > 0 {
			return review("没有可入库的无损缺轨："+missing[0], "NETEASE_ALBUM_INCOMPLETE")
		}
		if plan.NeteaseTrackOnly {
			return a.finishSkipAcquire(held, job, "网易云歌单目标曲目已在库")
		}
		return a.finishSkipAcquire(held, job, "网易云专辑的全部曲目已在库")
	}
	receipts := make([]publishReceipt, 0, len(stagedFiles))
	for _, file := range stagedFiles {
		if err := ctx.Err(); err != nil {
			return scheduler.Outcome{}, err
		}
		item, err := a.ingestPlaylistStream(ctx, job, file.plan, file.path, file.position)
		if err != nil {
			return review(err.Error(), "STREAM_INGEST_FAILED")
		}
		receipts = append(receipts, item)
	}
	return a.finishPlaylistStreams(held, job, receipts, fmt.Sprintf("网易云发行版共 %d 首，取到 %d 首无损，%d 首未取得；等待发布校验", len(tracks), len(receipts), len(missing)), missing...)
}
