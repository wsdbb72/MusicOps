package taskrepo

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

type Repository struct {
	engine  *dataengine.Engine
	wake    sync.Mutex
	changed chan struct{}
}

func New(engine *dataengine.Engine) *Repository {
	return &Repository{engine: engine, changed: make(chan struct{})}
}

// Changes is captured before a read to avoid a lost wakeup between observing an
// empty/paused queue and waiting. Notifications carry no private payloads.
func (r *Repository) Changes() <-chan struct{} {
	r.wake.Lock()
	defer r.wake.Unlock()
	return r.changed
}
func (r *Repository) notify() {
	r.wake.Lock()
	close(r.changed)
	r.changed = make(chan struct{})
	r.wake.Unlock()
}

func readTask(v *dataengine.View, id string) (*Task, []byte, error) {
	raw, found, err := v.Get(taskKey(id))
	if err != nil || !found {
		return nil, raw, err
	}
	var task Task
	if json.Unmarshal(raw, &task) != nil || task.ID != id || !validSpec(task.Spec) || !validState(task.State) || task.Revision == 0 || task.UpdatedNS < task.CreatedNS || len(task.PayloadHash) != 64 {
		return nil, nil, ErrIntegrity
	}
	return &task, raw, nil
}
func readAttempt(v *dataengine.View, token Token) (Attempt, error) {
	var attempt Attempt
	raw, found, err := v.Get(attemptKey(token))
	if err != nil {
		return attempt, err
	}
	if !found || json.Unmarshal(raw, &attempt) != nil || attempt.ID != token.ID || attempt.Fence != token.Fence {
		return attempt, ErrIntegrity
	}
	return attempt, nil
}
func condition(key, raw []byte) dataengine.Condition {
	return dataengine.Condition{Key: key, Value: raw, Absent: raw == nil}
}
func errorCode(err error) string {
	if errors.Is(err, ErrStale) {
		return "TASK_CHANGED"
	}
	if errors.Is(err, ErrNotFound) {
		return "TASK_NOT_FOUND"
	}
	return ""
}
func receiptError(code string) error {
	switch code {
	case "TASK_CHANGED":
		return ErrStale
	case "TASK_NOT_FOUND":
		return ErrNotFound
	}
	return nil
}
func readReceipt(v *dataengine.View, requestID, intent string) (*Receipt, error) {
	raw, found, err := v.Get(operationKey(requestID))
	if err != nil || !found {
		return nil, err
	}
	var receipt Receipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.Intent == "" || (receipt.Code != "" && receiptError(receipt.Code) == nil) {
		return nil, ErrIntegrity
	}
	if intent != "" && receipt.Intent != intent {
		return nil, ErrRequestConflict
	}
	receipt.Result.Replayed = true
	return &receipt, nil
}

// stateMutations owns every index and state counter. Callers cannot forget to
// update a summary or repair it later outside the business transaction.
type taskChange struct{ Old, Next *Task }

func stateMutations(v *dataengine.View, old, next *Task) ([]dataengine.Condition, []dataengine.Mutation, error) {
	return stateBatch(v, []taskChange{{Old: old, Next: next}})
}
func stateBatch(v *dataengine.View, transitions []taskChange) ([]dataengine.Condition, []dataengine.Mutation, error) {
	var writes []dataengine.Mutation
	deltas := map[string]int{}
	minimum := map[string]uint64{}
	for _, transition := range transitions {
		old, next := transition.Old, transition.Next
		raw, err := json.Marshal(next)
		if err != nil {
			return nil, nil, err
		}
		writes = append(writes, dataengine.Mutation{Key: taskKey(next.ID), Value: raw}, dataengine.Mutation{Key: indexKey(*next), Value: raw}, dataengine.Mutation{Key: recentKey(*next), Value: raw})
		if next.ParentID != "" {
			writes = append(writes, dataengine.Mutation{Key: childKey(next.ParentID, next.ID), Value: raw})
		}
		if old != nil {
			if !bytes.Equal(indexKey(*old), indexKey(*next)) {
				writes = append(writes, dataengine.Mutation{Key: indexKey(*old), Delete: true})
			}
			if !bytes.Equal(recentKey(*old), recentKey(*next)) {
				writes = append(writes, dataengine.Mutation{Key: recentKey(*old), Delete: true})
			}
		}
		if old == nil || old.Lane != next.Lane || old.State != next.State {
			if old != nil {
				key := string(countKey(old.Lane, old.State))
				deltas[key]--
				minimum[key]++
			}
			deltas[string(countKey(next.Lane, next.State))]++
		}
	}
	var conditions []dataengine.Condition
	for key, delta := range deltas {
		current, found, err := v.Get([]byte(key))
		if err != nil {
			return nil, nil, err
		}
		var count uint64
		if found {
			if len(current) != 8 {
				return nil, nil, ErrIntegrity
			}
			count = binary.BigEndian.Uint64(current)
		}
		if count < minimum[key] {
			return nil, nil, ErrIntegrity
		}
		if delta < 0 {
			count -= uint64(-delta)
		} else {
			if count > math.MaxUint64-uint64(delta) {
				return nil, nil, ErrIntegrity
			}
			count += uint64(delta)
		}
		value := make([]byte, 8)
		binary.BigEndian.PutUint64(value, count)
		conditions = append(conditions, condition([]byte(key), current))
		writes = append(writes, dataengine.Mutation{Key: []byte(key), Value: value})
	}
	return conditions, writes, nil
}

