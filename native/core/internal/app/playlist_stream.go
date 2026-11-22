package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func shouldPublishPlaylistStream(plan SearchPlan, validated string) bool {
	return strings.TrimSpace(validated) != "" && strings.TrimSpace(plan.PlaylistTrack) != ""
}
func playlistStreamReplacementPath(plan SearchPlan) string {
	if plan.Scene == "upgrade" && len(plan.LibraryTracks) > 0 {
		return plan.LibraryTracks[0].Path
	}
	return ""
}
func playlistStreamPosition(plan SearchPlan) int {
	if len(plan.LibraryTracks) > 0 {
		return trackNumberFromName(filepath.Base(plan.LibraryTracks[0].Path))
	}
	return 1
}

func (a *App) publishPlaylistStream(ctx context.Context, held taskrepo.Token, job workflow.Job, plan SearchPlan, path string) (scheduler.Outcome, error) {
	item, err := a.ingestPlaylistStream(ctx, job, plan, path, playlistStreamPosition(plan))
	if err != nil {
		return a.finishAcquire(held, job, "candidate", err.Error(), "STREAM_INGEST_FAILED", nil)
	}
	return a.finishPlaylistStreams(held, job, []publishReceipt{item}, "已取到可用音频并执行刮削，等待发布校验")
}

func (a *App) ingestPlaylistStream(ctx context.Context, job workflow.Job, plan SearchPlan, path string, position int) (publishReceipt, error) {
	root := a.workPath("incoming")
	name, title, scrape, err := a.scrapePlaylistIncoming(ctx, root, filepath.Base(path), plan.ExpectedArtist, plan.AlbumArtist, plan.ExpectedTitle, plan.PlaylistTrack, position)
	if err != nil {
		return publishReceipt{}, fmt.Errorf("流媒体音频刮削失败，尚未入库: %w", err)
	}
	diskTitle := title
	if isHotSlowCompilationAlbum(plan.ExpectedTitle) && plan.NeteaseSongID > 0 {
		// The shared folder can contain different recordings with the same
		// title and position. Use the catalogue ID in the filename only.
		diskTitle = "[" + strconv.FormatInt(plan.NeteaseSongID, 10) + "] " + title
	}
	id := audioRequestID(job.RootID, position)
	result, err := audiorelease.Release(ctx, a.engine, audiorelease.Request{
		RequestID: id, RootID: job.RootID, Track: position, Artist: publishComponent(plan.ExpectedArtist), FolderArtist: func() string {
			if strings.TrimSpace(plan.AlbumArtist) == "" {
				return ""
			}
			return publishComponent(plan.AlbumArtist)
		}(), Album: publishComponent(plan.ExpectedTitle), Title: publishComponent(diskTitle),
		SourceName: name, Incoming: root, Library: a.libraryDir(), Decoder: audiorelease.SystemDecoder(), InternalDownload: true,
		ReplacePath: playlistStreamReplacementPath(plan),
	})
	if err != nil {
		return publishReceipt{}, playlistReleaseError(err, result)
	}
	a.invalidatePlaylistLibraryCache()
	placed, found, err := audiorelease.PathFor(a.engine, id)
	if err != nil || !found {
		return publishReceipt{}, fmt.Errorf("已复制音频，但无法确认发布位置: %w", err)
	}
	if err := a.indexReleasedPath(placed); err != nil {
		return publishReceipt{}, fmt.Errorf("已复制音频，但无法登记本地索引: %w", err)
	}
	if scrape.lyrics != "" {
		if !hasLyricsSidecar(placed) {
			if err := writeLyricsSidecar(placed, scrape.lyrics); err != nil {
				return publishReceipt{}, fmt.Errorf("已复制音频，但歌词写入失败: %w", err)
			}
		}
	}
	removePublishedStreamStaging(root, path, name)
	return publishReceipt{TrackID: strconv.Itoa(position), RequestID: id, SHA256: result.SHA256, Format: result.Format, Origin: result.Origin, SourceName: result.SourceName, DecodeState: result.DecodeState, Scrape: scrape}, nil
}

func (a *App) indexReleasedPath(path string) error {
	root := a.libraryDir()
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("发布文件不在当前音乐库")
	}
	track := trackNumberFromName(filepath.Base(path))
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	_, err = audiorelease.IndexExisting(a.engine, audiorelease.Existing{
		RequestID: libraryFileID(path), RootID: libraryRootID(filepath.Dir(path)), Track: track,
		Path: path, Library: root, Format: ext, SourceName: "stream-release", MetadataOnly: true,
	})
	return err
}

func playlistReleaseError(err error, result audiorelease.Result) error {
	detail := strings.TrimSpace(result.Class)
	if detail == "" {
		switch {
		case errors.Is(err, audiorelease.ErrConflict):
			detail = "library_conflict"
		case errors.Is(err, audiorelease.ErrMissing):
			detail = "audio_missing"
		case errors.Is(err, audiorelease.ErrTool):
			detail = "decoder_missing"
		default:
			detail = "release_failed"
		}
	}
	return fmt.Errorf("流媒体音频未通过发布校验（%s），尚未入库: %w", detail, err)
}

func (a *App) finishPlaylistStreams(held taskrepo.Token, job workflow.Job, items []publishReceipt, message string, missing ...string) (scheduler.Outcome, error) {
	payload, err := json.Marshal(publishPayload{RootID: job.RootID, RequestID: publishRequestID(job.RootID, "stream"),
		Receipts: items, MissingTracks: missing})
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if _, err := a.flows.Complete(intentID("streaming", held.ID, held.Fence), held, workflow.Complete{
		Edge: workflow.EdgeAdvance, NextStage: "validating", TaskState: "completed", Result: payload, Message: message, NowNS: a.now().UnixNano(),
	}); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: message}, nil
}
