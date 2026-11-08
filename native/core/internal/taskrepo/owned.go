package taskrepo

import (
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

type childIntent struct {
	Spec Spec
	Hash string
}

func prepareFinish(token Token, end Completion, followups []NewTask) (string, []byte, []preparedTask, []childIntent, error) {
	if !validToken(token) || end.NowNS <= 0 || len(end.Message) > 4000 || len(end.ErrorClass) > 80 {
		return "", nil, nil, nil, dataengine.ErrInvalidRequest
	}
	switch end.State {
	case "completed", "failed", "cancelled", "blocked", "recovery_required":
	default:
		return "", nil, nil, nil, dataengine.ErrInvalidRequest
	}
	hash := ""
	var compressed []byte
	var err error
	if end.Payload != nil {
		hash, compressed, err = pack(end.Payload)
		if err != nil {
			return "", nil, nil, nil, err
		}
	}
	if len(followups) > 1000 || len(followups) > 0 && end.State != "completed" {
		return "", nil, nil, nil, dataengine.ErrInvalidRequest
	}
	total := len(end.Payload)
	seen := map[string]bool{token.ID: true}
	prepared := make([]preparedTask, 0, len(followups))
	for _, child := range followups {
		if !validSpec(child.Spec) || seen[child.Spec.ID] || (child.Spec.ParentID != "" && child.Spec.ParentID != token.ID) {
			return "", nil, nil, nil, dataengine.ErrInvalidRequest
		}
		total += len(child.Payload)
		if total > maxPayload {
			return "", nil, nil, nil, dataengine.ErrInvalidRequest
		}
		seen[child.Spec.ID] = true
		child.Spec.ParentID = token.ID
		childHash, childBlob, err := pack(child.Payload)
		if err != nil {
			return "", nil, nil, nil, err
		}
		prepared = append(prepared, preparedTask{Task: Task{Spec: child.Spec, State: "queued", UpdatedNS: child.Spec.CreatedNS, Revision: 1, PayloadHash: childHash}, Compressed: childBlob})
	}
	intents := make([]childIntent, 0, len(prepared))
	for _, child := range prepared {
		intents = append(intents, childIntent{Spec: child.Task.Spec, Hash: child.Task.PayloadHash})
	}
	return hash, compressed, prepared, intents, nil
}

// CreateOwned queues a task that can be finished only with its workflow job.
func (r *Repository) CreateOwned(requestID string, spec Spec, payload []byte, workflowJobID string, extra []byte, domain Domain) (Result, error) {
	if !validSpec(spec) || spec.ParentID != "" || !validID(workflowJobID) || spec.ID != workflowJobID || domain == nil {
		return Result{}, dataengine.ErrInvalidRequest
	}
	hash, compressed, err := pack(payload)
	if err != nil {
		return Result{}, err
	}
	intent := struct {
		Operation, Workflow, Hash string
		Spec                      Spec
		Extra                     []byte
	}{"workflow_start", workflowJobID, hash, spec, extra}
	return r.apply(requestID, spec.ID, intent, func(v *dataengine.View, old *Task) (*Task, []dataengine.Mutation, error) {
		if old != nil {
			return nil, nil, ErrStale
		}
		writes, err := blobWrites(v, hash, compressed)
		if err != nil {
			return nil, nil, err
		}
		return &Task{Spec: spec, State: "queued", UpdatedNS: spec.CreatedNS, PayloadHash: hash, WorkflowJobID: workflowJobID}, writes, nil
	}, domain)
}

// CreateFollowup queues a task owned by an existing workflow job. Unlike
// CreateOwned, the task id need not equal the job id: a followup carries the
// stage in its id (stage:<job>:<stage>) while the job keeps its own task.
// When the followup already exists it is kept and the domain callback still
// runs, so a repeated call records new evidence instead of dropping queued work.
func (r *Repository) CreateFollowup(requestID string, spec Spec, payload []byte, workflowJobID string, extra []byte, domain Domain) (Result, error) {
	if !validSpec(spec) || spec.ParentID != "" || !validID(workflowJobID) || domain == nil {
		return Result{}, dataengine.ErrInvalidRequest
	}
	hash, compressed, err := pack(payload)
	if err != nil {
		return Result{}, err
	}
	intent := struct {
		Operation, Workflow, Hash string
		Spec                      Spec
		Extra                     []byte
	}{"workflow_followup", workflowJobID, hash, spec, extra}
	return r.apply(requestID, spec.ID, intent, func(v *dataengine.View, old *Task) (*Task, []dataengine.Mutation, error) {
		if old != nil {
			return old, nil, nil
		}
		writes, err := blobWrites(v, hash, compressed)
		if err != nil {
			return nil, nil, err
		}
		return &Task{Spec: spec, State: "queued", UpdatedNS: spec.CreatedNS, PayloadHash: hash, WorkflowJobID: workflowJobID}, writes, nil
	}, domain)
}

// FinishOwned commits task completion together with workflow-owned mutations.
func (r *Repository) FinishOwned(requestID string, token Token, end Completion, followups []NewTask, owners map[string]string, pauseSuccessors bool, extra []byte, domain Domain) (Result, error) {
	if domain == nil {
		return Result{}, dataengine.ErrInvalidRequest
	}
	hash, compressed, prepared, intents, err := prepareFinish(token, end, followups)
	if err != nil {
		return Result{}, err
	}
	if len(owners) != len(prepared) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	for i := range prepared {
		jobID, ok := owners[prepared[i].Task.ID]
		if !ok || !validID(jobID) {
			return Result{}, dataengine.ErrInvalidRequest
		}
		prepared[i].Task.WorkflowJobID = jobID
		if pauseSuccessors {
			prepared[i].Task.State = "paused"
			prepared[i].Task.PauseRequested = true
		}
	}
	intent := struct {
		Operation                        string
		Token                            Token
		State, Hash, ErrorClass, Message string
		Children                         any
		Extra                            []byte
		Owners                           map[string]string
	}{"workflow_finish", token, end.State, hash, end.ErrorClass, end.Message, intents, extra, owners}
	return r.apply(requestID, token.ID, intent, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		return finishDecision(v, current, token, end, hash, compressed, true)
	}, domain, prepared...)
}

