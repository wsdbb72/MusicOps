package workflow

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// errRetry asks the caller to reread the job and build the successor set again.
// It must not consume the request receipt.
var errRetry = errors.New("reread workflow before commit")

type Repository struct {
	engine *dataengine.Engine
	tasks  *taskrepo.Repository
}

func New(engine *dataengine.Engine, tasks *taskrepo.Repository) *Repository {
	if engine == nil || tasks == nil {
		return nil
	}
	return &Repository{engine: engine, tasks: tasks}
}

func searchJobID(parent string) string       { return "search:" + parent }
func processJobID(parent string) string      { return "process:" + parent }
func stageTaskID(jobID, stage string) string { return "stage:" + jobID + ":" + stage }

type evidenceRef struct {
	ID, Kind, Source, SubjectID, AliasLanguage, PayloadHash  string
	IdentityConfirmed, TracklistVerified, ProductionVerified bool
}

func prepareEvidence(jobID string, inputs []EvidenceInput) ([]Evidence, [][]byte, []evidenceRef, error) {
	if len(inputs) > 32 {
		return nil, nil, nil, dataengine.ErrInvalidRequest
	}
	items := make([]Evidence, 0, len(inputs))
	payloads := make([][]byte, 0, len(inputs))
	refs := make([]evidenceRef, 0, len(inputs))
	seen := map[string]bool{}
	var total int
	for _, input := range inputs {
		total += len(input.Payload)
		if total > taskrepo.PayloadLimit {
			return nil, nil, nil, dataengine.ErrInvalidRequest
		}
		hash, err := taskrepo.DigestPayload(input.Payload)
		if err != nil {
			return nil, nil, nil, err
		}
		item := Evidence{ID: input.ID, JobID: jobID, Kind: input.Kind, Source: input.Source, SubjectID: input.SubjectID, AliasLanguage: input.AliasLanguage, IdentityConfirmed: input.IdentityConfirmed, TracklistVerified: input.TracklistVerified, ProductionVerified: input.ProductionVerified, PayloadHash: hash}
		if err = validateEvidence(item); err != nil {
			return nil, nil, nil, err
		}
		if seen[item.ID] {
			return nil, nil, nil, dataengine.ErrInvalidRequest
		}
		seen[item.ID] = true
		items = append(items, item)
		payloads = append(payloads, input.Payload)
		refs = append(refs, evidenceRef{ID: item.ID, Kind: item.Kind, Source: item.Source, SubjectID: item.SubjectID, AliasLanguage: item.AliasLanguage, PayloadHash: hash, IdentityConfirmed: item.IdentityConfirmed, TracklistVerified: item.TracklistVerified, ProductionVerified: item.ProductionVerified})
	}
	return items, payloads, refs, nil
}

func decode(result taskrepo.Result, err error) (Result, error) {
	out := Result{Task: result.Task, Followups: result.Followups, Replayed: result.Replayed, Changed: result.Changed}
	if len(result.Domain) > 0 && json.Unmarshal(result.Domain, &out) != nil {
		return Result{}, ErrIntegrity
	}
	out.Task = result.Task
	out.Followups = result.Followups
	out.Replayed = result.Replayed
	out.Changed = result.Changed
	return out, err
}

func (r *Repository) Start(requestID string, in Start) (Result, error) {
	spec, job, err := planStart(in)
	if err != nil {
		return Result{}, err
	}
	evidence, payloads, refs, err := prepareEvidence(job.ID, in.Evidence)
	if err != nil {
		return Result{}, err
	}
	extra, err := json.Marshal(struct {
		Scene, Stage, BatchID, Label   string
		SourceTrackCount               uint64
		SourceTrackDigest              string
		LibraryFileCount, MatchedCount uint64
		Evidence                       []evidenceRef
	}{job.Scene, job.Stage, job.BatchID, job.Label, job.SourceTrackCount, job.SourceTrackDigest, job.LibraryFileCount, job.MatchedCount, refs})
	if err != nil {
		return Result{}, err
	}
	result, err := r.tasks.CreateOwned(requestID, spec, in.Payload, job.ID, extra, func(v *dataengine.View, _, after *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		if after == nil || after.ID != job.ID || after.WorkflowJobID != job.ID {
			return nil, nil, nil, ErrIntegrity
		}
		if _, found, err := v.Get(jobKey(job.ID)); err != nil || found {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrStale
		}
		conditions, writes, err := writeEvidence(v, evidence, payloads)
		if err != nil {
			return nil, nil, nil, err
		}
		jobCondition, jobWrites, err := putJob(nil, job)
		if err != nil {
			return nil, nil, nil, err
		}
		attached, err := json.Marshal(Result{Job: &job, Evidence: evidence})
		if err != nil {
			return nil, nil, nil, err
		}
		return append(conditions, jobCondition), append(writes, jobWrites...), attached, nil
	})
	return decode(result, err)
}

