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
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
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

// playlistStreamTargetPath keeps the existing filename stem while allowing a
// lossy source to be replaced by the lossless FLAC produced by the streaming
// pipeline. Publishing FLAC bytes to a .mp3 target is invalid and used to park
// every MP3 upgrade on an opaque "invalid data request" review item.
func playlistStreamTargetPath(plan SearchPlan) string {
	path := playlistStreamReplacementPath(plan)
	if path == "" || strings.EqualFold(filepath.Ext(path), ".flac") {
		return path
	}
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".flac"
}

func playlistStreamAuthorizedReplacement(plan SearchPlan) string {
	source := playlistStreamReplacementPath(plan)
	target := playlistStreamTargetPath(plan)
	if source == "" || !sameLibraryPath(source, target) {
		return ""
	}
	return source
}

func sameLibraryPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(filepath.Clean(left))
	rightAbs, rightErr := filepath.Abs(filepath.Clean(right))
	if leftErr != nil || rightErr != nil {
		return false
	}
	return strings.EqualFold(leftAbs, rightAbs)
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
	targetPath := playlistStreamTargetPath(plan)
	result, err := audiorelease.Release(ctx, a.engine, audiorelease.Request{
		RequestID: id, RootID: job.RootID, Track: position, Artist: publishComponent(plan.ExpectedArtist), FolderArtist: func() string {
			if strings.TrimSpace(plan.AlbumArtist) == "" {
				return ""
			}
			return publishComponent(plan.AlbumArtist)
		}(), Album: publishComponent(plan.ExpectedTitle), Title: publishComponent(diskTitle),
		SourceName: name, Incoming: root, Library: a.libraryDir(), Decoder: audiorelease.SystemDecoder(), InternalDownload: true,
		ReplacePath: playlistStreamAuthorizedReplacement(plan), TargetPath: targetPath,
	})
	if err != nil {
		if errors.Is(err, dataengine.ErrInvalidRequest) {
			err = fmt.Errorf("发布参数无效（track=%d，目标=%q）: %w", position, targetPath, err)
		}
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
	if source := playlistStreamReplacementPath(plan); source != "" && !sameLibraryPath(source, placed) {
		if err := a.removeSupersededLibrarySource(source, placed); err != nil {
			return publishReceipt{}, fmt.Errorf("无损文件已入库，但旧有损文件尚未清理: %w", err)
		}
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

// removeSupersededLibrarySource removes the old lossy file only after the new
// FLAC has been decoded, published and indexed. A retry is idempotent: a file
// already removed by an interrupted cleanup is treated as success.
func (a *App) removeSupersededLibrarySource(source, published string) error {
	if sameLibraryPath(source, published) {
		return nil
	}
	library := a.libraryDir()
	sourceAbs, sourceErr := filepath.Abs(filepath.Clean(source))
	publishedAbs, publishedErr := filepath.Abs(filepath.Clean(published))
	if sourceErr != nil || publishedErr != nil {
		return errors.New("发布路径无效")
	}
	removeIndexed := func() error {
		return audiorelease.RemoveIndexed(a.engine, libraryFileID(sourceAbs), libraryRootID(filepath.Dir(sourceAbs)), trackNumberFromName(filepath.Base(sourceAbs)))
	}
	relSource, err := filepath.Rel(library, sourceAbs)
	if err != nil || relSource == ".." || strings.HasPrefix(relSource, ".."+string(filepath.Separator)) {
		return errors.New("旧音频不在当前音乐库")
	}
	relPublished, err := filepath.Rel(library, publishedAbs)
	if err != nil || relPublished == ".." || strings.HasPrefix(relPublished, ".."+string(filepath.Separator)) {
		return errors.New("新音频不在当前音乐库")
	}
	if !strings.EqualFold(filepath.Ext(publishedAbs), ".flac") {
		return errors.New("替换后的文件不是 FLAC")
	}
	codec := strings.TrimPrefix(strings.ToLower(filepath.Ext(sourceAbs)), ".")
	if !isLossyUpgradeSource(codec) {
		return errors.New("旧文件不是可替换的有损音频")
	}
	if info, statErr := os.Lstat(publishedAbs); statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		if statErr != nil {
			return statErr
		}
		return errors.New("新 FLAC 不是有效文件")
	}
	info, err := os.Lstat(sourceAbs)
	if os.IsNotExist(err) {
		return removeIndexed()
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("旧音频不是普通文件")
	}
	if err := os.Remove(sourceAbs); err != nil {
		return err
	}
	return removeIndexed()
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
	requestID, err := publishReceiptRequestID(job.RootID, items, missing)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	payload, err := json.Marshal(publishPayload{RootID: job.RootID, RequestID: requestID,
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
