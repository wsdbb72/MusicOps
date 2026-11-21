// Review is the human decision that resolves a job the machine blocked for
// review. It is the only path that moves a blocked task: finishDecision only
// accepts running or paused tasks, so a blocked task cannot go through
// Complete, and Control refuses blocked tasks because terminal() counts them
// as terminal. Review therefore performs its own atomic transition through
// taskrepo.ResolveOwned (accept/skip) or ControlOwned retry, together with
// the job update, in one commit.
//
// The three actions, and what each one is allowed to do:
//
//   - accept: only a search job in needs_review with ReviewKind "candidate".
//     The app rebuilds the searchdecision.Report from the stored evidence and
//     the stored search plan, applies the user's site/candidate selection, and
//     Review re-runs searchdecision.Decide on it: only a queue_request
//     decision is accepted, so the user supplies the selection but the
//     classifier still decides whether the selection is a real exact match.
//     The search task completes and a process child task is queued with the
//     enriched plan, exactly as the executor's queue_request edge does. A
//     process job in needs_review cannot be accepted: its candidate was
//     already selected when it was queued, and accepting it would queue a
//     second process job under it. Its way out is retry or skip.
//
//   - retry: a job in needs_review (ReviewKind search, candidate, token or metadata)
//     or a failed job. The task goes back to queued with the taskrepo retry
//     semantics, and the job stage is restored to the stage the task runs in
//     (see retryStage). Automatic search retries are bounded at 3, while an
//     explicit/AI review retry restarts that bounded cycle after evidence has
//     had time to change. Retry is refused for ReviewKind "error"
//     (no executor is registered, so a re-run would block again
//     deterministically), for "interrupted" (the task is recovery_required
//     and taskrepo retry deliberately refuses it until the external effects
//     are reconciled; startup recovery requeues only idempotent commands), for a process job whose
//     restored stage would be "fallback" (streaming acquisition is not wired,
//     so a re-run would block again), and for cancelled jobs (cancellation
//     is final). Retrying a paused job clears the pause: retry is an explicit
//     instruction to run again, and a paused job must not have a running task.
//
//   - skip: a job in needs_review with ReviewKind search, candidate, token
//     or error. The task completes and the job moves to skipped. This is the
//     user's explicit decision to abandon the search, not a classifier
//     output, and it is recorded as SearchAction "skip".
package workflow

