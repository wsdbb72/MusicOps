package app

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func (a *App) publishPlaylistStream(ctx context.Context, held taskrepo.Token, job workflow.Job, plan SearchPlan, path string) (scheduler.Outcome, error) {
	root := a.workPath("incoming")
	name, title, scrape, err := a.scrapePlaylistIncoming(ctx, root, filepath.Base(path), plan.ExpectedArtist, plan.ExpectedTitle, plan.PlaylistTrack, 1)
	if err != nil {
		return a.finishAcquire(held, job, "candidate", "流媒体音频刮削失败，尚未入库", "SCRAPE_FAILED", nil)
	}
	id := audioRequestID(job.RootID, 1)
	result, err := audiorelease.Release(ctx, a.engine, audiorelease.Request{
		RequestID: id, RootID: job.RootID, Track: 1, Artist: publishComponent(plan.ExpectedArtist), Album: publishComponent(plan.ExpectedTitle), Title: publishComponent(title),
		SourceName: name, Incoming: root, Library: a.libraryDir(), Decoder: audiorelease.SystemDecoder(), InternalDownload: true,
	})
	if err != nil {
		return a.finishAcquire(held, job, "candidate", "流媒体音频未通过发布校验，尚未入库", "STREAM_INGEST_FAILED", nil)
	}
	if scrape.lyrics != "" {
		placed, found, err := audiorelease.PathFor(a.engine, id)
		if err != nil || !found {
			return a.finishAcquire(held, job, "candidate", "已复制音频，但无法确认歌词写入位置", "SCRAPE_FAILED", nil)
		}
		if !hasLyricsSidecar(placed) {
			if err := writeLyricsSidecar(placed, scrape.lyrics); err != nil {
				return a.finishAcquire(held, job, "candidate", "已复制音频，但歌词写入失败", "SCRAPE_FAILED", nil)
			}
		}
	}
	payload, err := json.Marshal(publishPayload{RootID: job.RootID, RequestID: publishRequestID(job.RootID, "stream"),
		Receipts: []publishReceipt{{TrackID: "1", RequestID: id, SHA256: result.SHA256, Format: result.Format, Origin: result.Origin, SourceName: result.SourceName, DecodeState: result.DecodeState, Scrape: scrape}}})
	if err != nil {
		return scheduler.Outcome{}, err
	}
	message := "已取到无损单曲并执行刮削，等待发布校验"
	if _, err := a.flows.Complete(intentID("streaming", held.ID, held.Fence), held, workflow.Complete{
		Edge: workflow.EdgeAdvance, NextStage: "validating", TaskState: "completed", Result: payload, Message: message, NowNS: a.now().UnixNano(),
	}); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: message}, nil
}
