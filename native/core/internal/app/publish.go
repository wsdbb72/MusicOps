package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiopublish"
	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// publishReceipt is one verified ingest a publish task re-checks before the
// job completes. The flat fields on publishPayload carry the same receipt for
// the single-track local-import path; Receipts carries the multi-track
// download path.
type publishReceipt struct {
	TrackID     string `json:"track_id"`
	RequestID   string `json:"request_id"`
	SHA256      string `json:"sha256"`
	Format      string `json:"format"`
	Origin      string `json:"origin"`
	SourceName  string `json:"source_name"`
	DecodeState string `json:"decode_state"`
}

// publishPayload is the receipt one verified ingest hands to the publish task.
// The handler trusts nothing in it except as a lookup key: the library record
// and the file hash are re-checked before the job completes. SourceURL turns
// the task into the controlled-audio path instead: the file is fetched from a
// loopback URL and organised into the library by audiopublish, then the
// organised file is hash-verified the same way.
type publishPayload struct {
	RootID      string           `json:"root_id"`
	TrackID     string           `json:"track_id"`
	RequestID   string           `json:"request_id"`
	SHA256      string           `json:"sha256"`
	Format      string           `json:"format"`
	Origin      string           `json:"origin"`
	SourceName  string           `json:"source_name"`
	DecodeState string           `json:"decode_state"`
	SourceURL   string           `json:"source_url,omitempty"`
	Artist      string           `json:"artist,omitempty"`
	Album       string           `json:"album,omitempty"`
	Title       string           `json:"title,omitempty"`
	Track       int              `json:"track,omitempty"`
	Receipts    []publishReceipt `json:"receipts,omitempty"`
}

// publish verifies the library record for the released track and only then
// completes the job. Saving torrent bytes never counts as a download or an
// ingest, so there is no torrent path here at all: the only inputs are a
// verified audiorelease record whose file still hashes to its digest, or a
// controlled-audio source that audiopublish organises into the library first.
// A verification failure fails the task and the job (stage "failed", retryable
// from review). It must go through flows.Complete: a bare failed Outcome
// would leave a workflow-owned task running forever, because the coordinator
// cannot Finish a workflow-owned task (ErrWorkflowOwned) and the workflow
// layer is the only side that can finish it.
func (a *App) publish(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	var receipt publishPayload
	if err := json.Unmarshal(payload, &receipt); err != nil || receipt.RequestID == "" || receipt.RootID == "" {
		return a.failPublish(execution, receipt, "INVALID_PAYLOAD", "publish task has no release receipt")
	}
	if receipt.SourceURL != "" {
		return a.publishControlled(ctx, execution, receipt)
	}
	if len(receipt.Receipts) > 0 {
		return a.publishReceipts(ctx, execution, receipt)
	}
	verified, err := audiorelease.Verify(a.engine, receipt.RequestID)
	if err != nil {
		return a.failPublish(execution, receipt, "VERIFY_FAILED", "library record did not verify: "+err.Error())
	}
	if verified.RootID != receipt.RootID || verified.SHA256 != receipt.SHA256 || (receipt.Format != "" && verified.Format != receipt.Format) {
		return a.failPublish(execution, receipt, "VERIFY_FAILED", "library record does not match the release receipt")
	}
	evidence, err := json.Marshal(verified)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if err := a.completePublish(execution, receipt, "已入库并校验完成（本地导入，未经站点下载）。", []workflow.EvidenceInput{{ID: "publish-" + receipt.RequestID, Kind: "catalog", Source: "local-library", SubjectID: receipt.RootID, TracklistVerified: true, Payload: evidence}}, evidence); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: "已入库并校验完成"}, nil
}