// ControlOwned records a pause, resume or cancel on a workflow-owned task.
func (r *Repository) ControlOwned(requestID, id, action string, nowNS int64, extra []byte, domain Domain) (Result, error) {
	if !validID(id) || nowNS <= 0 || domain == nil {
		return Result{}, dataengine.ErrInvalidRequest
	}
	switch action {
	case "pause", "resume", "cancel":
	default:
		return Result{}, dataengine.ErrInvalidRequest
	}
	intent := struct {
		Operation, ID, Action string
		Extra                 []byte
	}{"workflow_control", id, action, extra}
	return r.apply(requestID, id, intent, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		return controlDecision(v, current, id, action, nowNS, true)
	}, domain)
}

// InterruptOwned marks a started workflow task as requiring reconciliation.
func (r *Repository) InterruptOwned(requestID string, token Token, nowNS int64, reason string, extra []byte, domain Domain) (Result, error) {
	if !validToken(token) || nowNS <= 0 || len(reason) > 4000 || domain == nil {
		return Result{}, dataengine.ErrInvalidRequest
	}
	intent := struct {
		Operation string
		Token     Token
		Reason    string
		Extra     []byte
	}{"workflow_interrupt", token, reason, extra}
	return r.apply(requestID, token.ID, intent, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		return interruptDecision(v, current, token, nowNS, reason, true)
	}, domain)
}

func DigestPayload(payload []byte) (string, error) {
	if len(payload) > maxPayload {
		return "", dataengine.ErrInvalidRequest
	}
	return digest(payload), nil
}

func WriteBlob(v *dataengine.View, payload []byte) (string, []dataengine.Mutation, error) {
	hash, compressed, err := pack(payload)
	if err != nil {
		return "", nil, err
	}
	writes, err := blobWrites(v, hash, compressed)
	return hash, writes, err
}

func ReadBlob(v *dataengine.View, hash string) ([]byte, error) {
	if len(hash) != 64 {
		return nil, dataengine.ErrInvalidRequest
	}
	compressed, found, err := v.Get(blobKey(hash))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrIntegrity
	}
	return unpack(hash, compressed)
}

func ReadSnapshot(v *dataengine.View, id string) (*Task, error) {
	task, _, err := readTask(v, id)
	return task, err
}