type change func(*dataengine.View, *Task) (*Task, []dataengine.Mutation, error)

// apply persists the business response together with its effects. Replaying a
// command returns that original response even after progress/control changes.
func (r *Repository) apply(requestID, id string, intent any, decide change, followers ...preparedTask) (Result, error) {
	if requestID == "" || len(requestID) > 128 || !validID(id) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	body, err := json.Marshal(intent)
	if err != nil {
		return Result{}, dataengine.ErrInvalidRequest
	}
	signature := digest(body)
	for retry := 0; retry < 256; retry++ {
		var replay *Receipt
		var receipt Receipt
		var conditions []dataengine.Condition
		var mutations []dataengine.Mutation
		err = r.engine.View(func(v *dataengine.View) error {
			var err error
			replay, err = readReceipt(v, requestID, signature)
			if err != nil || replay != nil {
				return err
			}
			old, raw, err := readTask(v, id)
			if err != nil {
				return err
			}
			next, extra, rejected := decide(v, old)
			var children []Task
			var childConditions []dataengine.Condition
			if rejected == nil && (old == nil || !old.CancelRequested) {
				for _, prepared := range followers {
					child, _, err := readTask(v, prepared.Task.ID)
					if err != nil {
						return err
					}
					if child != nil {
						rejected = ErrStale
						break
					}
					nextChild := prepared.Task
					if old != nil && old.PauseRequested {
						nextChild.State = "paused"
						nextChild.PauseRequested = true
					}
					if next != nil {
						nextChild.UpdatedNS = effectiveTime(nextChild.CreatedNS, next)
					}
					children = append(children, nextChild)
					childConditions = append(childConditions, dataengine.Condition{Key: taskKey(nextChild.ID), Absent: true})
					blobs, err := blobWrites(v, nextChild.PayloadHash, prepared.Compressed)
					if err != nil {
						return err
					}
					extra = append(extra, blobs...)
				}
			}
			if next != nil && len(followers) > 0 {
				next.FollowupCount = len(children)
			}
			receipt = Receipt{Intent: signature}
			conditions = []dataengine.Condition{{Key: operationKey(requestID), Absent: true}, condition(taskKey(id), raw)}
			if rejected != nil {
				receipt.Code = errorCode(rejected)
				if receipt.Code == "" {
					return rejected
				}
				receipt.Result.Task = old
			} else {
				if next == nil {
					return ErrIntegrity
				}
				changed := old == nil || *next != *old
				if changed {
					if old == nil {
						next.Revision = 1
					} else {
						if old.Revision == math.MaxUint64 {
							return ErrIntegrity
						}
						next.Revision = old.Revision + 1
					}
					if !validSpec(next.Spec) || !validState(next.State) || next.UpdatedNS < next.CreatedNS || len(next.PayloadHash) != 64 {
						return ErrIntegrity
					}
					transitions := []taskChange{{Old: old, Next: next}}
					for i := range children {
						transitions = append(transitions, taskChange{Next: &children[i]})
					}
					counters, writes, err := stateBatch(v, transitions)
					if err != nil {
						return err
					}
					conditions = append(conditions, counters...)
					conditions = append(conditions, childConditions...)
					mutations = append(mutations, writes...)
					mutations = append(mutations, extra...)
				}
				receipt.Result = Result{Task: next, Changed: changed, Followups: children}
			}
			value, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			mutations = append(mutations, dataengine.Mutation{Key: operationKey(requestID), Value: value})
			return nil
		})
		if err != nil {
			return Result{}, err
		}
		if replay != nil {
			return replay.Result, receiptError(replay.Code)
		}
		committed, err := r.engine.CompareAndSwap(conditions, mutations)
		if err != nil {
			return Result{}, err
		}
		if committed {
			if receipt.Result.Changed {
				r.notify()
			}
			return receipt.Result, receiptError(receipt.Code)
		}
	}
	return Result{}, ErrStale
}

