// Package taskrepo owns durable task state, indexes and command receipts.
package taskrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

var (
	ErrStale           = errors.New("task ownership or state changed")
	ErrRequestConflict = errors.New("request ID reused with different intent")
	ErrNotFound        = errors.New("task not found")
	ErrIntegrity       = errors.New("task storage integrity failed")
	// ErrWorkflowOwned means this task belongs to a workflow job. Generic
	// completion and control must not write it without that job.
	ErrWorkflowOwned = errors.New("workflow task commits only with its job")
)

const maxPayload = 64 << 20

// PayloadLimit is the maximum raw argument, result or evidence body.
const PayloadLimit = maxPayload

var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,79}$`)
var lanes = []string{"identify", "publish", "transcode", "aux"}
var states = []string{"queued", "running", "paused", "completed", "failed", "cancelled", "blocked", "recovery_required"}

type Spec struct {
	ID        string `json:"task_id"`
	Command   string `json:"command"`
	Lane      string `json:"lane"`
	Priority  int    `json:"priority"`
	CreatedNS int64  `json:"created_ns"`
	ParentID  string `json:"parent_id,omitempty"`
}
type Task struct {
	Spec
	State            string  `json:"state"`
	UpdatedNS        int64   `json:"updated_ns"`
	Revision         uint64  `json:"revision"`
	Owner            string  `json:"owner,omitempty"`
	Fence            uint64  `json:"fence"`
	Execution        string  `json:"execution,omitempty"`
	PauseRequested   bool    `json:"pause_requested,omitempty"`
	CancelRequested  bool    `json:"cancel_requested,omitempty"`
	CancelOutcome    string  `json:"cancel_outcome,omitempty"`
	PayloadHash      string  `json:"payload_hash"`
	ResultHash       string  `json:"result_hash,omitempty"`
	ProgressSequence uint64  `json:"progress_sequence"`
	ProgressHash     string  `json:"progress_hash,omitempty"`
	Progress         float64 `json:"progress"`
	Phase            string  `json:"phase,omitempty"`
	Message          string  `json:"message,omitempty"`
	ErrorClass       string  `json:"error_class,omitempty"`
	FollowupCount    int     `json:"followup_count,omitempty"`
	WorkflowJobID    string  `json:"workflow_job_id,omitempty"`
}
type Token struct {
	ID    string `json:"task_id"`
	Owner string `json:"owner"`
	Fence uint64 `json:"fence"`
}
type Attempt struct {
	Token
	State      string `json:"state"`
	ClaimedNS  int64  `json:"claimed_ns"`
	StartedNS  int64  `json:"started_ns,omitempty"`
	FinishedNS int64  `json:"finished_ns,omitempty"`
	ResultHash string `json:"result_hash,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
}
type Result struct {
	Task      *Task           `json:"task,omitempty"`
	Replayed  bool            `json:"replayed"`
	Changed   bool            `json:"changed"`
	Followups []Task          `json:"followups,omitempty"`
	Domain    json.RawMessage `json:"domain,omitempty"`
}
type Receipt struct {
	Intent string `json:"intent"`
	Code   string `json:"code,omitempty"`
	Result Result `json:"result"`
}
type RequestStatus struct {
	State  string  `json:"state"`
	Code   string  `json:"code,omitempty"`
	Result *Result `json:"result,omitempty"`
}
type Page struct {
	Tasks []Task `json:"tasks"`
	Next  []byte `json:"next"`
}
type Counts struct {
	States map[string]uint64            `json:"states"`
	Lanes  map[string]map[string]uint64 `json:"lanes"`
}
type Point struct {
	Sequence uint64  `json:"sequence"`
	Percent  float64 `json:"percent"`
	Phase    string  `json:"phase"`
	Message  string  `json:"message,omitempty"`
	NowNS    int64   `json:"now_ns"`
}
type Checkpoint struct {
	Task     Task   `json:"task"`
	Decision string `json:"decision"`
}
type Completion struct {
	State      string `json:"state"`
	NowNS      int64  `json:"now_ns"`
	ErrorClass string `json:"error_class,omitempty"`
	Message    string `json:"message,omitempty"`
	Payload    []byte `json:"payload,omitempty"`
}

type NewTask struct {
	Spec    Spec   `json:"spec"`
	Payload []byte `json:"payload"`
}
type preparedTask struct {
	Task       Task
	Compressed []byte
}

func digest(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
func validID(id string) bool     { return id != "" && len(id) <= 256 }
func validLane(lane string) bool {
	for _, value := range lanes {
		if lane == value {
			return true
		}
	}
	return false
}
func validState(state string) bool {
	for _, value := range states {
		if state == value {
			return true
		}
	}
	return false
}
func validSpec(s Spec) bool {
	return validID(s.ID) && namePattern.MatchString(s.Command) && validLane(s.Lane) && s.Priority >= 0 && s.Priority <= 9999 && s.CreatedNS > 0 && (s.ParentID == "" || validID(s.ParentID))
}
func validToken(t Token) bool {
	return validID(t.ID) && t.Owner != "" && len(t.Owner) <= 128 && t.Fence > 0
}
func taskKey(id string) []byte      { return []byte("task/" + digest([]byte(id))) }
func operationKey(id string) []byte { return []byte("task-operation/" + digest([]byte(id))) }
func blobKey(hash string) []byte    { return []byte("task-blob/" + hash) }
func childKey(parent, id string) []byte {
	return []byte("task-child/" + digest([]byte(parent)) + "/" + digest([]byte(id)))
}
func attemptKey(t Token) []byte {
	return []byte(fmt.Sprintf("task-attempt/%s/%020d", digest([]byte(t.ID)), t.Fence))
}
func indexKey(t Task) []byte {
	return []byte(fmt.Sprintf("task-index/%s/%s/%04d/%020d/%s", t.Lane, t.State, t.Priority, t.CreatedNS, digest([]byte(t.ID))))
}
func recentKey(t Task) []byte {
	return []byte(fmt.Sprintf("task-recent/%020d/%s", int64(^uint64(0)>>1)-t.UpdatedNS, digest([]byte(t.ID))))
}
func countKey(lane, state string) []byte { return []byte("task-count/" + lane + "/" + state) }
func cloneTask(task *Task) *Task {
	if task == nil {
		return nil
	}
	copied := *task
	return &copied
}
func matches(t *Task, token Token) bool {
	return t != nil && t.Owner == token.Owner && t.Fence == token.Fence
}
func effectiveTime(now int64, current *Task) int64 {
	if current != nil && now < current.UpdatedNS {
		return current.UpdatedNS
	}
	return now
}
func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled" || state == "blocked" || state == "recovery_required"
}