import (
	"encoding/json"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// ReviewDecision is one human decision on a blocked or failed job.
type ReviewDecision struct {
	// Action is "accept", "retry" or "skip".
	Action string
	// Site and CandidateID are the user's selection, accept only.
	Site        string
	CandidateID string
	// Search is the report the app rebuilt from stored evidence and the
	// stored search plan, accept only.
	Search *searchdecision.Report
	// Plan is the enriched search plan payload for the process child task,
	// accept only.
	Plan []byte
}

// ReviewAction is one decision the review page can offer, with the reason it
// is unavailable when it is not.
type ReviewAction struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// reviewSkipKinds lists the checkpoints a user may abandon. "metadata" is the
// unconfirmed-identity checkpoint: the machine cannot confirm the release, so
// the only human decision available there is to drop the job. Confirming the
// identity re-runs identification instead of skipping (a later batch).
var reviewSkipKinds = map[string]bool{"search": true, "candidate": true, "token": true, "error": true, "metadata": true}
var reviewRetryKinds = map[string]bool{"search": true, "candidate": true, "token": true, "metadata": true}

func (r *Repository) reviewTarget(jobID string) (*Job, *taskrepo.Task, error) {
	job, err := r.Job(jobID)
	if err != nil || job == nil {
		return nil, nil, err
	}
	if job.ActiveTaskID == "" {
		return job, nil, nil
	}
	task, err := r.tasks.Get(job.ActiveTaskID)
	if err != nil {
		return nil, nil, err
	}
	return job, task, nil
}

// ReviewActions reports which decisions the review page can offer for one
// job, with the reason each unavailable action cannot run. It shares its
// rules with Review; Review itself re-validates before writing.
func (r *Repository) ReviewActions(jobID string) ([]ReviewAction, error) {
	job, task, err := r.reviewTarget(jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, ErrNotFound
	}
	return []ReviewAction{
		r.acceptAction(job, task),
		r.retryAction(job, task),
		r.skipAction(job, task),
	}, nil
}

func (r *Repository) acceptAction(job *Job, task *taskrepo.Task) ReviewAction {
	if job.Stage != "needs_review" {
		return ReviewAction{Action: "accept", Reason: "只有待核对的任务可以确认候选"}
	}
	if job.ReviewKind != "candidate" {
		return ReviewAction{Action: "accept", Reason: "这次核对没有候选可确认"}
	}
	if job.TaskType != "search" {
		return ReviewAction{Action: "accept", Reason: "候选只能在检索任务上确认，获取任务请重试或跳过"}
	}
	if task == nil || task.State != "blocked" {
		return ReviewAction{Action: "accept", Reason: "任务状态不允许确认"}
	}
	return ReviewAction{Action: "accept", Allowed: true}
}

func (r *Repository) retryAction(job *Job, task *taskrepo.Task) ReviewAction {
	if task == nil {
		return ReviewAction{Action: "retry", Reason: "没有可重试的任务"}
	}
	switch job.Stage {
	case "needs_review":
		if job.ReviewKind == "interrupted" {
			if task.State != "recovery_required" {
				return ReviewAction{Action: "retry", Reason: "任务状态不允许重试"}
			}
			if _, err := r.interruptedRecoveryStage(*job, task); err != nil {
				return ReviewAction{Action: "retry", Reason: "已中断；该阶段可能产生外部影响，需先核对"}
			}
			return ReviewAction{Action: "retry", Allowed: true}
		}
		if task.State != "blocked" {
			return ReviewAction{Action: "retry", Reason: "任务状态不允许重试"}
		}
		switch job.ReviewKind {
		case "search", "candidate", "token", "metadata":
		case "error":
			return ReviewAction{Action: "retry", Reason: "执行器尚未注册，重试也不会推进"}
		default:
			return ReviewAction{Action: "retry", Reason: "这次核对不能重试"}
		}
	case "failed":
		if task.State != "failed" {
			return ReviewAction{Action: "retry", Reason: "任务状态不允许重试"}
		}
	default:
		return ReviewAction{Action: "retry", Reason: "当前阶段不能重试"}
	}
	if _, err := r.retryStage(*job, task); err != nil {
		return ReviewAction{Action: "retry", Reason: "无法确认重回阶段"}
	}
	return ReviewAction{Action: "retry", Allowed: true}
}

func (r *Repository) skipAction(job *Job, task *taskrepo.Task) ReviewAction {
	if job.Stage != "needs_review" {
		return ReviewAction{Action: "skip", Reason: "只有待核对的任务可以跳过"}
	}
	if !reviewSkipKinds[job.ReviewKind] {
		return ReviewAction{Action: "skip", Reason: "这次核对不能跳过"}
	}
	if task == nil || task.State != "blocked" {
		return ReviewAction{Action: "skip", Reason: "任务状态不允许跳过"}
	}
	return ReviewAction{Action: "skip", Allowed: true}
}

// Review applies one human decision to a blocked or failed job.
func (r *Repository) Review(requestID, jobID string, in ReviewDecision, nowNS int64) (Result, error) {
	if !validID(jobID) || requestID == "" || len(requestID) > 128 || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	switch in.Action {
	case "accept", "retry", "skip":
	default:
		return Result{}, dataengine.ErrInvalidRequest
	}
	job, task, err := r.reviewTarget(jobID)
	if err != nil {
		return Result{}, err
	}
	if job == nil {
		return Result{}, ErrNotFound
	}
	if task == nil {
		return Result{}, ErrStale
	}
	if job.CancelRequested || task.CancelRequested {
		return Result{}, ErrStale
	}
	switch in.Action {
	case "accept":
		return r.reviewAccept(requestID, *job, task, in, nowNS)
	case "skip":
		return r.reviewSkip(requestID, *job, task, in, nowNS)
	default:
		if job.ReviewKind == "interrupted" {
			return r.recoverInterruptedExplicit(requestID, job.ID, nowNS)
		}
		return r.reviewRetry(requestID, *job, task, nowNS)
	}
}

// RecoverInterrupted requeues a task interrupted because its previous process
// disappeared. It is intentionally limited to stable, idempotent commands:
// searches are read-only, downloads Query qBittorrent before resuming, and
// local identify/snapshot/publish work can be verified and replayed. Tracker
// acquire tasks stay in needs_review because a replay could spend another
// token. The one safe process exception is a NetEase playlist fallback: it
// spends no FL token, skips tracks already verified in the library, and can
// resume after a cancelled NetEase request.
func (r *Repository) RecoverInterrupted(requestID, jobID string, nowNS int64) (Result, error) {
	return r.recoverInterrupted(requestID, jobID, nowNS, false)
}

func (r *Repository) recoverInterruptedExplicit(requestID, jobID string, nowNS int64) (Result, error) {
	return r.recoverInterrupted(requestID, jobID, nowNS, true)
}

func (r *Repository) recoverInterrupted(requestID, jobID string, nowNS int64, explicit bool) (Result, error) {
	if !validID(jobID) || requestID == "" || len(requestID) > 128 || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	job, task, err := r.reviewTarget(jobID)
	if err != nil || job == nil || task == nil {
		if err != nil {
			return Result{}, err
		}
		return Result{}, ErrNotFound
	}
	if job.Stage != "needs_review" || job.ReviewKind != "interrupted" || task.State != "recovery_required" {
		return Result{}, ErrStale
	}
	if job.CancelRequested || task.CancelRequested {
		return Result{}, ErrStale
	}
	stage, err := r.interruptedRecoveryStage(*job, task)
	if err != nil {
		return Result{}, err
	}
	// Automatic recovery has its own consecutive-attempt budget. It never
	// consumes the search decision's external retry budget, because a worker
	// interruption is not a tracker response. A human retry starts this budget
	// again and is the explicit escape hatch after three interrupted attempts.
	if task.Command == cmdSearch && !explicit && job.RecoveryRetryCount >= 3 {
		return Result{}, ErrStale
	}
	// 0.4.90 stored the retry deadline only on the job and then slept inside
	// a scheduler worker. Do not auto-recover one of those legacy tasks into
	// the same blocking behaviour. A manual review retry clears that deadline.
	if task.Command == cmdSearch && job.SearchRetryAfterSec > 0 && task.NotBeforeNS == 0 && !explicit {
		return Result{}, ErrStale
	}
	extra, _ := json.Marshal(struct{ JobID, Action string }{job.ID, "recover"})
	result, err := r.tasks.ControlOwned(requestID, task.ID, "recover", nowNS, extra, func(v *dataengine.View, _, after *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		live, raw, err := readJob(v, job.ID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if live.Revision != job.Revision || live.ActiveTaskID != task.ID || live.Stage != "needs_review" || live.ReviewKind != "interrupted" {
			return nil, nil, nil, errRetry
		}
		if after == nil || after.State != "queued" || after.Owner != "" || after.Execution != "" {
			return nil, nil, nil, ErrStale
		}
		next := *live
		next.Stage = stage
		next.Paused = false
		next.ReviewKind = ""
		next.ErrorClass = ""
		next.Message = "旧进程遗留任务已自动恢复"
		if explicit {
			next.Message = "已手动恢复中断任务"
			next.RecoveryRetryCount = 0
			if task.Command == cmdSearch {
				next.SearchRetryAfterSec = 0
			}
		} else if task.Command == cmdSearch {
			next.RecoveryRetryCount = live.RecoveryRetryCount + 1
		}
		updated, err := bump(next, nowNS, next.Stage != live.Stage)
		if err != nil {
			return nil, nil, nil, err
		}
		condition, writes, err := putJob(raw, updated)
		if err != nil {
			return nil, nil, nil, err
		}
		attached, err := json.Marshal(Result{Job: &updated})
		return []dataengine.Condition{condition}, writes, attached, err
	})
	return decode(result, err)
}

func (r *Repository) interruptedRecoveryStage(job Job, task *taskrepo.Task) (string, error) {
	switch task.Command {
	case cmdSearch, cmdDownload, cmdIdentify, cmdSnapshot, cmdPublish:
	case cmdProcess:
		if !strings.HasPrefix(job.BatchID, "pl-netease-") {
			return "", ErrStale
		}
		// A process_release task can have crossed the token request boundary
		// before the process disappeared. Replaying DAC/Orpheus could charge a
		// second token, while replaying an ordinary link is forbidden by the
		// playlist policy. Resume such a task through the source-platform
		// fallback instead; that path never sends usetoken and skips tracks
		// already verified in the library.
		return "fallback", nil
	default:
		return "", ErrStale
	}
	return r.retryStage(job, task)
}

// RequeueTokenFallback sends an unresolved NetEase playlist token acquisition
// to the source-platform fallback. It never replays usetoken and never uses a
// normal tracker download. This is the terminal reconciliation path for a
// request whose external token effect cannot be proved: falling back avoids a
// second charge while still allowing the playlist row to finish.
func (r *Repository) RequeueTokenFallback(requestID, jobID string, nowNS int64, reason string) (Result, error) {
	if !validID(jobID) || requestID == "" || len(requestID) > 128 || nowNS <= 0 || len(reason) > 4000 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	job, task, err := r.reviewTarget(jobID)
	if err != nil || job == nil || task == nil {
		if err != nil {
			return Result{}, err
		}
		return Result{}, ErrNotFound
	}
	if job.Stage != "needs_review" || job.ReviewKind != "token" || job.CancelRequested || job.Paused ||
		task.State != "blocked" || task.CancelRequested || task.Command != cmdProcess || !strings.HasPrefix(job.BatchID, "pl-netease-") {
		return Result{}, ErrStale
	}
	extra, _ := json.Marshal(struct{ JobID, Action string }{job.ID, "token_fallback"})
	result, err := r.tasks.ControlOwned(requestID, task.ID, "retry", nowNS, extra, func(v *dataengine.View, _, after *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		live, raw, err := readJob(v, job.ID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if live.Revision != job.Revision || live.Stage != "needs_review" || live.ReviewKind != "token" || live.ActiveTaskID != task.ID {
			return nil, nil, nil, ErrStale
		}
		if after == nil || after.State != "queued" || after.Owner != "" || after.Execution != "" {
			return nil, nil, nil, ErrStale
		}
		next := *live
		next.Stage = "fallback"
		next.ReviewKind = ""
		next.ErrorClass = ""
		next.SearchAction = searchdecision.ActionFallback
		next.Message = messageOr(reason, "令牌结果无法安全确认，已自动改用网易云回退；未重复使用令牌")
		updated, err := bump(next, nowNS, true)
		if err != nil {
			return nil, nil, nil, err
		}
		condition, writes, err := putJob(raw, updated)
		if err != nil {
			return nil, nil, nil, err
		}
		attached, err := json.Marshal(Result{Job: &updated})
		return []dataengine.Condition{condition}, writes, attached, err
	})
	return decode(result, err)
}

func (r *Repository) reviewAccept(requestID string, job Job, task *taskrepo.Task, in ReviewDecision, nowNS int64) (Result, error) {
	if job.Paused {
		return Result{}, ErrStale
	}
	if job.Stage != "needs_review" || task.State != "blocked" {
		return Result{}, ErrStale
	}
	if job.TaskType != "search" || (job.ReviewKind != "candidate" && job.ReviewKind != "search") {
		return Result{}, ErrStale
	}
	decision := searchdecision.Decision{Action: searchdecision.ActionQueueRequest}
	{
		if in.Search == nil || len(in.Plan) == 0 {
			return Result{}, dataengine.ErrInvalidRequest
		}
		if in.Search.Scene != job.Scene || job.IdentificationJobID == "" {
			return Result{}, dataengine.ErrInvalidRequest
		}
		decided, err := searchdecision.Decide(*in.Search)
		if err != nil {
			return Result{}, dataengine.ErrInvalidRequest
		}
		decision = decided
		// The decision carries the selected site; the candidate id lives on the
		// report. Both must match the user's selection: the report is rebuilt
		// from stored evidence, so a selection that is not in the evidence
		// cannot classify as an exact match.
		if !((decision.Action == searchdecision.ActionQueueRequest && in.Site != "" && in.CandidateID != "" && decision.SelectedSite == in.Site && in.Search.SelectedSite == in.Site && in.Search.SelectedCandidateID == in.CandidateID) || (decision.Action == searchdecision.ActionFallback && in.Search.SourceFallback && in.Site == "" && in.CandidateID == "" && in.Search.SelectedCandidateID == "")) {
			return Result{}, dataengine.ErrInvalidRequest
		}
	}
	childID := processJobID(job.ID)
	child := job
	child.ID = childID
	child.ParentJobID = job.ID
	child.RootID = job.RootID
	child.Stage = "requesting"
	if decision.Action == searchdecision.ActionFallback {
		child.Stage = "fallback"
	}
	child.TaskType = "album"
	child.TaskGroup = "process"
	child.ActiveTaskID = childID
	child.Revision = 1
	child.CreatedNS = nowNS
	child.UpdatedNS = nowNS
	child.StageStartedNS = nowNS
	child.CancelRequested = false
	child.Paused = false
	child.ReviewKind = ""
	child.ErrorClass = ""
	child.SearchJobID = job.ID
	child.ProcessingJobID = ""
	child.IdentificationJobID = job.IdentificationJobID
	child.SearchRetryCount = 0
	child.RecoveryRetryCount = 0
	child.AIResearchCount = 0
	child.SearchRetryAfterSec = 0
	child.SearchAction = decision.Action
	child.Message = "已确认候选，等待获取；尚未下载、尚未入库"
	if child.Stage == "fallback" {
		child.Message = "站点候选未确认，等待配置的流媒体回退；尚未下载、尚未入库"
	}
	followup := taskrepo.NewTask{Spec: taskrepo.Spec{ID: childID, Command: cmdProcess, Lane: "aux", Priority: 1, CreatedNS: nowNS}, Payload: in.Plan}
	end := taskrepo.Completion{State: "completed", NowNS: nowNS, Message: child.Message, Payload: in.Plan}
	extra, _ := json.Marshal(struct{ JobID, Action string }{job.ID, "accept"})
	result, err := r.tasks.ResolveOwned(requestID, task.ID, end, []taskrepo.NewTask{followup}, map[string]string{childID: childID}, extra, func(v *dataengine.View, _, after *taskrepo.Task, children []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		live, raw, err := readJob(v, job.ID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if live.Revision != job.Revision || live.Stage != "needs_review" || live.ActiveTaskID != task.ID {
			return nil, nil, nil, errRetry
		}
		if after == nil || len(children) != 1 || children[0].ID != childID || children[0].WorkflowJobID != childID {
			return nil, nil, nil, ErrStale
		}
		next := *live
		next.Stage = "completed"
		next.ProcessingJobID = childID
		next.SearchAction = decision.Action
		next.ReviewKind = ""
		next.ErrorClass = ""
		next.Message = child.Message
		updated, err := bump(next, nowNS, true)
		if err != nil {
			return nil, nil, nil, err
		}
		condition, writes, err := putJob(raw, updated)
		if err != nil {
			return nil, nil, nil, err
		}
		childCondition, childWrites, err := putJob(nil, child)
		if err != nil {
			return nil, nil, nil, err
		}
		attached, err := json.Marshal(Result{Job: &updated, Children: []Job{child}, Search: &decision})
		return []dataengine.Condition{condition, childCondition}, append(writes, childWrites...), attached, err
	})
	return decode(result, err)
}

func (r *Repository) reviewSkip(requestID string, job Job, task *taskrepo.Task, in ReviewDecision, nowNS int64) (Result, error) {
	if job.Paused {
		return Result{}, ErrStale
	}
	if job.Stage != "needs_review" || task.State != "blocked" || !reviewSkipKinds[job.ReviewKind] {
		return Result{}, ErrStale
	}
	end := taskrepo.Completion{State: "completed", NowNS: nowNS, Message: "已跳过，未下载"}
	extra, _ := json.Marshal(struct{ JobID, Action string }{job.ID, "skip"})
	result, err := r.tasks.ResolveOwned(requestID, task.ID, end, nil, map[string]string{}, extra, func(v *dataengine.View, _, after *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		live, raw, err := readJob(v, job.ID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if live.Revision != job.Revision || live.Stage != "needs_review" || live.ActiveTaskID != task.ID {
			return nil, nil, nil, errRetry
		}
		if after == nil {
			return nil, nil, nil, ErrStale
		}
		next := *live
		next.Stage = "skipped"
		next.SearchAction = searchdecision.ActionSkip
		next.ReviewKind = ""
		next.ErrorClass = ""
		next.Message = "已跳过，未下载"
		updated, err := bump(next, nowNS, true)
		if err != nil {
			return nil, nil, nil, err
		}
		condition, writes, err := putJob(raw, updated)
		if err != nil {
			return nil, nil, nil, err
		}
		attached, err := json.Marshal(Result{Job: &updated})
		return []dataengine.Condition{condition}, writes, attached, err
	})
	return decode(result, err)
}

func (r *Repository) reviewRetry(requestID string, job Job, task *taskrepo.Task, nowNS int64) (Result, error) {
	switch job.Stage {
	case "needs_review":
		if task.State != "blocked" || !reviewRetryKinds[job.ReviewKind] {
			return Result{}, ErrStale
		}
	case "failed":
		if task.State != "failed" {
			return Result{}, ErrStale
		}
	default:
		return Result{}, ErrStale
	}
	stage, err := r.retryStage(job, task)
	if err != nil {
		return Result{}, err
	}
	extra, _ := json.Marshal(struct{ JobID, Action string }{job.ID, "retry"})
	result, err := r.tasks.ControlOwned(requestID, task.ID, "retry", nowNS, extra, func(v *dataengine.View, _, after *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		live, raw, err := readJob(v, job.ID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if live.Revision != job.Revision || live.ActiveTaskID != task.ID {
			return nil, nil, nil, errRetry
		}
		if after == nil || after.State != "queued" {
			return nil, nil, nil, ErrStale
		}
		next := *live
		next.Stage = stage
		// Retry is an explicit instruction to run again, so it also resumes:
		// a paused job must not have a running task.
		next.Paused = false
		next.ReviewKind = ""
		next.ErrorClass = ""
		next.SearchAction = ""
		next.SearchRetryAfterSec = 0
		if task.Command == cmdSearch {
			next.RecoveryRetryCount = 0
			// Three scheduler retries form one bounded cycle. A reviewed retry
			// starts a fresh cycle only after the user or the evidence-driven
			// auto reviewer has seen the latest site state; otherwise a stale
			// login plus a temporary rate limit stranded the job forever.
			next.SearchRetryCount = live.SearchRetryCount + 1
			if next.SearchRetryCount >= 3 {
				next.SearchRetryCount = 0
			}
		}
		next.Message = "已安排重新执行"
		updated, err := bump(next, nowNS, next.Stage != live.Stage)
		if err != nil {
			return nil, nil, nil, err
		}
		condition, writes, err := putJob(raw, updated)
		if err != nil {
			return nil, nil, nil, err
		}
		attached, err := json.Marshal(Result{Job: &updated})
		return []dataengine.Condition{condition}, writes, attached, err
	})
	return decode(result, err)
}

// retryStage restores the stage a retried task runs in. The stage is derived
// from stored data only: search tasks always run in "searching"; stage tasks
// carry their stage in the task id; a process child runs in "requesting" or
// "fallback" according to the parent search decision that created it.
func (r *Repository) retryStage(job Job, task *taskrepo.Task) (string, error) {
	switch task.Command {
	case cmdSearch:
		return "searching", nil
	case cmdDownload:
		// A failed download task re-runs in "downloading": the torrent
		// stays in qBittorrent, so the executor resumes it through Query
		// instead of adding it again. The credentials were cleared when
		// the attempt failed, so a retry needs them re-supplied through
		// POST /v1/jobs/{id}/download; a bare review retry fails honestly
		// with MISSING_CREDENTIALS instead of silently reusing a secret.
		return "downloading", nil
	case cmdIdentify:
		return "identifying", nil
	case cmdSnapshot:
		return "snapshotting", nil
	case cmdPrepare:
		return "preparing", nil
	case cmdRepair:
		return "metadata", nil
	case cmdProcess, cmdPublish:
		if stage, ok := strings.CutPrefix(task.ID, "stage:"+job.ID+":"); ok && stages[stage] {
			return stage, nil
		}
		if task.Command == cmdPublish {
			return "", ErrStale
		}
		if job.ParentJobID == "" {
			return "", ErrStale
		}
		parent, err := r.Job(job.ParentJobID)
		if err != nil {
			return "", err
		}
		if parent == nil {
			return "", ErrStale
		}
		if parent.SearchAction == searchdecision.ActionFallback {
			return "fallback", nil
		}
		return "requesting", nil
	default:
		return "", ErrStale
	}
}