func (r *Repository) Get(id string) (*Task, error) {
	if !validID(id) {
		return nil, dataengine.ErrInvalidRequest
	}
	var task *Task
	err := r.engine.View(func(v *dataengine.View) error { var err error; task, _, err = readTask(v, id); return err })
	return task, err
}
func (r *Repository) Request(requestID string) (RequestStatus, error) {
	if requestID == "" || len(requestID) > 128 {
		return RequestStatus{}, dataengine.ErrInvalidRequest
	}
	result := RequestStatus{State: "UNKNOWN"}
	err := r.engine.View(func(v *dataengine.View) error {
		receipt, err := readReceipt(v, requestID, "")
		if err != nil || receipt == nil {
			return err
		}
		result = RequestStatus{State: "COMMITTED", Code: receipt.Code, Result: &receipt.Result}
		if receipt.Code != "" {
			result.State = "REJECTED"
		}
		return nil
	})
	return result, err
}
func (r *Repository) Attempt(id string, fence uint64) (*Attempt, error) {
	if !validID(id) || fence == 0 {
		return nil, dataengine.ErrInvalidRequest
	}
	var result *Attempt
	err := r.engine.View(func(v *dataengine.View) error {
		raw, found, err := v.Get(attemptKey(Token{ID: id, Fence: fence}))
		if err != nil || !found {
			return err
		}
		var attempt Attempt
		if json.Unmarshal(raw, &attempt) != nil || attempt.ID != id || attempt.Fence != fence {
			return ErrIntegrity
		}
		result = &attempt
		return nil
	})
	return result, err
}
func (r *Repository) Counts() (Counts, error) {
	result := Counts{States: map[string]uint64{}, Lanes: map[string]map[string]uint64{}}
	err := r.engine.View(func(v *dataengine.View) error {
		for _, lane := range lanes {
			result.Lanes[lane] = map[string]uint64{}
			for _, state := range states {
				raw, found, err := v.Get(countKey(lane, state))
				if err != nil {
					return err
				}
				var count uint64
				if found {
					if len(raw) != 8 {
						return ErrIntegrity
					}
					count = binary.BigEndian.Uint64(raw)
				}
				result.Lanes[lane][state] = count
				result.States[state] += count
			}
		}
		return nil
	})
	return result, err
}
func (r *Repository) page(prefix, after []byte, limit int) (Page, error) {
	result := Page{Tasks: make([]Task, 0)}
	err := r.engine.View(func(v *dataengine.View) error {
		page, err := v.Scan(prefix, after, limit)
		if err != nil {
			return err
		}
		result.Next = page.Next
		for _, item := range page.Items {
			var task Task
			if json.Unmarshal(item.Value, &task) != nil || !validSpec(task.Spec) {
				return ErrIntegrity
			}
			result.Tasks = append(result.Tasks, task)
		}
		return nil
	})
	return result, err
}
func (r *Repository) List(lane, state string, after []byte, limit int) (Page, error) {
	if !validLane(lane) || !validState(state) {
		return Page{}, dataengine.ErrInvalidRequest
	}
	return r.page([]byte("task-index/"+lane+"/"+state+"/"), after, limit)
}
func (r *Repository) Recent(after []byte, limit int) (Page, error) {
	return r.page([]byte("task-recent/"), after, limit)
}

func (r *Repository) Children(parent string, after []byte, limit int) (Page, error) {
	if !validID(parent) {
		return Page{}, dataengine.ErrInvalidRequest
	}
	return r.page([]byte("task-child/"+digest([]byte(parent))+"/"), after, limit)
}

func pack(payload []byte) (string, []byte, error) {
	if len(payload) > maxPayload {
		return "", nil, dataengine.ErrInvalidRequest
	}
	var output bytes.Buffer
	writer, _ := zlib.NewWriterLevel(&output, zlib.BestSpeed)
	if _, err := writer.Write(payload); err != nil {
		return "", nil, err
	}
	if err := writer.Close(); err != nil {
		return "", nil, err
	}
	return digest(payload), output.Bytes(), nil
}
func blobWrites(v *dataengine.View, hash string, compressed []byte) ([]dataengine.Mutation, error) {
	marker := []byte("task-blob-info/" + hash)
	_, exists, err := v.Get(marker)
	if err != nil || exists {
		return nil, err
	}
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(compressed)))
	return []dataengine.Mutation{{Key: blobKey(hash), Value: compressed}, {Key: marker, Value: size}}, nil
}
func unpack(hash string, compressed []byte) ([]byte, error) {
	input := bytes.NewReader(compressed)
	reader, err := zlib.NewReader(input)
	if err != nil {
		return nil, ErrIntegrity
	}
	defer reader.Close()
	payload, err := io.ReadAll(io.LimitReader(reader, maxPayload+1))
	if err != nil || len(payload) > maxPayload || input.Len() != 0 || digest(payload) != hash {
		return nil, ErrIntegrity
	}
	return payload, nil
}
func (r *Repository) Data(id, part string, fence uint64) ([]byte, bool, error) {
	if !validID(id) || (part != "arguments" && part != "result") || (fence != 0 && part != "result") {
		return nil, false, dataengine.ErrInvalidRequest
	}
	var compressed []byte
	var hash string
	var found bool
	err := r.engine.View(func(v *dataengine.View) error {
		task, _, err := readTask(v, id)
		if err != nil || task == nil {
			return err
		}
		if part == "arguments" {
			hash = task.PayloadHash
		} else {
			hash = task.ResultHash
		}
		if fence != 0 {
			attempt, err := readAttempt(v, Token{ID: id, Fence: fence})
			if err != nil {
				return err
			}
			hash = attempt.ResultHash
		}
		if hash == "" {
			return nil
		}
		compressed, found, err = v.Get(blobKey(hash))
		if err != nil {
			return err
		}
		if !found {
			return ErrIntegrity
		}
		return nil
	})
	if err != nil || !found {
		return nil, false, err
	}
	payload, err := unpack(hash, compressed)
	return payload, err == nil, err
}
