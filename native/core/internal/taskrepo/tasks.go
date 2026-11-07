package taskrepo

import (
	"encoding/json"
	"math"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func (r *Repository) Enqueue(requestID string, spec Spec, payload []byte) (Result, error) {
	if !validSpec(spec) || spec.ParentID != "" {
		return Result{}, dataengine.ErrInvalidRequest
	}
	hash, compressed, err := pack(payload)
	if err != nil {
		return Result{}, err
	}
	intent := struct {
		Operation string
		Spec      Spec
		Hash      string
	}{"enqueue", spec, hash}
	return r.apply(requestID, spec.ID, intent, func(v *dataengine.View, old *Task) (*Task, []dataengine.Mutation, error) {
		if old != nil {
			return nil, nil, ErrStale
		}
		extra, err := blobWrites(v, hash, compressed)
		return &Task{Spec: spec, State: "queued", UpdatedNS: spec.CreatedNS, PayloadHash: hash}, extra, err
	})
}

func (r *Repository) Claim(requestID, lane, owner string, nowNS int64) (Result, error) {
	if requestID == "" || len(requestID) > 128 || !validLane(lane) || owner == "" || len(owner) > 128 || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	intent := struct{ Operation, Lane, Owner string }{"claim", lane, owner}
	signature, _ := json.Marshal(intent)
	for retry := 0; retry < 256; retry++ {
		var receipt *Receipt
		err := r.engine.View(func(v *dataengine.View) error {
			var err error
			receipt, err = readReceipt(v, requestID, digest(signature))
			return err
		})
		if err != nil {
			return Result{}, err
		}
		if receipt != nil {
			return receipt.Result, receiptError(receipt.Code)
		}
		page, err := r.List(lane, "queued", nil, 1)
		if err != nil {
			return Result{}, err
		}
		// Empty polling is not a durable command; do not grow a receipt per idle tick.
		if len(page.Tasks) == 0 {
			return Result{}, nil
		}
		selected := page.Tasks[0]
		// A raced candidate is retried without recording a rejected claim. Its
		// intended entity is "next in this lane", rather than that stale candidate.
		result, err := r.applyClaim(requestID, selected, intent, owner, nowNS)
		if err == ErrStale {
			continue
		}
		return result, err
	}
	return Result{}, ErrStale
}

func (r *Repository) applyClaim(requestID string, selected Task, intent any, owner string, nowNS int64) (Result, error) {
	// apply rejects only after it has compared the live task inside a snapshot.
	// Returning a transient conflict here must not consume the logical request ID.
	rawIntent, _ := json.Marshal(intent)
	signature := digest(rawIntent)
	var response Result
	var conditions []dataengine.Condition
	var mutations []dataengine.Mutation
	var replay *Receipt
	err := r.engine.View(func(v *dataengine.View) error {
		var err error
		replay, err = readReceipt(v, requestID, signature)
		if err != nil || replay != nil {
			return err
		}
		current, raw, err := readTask(v, selected.ID)
		if err != nil {
			return err
		}
		if current == nil || current.State != "queued" || current.Revision != selected.Revision {
			return ErrStale
		}
		if current.Fence == math.MaxUint64 || current.Revision == math.MaxUint64 {
			return ErrIntegrity
		}
		next := *current
		next.State = "running"
		next.Execution = "claimed"
		next.Owner = owner
		next.Fence++
		next.Revision++
		next.UpdatedNS = effectiveTime(nowNS, current)
		next.ProgressSequence = 0
		next.ProgressHash = ""
		next.Progress = 0
		next.PauseRequested = false
		next.CancelRequested = false
		next.CancelOutcome = ""
		next.ErrorClass = ""
		next.ResultHash = ""
		conditions, mutations, err = stateMutations(v, current, &next)
		if err != nil {
			return err
		}
		conditions = append(conditions, condition(taskKey(current.ID), raw), dataengine.Condition{Key: operationKey(requestID), Absent: true})
		attempt := Attempt{Token: Token{ID: next.ID, Owner: owner, Fence: next.Fence}, State: "claimed", ClaimedNS: next.UpdatedNS}
		attemptRaw, _ := json.Marshal(attempt)
		response = Result{Task: &next, Changed: true}
		receiptRaw, _ := json.Marshal(Receipt{Intent: signature, Result: response})
		mutations = append(mutations, dataengine.Mutation{Key: attemptKey(attempt.Token), Value: attemptRaw}, dataengine.Mutation{Key: operationKey(requestID), Value: receiptRaw})
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if replay != nil {
		return replay.Result, receiptError(replay.Code)
	}
	applied, err := r.engine.CompareAndSwap(conditions, mutations)
	if err != nil {
		return Result{}, err
	}
	if !applied {
		return Result{}, ErrStale
	}
	r.notify()
	return response, nil
}

func (r *Repository) Begin(requestID string, token Token, nowNS int64) (Result, error) {
	if !validToken(token) || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	return r.apply(requestID, token.ID, struct {
		Operation string
		Token     Token
	}{"begin", token}, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		if !matches(current, token) || current.State != "running" || current.Execution != "claimed" || current.CancelRequested || current.PauseRequested {
			return nil, nil, ErrStale
		}
		next := *current
		next.Execution = "started"
		next.UpdatedNS = effectiveTime(nowNS, current)
		attempt, err := readAttempt(v, token)
		if err != nil {
			return nil, nil, err
		}
		attempt.State = "started"
		attempt.StartedNS = next.UpdatedNS
		raw, _ := json.Marshal(attempt)
		return &next, []dataengine.Mutation{{Key: attemptKey(token), Value: raw}}, nil
	})
}

func decision(task Task) string {
	if task.CancelRequested {
		return "cancel"
	}
	if task.PauseRequested {
		return "pause"
	}
	return "continue"
}

// Progress is deduplicated by owner/fence/sequence, without a receipt for every
// poll. It always returns current control flags, even for a repeated sequence.
func (r *Repository) Progress(token Token, point Point) (Checkpoint, error) {
	if !validToken(token) || point.Sequence == 0 || point.NowNS <= 0 || math.IsNaN(point.Percent) || math.IsInf(point.Percent, 0) || point.Percent < 0 || point.Percent > 100 || !namePattern.MatchString(point.Phase) || len(point.Message) > 4000 {
		return Checkpoint{}, dataengine.ErrInvalidRequest
	}
	content, _ := json.Marshal(struct {
		Phase, Message string
		Percent        float64
	}{point.Phase, point.Message, point.Percent})
	hash := digest(content)
	for retry := 0; retry < 256; retry++ {
		var result Checkpoint
		var conditions []dataengine.Condition
		var mutations []dataengine.Mutation
		err := r.engine.View(func(v *dataengine.View) error {
			current, raw, err := readTask(v, token.ID)
			if err != nil {
				return err
			}
			if !matches(current, token) || current.Execution != "started" || (current.State != "running" && current.State != "paused") {
				return ErrStale
			}
			if point.Sequence == current.ProgressSequence && hash != current.ProgressHash {
				return ErrRequestConflict
			}
			next := *current
			if point.Sequence > current.ProgressSequence {
				next.ProgressSequence = point.Sequence
				next.ProgressHash = hash
				next.Progress = point.Percent
				next.Phase = point.Phase
				next.Message = point.Message
			}
			if next.PauseRequested && next.State == "running" {
				next.State = "paused"
			}
			result = Checkpoint{Task: next, Decision: decision(next)}
			if next == *current {
				return nil
			}
			if current.Revision == math.MaxUint64 {
				return ErrIntegrity
			}
			next.Revision++
			next.UpdatedNS = effectiveTime(point.NowNS, current)
			conditions, mutations, err = stateMutations(v, current, &next)
			if err != nil {
				return err
			}
			conditions = append(conditions, condition(taskKey(current.ID), raw))
			result.Task = next
			return nil
		})
		if err != nil {
			return Checkpoint{}, err
		}
		if len(mutations) == 0 {
			return result, nil
		}
		applied, err := r.engine.CompareAndSwap(conditions, mutations)
		if err != nil {
			return Checkpoint{}, err
		}
		if applied {
			return result, nil
		}
	}
	return Checkpoint{}, ErrStale
}

func (r *Repository) Finish(requestID string, token Token, end Completion, followups []NewTask) (Result, error) {
	if !validToken(token) || end.NowNS <= 0 || len(end.Message) > 4000 || len(end.ErrorClass) > 80 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	switch end.State {
	case "completed", "failed", "cancelled", "blocked", "recovery_required":
	default:
		return Result{}, dataengine.ErrInvalidRequest
	}
	hash := ""
	var compressed []byte
	var err error
	if end.Payload != nil {
		hash, compressed, err = pack(end.Payload)
		if err != nil {
			return Result{}, err
		}
	}
	if len(followups) > 1000 || len(followups) > 0 && end.State != "completed" {
		return Result{}, dataengine.ErrInvalidRequest
	}
	total := len(end.Payload)
	seen := map[string]bool{token.ID: true}
	prepared := make([]preparedTask, 0, len(followups))
	for _, child := range followups {
		if !validSpec(child.Spec) || seen[child.Spec.ID] || (child.Spec.ParentID != "" && child.Spec.ParentID != token.ID) {
			return Result{}, dataengine.ErrInvalidRequest
		}
		total += len(child.Payload)
		if total > maxPayload {
			return Result{}, dataengine.ErrInvalidRequest
		}
		seen[child.Spec.ID] = true
		child.Spec.ParentID = token.ID
		childHash, childBlob, err := pack(child.Payload)
		if err != nil {
			return Result{}, err
		}
		prepared = append(prepared, preparedTask{Task: Task{Spec: child.Spec, State: "queued", UpdatedNS: child.Spec.CreatedNS, Revision: 1, PayloadHash: childHash}, Compressed: childBlob})
	}
	childIntents := make([]struct {
		Spec Spec
		Hash string
	}, 0, len(prepared))
	for _, child := range prepared {
		childIntents = append(childIntents, struct {
			Spec Spec
			Hash string
		}{child.Task.Spec, child.Task.PayloadHash})
	}
	intent := struct {
		Operation                        string
		Token                            Token
		State, Hash, ErrorClass, Message string
		Children                         any
	}{"finish", token, end.State, hash, end.ErrorClass, end.Message, childIntents}

	return r.apply(requestID, token.ID, intent, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		if !matches(current, token) || current.Execution != "started" || (current.State != "running" && current.State != "paused") {
			return nil, nil, ErrStale
		}
		next := *current
		next.State = end.State
		next.Execution = "finished"
		next.PauseRequested = false
		next.ResultHash = hash
		next.ErrorClass = end.ErrorClass
		next.Message = end.Message
		next.UpdatedNS = effectiveTime(end.NowNS, current)
		if end.State == "completed" {
			next.Progress = 100
		}
		if end.State == "cancelled" {
			next.CancelOutcome = "cancelled_at_checkpoint"
		} else if next.CancelRequested && end.State == "completed" {
			next.CancelOutcome = "completed_with_cancel_request"
		}
		attempt, err := readAttempt(v, token)
		if err != nil {
			return nil, nil, err
		}
		attempt.State = end.State
		attempt.FinishedNS = next.UpdatedNS
		attempt.ResultHash = hash
		attempt.ErrorClass = end.ErrorClass
		raw, _ := json.Marshal(attempt)
		extra := []dataengine.Mutation{{Key: attemptKey(token), Value: raw}}
		if hash != "" {
			writes, err := blobWrites(v, hash, compressed)
			if err != nil {
				return nil, nil, err
			}
			extra = append(extra, writes...)
		}
		return &next, extra, nil
	}, prepared...)
}

func (r *Repository) Control(requestID, id, action string, nowNS int64) (Result, error) {
	if !validID(id) || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	switch action {
	case "pause", "resume", "cancel", "retry":
	default:
		return Result{}, dataengine.ErrInvalidRequest
	}
	intent := struct{ Operation, ID, Action string }{"control", id, action}
	return r.apply(requestID, id, intent, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		if current == nil {
			return nil, nil, ErrNotFound
		}
		next := *current
		var extra []dataengine.Mutation
		switch action {
		case "pause":
			if current.State == "paused" || current.PauseRequested {
				return &next, nil, nil
			}
			if current.State != "queued" && current.State != "running" {
				return nil, nil, ErrStale
			}
			next.PauseRequested = true
			if current.State == "queued" || current.Execution == "claimed" {
				next.State = "paused"
			}
		case "resume":
			if (current.State == "running" || current.State == "queued") && !current.PauseRequested {
				return &next, nil, nil
			}
			if current.State != "paused" && current.State != "running" {
				return nil, nil, ErrStale
			}
			next.PauseRequested = false
			if current.Owner == "" {
				next.State = "queued"
			} else {
				next.State = "running"
			}
		case "cancel":
			if current.State == "cancelled" || current.CancelRequested {
				return &next, nil, nil
			}
			if terminal(current.State) {
				return nil, nil, ErrStale
			}
			next.CancelRequested = true
			if current.Owner == "" || current.Execution == "claimed" {
				next.State = "cancelled"
				next.Execution = "finished"
				next.PauseRequested = false
				next.CancelOutcome = "cancelled_before_start"
				if current.Owner != "" {
					token := Token{ID: id, Owner: current.Owner, Fence: current.Fence}
					attempt, err := readAttempt(v, token)
					if err != nil {
						return nil, nil, err
					}
					attempt.State = "cancelled"
					attempt.FinishedNS = effectiveTime(nowNS, current)
					raw, _ := json.Marshal(attempt)
					extra = append(extra, dataengine.Mutation{Key: attemptKey(token), Value: raw})
				}
			}
		case "retry":
			if current.State != "failed" && current.State != "cancelled" && current.State != "blocked" {
				return nil, nil, ErrStale
			}
			next.State = "queued"
			next.Owner = ""
			next.Execution = ""
			next.PauseRequested = false
			next.CancelRequested = false
			next.CancelOutcome = ""
			next.Progress = 0
			next.ProgressSequence = 0
			next.ProgressHash = ""
			next.Phase = ""
			next.Message = ""
			next.ErrorClass = ""
			next.ResultHash = ""
		}
		if next != *current {
			next.UpdatedNS = effectiveTime(nowNS, current)
		}
		return &next, extra, nil
	})
}