func planStart(in Start) (taskrepo.Spec, Job, error) {
	if !validID(in.JobID) || !validID(in.BatchID) || !scenes[in.Scene] || in.NowNS <= 0 || len(in.Label) > 500 {
		return taskrepo.Spec{}, Job{}, dataengine.ErrInvalidRequest
	}
	count, digest, err := trackDigest(in.SourceTrackIDs)
	if err != nil {
		return taskrepo.Spec{}, Job{}, err
	}
	if in.Scene == "playlist" && (count == 0 || in.MatchedCount > count) || in.Scene != "playlist" && (count != 0 || in.MatchedCount != 0) {
		return taskrepo.Spec{}, Job{}, dataengine.ErrInvalidRequest
	}
	job := Job{ID: in.JobID, BatchID: in.BatchID, Scene: in.Scene, Stage: in.Stage, RootID: in.JobID, ActiveTaskID: in.JobID, Label: in.Label, Message: "等待执行", SourceTrackCount: count, SourceTrackDigest: digest, LibraryFileCount: in.LibraryFileCount, MatchedCount: in.MatchedCount, Revision: 1, CreatedNS: in.NowNS, UpdatedNS: in.NowNS, StageStartedNS: in.NowNS}
	var command, lane string
	switch in.Stage {
	case "identifying":
		if in.Scene == "complete" {
			return taskrepo.Spec{}, Job{}, dataengine.ErrInvalidRequest
		}
		job.Kind, job.TaskType, job.TaskGroup, command, lane = "album", "identification", "identify", cmdIdentify, "identify"
	case "snapshotting":
		if in.Scene != "upgrade" && in.Scene != "fill" && in.Scene != "expand" {
			return taskrepo.Spec{}, Job{}, dataengine.ErrInvalidRequest
		}
		job.Kind, job.TaskType, job.TaskGroup, command, lane = "album", "identification", "identify", cmdSnapshot, "identify"
	case "metadata":
		if in.Scene != "complete" {
			return taskrepo.Spec{}, Job{}, dataengine.ErrInvalidRequest
		}
		job.Kind, job.TaskType, job.TaskGroup, command, lane = "album", "album", "repair", cmdRepair, "aux"
	case "preparing":
		job.Kind, job.TaskType, job.TaskGroup, command, lane = "batch", "preparation", "identify", cmdPrepare, "aux"
	default:
		return taskrepo.Spec{}, Job{}, dataengine.ErrInvalidRequest
	}
	if !validJob(job) {
		return taskrepo.Spec{}, Job{}, dataengine.ErrInvalidRequest
	}
	return taskrepo.Spec{ID: job.ID, Command: command, Lane: lane, Priority: 1, CreatedNS: in.NowNS}, job, nil
}

func (r *Repository) Complete(requestID string, token taskrepo.Token, in Complete) (Result, error) {
	if len(in.Message) > 4000 || len(in.ErrorClass) > 80 || in.NowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	for attempt := 0; attempt < 8; attempt++ {
		result, err := r.completeOnce(requestID, token, in)
		if errors.Is(err, errRetry) {
			continue
		}
		return result, err
	}
	return Result{}, ErrStale
}

