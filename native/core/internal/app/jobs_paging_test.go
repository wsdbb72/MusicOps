package app

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The review view used to fetch the newest 100 jobs and filter in the browser,
// so anything older than one page was invisible. The server now filters, and
// the page asks it only for review work (P1-22).
func TestJobsReviewFilterReturnsOnlyReviewWork(t *testing.T) {
	application := newApp(t)
	// Two unconfirmed identities park in needs_review/metadata (P0-1).
	for _, id := range []string{"review-a", "review-b"} {
		body := tokenBody(id, "http://127.0.0.1:9", []string{"dicmusic"})
		body["identity"] = map[string]any{
			"evidence_id": "ev-" + id, "kind": "catalog", "source": "musicbrainz",
			"tracklist_verified": false, "identity_confirmed": false, "payload": "catalogue-row",
		}
		post(t, application, "/v1/search", body)
		waitJob(t, application, id, "needs_review")
	}
	// One confirmed identity completes identification and leaves the list.
	post(t, application, "/v1/search", searchBody("review-done", "http://127.0.0.1:9", nil, false))
	waitJob(t, application, "review-done", "completed")

	recorder := getAuth(t, application, "/v1/jobs?review=1")
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Jobs []map[string]any `json:"jobs"`
		Next string           `json:"next"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Jobs) != 2 {
		t.Fatal(decoded.Jobs)
	}
	for _, job := range decoded.Jobs {
		if job["stage"] != "needs_review" {
			t.Fatal(job)
		}
	}
	if decoded.Next != "" {
		t.Fatal("a single short page must not offer a cursor:", decoded.Next)
	}

	// Without the filter every job is listed, including the completed one.
	all := getAuth(t, application, "/v1/jobs")
	var every struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(all.Body.Bytes(), &every); err != nil {
		t.Fatal(err)
	}
	// Three roots plus the search child the confirmed entry creates.
	if len(every.Jobs) != 4 {
		t.Fatal(len(every.Jobs))
	}
}

// A malformed cursor is refused instead of being ignored.
func TestJobsRejectsAMalformedCursor(t *testing.T) {
	application := newApp(t)
	recorder := getAuth(t, application, "/v1/jobs?review=1&after=@@@")
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}
