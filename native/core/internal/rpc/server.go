// Package rpc exposes only an authenticated local data protocol, never SQL.
package rpc

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"sync/atomic"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/mediatx"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const maxBody = 96 << 20

var checkpointIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,96}$`)

type Handler struct {
	Engine          *dataengine.Engine
	Token           string
	BackupDirectory string
	slots           chan struct{}
	bodyBytes       atomic.Int64
	Tasks           *taskrepo.Repository
	Workflows       *workflow.Repository
	Media           *mediatx.Store
}

type Request struct {
	Operation string          `json:"operation"`
	RequestID string          `json:"request_id,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
}

func decodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return dataengine.ErrInvalidRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return dataengine.ErrInvalidRequest
	}
	return nil
}

func New(engine *dataengine.Engine, token, backups string) (*Handler, error) {
	if engine == nil || len(token) < 32 || backups == "" {
		return nil, errors.New("private local data service needs an engine, authentication and backup directory")
	}
	tasks := taskrepo.New(engine)
	roots, err := mediatx.OpenRoots(filepath.Join(filepath.Dir(backups), "media-fixture"))
	if err != nil {
		return nil, err
	}
	media, err := mediatx.New(engine, roots)
	if err != nil {
		return nil, err
	}
	return &Handler{Engine: engine, Token: token, BackupDirectory: backups, slots: make(chan struct{}, 16), Tasks: tasks, Workflows: workflow.New(engine, tasks), Media: media}, nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.Token)) != 1 {
		reply(w, 401, map[string]any{"ok": false, "code": "AUTH_REQUIRED"})
		return
	}
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		reply(w, 200, map[string]any{"ok": true, "engine": "pebble", "data_schema": dataengine.SchemaVersion})
		return
	}
	if r.URL.Path != "/rpc" || r.Method != http.MethodPost {
		reply(w, 404, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if r.ContentLength > maxBody {
		reply(w, 413, map[string]any{"ok": false, "code": "REQUEST_TOO_LARGE"})
		return
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		reply(w, 415, map[string]any{"ok": false, "code": "JSON_REQUIRED"})
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		reply(w, 429, map[string]any{"ok": false, "code": "BUSY"})
		return
	}
	// Bound aggregate input memory as well as request count. Sixteen concurrent
	// 96 MiB JSON bodies would otherwise consume gigabytes before batch checks.
	reservation := r.ContentLength
	if reservation < 0 {
		reservation = maxBody
	}
	if reservation < 64<<10 {
		reservation = 64 << 10
	}
	for {
		used := h.bodyBytes.Load()
		if used+reservation > 128<<20 {
			reply(w, 429, map[string]any{"ok": false, "code": "BUSY"})
			return
		}
		if h.bodyBytes.CompareAndSwap(used, used+reservation) {
			break
		}
	}
	defer h.bodyBytes.Add(-reservation)
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			reply(w, 413, map[string]any{"ok": false, "code": "REQUEST_TOO_LARGE"})
		} else {
			reply(w, 400, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		}
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		reply(w, 400, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	result, err := h.dispatch(request)

	if err != nil {
		status, code := 500, "DATA_OPERATION_FAILED"
		if errors.Is(err, taskrepo.ErrRequestConflict) {
			status, code = 409, "REQUEST_ID_CONFLICT"
		}
		if errors.Is(err, dataengine.ErrClosed) {
			status, code = 503, "ENGINE_CLOSED"
		}
		if errors.Is(err, dataengine.ErrInvalidRequest) {
			status, code = 400, "INVALID_REQUEST"
		}
		if errors.Is(err, taskrepo.ErrNotFound) {
			status, code = 404, "TASK_NOT_FOUND"
		}
		if errors.Is(err, taskrepo.ErrStale) {
			status, code = 409, "TASK_CHANGED"
		}
		if errors.Is(err, taskrepo.ErrWorkflowOwned) {
			status, code = 409, "WORKFLOW_OWNED"
		}
		reply(w, status, map[string]any{"ok": false, "code": code})
		return
	}
	reply(w, 200, map[string]any{"ok": true, "result": result})
}

// All data mutations go through typed repositories. No raw KV write or SQL
// compatibility endpoint is exposed to the application transport.
func (h *Handler) dispatch(request Request) (any, error) {
	tasks := h.Tasks
	switch request.Operation {
	case "task_enqueue":
		var p struct {
			Spec    taskrepo.Spec `json:"spec"`
			Payload []byte        `json:"payload"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Enqueue(request.RequestID, p.Spec, p.Payload)
	case "task_get":
		var p struct {
			ID string `json:"task_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		task, err := tasks.Get(p.ID)
		return map[string]any{"found": task != nil, "task": task}, err
	case "task_data":
		var p struct {
			ID    string `json:"task_id"`
			Part  string `json:"part"`
			Fence uint64 `json:"fence,omitempty"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		data, found, err := tasks.Data(p.ID, p.Part, p.Fence)
		return map[string]any{"found": found, "data": data}, err
	case "task_attempt":
		var p struct {
			ID    string `json:"task_id"`
			Fence uint64 `json:"fence"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Attempt(p.ID, p.Fence)
	case "task_list":
		var p struct {
			Lane  string `json:"lane"`
			State string `json:"state"`
			After []byte `json:"after,omitempty"`
			Limit int    `json:"limit,omitempty"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		if p.Limit == 0 {
			p.Limit = 200
		}
		return tasks.List(p.Lane, p.State, p.After, p.Limit)
	case "task_recent":
		var p struct {
			After []byte `json:"after,omitempty"`
			Limit int    `json:"limit,omitempty"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		if p.Limit == 0 {
			p.Limit = 200
		}
		return tasks.Recent(p.After, p.Limit)
	case "task_counts":
		var p struct{}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Counts()
	case "task_claim":
		var p struct {
			Lane  string `json:"lane"`
			Owner string `json:"owner"`
			NowNS int64  `json:"now_ns"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Claim(request.RequestID, p.Lane, p.Owner, p.NowNS)
	case "task_begin":
		var p struct {
			Token taskrepo.Token `json:"token"`
			NowNS int64          `json:"now_ns"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Begin(request.RequestID, p.Token, p.NowNS)
	case "task_progress":
		var p struct {
			Token taskrepo.Token `json:"token"`
			Point taskrepo.Point `json:"point"`
		}
		if request.RequestID != "" {
			return nil, dataengine.ErrInvalidRequest
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Progress(p.Token, p.Point)
	case "task_control":
		var p struct {
			ID     string `json:"task_id"`
			Action string `json:"action"`
			NowNS  int64  `json:"now_ns"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Control(request.RequestID, p.ID, p.Action, p.NowNS)
	case "task_finish":
		var p struct {
			Token      taskrepo.Token      `json:"token"`
			Completion taskrepo.Completion `json:"completion"`
			Followups  []taskrepo.NewTask  `json:"followups,omitempty"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Finish(request.RequestID, p.Token, p.Completion, p.Followups)
	case "task_children":
		var p struct {
			ID    string `json:"task_id"`
			After []byte `json:"after,omitempty"`
			Limit int    `json:"limit,omitempty"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		if p.Limit == 0 {
			p.Limit = 200
		}
		return tasks.Children(p.ID, p.After, p.Limit)
	case "task_interrupt":
		var p struct {
			Token  taskrepo.Token `json:"token"`
			NowNS  int64          `json:"now_ns"`
			Reason string         `json:"reason"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Interrupt(request.RequestID, p.Token, p.NowNS, p.Reason)
	case "task_request":
		var p struct{}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return tasks.Request(request.RequestID)
	case "workflow_start":
		var p workflow.Start
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return h.Workflows.Start(request.RequestID, p)
	case "workflow_complete":
		var p struct {
			Token taskrepo.Token    `json:"token"`
			Input workflow.Complete `json:"input"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return h.Workflows.Complete(request.RequestID, p.Token, p.Input)
	case "workflow_control":
		var p struct {
			JobID  string `json:"job_id"`
			Action string `json:"action"`
			NowNS  int64  `json:"now_ns"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return h.Workflows.Control(request.RequestID, p.JobID, p.Action, p.NowNS)
	case "workflow_get":
		var p struct {
			ID string `json:"job_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		job, err := h.Workflows.Job(p.ID)
		return map[string]any{"found": job != nil, "job": job}, err
	case "workflow_inspect":
		var p struct {
			ID string `json:"job_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return h.Workflows.Inspect(p.ID)
	case "workflow_evidence":
		var p struct {
			ID string `json:"evidence_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		item, err := h.Workflows.Evidence(p.ID)
		return map[string]any{"found": item != nil, "evidence": item}, err
	case "workflow_evidence_data":
		var p struct {
			ID string `json:"evidence_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		data, found, err := h.Workflows.EvidenceData(p.ID)
		return map[string]any{"found": found, "data": data}, err
	case "workflow_children":
		var p struct {
			ID    string `json:"job_id"`
			After []byte `json:"after,omitempty"`
			Limit int    `json:"limit,omitempty"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		if p.Limit == 0 {
			p.Limit = 200
		}
		return h.Workflows.Children(p.ID, p.After, p.Limit)
	case "workflow_request":
		var p struct{}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return h.Workflows.Request(request.RequestID)
	case "media_stage":
		var p struct {
			Name      string `json:"name"`
			Content   []byte `json:"content"`
			Replace   bool   `json:"replace"`
			Synthetic bool   `json:"synthetic"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		return h.Media.Stage(mediatx.Request{RequestID: request.RequestID, Name: p.Name, Content: p.Content, Replace: p.Replace, Synthetic: p.Synthetic})
	case "media_publish":
		var p struct {
			ID string `json:"request_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		id := p.ID
		if id == "" {
			id = request.RequestID
		}
		return h.Media.Publish(id)
	case "media_rollback":
		var p struct {
			ID string `json:"request_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		id := p.ID
		if id == "" {
			id = request.RequestID
		}
		return h.Media.Rollback(id)
	case "media_get":
		var p struct {
			ID string `json:"request_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		id := p.ID
		if id == "" {
			id = request.RequestID
		}
		result, found, err := h.Media.Get(id)
		return map[string]any{"found": found, "publication": result}, err
	case "checkpoint":
		var p struct {
			ID string `json:"checkpoint_id"`
		}
		if err := decodeParams(request.Params, &p); err != nil {
			return nil, err
		}
		if !checkpointIDPattern.MatchString(p.ID) {
			return nil, dataengine.ErrInvalidRequest
		}
		err := h.Engine.Checkpoint(filepath.Join(h.BackupDirectory, p.ID))
		return map[string]any{"checkpoint_id": p.ID}, err
	default:
		return nil, dataengine.ErrInvalidRequest
	}
}
