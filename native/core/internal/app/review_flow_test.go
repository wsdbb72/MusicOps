package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestAIReviewEligibilityCoversNonSearchButNeverTokenHolds(t *testing.T) {
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
	token.ErrorClass = "account_token_hold"
	token.BatchID = "pl-netease-fixture"
	if !application.aiReviewEligible(token) {
		t.Fatal("proven zero-spend token hold should retry through the safe fallback")
	}
	spent := token
	spent.ErrorClass = "token_spent"
	if application.aiReviewEligible(spent) {
		t.Fatal("consumed token hold became eligible for automatic retry")
	}
	complete := base
	complete.Stage = "completed"
	if application.aiReviewEligible(complete) {
		t.Fatal("completed job became AI-review eligible")
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
