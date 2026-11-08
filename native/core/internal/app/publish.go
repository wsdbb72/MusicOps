package app

import (
	"context"
	"encoding/json"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// publishPayload is the receipt one verified ingest hands to the publish task.
// The handler trusts nothing in it except as a lookup key: the library record
// and the file hash are re-checked before the job completes.
type publishPayload struct {
	RootID      string `json:"root_id"`
	TrackID     string `json:"track_id"`
	RequestID   string `json:"request_id"`
	SHA256      string `json:"sha256"`
	Format      string `json:"format"`
	Origin      string `json:"origin"`
	SourceName  string `json:"source_name"`
	DecodeState string `json:"decode_state"`
}

// publish verifies the library record for the released track and only then
// completes the job. Saving torrent bytes never counts as a download or an
// ingest, so there is no torrent path here at all: the only input is a
// verified audiorelease record whose file still hashes to its digest. A
// verification failure fails the task without completing the job, so the
// failure stays visible in the queue instead of being marked complete.
func (a *App) publish(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	var receipt publishPayload
	if err := json.Unmarshal(payload, &receipt); err != nil || receipt.RequestID == "" || receipt.RootID == "" {
		return scheduler.Outcome{State: "failed", ErrorClass: "INVALID_PAYLOAD", Message: "publish task has no release receipt"}, nil
	}
	verified, err := audiorelease.Verify(a.engine, receipt.RequestID)
	if err != nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "VERIFY_FAILED", Message: "library record did not verify: " + err.Error()}, nil
	}
	if verified.RootID != receipt.RootID || verified.SHA256 != receipt.SHA256 || (receipt.Format != "" && verified.Format != receipt.Format) {
		return scheduler.Outcome{State: "failed", ErrorClass: "VERIFY_FAILED", Message: "library record does not match the release receipt"}, nil
	}
	evidence, err := json.Marshal(verified)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	token := execution.Token()
	if _, err := a.flows.Complete(intentID("publish", token.ID, token.Fence), token, workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "completed", TaskState: "completed",
		Message:  "已入库并校验完成（本地导入，未经站点下载）。",
		Result:   evidence,
		Evidence: []workflow.EvidenceInput{{ID: "publish-" + receipt.RequestID, Kind: "catalog", Source: "local-library", SubjectID: receipt.RootID, TracklistVerified: true, Payload: evidence}},
		NowNS:    a.now().UnixNano(),
	}); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: "已入库并校验完成"}, nil
}
