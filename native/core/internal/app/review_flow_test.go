package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestAIReviewEligibilityCoversSafeNonSearchAndTokenHoldPaths(t *testing.T) {
	withoutAI := newApp(t)
	neteaseSource := workflow.Job{Stage: "needs_review", TaskType: "search", ReviewKind: "candidate", BatchID: "pl-netease-fixture"}
	if !withoutAI.aiReviewEligible(neteaseSource) {
		t.Fatal("NetEase candidate fallback must not depend on an AI model")
	}
	ordinarySearch := neteaseSource
	ordinarySearch.BatchID = "batch-1"
	ordinarySearch.ReviewKind = "search"
	if !withoutAI.aiReviewEligible(ordinarySearch) {
		t.Fatal("evidence-driven search review must remain eligible without a model")
	}
	application := newApp(t)
	calls := atomic.Int32{}
	application.setReviewCompleter(reviewCaller{calls: &calls, body: []byte(`{"decision":"review","reason":"unused"}`)})
	application.reviews = reviewai.NewCache(1)
	base := workflow.Job{Stage: "needs_review", TaskType: "album", ReviewKind: "candidate"}
	if !application.aiReviewEligible(base) {
		t.Fatal("album review was not eligible for automatic replay")
	}
	token := base
	token.ReviewKind = "token"
	for _, class := range []string{"account_token_hold", "rate_limited", "token_pending"} {
		token.ErrorClass = class
		token.BatchID = ""
		if !application.aiReviewEligible(token) {
			t.Fatalf("%s is a proven zero-spend review and should retry automatically", class)
		}
	}
	for _, class := range []string{"token_spent", "REQUEST_UNKNOWN", "SPEND_CONFIRMED"} {
		token.ErrorClass = class
		if !application.aiReviewEligible(token) {
			t.Fatalf("%s needs a deterministic reconciliation action, not a hidden disabled button", class)
		}
	}
	token.Paused = true
	if application.aiReviewEligible(token) {
		t.Fatal("a paused token review must not be auto-processed")
	}
	complete := base
	complete.Stage = "completed"
	if application.aiReviewEligible(complete) {
		t.Fatal("completed job became AI-review eligible")
	}
}

