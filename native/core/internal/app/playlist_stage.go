package app

// The playlist identify step: a playlist job has a list, not an album. It
// reads incoming/playlist-<job>.m3u (written when the job was created),
// records the entries as evidence and stops in needs_review. Downloading is
// the fallback stage's decision and is not taken here.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func (a *App) identifyPlaylist(ctx context.Context, execution *scheduler.Execution, plan SearchPlan, token taskrepo.Token) (scheduler.Outcome, error) {
	complete := workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "candidate",
		TaskState: "blocked", NowNS: a.now().UnixNano(),
	}
	path, found := a.playlistFile(plan.RootJobID)
	if !found {
		complete.Message = "没有找到这份歌单的曲目文件，未下载、未入库；请重新创建任务"
		if _, err := a.flows.Complete(intentID("playlist", token.ID, token.Fence), token, complete); err != nil {
			return scheduler.Outcome{}, err
		}
		return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		complete.Message = "歌单文件读不出来，未下载、未入库"
		if _, err := a.flows.Complete(intentID("playlist", token.ID, token.Fence), token, complete); err != nil {
			return scheduler.Outcome{}, err
		}
		return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
	}
	rows, name, err := parseM3U(data)
	if err != nil {
		complete.Message = "歌单文件无法解析：" + err.Error() + "；未下载、未入库"
		if _, err := a.flows.Complete(intentID("playlist", token.ID, token.Fence), token, complete); err != nil {
			return scheduler.Outcome{}, err
		}
		return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	complete.Evidence = []workflow.EvidenceInput{{
		ID: evidenceID(token.ID, token.Fence, "playlist"), Kind: "catalog", Source: "playlist",
		SubjectID: plan.RootJobID, Payload: payload,
	}}
	if plan.PlaylistMode == "all" {
		count := len(rows)
		if count > maxPlaylistTracks {
			count = maxPlaylistTracks
		}
		indices := make([]int, count)
		for i := range indices {
			indices[i] = i
		}
		report, enqueueErr := a.enqueuePlaylistTracks(ctx, plan.RootJobID, rows, indices, playlistEnqueueOptions{AlbumCompletion: true})
		if enqueueErr != nil {
			complete.Message = "歌单已读取 " + fmt.Sprint(len(rows)) + " 首，但任务没有创建：" + enqueueErr.Error()
			if _, err := a.flows.Complete(intentID("playlist", token.ID, token.Fence), token, complete); err != nil {
				return scheduler.Outcome{}, err
			}
			return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
		}
		complete.NextStage = "completed"
		complete.ReviewKind = ""
		complete.TaskState = "completed"
		complete.Message = fmt.Sprintf("歌单%s共 %d 首：库内已核验 %d 首，已创建 %d 个任务，%d 项无法创建。无法创建的明细已保存，子任务继续自动检索、下载、刮削和入库。",
			playlistSuffix(name), len(rows), len(report.Skipped), len(report.Created), len(report.Unmatched))
		if reportJSON, marshalErr := json.Marshal(report); marshalErr == nil {
			complete.Result = reportJSON
		}
		if _, err := a.flows.Complete(intentID("playlist", token.ID, token.Fence), token, complete); err != nil {
			return scheduler.Outcome{}, err
		}
		a.startLibraryAutomationRun("after-playlist")
		return scheduler.Outcome{State: complete.TaskState, Message: complete.Message}, nil
	}
	complete.Message = fmt.Sprintf("歌单已读取 %d 首%s，请选择要入库的曲目；尚未下载、尚未入库", len(rows), playlistSuffix(name))
	if _, err := a.flows.Complete(intentID("playlist", token.ID, token.Fence), token, complete); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
}
