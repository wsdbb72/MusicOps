package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
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

func TestJobsLiveFilterProjectsDescendantsOntoOneRoot(t *testing.T) {
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
	searchID := "search:" + rootID
	token = claimAndBeginWorkflowTask(t, application, searchID, "identify", now+300)
	dueNS := now + int64(time.Hour)
	if _, err := application.flows.DeferSearchUntil("defer-"+rootID, token, now+400, dueNS, "站点暂时不可用，等待冷却重试"); err != nil {
		t.Fatal(err)
	}

	live := getAuth(t, application, "/v1/jobs?live=1")
	if live.Code != http.StatusOK {
		t.Fatal(live.Code, live.Body.String())
	}
	var liveBody struct {
		Jobs []jobView `json:"jobs"`
	}
	if err := json.Unmarshal(live.Body.Bytes(), &liveBody); err != nil {
		t.Fatal(err)
	}
	foundChild := false
	for _, job := range liveBody.Jobs {
		if job.ID == searchID {
			foundChild = true
			if job.ParentID != rootID || job.Stage != "searching" || job.ExecutionState != "queued" || job.RetryAfterNS != dueNS {
				t.Fatalf("live descendant lost its cooldown or parent: %+v", job)
			}
		}
	}
	if !foundChild {
		t.Fatalf("live=1 hid the queued descendant: %+v", liveBody.Jobs)
	}

	rootsResponse := getAuth(t, application, "/v1/jobs?roots=1&live=1")
	if rootsResponse.Code != http.StatusOK {
		t.Fatal(rootsResponse.Code, rootsResponse.Body.String())
	}
	var rootsBody struct {
		Jobs []jobView `json:"jobs"`
	}
	if err := json.Unmarshal(rootsResponse.Body.Bytes(), &rootsBody); err != nil {
		t.Fatal(err)
	}
	if len(rootsBody.Jobs) != 1 {
		t.Fatalf("roots=1 returned duplicate family rows: %+v", rootsBody.Jobs)
	}
	root := rootsBody.Jobs[0]
	if root.ID != rootID || root.ParentID != "" || root.Stage != "searching" || root.ExecutionState != "queued" || root.RetryAfterNS != dueNS {
		t.Fatalf("root family was not projected onto one stable row: %+v", root)
	}
	if root.RootStatus == nil || root.RootStatus.JobID != searchID {
		t.Fatalf("root summary did not identify the actionable child: %+v", root.RootStatus)
	}
}

