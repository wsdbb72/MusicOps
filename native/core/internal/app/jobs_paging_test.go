package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
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
	waitJob(t, application, "search:review-done", "needs_review")

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
	if len(decoded.Jobs) != 3 {
		t.Fatal(decoded.Jobs)
	}
	var doneFamily map[string]any
	for _, job := range decoded.Jobs {
		if job["stage"] != "needs_review" {
			t.Fatal(job)
		}
		if job["job_id"] == "review-done" {
			doneFamily = job
		}
	}
	if doneFamily == nil {
		t.Fatal("completed root with a review child is missing from the review page")
	}
	reviewIDs, _ := doneFamily["review_job_ids"].([]any)
	if len(reviewIDs) != 1 || reviewIDs[0] != "search:review-done" {
		t.Fatalf("review page did not expose the actionable child: %#v", doneFamily)
	}
	rootStatus, _ := doneFamily["root_status"].(map[string]any)
	if rootStatus == nil || rootStatus["job_id"] != "search:review-done" {
		t.Fatalf("review summary did not point at the actionable child: %#v", doneFamily)
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

func TestJobsLiveFilterIncludesAQueuedWorkflowDescendant(t *testing.T) {
	application := newApp(t)
	if err := application.runner.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	rootID := "live-descendant-root"
	plan, err := json.Marshal(SearchPlan{RootJobID: rootID, Query: "Artist Album", ExpectedTitle: "Album", ExpectedArtist: "Artist", Scene: "expand"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.flows.Start("start-"+rootID, workflow.Start{
		JobID: rootID, BatchID: "pl-netease-live-filter", Scene: "expand", Stage: "identifying", Label: "Artist - Album", Payload: plan, NowNS: now,
	}); err != nil {
		t.Fatal(err)
	}
	token := claimAndBeginWorkflowTask(t, application, rootID, "identify", now+100)
	if _, err := application.flows.Complete("handoff-"+rootID, token, workflow.Complete{
		JobID: rootID, Edge: workflow.EdgeQueueSearch, TaskState: "completed", NowNS: now + 200,
		Evidence: []workflow.EvidenceInput{{ID: "identity-" + rootID, Kind: "api", Source: "musicbrainz", SubjectID: "release-live", TracklistVerified: true, IdentityConfirmed: true, Payload: []byte("verified tracklist")}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/v1/jobs?live=1", "/v1/jobs?roots=1&live=1"} {
		recorder := getAuth(t, application, path)
		if recorder.Code != http.StatusOK {
			t.Fatal(path, recorder.Code, recorder.Body.String())
		}
		var decoded struct {
			Jobs []jobView `json:"jobs"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, job := range decoded.Jobs {
			if job.ID == "search:"+rootID {
				found = true
				if job.ParentID != rootID || job.Stage != "searching" {
					t.Fatalf("live descendant was flattened incorrectly: %+v", job)
				}
			}
		}
		if !found {
			t.Fatalf("%s hid the queued descendant: %+v", path, decoded.Jobs)
		}
	}
}

func TestClassifyJobCountsSeparatesWorkersFromRetryBacklog(t *testing.T) {
	jobs := []workflow.Job{
		{ID: "run-root", RootID: "run-root", Stage: "completed", ActiveTaskID: "run-search"},
		{ID: "run-search", RootID: "run-root", ParentJobID: "run-root", Stage: "searching", ActiveTaskID: "run-task"},
		{ID: "run-old-failure", RootID: "run-root", ParentJobID: "run-root", Stage: "failed"},
		{ID: "wait-root", RootID: "wait-root", Stage: "completed", ActiveTaskID: "wait-search"},
		{ID: "wait-search", RootID: "wait-root", ParentJobID: "wait-root", Stage: "searching", ActiveTaskID: "wait-task"},
		{ID: "review-root", RootID: "review-root", Stage: "needs_review", ActiveTaskID: "review-task"},
		{ID: "failed-root", RootID: "failed-root", Stage: "completed", ActiveTaskID: "failed-search"},
		{ID: "failed-search", RootID: "failed-root", ParentJobID: "failed-root", Stage: "failed", ActiveTaskID: "failed-task"},
		{ID: "done-root", RootID: "done-root", Stage: "completed"},
	}
	counts := classifyJobCounts(jobs, map[string]bool{"run-task": true})
	want := map[string]int{"total": 5, "running": 1, "executing": 1, "queued": 1, "review": 1, "failed": 1, "finished": 1}
	for key, value := range want {
		if counts[key] != value {
			t.Fatalf("%s=%d want=%d all=%v", key, counts[key], value, counts)
		}
	}
}

func TestJobsCountsOnlyIncludesLiveRuntimeState(t *testing.T) {
	application := newApp(t)
	recorder := getAuth(t, application, "/v1/jobs?counts=only")
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["active_count"]; !ok {
		t.Fatalf("counts-only response has no active_count: %s", recorder.Body.String())
	}
	if _, ok := body["pipeline_paused"]; !ok {
		t.Fatalf("counts-only response has no pipeline_paused: %s", recorder.Body.String())
	}
	if _, ok := body["auto_review"]; !ok {
		t.Fatalf("counts-only response has no auto-review runtime: %s", recorder.Body.String())
	}
}

func TestRootSummaryDoesNotHideFailedDescendant(t *testing.T) {
	summary := summarizeRootJobs([]jobView{
		{ID: "root", RootID: "root", Stage: "completed", ExecutionState: "completed", Message: "identity complete", UpdatedNS: 1},
		{ID: "search:root", RootID: "root", ParentID: "root", Stage: "completed", ExecutionState: "completed", UpdatedNS: 2},
		{ID: "process:search:root", RootID: "root", ParentID: "search:root", Stage: "failed", ExecutionState: "failed", Progress: "未能继续", Headline: "下载失败", ErrorClass: "QBITTORENT_UNAVAILABLE", Message: "qBittorrent 登录失败", UpdatedNS: 3, Result: &jobResultView{Stage: "failed", State: "failed", ErrorClass: "QBITTORENT_UNAVAILABLE"}},
	})
	if summary.JobID != "process:search:root" || summary.Stage != "failed" || summary.ExecutionState != "failed" || summary.Progress != "未能继续" || summary.ErrorClass != "QBITTORENT_UNAVAILABLE" {
		t.Fatalf("root summary hid the failed child: %+v", summary)
	}
	if summary.Result == nil || summary.Result.ErrorClass != "QBITTORENT_UNAVAILABLE" {
		t.Fatalf("root summary lost the actionable diagnostic: %+v", summary)
	}
}
