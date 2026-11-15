package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func (a *App) publishPlaylistStream(ctx context.Context, held taskrepo.Token, job workflow.Job, plan SearchPlan, path string) (scheduler.Outcome, error) {
	item, err := a.ingestPlaylistStream(ctx, job, plan, path, 1)
	if err != nil {
		return a.finishAcquire(held, job, "candidate", err.Error(), "STREAM_INGEST_FAILED", nil)
	}
	return a.finishPlaylistStreams(held, job, []publishReceipt{item}, "已取到无损单曲并执行刮削，等待发布校验")
}

func (a *App) ingestPlaylistStream(ctx context.Context, job workflow.Job, plan SearchPlan, path string, position int) (publishReceipt, error) {
	root := a.workPath("incoming")
	name, title, scrape, err := a.scrapePlaylistIncoming(ctx, root, filepath.Base(path), plan.ExpectedArtist, plan.ExpectedTitle, plan.PlaylistTrack, position)
	if err != nil {
		return publishReceipt{}, errors.New("流媒体音频刮削失败，尚未入库")
	}
	id := audioRequestID(job.RootID, position)
	result, err := audiorelease.Release(ctx, a.engine, audiorelease.Request{
		RequestID: id, RootID: job.RootID, Track: position, Artist: publishComponent(plan.ExpectedArtist), Album: publishComponent(plan.ExpectedTitle), Title: publishComponent(title),
		SourceName: name, Incoming: root, Library: a.libraryDir(), Decoder: audiorelease.SystemDecoder(), InternalDownload: true,
	})
	if err != nil {
		return publishReceipt{}, errors.New("流媒体音频未通过发布校验，尚未入库")
	}
	if scrape.lyrics != "" {
		placed, found, err := audiorelease.PathFor(a.engine, id)
		if err != nil || !found {
			return publishReceipt{}, errors.New("已复制音频，但无法确认歌词写入位置")
		}
		if !hasLyricsSidecar(placed) {
			if err := writeLyricsSidecar(placed, scrape.lyrics); err != nil {
				return publishReceipt{}, errors.New("已复制音频，但歌词写入失败")
			}
		}
	}
	return publishReceipt{TrackID: strconv.Itoa(position), RequestID: id, SHA256: result.SHA256, Format: result.Format, Origin: result.Origin, SourceName: result.SourceName, DecodeState: result.DecodeState, Scrape: scrape}, nil
}

func (a *App) finishPlaylistStreams(held taskrepo.Token, job workflow.Job, items []publishReceipt, message string) (scheduler.Outcome, error) {
	payload, err := json.Marshal(publishPayload{RootID: job.RootID, RequestID: publishRequestID(job.RootID, "stream"),
		Receipts: items})
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
