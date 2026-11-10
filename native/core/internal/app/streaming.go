package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// Streaming fallback for fallback-stage process jobs.
//
// 0.4.31 reached TIDAL, Apple Music, QQ Music and NetEase through external
// go-music-dl and musicdl processes. Those programs are not part of this
// core. A missing engine stays "未接入" and is never reported as a download.
//
// The walk follows downloadpolicy order (tidal, applemusic, qqmusic,
// netease, playlist). The four platform sources have no credentials or
// engine in this core, so each attempt is honestly recorded as no_result
// with the reason in the job message, never as a download. The playlist
// step imports track metadata from a user-placed M3U file, following the
// Python playlist_files contract: EXTINF rows only, the URI line is
// metadata-only and is never opened or fetched. Nothing here downloads
// audio from a streaming service.

// playlistTrack is one M3U entry: artist and title from EXTINF, nothing
// else. The URI line is discarded, exactly like the Python importer.
type playlistTrack struct {
	Artist   string `json:"artist"`
	Title    string `json:"title"`
	Album    string `json:"album,omitempty"`
	Duration string `json:"duration"`
}

const maxPlaylistTracks = 50000

var extinfPattern = regexp.MustCompile(`^#EXTINF:([+-]?[\d.]+)(?:\s+[^,]*)?,(.*)$`)

// parseM3U parses an M3U/M3U8 playlist the way Python's _m3u_rows does:
// only entries with an EXTINF line count, the URI line is metadata-only,
// a dangling EXTINF or a bare URI without EXTINF is an error.
func parseM3U(data []byte) ([]playlistTrack, string, error) {
	var rows []playlistTrack
	var name string
	var album string
	var pending *playlistTrack
	for _, raw := range strings.Split(string(data), "\n") {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if strings.HasPrefix(value, "#PLAYLIST:") {
			name = strings.TrimSpace(strings.TrimPrefix(value, "#PLAYLIST:"))
			continue
		}
		if strings.HasPrefix(value, "#MUSICOPS-ALBUM:") {
			album = strings.TrimSpace(strings.TrimPrefix(value, "#MUSICOPS-ALBUM:"))
			continue
		}
		if strings.HasPrefix(value, "#EXTINF:") {
			if pending != nil {
				return nil, "", errors.New("M3U 的 EXTINF 缺少对应条目行")
			}
			match := extinfPattern.FindStringSubmatch(value)
			if match == nil {
				return nil, "", errors.New("M3U EXTINF 格式无效")
			}
			description := match[2]
			parts := strings.SplitN(description, " - ", 2)
			track := playlistTrack{Duration: match[1], Title: parts[len(parts)-1]}
			if len(parts) == 2 {
				track.Artist = parts[0]
			}
			track.Album = album
			album = ""
			pending = &track
			continue
		}
		if strings.HasPrefix(value, "#") {
			continue
		}
		if pending == nil {
			return nil, "", errors.New("M3U 仅支持带 EXTINF 歌名元数据的条目，不从音源路径猜歌名")
		}
		rows = append(rows, *pending)
		pending = nil
		if len(rows) > maxPlaylistTracks {
			return nil, "", errors.New("单个文件最多导入 50000 首")
		}
	}
	if pending != nil {
		return nil, "", errors.New("M3U 的最后一项缺少条目行")
	}
	return rows, name, nil
}

// streamSourceStatus is one source's wiring state for the settings view.
// Configured means an engine for it exists in this core; the four platform
// sources have none, so they report why instead of pretending to work.
type streamSourceStatus struct {
	Name       string `json:"name"`
	Tier       int    `json:"tier"`
	Quality    string `json:"quality"`
	Enabled    bool   `json:"enabled"`
	Configured bool   `json:"configured"`
	Reason     string `json:"reason"`
}

// streamingSourceStatus reports every default source's wiring state, in
// the saved fallback order. Configured means an engine for it exists in
// this core; the four platform sources have none, so they report why
// instead of pretending to work.
func (a *App) streamingSourceStatus() []streamSourceStatus {
	ordered, err := downloadpolicy.OrderedSources(a.streamingOrder())
	if err != nil {
		ordered = downloadpolicy.DefaultSources()
	}
	out := make([]streamSourceStatus, 0, len(ordered))
	for _, source := range ordered {
		status := streamSourceStatus{Name: source.Name, Tier: source.Tier, Quality: source.Quality, Enabled: source.Enabled}
		switch source.Name {
		case "playlist":
			status.Configured = true
			status.Reason = "歌单文件导入已接入（仅元数据，不下载音频）"
		case "qobuz", "deezer":
			status.Reason = "未启用"
		default:
			status.Reason = "无凭证与引擎，未接入"
		}
		out = append(out, status)
	}
	return out
}