func (r *Repository) completeOnce(requestID string, token taskrepo.Token, in Complete) (Result, error) {
	if in.JobID == "" {
		task, err := r.tasks.Get(token.ID)
		if err != nil {
			return Result{}, err
		}
		if task == nil || task.WorkflowJobID == "" {
			return Result{}, ErrNotFound
		}
		in.JobID = task.WorkflowJobID
	}
	if !validID(in.JobID) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	current, err := r.Job(in.JobID)
	if err != nil {
		return Result{}, err
	}
	if current == nil {
		return Result{}, ErrNotFound
	}
	task, err := r.tasks.Get(token.ID)
	if err != nil {
		return Result{}, err
	}
	if task == nil || task.WorkflowJobID != current.ID {
		return Result{}, ErrIntegrity
	}
	evidence, payloads, refs, err := prepareEvidence(current.ID, in.Evidence)
	if err != nil {
		return Result{}, err
	}
	cancelled := current.CancelRequested || task.CancelRequested
	var decision *searchdecision.Decision
	if !cancelled {
		decision, err = gateSearch(*current, in)
		if err != nil {
			return Result{}, err
		}
	}
	followups, owners, childPlan, err := planFollowups(*current, in, decision)
	if err != nil {
		return Result{}, err
	}
	if !cancelled {
		if err = evidenceAllows(in.Edge, evidence); err != nil {
			return Result{}, err
		}
	}
	endState := in.TaskState
	if !cancelled && !taskStateMatches(in) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	if cancelled && in.TaskState != "completed" && in.TaskState != "cancelled" {
		return Result{}, dataengine.ErrInvalidRequest
	}
	var decisionIntent []byte
	if decision != nil {
		decisionIntent, err = json.Marshal(decision)
		if err != nil {
			return Result{}, err
		}
	}
	extra, err := json.Marshal(struct {
		JobID, Edge, NextStage, ReviewKind, TaskState string
		Evidence                                      []evidenceRef
		Decision                                      json.RawMessage `json:",omitempty"`
	}{in.JobID, in.Edge, in.NextStage, in.ReviewKind, in.TaskState, refs, decisionIntent})
	if err != nil {
		return Result{}, err
	}
	paused := current.Paused || task.PauseRequested
	if !cancelled && current.RootID != current.ID {
		root, err := r.Job(current.RootID)
		if err != nil {
			return Result{}, err
		}
		if root != nil && root.Paused {
			paused = true
		}
	}
	result, err := r.tasks.FinishOwned(requestID, token, taskrepo.Completion{State: endState, NowNS: in.NowNS, ErrorClass: in.ErrorClass, Message: in.Message, Payload: in.Result}, followups, owners, paused && !cancelled, extra, func(v *dataengine.View, before, _ *taskrepo.Task, children []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		return r.finishDomain(v, *current, in, evidence, payloads, childPlan, decision, before, children)
	})
	return decode(result, err)
}

type childPlan struct {
	Job    *Job
	TaskID string
}

func taskStateMatches(in Complete) bool {
	switch in.Edge {
	case edgeQueueSearch, edgeQueueProcess, edgeAdvance, edgeDefer:
		return in.TaskState == "completed"
	case edgeTerminal:
		switch in.NextStage {
		case "completed", "skipped":
			return in.TaskState == "completed"
		case "failed":
			return in.TaskState == "failed"
		case "needs_review":
			return in.TaskState == "blocked"
		case "cancelled":
			return in.TaskState == "cancelled"
		default:
			return false
		}
	default:
		return false
	}
}

func evidenceAllows(edge string, items []Evidence) error {
	switch edge {
	case edgeQueueSearch:
		if !confirmedIdentity(items) {
			return dataengine.ErrInvalidRequest
		}
	case edgeQueueProcess:
		if !observedSource(items) {
			return dataengine.ErrInvalidRequest
		}
	}
	return nil
}

func gateSearch(job Job, in Complete) (*searchdecision.Decision, error) {
	searchEdge := job.TaskType == "search" && (in.Edge == edgeQueueProcess || in.Edge == edgeDefer || in.Edge == edgeTerminal)
	if in.Search != nil && !searchEdge {
		return nil, dataengine.ErrInvalidRequest
	}
	if !searchEdge {
		return nil, nil
	}
	if in.Search == nil || in.Search.Scene != job.Scene {
		return nil, dataengine.ErrInvalidRequest
	}
	decision, err := searchdecision.Decide(*in.Search)
	if err != nil {
		return nil, err
	}
	switch in.Edge {
	case edgeQueueProcess:
		switch in.NextStage {
		case "requesting":
			if decision.Action != searchdecision.ActionQueueRequest || job.IdentificationJobID == "" {
				return nil, dataengine.ErrInvalidRequest
			}
		case "fallback":
			if decision.Action != searchdecision.ActionFallback || job.IdentificationJobID == "" {
				return nil, dataengine.ErrInvalidRequest
			}
		default:
			return nil, dataengine.ErrInvalidRequest
		}
	case edgeDefer:
		if decision.Action != searchdecision.ActionRetry && decision.Action != searchdecision.ActionResearch {
			return nil, dataengine.ErrInvalidRequest
		}
	case edgeTerminal:
		switch in.NextStage {
		case "completed":
			return nil, dataengine.ErrInvalidRequest
		case "skipped":
			if decision.Action != searchdecision.ActionSkip {
				return nil, dataengine.ErrInvalidRequest
			}
		case "needs_review":
			if decision.Action == searchdecision.ActionReviewSearch && in.ReviewKind != "search" || decision.Action == searchdecision.ActionReviewCandidate && in.ReviewKind != "candidate" || decision.Action != searchdecision.ActionReviewSearch && decision.Action != searchdecision.ActionReviewCandidate {
				return nil, dataengine.ErrInvalidRequest
			}
		case "failed":
			if decision.Action == searchdecision.ActionQueueRequest || decision.Action == searchdecision.ActionFallback {
				return nil, dataengine.ErrInvalidRequest
			}
		case "cancelled":
		default:
			return nil, dataengine.ErrInvalidRequest
		}
	}
	return &decision, nil
}

