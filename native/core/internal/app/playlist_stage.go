package app

// The playlist identify step: a playlist job has a list, not an album. It
// reads incoming/playlist-<job>.m3u (written when the job was created),
// records the entries as evidence and stops in needs_review. Downloading is
// the fallback stage's decision and is not taken here.

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func (a *App) identifyPlaylist(execution *scheduler.Execution, plan SearchPlan, token taskrepo.Token) (scheduler.Outcome, error) {
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
	complete.Message = fmt.Sprintf("歌单已读取 %d 首%s，待核对；尚未下载、尚未入库", len(rows), playlistSuffix(name))
	if _, err := a.flows.Complete(intentID("playlist", token.ID, token.Fence), token, complete); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
}
