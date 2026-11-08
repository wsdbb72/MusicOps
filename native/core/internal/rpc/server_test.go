package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const testToken = "synthetic-local-engine-authentication-key"

func fixture(t *testing.T) *Handler {
	t.Helper()
	engine, err := dataengine.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	handler, err := New(engine, testToken, filepath.Join(t.TempDir(), "backups"))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func call(t *testing.T, h *Handler, request Request) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(request)
	r := httptest.NewRequest("POST", "http://127.0.0.1/rpc", bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return w.Code, value
}

func TestAuthenticationAndHealth(t *testing.T) {
	h := fixture(t)
	request := httptest.NewRequest("GET", "http://127.0.0.1/health", nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated health accepted")
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "pebble") {
		t.Fatal(response.Body.String())
	}
}

func TestAggregateRequestBudgetRejectsBeforeDecoding(t *testing.T) {
	h := fixture(t)
	h.bodyBytes.Store(128 << 20)
	code, value := call(t, h, Request{Operation: "task_counts"})
	if code != 429 || value["code"] != "BUSY" {
		t.Fatal("memory budget ignored", code, value)
	}
	h.bodyBytes.Store(0)
	code, _ = call(t, h, Request{Operation: "task_counts"})
	if code != 200 || h.bodyBytes.Load() != 0 {
		t.Fatal("request budget not released")
	}
}

func params(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

func TestRPCDurableWriteReplayAndStatus(t *testing.T) {
	h := fixture(t)
	spec := map[string]any{"task_id": "a", "command": "identify_metadata", "lane": "identify", "priority": 1, "created_ns": 1000}
	input := map[string]any{"spec": spec, "payload": []byte("data")}
	request := Request{Operation: "task_enqueue", RequestID: "stable", Params: params(input)}
	code, result := call(t, h, request)
	if code != 200 || result["ok"] != true {
		t.Fatal(result)
	}
	_, result = call(t, h, request)
	if result["result"].(map[string]any)["replayed"] != true {
		t.Fatal("receipt lost")
	}
	code, result = call(t, h, Request{Operation: "task_request", RequestID: "stable"})
	if code != 200 || result["result"].(map[string]any)["state"] != "COMMITTED" {
		t.Fatal(result)
	}
	input["payload"] = []byte("different")
	request.Params = params(input)
	code, _ = call(t, h, request)
	if code != 409 {
		t.Fatal("conflicting intent accepted")
	}
	code, result = call(t, h, Request{Operation: "task_get", Params: params(map[string]any{"task_id": "a"})})
	if code != 200 || result["result"].(map[string]any)["found"] != true {
		t.Fatal(result)
	}
	code, _ = call(t, h, Request{Operation: "apply", Params: params(map[string]any{})})
	if code != 400 {
		t.Fatal("obsolete raw mutation route survived")
	}
	code, _ = call(t, h, Request{Operation: "task_get", Params: params(map[string]any{"task_id": "a", "unknown": true})})
	if code != 400 {
		t.Fatal("unknown parameter ignored")
	}
}

func TestWorkflowRPCReplayAndOwnedTaskGuard(t *testing.T) {
	h := fixture(t)
	start := map[string]any{"job_id": "album1", "batch_id": "batch1", "scene": "playlist", "stage": "identifying", "source_track_ids": []string{"track-1"}, "library_file_count": 3, "payload": []byte("tracks"), "now_ns": 1000}
	code, result := call(t, h, Request{Operation: "workflow_start", RequestID: "start", Params: params(start)})
	if code != 200 || result["result"].(map[string]any)["job"].(map[string]any)["stage"] != "identifying" {
		t.Fatal(code, result)
	}
	_, result = call(t, h, Request{Operation: "workflow_start", RequestID: "start", Params: params(start)})
	if result["result"].(map[string]any)["replayed"] != true {
		t.Fatal(result)
	}
	start["payload"] = []byte("changed")
	code, _ = call(t, h, Request{Operation: "workflow_start", RequestID: "start", Params: params(start)})
	if code != 409 {
		t.Fatal(code)
	}
	code, _ = call(t, h, Request{Operation: "task_finish", RequestID: "bare", Params: params(map[string]any{"token": map[string]any{"task_id": "album1", "owner": "nobody", "fence": 1}, "completion": map[string]any{"state": "completed", "now_ns": 2000}})})
	if code != 409 {
		t.Fatal("workflow task accepted a bare finish", code)
	}
}

func TestRejectMalformedUnknownOversizedAndEscapingRequests(t *testing.T) {
	h := fixture(t)
	for _, body := range []string{`{"operation":"get","unknown":"x"}`, `{"operation":"get"} {"operation":"apply"}`} {
		r := httptest.NewRequest("POST", "http://127.0.0.1/rpc", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("malformed request accepted: %s", body)
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/rpc", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = maxBody + 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal("oversized request accepted")
	}
	code, _ := call(t, h, Request{Operation: "checkpoint", Params: params(map[string]any{"checkpoint_id": "../escape"})})
	if code != 400 {
		t.Fatal("checkpoint escaped backup root")
	}
	code, _ = call(t, h, Request{Operation: "task_list", Params: params(map[string]any{"lane": "identify", "state": "queued", "limit": 10000})})
	if code != 400 {
		t.Fatal("unbounded scan accepted")
	}
}