func planFollowups(job Job, in Complete, decision *searchdecision.Decision) ([]taskrepo.NewTask, map[string]string, *childPlan, error) {
	id, command, lane, taskType, group, stage, newJob, err := successor(job, in, decision)
	if err != nil {
		return nil, nil, nil, err
	}
	if in.Edge == edgeTerminal {
		if in.NextStage == "needs_review" && !reviewKinds[in.ReviewKind] {
			return nil, nil, nil, dataengine.ErrInvalidRequest
		}
		return nil, map[string]string{}, nil, nil
	}
	payload := in.Result
	if payload == nil {
		payload = []byte(`{"schema":1}`)
	}
	followup := taskrepo.NewTask{Spec: taskrepo.Spec{ID: id, Command: command, Lane: lane, Priority: 1, CreatedNS: in.NowNS}, Payload: payload}
	owner := job.ID
	var child *Job
	if newJob {
		owner = id
		next := job
		next.ID = id
		next.ParentJobID = job.ID
		next.RootID = job.RootID
		next.Stage = stage
		next.TaskType = taskType
		next.TaskGroup = group
		next.ActiveTaskID = id
		next.Revision = 1
		next.CreatedNS = in.NowNS
		next.UpdatedNS = in.NowNS
		next.StageStartedNS = in.NowNS
		next.CancelRequested = false
		next.ReviewKind = ""
		next.ErrorClass = ""
		next.SearchJobID = ""
		next.ProcessingJobID = ""
		next.Message = "等待执行"
		if in.Edge == edgeQueueSearch {
			next.IdentificationJobID = job.ID
			next.Message = "身份已核对，等待检索"
		}
		if in.Edge == edgeQueueProcess {
			next.IdentificationJobID = job.IdentificationJobID
			next.SearchJobID = job.ID
			next.SearchRetryCount = 0
			next.AIResearchCount = 0
			next.SearchRetryAfterSec = 0
			next.SearchAction = ""
			if decision != nil {
				next.SearchAction = decision.Action
			}
			next.Message = messageOr(in.Message, "等待执行")
			if stage == "requesting" {
				next.Message = messageOr(in.Message, "检索命中，等待获取；尚未下载、尚未入库")
			}
			if stage == "fallback" {
				next.Message = messageOr(in.Message, "已排队流媒体回退；尚未下载、尚未入库")
			}
		}
		child = &next
	}
	return []taskrepo.NewTask{followup}, map[string]string{id: owner}, &childPlan{Job: child, TaskID: id}, nil
}

func legalEdge(job Job, edge, next string) error {
	switch edge {
	case edgeQueueSearch:
		if job.TaskType != "identification" || (job.Stage != "identifying" && job.Stage != "snapshotting") {
			return ErrStale
		}
	case edgeQueueProcess:
		if job.TaskType != "search" || job.Stage != "searching" {
			return ErrStale
		}
		switch next {
		case "requesting", "fallback":
		default:
			return dataengine.ErrInvalidRequest
		}
	case edgeDefer:
		if job.TaskType != "search" || job.Stage != "searching" {
			return ErrStale
		}
		if next != "" && next != "searching" {
			return dataengine.ErrInvalidRequest
		}
	case edgeAdvance:
		switch job.Stage + ">" + next {
		case "metadata>validating":
			if job.TaskType != "album" {
				return ErrStale
			}
		case "validating>publishing", "publishing>notifying", "requesting>downloading", "downloading>validating":
		default:
			return ErrStale
		}
	case edgeTerminal:
		if job.TaskType == "search" && next == "completed" {
			return dataengine.ErrInvalidRequest
		}
		if !terminalStage(next) || job.Stage == next {
			return ErrStale
		}
	default:
		return dataengine.ErrInvalidRequest
	}
	return nil
}

