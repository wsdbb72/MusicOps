package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// An unconfirmed identity is the default state of a search started from a bare
// album name. The identify executor must stop at a human checkpoint instead of
// asking the workflow for a search edge it cannot grant: before the fix that
// request was rejected, the scheduler interrupted the task and the job sat in
// recovery_required with "执行已退出" (P0-1).
func TestUnconfirmedIdentityLandsInReviewWithoutReadingSites(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		http.Error(w, "the search must not run without a confirmed identity", http.StatusInternalServerError)
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-unconfirmed", server.URL, []string{"dicmusic"})
	body["identity"] = map[string]any{
		"evidence_id": "ev-album-unconfirmed", "kind": "catalog", "source": "musicbrainz",
		"tracklist_verified": false, "identity_confirmed": false, "payload": "catalogue-row",
	}
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "album-unconfirmed", "needs_review")
	if job.ReviewKind != "metadata" || job.ErrorClass != "IDENTITY_UNCONFIRMED" {
		t.Fatal(job.ReviewKind, job.ErrorClass, job.Message)
	}
	if reads.Load() != 0 {
		t.Fatal("sites were read without a confirmed identity:", reads.Load())
	}
	if strings.Contains(job.Message, "执行已退出") {
		t.Fatal(job.Message)
	}
	task, err := application.tasks.Get("album-unconfirmed")
	if err != nil || task == nil {
		t.Fatal(err)
	}
	if task.State != "blocked" {
		t.Fatal(task.State, task.Message)
	}
	// No search child may exist: the identity checkpoint is upstream of search.
	if inspection := inspect(t, application, "search:album-unconfirmed"); inspection.Job.ID != "" {
		t.Fatal(inspection.Job)
	}
}

// The metadata checkpoint is resolvable by the user: a job parked there can be
// skipped, so it never becomes an unresolvable dead end in the review queue.
func TestUnconfirmedIdentityReviewCanBeSkipped(t *testing.T) {
	application := newApp(t)
	body := tokenBody("album-unconfirmed-skip", "http://127.0.0.1:9", []string{"dicmusic"})
	body["identity"] = map[string]any{
		"evidence_id": "ev-album-unconfirmed-skip", "kind": "catalog", "source": "musicbrainz",
		"tracklist_verified": false, "identity_confirmed": false, "payload": "catalogue-row",
	}
	post(t, application, "/v1/search", body)
	waitJob(t, application, "album-unconfirmed-skip", "needs_review")
	recorder := postReview(t, application, "album-unconfirmed-skip", map[string]any{"action": "skip"})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	job := waitJob(t, application, "album-unconfirmed-skip", "skipped")
	if job.ReviewKind != "" {
		t.Fatal(job.ReviewKind, job.Message)
	}
}