// publishControlled fetches one controlled audio file from a loopback URL,
// organises it into the library, re-verifies the organised file's hash, and
// only then completes the job.
func (a *App) publishControlled(ctx context.Context, execution *scheduler.Execution, receipt publishPayload) (scheduler.Outcome, error) {
	result, err := audiopublish.Publish(ctx, a.engine, audiopublish.Request{
		RequestID: receipt.RequestID, Artist: receipt.Artist, Album: receipt.Album, Title: receipt.Title,
		Track: receipt.Track, Library: filepath.Join(a.Dir, "library-audio"), SourceURL: receipt.SourceURL, Now: a.now(),
	})
	if err != nil {
		if errors.Is(err, audiopublish.ErrConflict) {
			return a.failPublish(execution, receipt, "LIBRARY_CONFLICT", "目标位置已有同名文件，拒绝覆盖")
		}
		return a.failPublish(execution, receipt, "PUBLISH_FAILED", "受控音频发布失败: "+err.Error())
	}
	// Publish already hash-checked the file after the rename; check it once
	// more from disk so a concurrent change cannot slip through.
	if digest, err := hashFile(result.Path); err != nil || digest != result.SHA256 {
		return a.failPublish(execution, receipt, "VERIFY_FAILED", "已发布文件与记录的摘要不一致")
	}
	// Record the placed file in the library catalogue, or the library
	// view, dedup scan and artist-folder planning would never see it even
	// though the file is on disk. A retry replays both the publish and
	// this record, so a failure here converges instead of duplicating.
	if _, err := audiorelease.RecordPublished(ctx, a.engine, audiorelease.PublishedInput{
		RequestID: receipt.RequestID, RootID: receipt.RootID,
		Artist: receipt.Artist, Album: receipt.Album, Title: receipt.Title, Track: receipt.Track,
		Path: result.Path, Library: filepath.Join(a.Dir, "library-audio"),
		Decoder: audiorelease.SystemDecoder(),
	}); err != nil {
		return a.failPublish(execution, receipt, "LIBRARY_RECORD_FAILED", "已发布文件未能记入库目录: "+err.Error())
	}
	evidence, err := json.Marshal(result)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if err := a.completePublish(execution, receipt, "已发布到库中（受控音频）。", []workflow.EvidenceInput{{ID: "publish-" + receipt.RequestID, Kind: "catalog", Source: "controlled-audio", SubjectID: receipt.RootID, TracklistVerified: true, Payload: evidence}}, evidence); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: "已发布到库中"}, nil
}

// publishReceipts verifies every receipt in a multi-track publish (the
// download path) and only completes the job when all of them check out.
func (a *App) publishReceipts(ctx context.Context, execution *scheduler.Execution, receipt publishPayload) (scheduler.Outcome, error) {
	evidence := make([]workflow.EvidenceInput, 0, len(receipt.Receipts))
	verified := make([]audiorelease.Result, 0, len(receipt.Receipts))
	for _, item := range receipt.Receipts {
		if item.RequestID == "" || item.SHA256 == "" {
			return a.failPublish(execution, receipt, "INVALID_PAYLOAD", "publish task has a receipt without request id or digest")
		}
		record, err := audiorelease.Verify(a.engine, item.RequestID)
		if err != nil {
			return a.failPublish(execution, receipt, "VERIFY_FAILED", "library record did not verify: "+err.Error())
		}
		if record.RootID != receipt.RootID || record.SHA256 != item.SHA256 || (item.Format != "" && record.Format != item.Format) {
			return a.failPublish(execution, receipt, "VERIFY_FAILED", "library record does not match the release receipt")
		}
		verified = append(verified, record)
		encoded, err := json.Marshal(record)
		if err != nil {
			return scheduler.Outcome{}, err
		}
		evidence = append(evidence, workflow.EvidenceInput{ID: "publish-" + item.RequestID, Kind: "catalog", Source: "local-library", SubjectID: receipt.RootID, TracklistVerified: true, Payload: encoded})
	}
	result, err := json.Marshal(verified)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if err := a.completePublish(execution, receipt, "已入库并校验完成。", evidence, result); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: "已入库并校验完成"}, nil
}

// failPublish fails the publish task and moves the job to the "failed"
// stage in one atomic workflow step. The job stays visible as failed and can
// be retried from review, which re-runs the publish task.
func (a *App) failPublish(execution *scheduler.Execution, receipt publishPayload, class, message string) (scheduler.Outcome, error) {
	token := execution.Token()
	if _, err := a.flows.Complete(intentID("publish", token.ID, token.Fence), token, workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "failed", TaskState: "failed",
		ErrorClass: class, Message: message, NowNS: a.now().UnixNano(),
	}); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "failed", ErrorClass: class, Message: message}, nil
}

// completePublish completes the root job terminally with the given evidence.
// It is the single place a publish task may complete a job; failures go
// through failPublish instead, so no path can complete a job it did not verify.
func (a *App) completePublish(execution *scheduler.Execution, receipt publishPayload, message string, evidence []workflow.EvidenceInput, result []byte) error {
	token := execution.Token()
	_, err := a.flows.Complete(intentID("publish", token.ID, token.Fence), token, workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "completed", TaskState: "completed",
		Message:  message,
		Result:   result,
		Evidence: evidence,
		NowNS:    a.now().UnixNano(),
	})
	return err
}

// publishRequestID derives a deterministic, replay-safe request id for a
// publish task from its inputs.
func publishRequestID(parts ...string) string {
	sum := sha256.Sum256([]byte("publish/" + joinParts(parts)))
	return "pub-" + hex.EncodeToString(sum[:16])
}

func joinParts(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += "/"
		}
		out += part
	}
	return out
}