// playlistFile finds the user-placed playlist file for a root job, if any.
// The file lives under incoming/ and is named after the root job so one
// playlist cannot leak into another job.
func (a *App) playlistFile(rootJobID string) (string, bool) {
	for _, suffix := range []string{".m3u", ".m3u8"} {
		candidate, err := resolveInDir(a.Dir, filepath.Join("incoming", "playlist-"+rootJobID+suffix))
		if err != nil {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// acquireFallback runs the streaming walk for a fallback-stage process job.
// It replaces the old "尚未接入" punt: the walk really runs, each source's
// outcome is recorded, and the job ends in needs_review with the walk as
// evidence instead of an empty placeholder message.
func (a *App) acquireFallback(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan) (scheduler.Outcome, error) {
	// The fallback job carries album-level identity only; it has no
	// per-track source or identifying information, so the playlist step
	// stays in the plan. The walk follows the saved streaming order
	// (canonical when nothing was saved).
	steps, err := downloadpolicy.PlanOrdered(downloadpolicy.DefaultSources(), a.streamingOrder(), downloadpolicy.Track{})
	if err != nil {
		return scheduler.Outcome{}, err
	}
	outcomes := make([]downloadpolicy.Outcome, 0, len(steps))
	notes := make([]string, 0, len(steps))
	var imported []playlistTrack
	var importedName string
	for _, step := range steps {
		if ctx.Err() != nil {
			return scheduler.Outcome{}, ctx.Err()
		}
		switch step.Name {
		case "playlist":
			path, found := a.playlistFile(plan.RootJobID)
			if !found {
				outcomes = append(outcomes, downloadpolicy.Outcome{Source: "playlist", Class: downloadpolicy.ClassNoResult})
				notes = append(notes, "playlist 未找到歌单文件 incoming/playlist-"+plan.RootJobID+".m3u")
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				outcomes = append(outcomes, downloadpolicy.Outcome{Source: "playlist", Class: downloadpolicy.ClassNoResult})
				notes = append(notes, "playlist 歌单文件无法读取")
				continue
			}
			rows, name, err := parseM3U(data)
			if err != nil {
				outcomes = append(outcomes, downloadpolicy.Outcome{Source: "playlist", Class: downloadpolicy.ClassNoResult})
				notes = append(notes, "playlist 歌单文件无法解析："+err.Error())
				continue
			}
			imported, importedName = rows, name
			outcomes = append(outcomes, downloadpolicy.Outcome{Source: "playlist", Class: downloadpolicy.ClassImport})
			notes = append(notes, fmt.Sprintf("playlist 已读取 %d 首歌单曲目", len(rows)))
		default:
			// No credentials or engine for the platform sources: the
			// attempt is recorded as no result, never as a download.
			outcomes = append(outcomes, downloadpolicy.Outcome{Source: step.Name, Class: downloadpolicy.ClassNoResult})
			notes = append(notes, step.Name+" 未接入（无凭证与引擎）")
		}
	}
	walk, err := downloadpolicy.Apply(steps, outcomes, 0)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	complete := workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "candidate", TaskState: "blocked",
		Message: "流媒体回退已走完：" + strings.Join(notes, "、") + "；尚未下载、尚未入库", NowNS: a.now().UnixNano(),
	}
	if walk.ReviewRequired && len(imported) > 0 {
		complete.Message = fmt.Sprintf("歌单已读取 %d 首曲目%s，待核对；尚未下载、尚未入库", len(imported), playlistSuffix(importedName))
	}
	if walk.Recoverable {
		complete.Message += fmt.Sprintf("（%d 秒后可重试）", walk.RetryAfterSec)
	}
	encoded, err := json.Marshal(walk)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	complete.Evidence = []workflow.EvidenceInput{{
		ID: evidenceID(held.ID, held.Fence, "streaming"), Kind: "api", Source: "streaming",
		SubjectID: job.RootID, Payload: encoded,
	}}
	if len(imported) > 0 {
		rows, err := json.Marshal(imported)
		if err != nil {
			return scheduler.Outcome{}, err
		}
		complete.Evidence = append(complete.Evidence, workflow.EvidenceInput{
			ID: evidenceID(held.ID, held.Fence, "playlist"), Kind: "catalog", Source: "playlist",
			SubjectID: job.RootID, TracklistVerified: false, Payload: rows,
		})
	}
	if _, err := a.flows.Complete(intentID("streaming", held.ID, held.Fence), held, complete); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
}

func playlistSuffix(name string) string {
	if name == "" {
		return ""
	}
	return "（" + name + "）"
}
