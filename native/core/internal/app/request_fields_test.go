package app

import (
	"net/http"
	"strings"
	"testing"
)

// The page never sent batch_id, so the repository refused every search started
// from the UI with a bare 409. The entrance now defaults it to the job id and
// names the field it rejects (P1-1/P1-2).
func TestSearchDefaultsBatchIDAndCreatesTheJob(t *testing.T) {
	application := newApp(t)
	body := searchBody("web-defaults", "http://127.0.0.1:9", []string{}, false)
	delete(body, "batch_id")
	recorder := postRaw(t, application, "/v1/search", body)
	if recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	job := waitJob(t, application, "web-defaults", "completed")
	if job.BatchID != "web-defaults" {
		t.Fatal(job.BatchID)
	}
}

func TestSearchNamesTheRejectedField(t *testing.T) {
	application := newApp(t)
	badJob := searchBody("bad id", "http://127.0.0.1:9", nil, false)
	recorder := postRaw(t, application, "/v1/search", badJob)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"field":"job_id"`) {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	noEvidence := searchBody("no-evidence", "http://127.0.0.1:9", nil, false)
	noEvidence["identity"] = map[string]any{}
	recorder = postRaw(t, application, "/v1/search", noEvidence)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"field":"identity"`) {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}