func successor(job Job, in Complete, decision *searchdecision.Decision) (id, command, lane, taskType, group, stage string, newJob bool, err error) {
	switch in.Edge {
	case edgeQueueSearch:
		return searchJobID(job.ID), cmdSearch, "identify", "search", "search", "searching", true, nil
	case edgeQueueProcess:
		switch in.NextStage {
		case "requesting", "fallback":
			return processJobID(job.ID), cmdProcess, "aux", "album", "process", in.NextStage, true, nil
		default:
			return "", "", "", "", "", "", false, dataengine.ErrInvalidRequest
		}
	case edgeDefer:
		if decision == nil || in.Search == nil {
			return "", "", "", "", "", "", false, dataengine.ErrInvalidRequest
		}
		var kind string
		var nextCount int
		switch decision.Action {
		case searchdecision.ActionRetry:
			kind, nextCount = "retry", int(in.Search.RetryCount)+1
		case searchdecision.ActionResearch:
			kind, nextCount = "research", int(in.Search.AIResearchCount)+1
		default:
			return "", "", "", "", "", "", false, dataengine.ErrInvalidRequest
		}
		return stageTaskID(job.ID, kind+":"+strconv.Itoa(nextCount)), cmdSearch, "identify", job.TaskType, job.TaskGroup, "searching", false, nil
	case edgeAdvance:
		switch in.NextStage {
		case "validating", "downloading":
			return stageTaskID(job.ID, in.NextStage), cmdProcess, "aux", job.TaskType, job.TaskGroup, in.NextStage, false, nil
		case "publishing", "notifying":
			return stageTaskID(job.ID, in.NextStage), cmdPublish, "publish", job.TaskType, job.TaskGroup, in.NextStage, false, nil
		default:
			return "", "", "", "", "", "", false, dataengine.ErrInvalidRequest
		}
	case edgeTerminal:
		if !terminalStage(in.NextStage) {
			return "", "", "", "", "", "", false, dataengine.ErrInvalidRequest
		}
		return "", "", "", "", "", in.NextStage, false, nil
	default:
		return "", "", "", "", "", "", false, dataengine.ErrInvalidRequest
	}
}

func (r *Repository) finishDomain(v *dataengine.View, planned Job, in Complete, evidence []Evidence, payloads [][]byte, child *childPlan, decision *searchdecision.Decision, before *taskrepo.Task, children []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
	live, raw, err := readJob(v, planned.ID)
	if err != nil || live == nil {
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, ErrNotFound
	}
	if live.Revision != planned.Revision || live.Stage != planned.Stage || live.ActiveTaskID != planned.ActiveTaskID {
		return nil, nil, nil, errRetry
	}
	var rootRaw []byte
	rootPaused, rootCancelled := false, false
	if live.RootID != live.ID {
		root, rootBytes, err := readJob(v, live.RootID)
		if err != nil {
			return nil, nil, nil, err
		}
		if root == nil {
			return nil, nil, nil, ErrIntegrity
		}
		rootRaw = rootBytes
		rootPaused, rootCancelled = root.Paused, root.CancelRequested
	}
	cancelled := live.CancelRequested || rootCancelled || before != nil && before.CancelRequested
	if !cancelled {
		if err = legalEdge(*live, in.Edge, in.NextStage); err != nil {
			return nil, nil, nil, err
		}
		if decision != nil && (decision.Action == searchdecision.ActionQueueRequest || decision.Action == searchdecision.ActionFallback) && live.IdentificationJobID == "" {
			return nil, nil, nil, dataengine.ErrInvalidRequest
		}
		if in.Edge == edgeDefer && (decision == nil || in.Search == nil || live.SearchRetryCount != in.Search.RetryCount || live.AIResearchCount != in.Search.AIResearchCount) {
			return nil, nil, nil, ErrStale
		}
	}
	if cancelled {
		if len(children) != 0 {
			return nil, nil, nil, errRetry
		}
		return r.commitJob(v, raw, rootRaw, *live, in, evidence, payloads, nil, "", decision, true)
	}
	if in.Edge == edgeTerminal {
		if len(children) != 0 {
			return nil, nil, nil, errRetry
		}
		return r.commitJob(v, raw, rootRaw, *live, in, evidence, payloads, nil, "", decision, false)
	}
	if child == nil || len(children) != 1 || children[0].ID != child.TaskID {
		return nil, nil, nil, errRetry
	}
	paused := children[0].PauseRequested || children[0].State == "paused"
	if (live.Paused || rootPaused) && !paused {
		return nil, nil, nil, errRetry
	}
	if child.Job != nil {
		if children[0].WorkflowJobID != child.Job.ID {
			return nil, nil, nil, errRetry
		}
		child.Job.Paused = paused || live.Paused || rootPaused
		return r.commitJob(v, raw, rootRaw, *live, in, evidence, payloads, child.Job, "", decision, false)
	}
	if children[0].WorkflowJobID != live.ID {
		return nil, nil, nil, errRetry
	}
	return r.commitJob(v, raw, rootRaw, *live, in, evidence, payloads, nil, children[0].ID, decision, false)
}