// Interrupt is called after an owned executor is confirmed stopped, never just
// because a timer elapsed. Reconciliation must later prove external effects
// before recovery_required can be requeued; ordinary Retry deliberately refuses.
func (r *Repository) Interrupt(requestID string, token Token, nowNS int64, reason string) (Result, error) {
	if !validToken(token) || nowNS <= 0 || len(reason) > 4000 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	return r.apply(requestID, token.ID, struct {
		Operation string
		Token     Token
		Reason    string
	}{"interrupt", token, reason}, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		if !matches(current, token) || (current.State != "running" && current.State != "paused") {
			return nil, nil, ErrStale
		}
		if current.Fence == math.MaxUint64 {
			return nil, nil, ErrIntegrity
		}
		next := *current
		next.State = "recovery_required"
		next.Execution = "interrupted"
		next.Owner = ""
		next.Fence++
		next.PauseRequested = false
		next.UpdatedNS = effectiveTime(nowNS, current)
		next.ErrorClass = "RECOVERY_REQUIRED"
		next.Message = reason
		attempt, err := readAttempt(v, token)
		if err != nil {
			return nil, nil, err
		}
		attempt.State = "recovery_required"
		attempt.FinishedNS = next.UpdatedNS
		raw, _ := json.Marshal(attempt)
		return &next, []dataengine.Mutation{{Key: attemptKey(token), Value: raw}}, nil
	})
}

