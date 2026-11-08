package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type controlResponse struct {
	OK  bool    `json:"ok"`
	Job jobView `json:"job"`
}

func postControl(t *testing.T, application *App, id, action string) *httptest.ResponseRecorder {
	t.Helper()
	return postCode(t, application, "/v1/jobs/"+id+"/control", map[string]any{"action": action})
}

func decodeControl(t *testing.T, recorder *httptest.ResponseRecorder) controlResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var decoded controlResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// The scheduler is stopped so the identify task stays queued: pausing,
// resuming and cancelling a live job is then deterministic.
func TestJobControlLiveJob(t *testing.T) {
	application := newApp(t)
	if err := application.runner.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/search", searchBody("control-1", "http://127.0.0.1:9", []string{}, false))
	created := inspect(t, application, "control-1")
	if created.Job.ID != "control-1" {
		t.Fatal(created.Job)
	}

	recorder := postControl(t, application, "no-such-job", "pause")
	if recorder.Code != http.StatusNotFound {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	recorder = postControl(t, application, "control-1", "retry")
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}

	paused := decodeControl(t, postControl(t, application, "control-1", "pause"))
	if !paused.Job.Paused || paused.Job.Stage != created.Job.Stage {
		t.Fatal(paused.Job)
	}
	resumed := decodeControl(t, postControl(t, application, "control-1", "resume"))
	if resumed.Job.Paused || resumed.Job.Stage != created.Job.Stage {
		t.Fatal(resumed.Job)
	}
	cancelled := decodeControl(t, postControl(t, application, "control-1", "cancel"))
	if cancelled.Job.Stage != "cancelled" || !cancelled.Job.CancelRequested {
		t.Fatal(cancelled.Job)
	}
	// Cancel is idempotent: repeating it succeeds without changing state.
	// Pausing or resuming a cancelled job is stale.
	recorder = postControl(t, application, "control-1", "cancel")
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	for _, action := range []string{"pause", "resume"} {
		recorder = postControl(t, application, "control-1", action)
		if recorder.Code != http.StatusConflict {
			t.Fatal(action, recorder.Code, recorder.Body.String())
		}
	}
}

// A needs_review job has a blocked task and the completed root is terminal,
// so every control is stale rather than silently accepted.
func TestJobControlNeedsReview(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("control-2", "http://127.0.0.1:9", []string{}, false))
	waitJob(t, application, "search:control-2", "needs_review")

	for _, action := range []string{"pause", "resume", "cancel"} {
		recorder := postControl(t, application, "search:control-2", action)
		if recorder.Code != http.StatusConflict {
			t.Fatal(action, recorder.Code, recorder.Body.String())
		}
	}
	recorder := postControl(t, application, "control-2", "pause")
	if recorder.Code != http.StatusConflict {
		t.Fatal(recorder.Code, recorder.Body.String())
	}

	home := getAuth(t, application, "/")
	script := getAuth(t, application, "/app.js")
	page := home.Body.String() + script.Body.String()
	for _, fragment := range []string{`id="pause-job"`, `id="resume-job"`, `id="cancel-job"`, "/control", "window.confirm("} {
		if !strings.Contains(page, fragment) {
			t.Fatal("missing job control fragment", fragment)
		}
	}
}