func TestPrioritizeLiveWorkflowJobsKeepsOldRunningTask(t *testing.T) {
	jobs := []workflow.Job{
		{ID: "queued-new", UpdatedNS: 300},
		{ID: "queued-old", UpdatedNS: 200},
		{ID: "running-old", UpdatedNS: 100},
	}
	got := prioritizeLiveWorkflowJobs(jobs, map[string]string{
		"queued-new": "queued", "queued-old": "queued", "running-old": "running",
	}, 2)
	if len(got) != 2 || got[0].ID != "running-old" || got[1].ID != "queued-new" {
		t.Fatalf("live priority truncated the running task: %+v", got)
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
	counts := classifyJobCounts(jobs, map[string]bool{"run-task": true}, map[string]bool{})
	want := map[string]int{"total": 5, "running": 1, "executing": 1, "queued": 1, "review": 1, "failed": 1, "finished": 1}
	for key, value := range want {
		if counts[key] != value {
			t.Fatalf("%s=%d want=%d all=%v", key, counts[key], value, counts)
		}
	}
}

func TestLiveJobOrderingPutsActiveDownloadsFirst(t *testing.T) {
	jobs := []jobView{
		{ID: "queued-new", Stage: "searching", ExecutionState: "queued", UpdatedNS: 500},
		{ID: "done-new", Stage: "completed", ExecutionState: "completed", UpdatedNS: 900},
		{ID: "download-old", Stage: "downloading", ExecutionState: "running", UpdatedNS: 100},
		{ID: "publish-old", Stage: "publishing", ExecutionState: "running", UpdatedNS: 90},
	}
	sortJobsForLiveView(jobs)
	want := []string{"download-old", "publish-old", "queued-new", "done-new"}
	for i, id := range want {
		if jobs[i].ID != id {
			t.Fatalf("order[%d]=%s want=%s; all=%+v", i, jobs[i].ID, id, jobs)
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

func TestRootSummarySurfacesFailedTaskUnderCompletedJob(t *testing.T) {
	summary := summarizeRootJobs([]jobView{
		{ID: "root", RootID: "root", Stage: "completed", ExecutionState: "completed", Message: "已完成", UpdatedNS: 1},
		{
			ID: "search:root", RootID: "root", ParentID: "root", Stage: "completed", ExecutionState: "completed", UpdatedNS: 2,
			Result: &jobResultView{Stage: "completed", State: "failed", ErrorClass: "VERIFY_FAILED", Message: "library record did not verify: file missing"},
		},
	})
	if summary.JobID != "search:root" || summary.Stage != "failed" || summary.ExecutionState != "failed" || summary.ErrorClass != "VERIFY_FAILED" {
		t.Fatalf("completed job hid its failed active task: %+v", summary)
	}
	if summary.Result == nil || summary.Result.State != "failed" || !strings.Contains(summary.Headline, "file missing") {
		t.Fatalf("failed task diagnostic was lost: %+v", summary)
	}
}

func TestClassifyJobCountsIncludesFailedTaskUnderCompletedJob(t *testing.T) {
	jobs := []workflow.Job{
		{ID: "failed-task-root", RootID: "failed-task-root", Stage: "completed", ActiveTaskID: "stage:failed-task-root:publishing"},
		{ID: "finished-root", RootID: "finished-root", Stage: "completed"},
	}
	counts := classifyJobCounts(jobs, map[string]bool{}, map[string]bool{"stage:failed-task-root:publishing": true})
	if counts["failed"] != 1 || counts["finished"] != 1 || counts["total"] != 2 {
		t.Fatalf("failed active task under completed root was not counted: %+v", counts)
	}
}

func TestAlbumProgressShowsLiveAlbumCheckpoint(t *testing.T) {
	progress, headline := albumProgress([]jobView{{
		ID: "fallback-root", RootID: "fallback-root", Stage: "fallback", ExecutionState: "running",
		TaskProgress: 67.5, TaskPhase: "netease_album", TaskMessage: "Ondersteboven", UpdatedNS: 2,
	}})
	if progress != "网易云专辑处理中 67.5%" {
		t.Fatalf("progress=%q", progress)
	}
	if headline != "网易云专辑处理中 67.5%：Ondersteboven" {
		t.Fatalf("headline=%q", headline)
	}
}

func TestWorkflowJobViewProjectsRunningTaskCheckpoint(t *testing.T) {
	application := newApp(t)
	if err := application.runner.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	rootID := "checkpoint-root"
	plan, err := json.Marshal(SearchPlan{RootJobID: rootID, Query: "Artist Album", ExpectedTitle: "Album", ExpectedArtist: "Artist", Scene: "expand"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.flows.Start("start-"+rootID, workflow.Start{
		JobID: rootID, BatchID: "checkpoint-playlist", Scene: "expand", Stage: "identifying", Label: "Artist - Album", Payload: plan, NowNS: now,
	}); err != nil {
		t.Fatal(err)
	}
	token := claimAndBeginWorkflowTask(t, application, rootID, "identify", now+100)
	if _, err := application.flows.Complete("handoff-"+rootID, token, workflow.Complete{
		JobID: rootID, Edge: workflow.EdgeQueueSearch, TaskState: "completed", NowNS: now + 200,
		Evidence: []workflow.EvidenceInput{{ID: "identity-" + rootID, Kind: "api", Source: "musicbrainz", SubjectID: "release-checkpoint", TracklistVerified: true, IdentityConfirmed: true, Payload: []byte("verified tracklist")}},
	}); err != nil {
		t.Fatal(err)
	}
	searchID := "search:" + rootID
	token = claimAndBeginWorkflowTask(t, application, searchID, "identify", now+300)
	if _, err := application.tasks.Progress(token, taskrepo.Point{Sequence: 1, Percent: 67.5, Phase: "netease_album", Message: "Ondersteboven", NowNS: now + 400}); err != nil {
		t.Fatal(err)
	}
	job, err := application.flows.Job(searchID)
	if err != nil || job == nil {
		t.Fatalf("job: %+v %v", job, err)
	}
	view := application.workflowJobView(*job)
	if view.ExecutionState != "running" || view.TaskProgress != 67.5 || view.TaskPhase != "netease_album" || view.TaskMessage != "Ondersteboven" || view.UpdatedNS != now+400 {
		t.Fatalf("live task checkpoint was not projected: %+v", view)
	}
}