func (r *Repository) commitJob(v *dataengine.View, raw, rootRaw []byte, live Job, in Complete, evidence []Evidence, payloads [][]byte, child *Job, nextTaskID string, decision *searchdecision.Decision, cancelled bool) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
	next := live
	var stage string
	switch {
	case cancelled:
		stage = "cancelled"
		next.Message = "已取消，未创建后继任务"
		next.ReviewKind = ""
	case in.Edge == edgeQueueSearch:
		stage = "completed"
		next.SearchJobID = child.ID
		next.Message = messageOr(in.Message, "识别完成，已创建独立搜索任务")
	case in.Edge == edgeQueueProcess:
		stage = "completed"
		next.ProcessingJobID = child.ID
		next.Message = messageOr(in.Message, "检索完成，已创建独立处理任务")
		if decision != nil && decision.Action == searchdecision.ActionFallback {
			next.Message = messageOr(in.Message, "检索未命中，已排队流媒体回退；尚未下载、尚未入库")
		}
		if decision != nil && decision.Action == searchdecision.ActionQueueRequest {
			next.Message = messageOr(in.Message, "检索命中，已创建获取任务；尚未下载、尚未入库")
		}
	case in.Edge == edgeDefer:
		stage = "searching"
		next.ActiveTaskID = nextTaskID
		if decision == nil {
			return nil, nil, nil, ErrIntegrity
		}
		switch decision.Action {
		case searchdecision.ActionRetry:
			next.SearchRetryCount = live.SearchRetryCount + 1
			next.SearchRetryAfterSec = decision.RetryAfterSec
			next.Message = messageOr(in.Message, "站点暂时不可用，已安排重新检索；尚未回退流媒体")
		case searchdecision.ActionResearch:
			next.AIResearchCount = live.AIResearchCount + 1
			next.SearchRetryAfterSec = 0
			next.Message = messageOr(in.Message, "使用目录别名重新检索；别名未确认新身份")
		default:
			return nil, nil, nil, ErrIntegrity
		}
	case in.Edge == edgeAdvance:
		stage = in.NextStage
		next.ActiveTaskID = nextTaskID
		next.Message = messageOr(in.Message, "进入下一阶段")
		next.ReviewKind = ""
		next.ErrorClass = ""
	default:
		stage = in.NextStage
		next.Message = in.Message
		next.ReviewKind = in.ReviewKind
		next.ErrorClass = in.ErrorClass
	}
	changedStage := stage != next.Stage
	next.Stage = stage
	updated, err := bump(next, in.NowNS, changedStage)
	if err != nil {
		return nil, nil, nil, err
	}
	if cancelled {
		updated.CancelRequested = true
	} else if decision != nil {
		updated.SearchAction = decision.Action
	}
	conditions, writes, err := writeEvidence(v, evidence, payloads)
	if err != nil {
		return nil, nil, nil, err
	}
	jobCondition, jobWrites, err := putJob(raw, updated)
	if err != nil {
		return nil, nil, nil, err
	}
	conditions = append(conditions, jobCondition)
	writes = append(writes, jobWrites...)
	if rootRaw != nil {
		conditions = append(conditions, dataengine.Condition{Key: jobKey(live.RootID), Value: rootRaw})
	}
	var children []Job
	if child != nil {
		if _, found, err := v.Get(jobKey(child.ID)); err != nil || found {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrStale
		}
		childCondition, childWrites, err := putJob(nil, *child)
		if err != nil {
			return nil, nil, nil, err
		}
		conditions = append(conditions, childCondition)
		writes = append(writes, childWrites...)
		children = []Job{*child}
	}
	for i := range evidence {
		evidence[i].JobID = live.ID
	}
	attached, err := json.Marshal(Result{Job: &updated, Evidence: evidence, Children: children, Search: decision})
	if err != nil {
		return nil, nil, nil, err
	}
	return conditions, writes, attached, nil
}

func bump(job Job, now int64, stageChanged bool) (Job, error) {
	if job.Revision == ^uint64(0) {
		return Job{}, ErrIntegrity
	}
	job.Revision++
	job.UpdatedNS = later(now, job.UpdatedNS)
	if stageChanged {
		job.StageStartedNS = job.UpdatedNS
	}
	return job, nil
}

func messageOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func (r *Repository) Control(requestID, jobID, action string, nowNS int64) (Result, error) {
	if !validID(jobID) || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	switch action {
	case "pause", "resume", "cancel":
	default:
		return Result{}, dataengine.ErrInvalidRequest
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, err := r.Job(jobID)
		if err != nil {
			return Result{}, err
		}
		if current == nil {
			return Result{}, ErrNotFound
		}
		if current.ActiveTaskID == "" {
			return Result{}, ErrStale
		}
		extra, _ := json.Marshal(struct{ JobID, Action string }{jobID, action})
		result, err := r.tasks.ControlOwned(requestID, current.ActiveTaskID, action, nowNS, extra, func(v *dataengine.View, _, after *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
			live, raw, err := readJob(v, jobID)
			if err != nil || live == nil {
				if err != nil {
					return nil, nil, nil, err
				}
				return nil, nil, nil, ErrNotFound
			}
			if after == nil || live.ActiveTaskID != after.ID || live.Revision != current.Revision {
				return nil, nil, nil, errRetry
			}
			if action == "resume" && (live.CancelRequested || after.CancelRequested) {
				return nil, nil, nil, ErrStale
			}
			next := *live
			next.Paused = after.PauseRequested || after.State == "paused"
			next.CancelRequested = after.CancelRequested || after.State == "cancelled"
			if after.State == "cancelled" && !terminalStage(next.Stage) {
				next.Stage = "cancelled"
				next.Message = "已取消后续处理；未开始的阶段不会继续"
				next.StageStartedNS = later(nowNS, next.UpdatedNS)
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
		if errors.Is(err, errRetry) {
			continue
		}
		return decode(result, err)
	}
	return Result{}, ErrStale
}

func (r *Repository) Interrupt(requestID string, token taskrepo.Token, nowNS int64, reason string) (Result, error) {
	if len(reason) > 4000 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	task, err := r.tasks.Get(token.ID)
	if err != nil {
		return Result{}, err
	}
	if task == nil || task.WorkflowJobID == "" {
		return Result{}, ErrNotFound
	}
	extra, _ := json.Marshal(struct{ JobID, Reason string }{task.WorkflowJobID, reason})
	result, err := r.tasks.InterruptOwned(requestID, token, nowNS, reason, extra, func(v *dataengine.View, before, _ *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		if before == nil || before.WorkflowJobID != task.WorkflowJobID {
			return nil, nil, nil, errRetry
		}
		live, raw, err := readJob(v, task.WorkflowJobID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if terminalStage(live.Stage) || live.ActiveTaskID != token.ID {
			return nil, nil, nil, ErrStale
		}
		next := *live
		next.Stage = "needs_review"
		next.ReviewKind = "interrupted"
		next.ErrorClass = "RECOVERY_REQUIRED"
		next.Message = reason
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

// BlockUnregistered records that no executor exists. It does not create a successor.
func (r *Repository) BlockUnregistered(requestID string, token taskrepo.Token, nowNS int64) (Result, error) {
	return r.Complete(requestID, token, Complete{Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "error", Message: "执行器尚未注册，工作流未推进", TaskState: "blocked", ErrorClass: "UNSUPPORTED_COMMAND", NowNS: nowNS})
}

func (r *Repository) Job(id string) (*Job, error) {
	if !validID(id) {
		return nil, dataengine.ErrInvalidRequest
	}
	var job *Job
	err := r.engine.View(func(v *dataengine.View) error {
		var err error
		job, _, err = readJob(v, id)
		return err
	})
	return job, err
}

func (r *Repository) Evidence(id string) (*Evidence, error) {
	if !validID(id) {
		return nil, dataengine.ErrInvalidRequest
	}
	var item *Evidence
	err := r.engine.View(func(v *dataengine.View) error {
		var err error
		item, _, err = readEvidence(v, id)
		return err
	})
	return item, err
}

func (r *Repository) EvidenceData(id string) ([]byte, bool, error) {
	if !validID(id) {
		return nil, false, dataengine.ErrInvalidRequest
	}
	var payload []byte
	var found bool
	err := r.engine.View(func(v *dataengine.View) error {
		item, _, err := readEvidence(v, id)
		if err != nil || item == nil {
			return err
		}
		found = true
		payload, err = taskrepo.ReadBlob(v, item.PayloadHash)
		return err
	})
	return payload, found, err
}

// List reads the root index. Each stored job is present, including child jobs.
// The cursor is the previous storage key. This does not read evidence payloads.
func (r *Repository) List(after []byte, limit int) (JobPage, error) {
	if limit < 1 || limit > 200 {
		return JobPage{}, dataengine.ErrInvalidRequest
	}
	prefix := []byte("workflow-root/")
	page := JobPage{Jobs: []Job{}}
	err := r.engine.View(func(v *dataengine.View) error {
		scanned, err := v.Scan(prefix, after, limit)
		if err != nil {
			return err
		}
		page.Next = scanned.Next
		for _, item := range scanned.Items {
			var job Job
			if json.Unmarshal(item.Value, &job) != nil || !validJob(job) {
				return ErrIntegrity
			}
			page.Jobs = append(page.Jobs, job)
		}
		return nil
	})
	return page, err
}

func (r *Repository) Children(parent string, after []byte, limit int) (JobPage, error) {
	if !validID(parent) {
		return JobPage{}, dataengine.ErrInvalidRequest
	}
	return r.jobs([]byte("workflow-child/"+digestBytes([]byte(parent))+"/"), after, limit, parent)
}

func (r *Repository) EvidenceFor(jobID string, after []byte, limit int) (EvidencePage, error) {
	if !validID(jobID) || limit < 1 || limit > 200 {
		return EvidencePage{}, dataengine.ErrInvalidRequest
	}
	page := EvidencePage{Evidence: []Evidence{}}
	err := r.engine.View(func(v *dataengine.View) error {
		scanned, err := v.Scan([]byte("workflow-evidence-job/"+digestBytes([]byte(jobID))+"/"), after, limit)
		if err != nil {
			return err
		}
		page.Next = scanned.Next
		for _, item := range scanned.Items {
			var evidence Evidence
			if json.Unmarshal(item.Value, &evidence) != nil || validateEvidence(evidence) != nil {
				return ErrIntegrity
			}
			page.Evidence = append(page.Evidence, evidence)
		}
		return nil
	})
	return page, err
}

func (r *Repository) jobs(prefix, after []byte, limit int, parent string) (JobPage, error) {
	if !validID(parent) || limit < 1 || limit > 200 {
		return JobPage{}, dataengine.ErrInvalidRequest
	}
	page := JobPage{Jobs: []Job{}}
	err := r.engine.View(func(v *dataengine.View) error {
		scanned, err := v.Scan(prefix, after, limit)
		if err != nil {
			return err
		}
		page.Next = scanned.Next
		for _, item := range scanned.Items {
			var job Job
			if json.Unmarshal(item.Value, &job) != nil || !validJob(job) {
				return ErrIntegrity
			}
			page.Jobs = append(page.Jobs, job)
		}
		return nil
	})
	return page, err
}

func (r *Repository) Inspect(id string) (Inspection, error) {
	if !validID(id) {
		return Inspection{}, dataengine.ErrInvalidRequest
	}
	var out Inspection
	err := r.engine.View(func(v *dataengine.View) error {
		job, _, err := readJob(v, id)
		if err != nil || job == nil {
			if err != nil {
				return err
			}
			return ErrNotFound
		}
		out.Job = *job
		if job.ActiveTaskID != "" {
			task, err := taskrepo.ReadSnapshot(v, job.ActiveTaskID)
			if err != nil {
				return err
			}
			out.Task = task
		}
		jobs, err := collectJobs(v, []byte("workflow-child/"+digestBytes([]byte(id))+"/"))
		if err != nil {
			return err
		}
		out.Children = jobs
		for _, child := range jobs {
			if child.ActiveTaskID == "" {
				continue
			}
			task, err := taskrepo.ReadSnapshot(v, child.ActiveTaskID)
			if err != nil {
				return err
			}
			if task != nil {
				out.ChildTasks = append(out.ChildTasks, *task)
			}
		}
		raws, err := collectValues(v, []byte("workflow-evidence-job/"+digestBytes([]byte(id))+"/"))
		if err != nil {
			return err
		}
		for _, raw := range raws {
			var item Evidence
			if json.Unmarshal(raw, &item) != nil || validateEvidence(item) != nil {
				return ErrIntegrity
			}
			out.Evidence = append(out.Evidence, item)
		}
		return nil
	})
	return out, err
}

func collectJobs(v *dataengine.View, prefix []byte) ([]Job, error) {
	raws, err := collectValues(v, prefix)
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(raws))
	for _, raw := range raws {
		var job Job
		if json.Unmarshal(raw, &job) != nil || !validJob(job) {
			return nil, ErrIntegrity
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func collectValues(v *dataengine.View, prefix []byte) ([][]byte, error) {
	var after []byte
	var values [][]byte
	for {
		page, err := v.Scan(prefix, after, 200)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			values = append(values, item.Value)
		}
		if len(page.Next) == 0 {
			return values, nil
		}
		if len(values) > 1000 {
			return nil, ErrIntegrity
		}
		after = page.Next
	}
}

func (r *Repository) Request(requestID string) (Status, error) {
	status, err := r.tasks.Request(requestID)
	if err != nil || status.Result == nil {
		return Status{State: status.State, Code: status.Code}, err
	}
	decoded, err := decode(*status.Result, nil)
	if err != nil {
		return Status{}, err
	}
	return Status{State: status.State, Code: status.Code, Result: &decoded}, nil
}