// ReleaseClaim yields a ticket that never reached Begin. No external action may
// start before Begin, so this transition is safe without effect reconciliation.
func (r *Repository) ReleaseClaim(requestID string, token Token, nowNS int64) (Result, error) {
	if !validToken(token) || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	return r.apply(requestID, token.ID, struct {
		Operation string
		Token     Token
	}{"release_claim", token}, func(v *dataengine.View, current *Task) (*Task, []dataengine.Mutation, error) {
		if !matches(current, token) || current.Execution != "claimed" || (current.State != "running" && current.State != "paused") || current.CancelRequested {
			return nil, nil, ErrStale
		}
		if current.Fence == math.MaxUint64 {
			return nil, nil, ErrIntegrity
		}
		next := *current
		next.Owner = ""
		next.Fence++
		next.Execution = ""
		next.State = "queued"
		if next.PauseRequested {
			next.State = "paused"
		}
		next.UpdatedNS = effectiveTime(nowNS, current)
		attempt, err := readAttempt(v, token)
		if err != nil {
			return nil, nil, err
		}
		attempt.State = "released_unstarted"
		attempt.FinishedNS = next.UpdatedNS
		raw, _ := json.Marshal(attempt)
		return &next, []dataengine.Mutation{{Key: attemptKey(token), Value: raw}}, nil
	})
}