func TestAIReviewRetriesAZeroSpendTokenHoldWithoutCallingTheModel(t *testing.T) {
	application := newApp(t)
	application.mu.Lock()
	application.paused = true
	application.mu.Unlock()
	now := time.Now().UnixNano()
	rootID := "ai-token-hold-root"
	processID := "process:search:" + rootID
	plan, _ := json.Marshal(SearchPlan{RootJobID: rootID, Query: "Artist Album", ExpectedTitle: "Album", ExpectedArtist: "Artist", SelectedSite: "dicmusic", SelectedCandidateID: "dicmusic:11", Budget: 0})
	if _, err := application.flows.Start("start-"+rootID, workflow.Start{
		JobID: rootID, BatchID: "pl-netease-ai-token-hold", Scene: "expand", Stage: "identifying", Label: "Artist - Album", Payload: plan, NowNS: now,
	}); err != nil {
		t.Fatal(err)
	}
	token := claimAndBeginWorkflowTask(t, application, rootID, "identify", now+100)
	if _, err := application.flows.Complete("handoff-"+rootID, token, workflow.Complete{
		JobID: rootID, Edge: workflow.EdgeQueueSearch, TaskState: "completed", NowNS: now + 200,
		Evidence: []workflow.EvidenceInput{{ID: "identity-" + rootID, Kind: "api", Source: "musicbrainz", SubjectID: "release-ai-token", TracklistVerified: true, IdentityConfirmed: true, Payload: []byte("verified tracklist")}},
	}); err != nil {
		t.Fatal(err)
	}
	token = claimAndBeginWorkflowTask(t, application, "search:"+rootID, "identify", now+300)
	report := searchdecision.Report{
		Scene: "expand", IdentityConfirmed: true, SelectedSite: "dicmusic", SelectedCandidateID: "dicmusic:11",
		Sites: []searchdecision.SiteReport{
			{Site: "open.cd", Status: "DISABLED"},
			{Site: "dicmusic", Status: "OK", Attempted: true, CandidateCount: 1, Exact: true, ObservedTitle: "Album", ExpectedTitle: "Album", Seeders: 3, SeedersKnown: true},
			{Site: "orpheus", Status: "DISABLED"},
			{Site: "redacted", Status: "DISABLED"},
		},
	}
	if _, err := application.flows.Complete("process-"+rootID, token, workflow.Complete{
		JobID: "search:" + rootID, Edge: workflow.EdgeQueueProcess, NextStage: "requesting", TaskState: "completed", NowNS: now + 400,
		Result: plan, Search: &report,
		Evidence: []workflow.EvidenceInput{{ID: "search-" + rootID, Kind: "catalog", Source: "dicmusic", Payload: []byte("observed candidate")}},
	}); err != nil {
		t.Fatal(err)
	}
	token = claimAndBeginWorkflowTask(t, application, processID, "aux", now+500)
	blocked, err := application.flows.Complete("block-"+rootID, token, workflow.Complete{
		JobID: processID, Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "token", TaskState: "blocked",
		ErrorClass: "account_token_hold", Message: "余额不足，未向站点请求令牌", NowNS: now + 600,
	})
	if err != nil || blocked.Job == nil || blocked.Job.Stage != "needs_review" || !application.aiReviewEligible(*blocked.Job) {
		t.Fatalf("blocked=%+v err=%v", blocked.Job, err)
	}
	calls := atomic.Int32{}
	application.setReviewCompleter(reviewCaller{calls: &calls, body: []byte(`{"decision":"pass","candidate_id":"dicmusic:11","reason":"should not run"}`)})
	application.reviews = reviewai.NewCache(1)
	recorder := postCode(t, application, "/v1/jobs/"+processID+"/review", map[string]any{"action": "ai"})
	if recorder.Code != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("code=%d calls=%d body=%s", recorder.Code, calls.Load(), recorder.Body.String())
	}
	job, err := application.flows.Job(processID)
	if err != nil || job == nil || job.Stage != "requesting" || job.ReviewKind != "" || job.ErrorClass != "" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	task, err := application.tasks.Get(job.ActiveTaskID)
	if err != nil || task == nil || task.State != "queued" || task.Owner != "" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func claimAndBeginWorkflowTask(t *testing.T, application *App, id, lane string, now int64) taskrepo.Token {
	t.Helper()
	// Open starts the workers before a test can set the pipeline pause flag.
	// On a fast runner (notably macOS), a worker may therefore claim the task
	// in that tiny window. Release an unstarted claim and retry so the test is
	// deterministic instead of relying on scheduler timing.
	deadline := time.Now().Add(3 * time.Second)
	for attempt := int64(0); ; attempt++ {
		requestID := "claim-" + id + "-" + time.Now().Format("150405.000000000")
		claim, err := application.tasks.Claim(requestID, lane, "review-test-owner", now+attempt)
		if err == nil && claim.Task != nil {
			token := taskrepo.Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
			if _, err := application.tasks.Begin("begin-"+id, token, now+attempt+1); err != nil {
				t.Fatal(err)
			}
			return token
		}
		current, inspectErr := application.tasks.Get(id)
		if inspectErr != nil {
			t.Fatalf("inspect claim %s: %v", id, inspectErr)
		}
		if current != nil && current.Execution == "claimed" && current.Owner != "" {
			token := taskrepo.Token{ID: current.ID, Owner: current.Owner, Fence: current.Fence}
			_, releaseErr := application.tasks.ReleaseClaim("release-claim-"+id+"-"+time.Now().Format("150405.000000000"), token, now+attempt+1)
			if releaseErr != nil && !errors.Is(releaseErr, taskrepo.ErrStale) {
				t.Fatalf("release competing claim %s: %v", id, releaseErr)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim %s: %+v %v task=%+v", id, claim, err, current)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNormalizedAndSameReleaseCanQueueWithoutAModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-MusicOps-Site") {
		case "dicmusic":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Ｅｘａｍｐｌｅ　＆　Ｔｅｓｔ","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
		case "redacted":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":12,"groupName":"Example & Test","artist":"Artist","torrents":[{"torrentId":13,"format":"FLAC","encoding":"Lossless","seeders":2}]}]}}`)
		case "open.cd":
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
		default:
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	var calls atomic.Int32
	application.setReviewCompleter(reviewCaller{calls: &calls, body: []byte(`{"decision":"pass","candidate_id":"dicmusic:11","reason":"unused"}`)})
	body := searchBody("album-fold", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-fold", "completed")
	if job.SearchAction != "queue_request" || calls.Load() != 0 {
		t.Fatal(job.SearchAction, calls.Load())
	}
}

func TestEditionArtistAndIncompleteEvidenceStayInReview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") == "dicmusic" {
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example & Test Live","artist":"Tribute Band","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":4,"remasterTitle":"Live"}]}]}}`)
			return
		}
		if r.Header.Get("X-MusicOps-Site") == "open.cd" {
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()
	application := newApp(t)
	calls := atomic.Int32{}
	application.setReviewCompleter(reviewCaller{calls: &calls, err: context.DeadlineExceeded})
	body := searchBody("album-live", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	body["expected_artist"] = "Artist"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-live", "needs_review")
	if job.SearchAction == "queue_request" || !strings.Contains(job.Message, "暂时没有完成自动核对") || calls.Load() == 0 {
		t.Fatal(job.SearchAction, job.Message, calls.Load())
	}
}

func TestSecondSearchPageCanFindTheRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") != "dicmusic" {
			if r.Header.Get("X-MusicOps-Site") == "open.cd" {
				io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
				return
			}
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			io.WriteString(w, `{"status":"success","response":{"currentPage":2,"pages":2,"results":[{"groupId":10,"groupName":"Example & Test","artist":"Artist","aliases":["Example and Test"],"torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":2,"results":[{"groupId":9,"groupName":"Something Else","artist":"Artist","torrents":[{"torrentId":8,"format":"FLAC","encoding":"Lossless","seeders":1}]}]}}`)
	}))
	defer server.Close()
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("album-page", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false))
	job := waitJob(t, application, "search:album-page", "completed")
	if job.SearchAction != "queue_request" {
		t.Fatal(job)
	}
}

func TestModelPassCannotOverrideMissingEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") == "dicmusic" {
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"蓝 Live","artist":"Artist","aliases":["Blue"],"torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
			return
		}
		if r.Header.Get("X-MusicOps-Site") == "open.cd" {
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()
	application := newApp(t)
	calls := atomic.Int32{}
	application.setReviewCompleter(reviewCaller{calls: &calls, body: []byte(`{"decision":"pass","candidate_id":"dicmusic:11","reason":"sounds the same"}`)})
	body := searchBody("album-hallucination", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	body["expected_title"] = "Blue"
	body["expected_artist"] = "Artist"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-hallucination", "needs_review")
	if job.SearchAction == "queue_request" || calls.Load() == 0 || strings.Contains(job.Message, "已入库") {
		t.Fatal(job.SearchAction, job.Message, calls.Load())
	}
	recorder := postCode(t, application, "/v1/jobs/"+job.ID+"/review", map[string]any{"action": "ai"})
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "AI_NO_CONFIRMED_MATCH") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	unchanged := inspect(t, application, job.ID)
	if unchanged.Job.Stage != "needs_review" {
		t.Fatal(unchanged.Job)
	}
	detail := getAuth(t, application, "/v1/jobs/"+job.ID)
	var diagnostic struct {
		Result struct {
			State   string `json:"state"`
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"result"`
	}
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &diagnostic) != nil || diagnostic.Result.State != "blocked" || diagnostic.Result.Phase == "" || diagnostic.Result.Message == "" {
		t.Fatalf("blocked job diagnostic missing: %d %s", detail.Code, detail.Body.String())
	}
}

func TestNeteaseCandidateReviewAdvancesToAlbumFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-MusicOps-Site") {
		case "dicmusic":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example & Test Live","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
		case "open.cd":
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
		default:
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := searchBody("album-netease-fallback", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, true)
	body["batch_id"] = "pl-netease-590536461-test"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-netease-fallback", "needs_review")
	if job.ReviewKind != "candidate" {
		t.Fatal(job)
	}
	inspection := inspect(t, application, job.ID)
	raw, found, dataErr := application.tasks.Data(inspection.Task.ID, "arguments", 0)
	var plan SearchPlan
	if dataErr != nil || !found || json.Unmarshal(raw, &plan) != nil {
		t.Fatal(dataErr, found)
	}
	observations, _, _, observationErr := application.reviewObservations(inspection, plan)
	if observationErr != nil {
		t.Fatal(observationErr)
	}
	report := application.rebuildReviewReport(job, plan, observations, "", "")
	markNeteaseSourceFallback(&report)
	report.ExplicitFallback = true
	decision, decisionErr := searchdecision.Decide(report)
	if decisionErr != nil || decision.Action != searchdecision.ActionFallback {
		t.Fatalf("fallback report decision=%+v err=%v report=%+v", decision, decisionErr, report)
	}
	encodedPlan, encodeErr := json.Marshal(plan)
	var roundTrip searchdecision.Report
	reportRaw, _ := json.Marshal(report)
	if encodeErr != nil || json.Unmarshal(reportRaw, &roundTrip) != nil || len(encodedPlan) == 0 || report.Scene != job.Scene || job.IdentificationJobID == "" || !report.SourceFallback || report.SelectedCandidateID != "" {
		t.Fatalf("fallback transport invalid encode=%v plan=%d scene=%q/%q identification=%q source=%v selected=%q report=%s", encodeErr, len(encodedPlan), report.Scene, job.Scene, job.IdentificationJobID, report.SourceFallback, report.SelectedCandidateID, reportRaw)
	}
	recorder := postCode(t, application, "/v1/jobs/"+job.ID+"/review", map[string]any{"action": "ai"})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	inspection = inspect(t, application, job.ID)
	if inspection.Job.Stage != "completed" || inspection.Job.ProcessingJobID == "" || inspection.Job.SearchAction != "fallback" {
		t.Fatalf("candidate fallback did not advance: %+v", inspection.Job)
	}
}

func TestExpiredLoginDoesNotStrandDefinitiveSearchReview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") == "open.cd" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "login required")
			return
		}
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()
	application := newApp(t)
	body := searchBody("album-expired-auth", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, true)
	body["batch_id"] = "pl-netease-590536461-test"
	post(t, application, "/v1/search", body)
	search := waitJob(t, application, "search:album-expired-auth", "needs_review")
	if search.ReviewKind != "search" {
		t.Fatal(search)
	}
	if err := application.runner.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := application.autoReviewPass(context.Background())
	if stats.Candidates != 1 || stats.Accepted != 1 || stats.Held != 0 {
		t.Fatalf("auto review did not resolve the definitive search: %+v", stats)
	}
	job, err := application.flows.Job("search:album-expired-auth")
	if err != nil || job == nil || job.Stage != "completed" || job.SearchAction != searchdecision.ActionFallback || job.ProcessingJobID == "" {
		t.Fatalf("search did not queue fallback: %+v err=%v", job, err)
	}
}

type reviewCaller struct {
	calls *atomic.Int32
	body  []byte
	err   error
}

func (c reviewCaller) Complete(context.Context, []byte) ([]byte, error) {
	if c.calls != nil {
		c.calls.Add(1)
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.body, nil
}

var _ reviewai.Completer = reviewCaller{}

func TestRebuildReviewReportKeepsMissingEnabledSitesUnsearched(t *testing.T) {
	application := newApp(t)
	report := application.rebuildReviewReport(workflow.Job{Scene: "expand"}, SearchPlan{
		ExpectedTitle: "Album", EnabledSites: []string{"open.cd", "dicmusic"},
	}, nil, "", "")
	bySite := map[string]searchdecision.SiteReport{}
	for _, row := range report.Sites {
		bySite[row.Site] = row
	}
	if bySite["open.cd"].Status != "NOT_SEARCHED" || bySite["dicmusic"].Status != "NOT_SEARCHED" {
		t.Fatalf("enabled missing sites=%+v", report.Sites)
	}
	if bySite["orpheus"].Status != "DISABLED" || bySite["redacted"].Status != "DISABLED" {
		t.Fatalf("disabled sites=%+v", report.Sites)
	}
}

func TestInterruptedReviewAutoRecoversSafeReadsAndNeteaseFallback(t *testing.T) {
	job := workflow.Job{BatchID: "pl-netease-590536461-test"}
	if !interruptedReviewSafe(job, &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandSearch}}) {
		t.Fatal("read-only search should auto-resume")
	}
	if !interruptedReviewSafe(job, &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandProcess}, Phase: "netease_album"}) {
		t.Fatal("NetEase album phase should auto-resume")
	}
	if !interruptedReviewSafe(job, &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandProcess}, Phase: "fetch_dicmusic"}) {
		t.Fatal("an interrupted NetEase process must resume through the safe source fallback")
	}
	if interruptedReviewSafe(workflow.Job{BatchID: "pl-other-test"}, &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandProcess}, Phase: "fetch_dicmusic"}) {
		t.Fatal("a non-NetEase tracker acquire must remain explicit-review")
	}
	if interruptedReviewSafe(job, &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandDownload}}) {
		t.Fatal("download recovery must reconcile qBittorrent first")
	}
	cancelled := workflow.Job{BatchID: job.BatchID, CancelRequested: true}
	if interruptedReviewSafe(cancelled, &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandSearch}}) {
		t.Fatal("cancelled job became eligible")
	}
}
