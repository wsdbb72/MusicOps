package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestAutoReviewPassRunsWithoutAButtonAndCoolsRepeatedMetadataRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "identity review must not read trackers", http.StatusInternalServerError)
	}))
	defer server.Close()
	application := newApp(t)
	calls := atomic.Int32{}
	application.setReviewCompleter(reviewCaller{calls: &calls, body: []byte(`{"decision":"review","reason":"unused"}`)})
	application.reviews = reviewai.NewCache(1)
	body := tokenBody("auto-review-metadata", server.URL, []string{"dicmusic"})
	body["identity"] = map[string]any{
		"evidence_id": "ev-auto-review", "kind": "catalog", "source": "musicbrainz",
		"tracklist_verified": false, "identity_confirmed": false, "payload": "catalogue-row",
	}
	post(t, application, "/v1/search", body)
	waitJob(t, application, "auto-review-metadata", "needs_review")

	stats := application.autoReviewPass(context.Background())
	if stats.Candidates != 1 || stats.Accepted != 1 || stats.Held != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if !application.autoReviewCoolingDown("auto-review-metadata", application.now()) {
		t.Fatal("metadata retry was not cooled down")
	}
	second := application.autoReviewPass(context.Background())
	if second.Candidates != 0 {
		t.Fatalf("tight auto-review loop: %+v", second)
	}
	if calls.Load() != 0 {
		t.Fatal("metadata replay should not ask the model to invent identity:", calls.Load())
	}
}

func TestAutoReviewCooldownBlocksImmediateRepeatedAttempts(t *testing.T) {
	application := newApp(t)
	calls := atomic.Int32{}
	application.setReviewCompleter(reviewCaller{calls: &calls, body: []byte(`{"decision":"review","reason":"unused"}`)})
	application.reviews = reviewai.NewCache(1)
	stats := application.autoReviewPass(context.Background())
	if stats.Candidates != 0 || calls.Load() != 0 {
		t.Fatalf("stats=%+v calls=%d", stats, calls.Load())
	}
	now := time.Now()
	application.setAutoReviewCooldown("token-hold", now)
	if !application.autoReviewCoolingDown("token-hold", now.Add(time.Minute)) {
		t.Fatal("cooldown was not applied")
	}
}

func TestAutoReviewMovesTokenHoldToDurableCooldown(t *testing.T) {
	application, _, processID := seedTokenReviewJob(t, false, "account_token_hold")
	stats := application.autoReviewPass(context.Background())
	if stats.Candidates != 1 || stats.Accepted != 1 || stats.Held != 0 || stats.Failed != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	inspection := inspect(t, application, processID)
	if inspection.Job.Stage != "requesting" || inspection.Job.ReviewKind != "" || inspection.Job.RetryAfterNS <= time.Now().UnixNano() {
		t.Fatalf("job did not enter a durable cooldown: %+v", inspection.Job)
	}
	if inspection.Task == nil || inspection.Task.State != "queued" || inspection.Task.NotBeforeNS != inspection.Job.RetryAfterNS {
		t.Fatalf("task=%+v", inspection.Task)
	}
	if !application.autoReviewCoolingDown(processID, application.now()) && stats.Held != 0 {
		t.Fatal("a queued retry was incorrectly treated as a held review")
	}
}

func TestInterruptedSearchAutomaticBudgetIsBounded(t *testing.T) {
	task := &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandSearch}, State: "recovery_required"}
	job := workflow.Job{Stage: "needs_review", ReviewKind: "interrupted", RecoveryRetryCount: 2, SearchRetryCount: 1}
	if interruptedSearchAutomaticBudgetExhausted(job, task) {
		t.Fatal("automatic budget ended before either limit")
	}
	job.RecoveryRetryCount = 3
	if !interruptedSearchAutomaticBudgetExhausted(job, task) {
		t.Fatal("exhausted recovery budget did not reach terminal policy")
	}
	job.RecoveryRetryCount = 0
	job.SearchRetryCount = 3
	if !interruptedSearchAutomaticBudgetExhausted(job, task) {
		t.Fatal("exhausted search budget did not reach terminal policy")
	}
	unsafe := *task
	unsafe.Spec.Command = workflow.CommandProcess
	if interruptedSearchAutomaticBudgetExhausted(job, &unsafe) {
		t.Fatal("unsafe process recovery was treated as bounded replay")
	}
}

func TestInterruptedSearchRecoveryAlwaysHasTerminalOutcome(t *testing.T) {
	task := &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandSearch}, State: "recovery_required"}
	job := workflow.Job{Stage: "needs_review", ReviewKind: "interrupted", RecoveryRetryCount: 2, SearchRetryCount: 1}
	if got := interruptedSearchRecoveryAction(job, task); got != "retry" {
		t.Fatalf("early interrupted search action = %q", got)
	}
	job.RecoveryRetryCount = 3
	if got := interruptedSearchRecoveryAction(job, task); got != "abandon" {
		t.Fatalf("exhausted recovery action = %q", got)
	}
	job.RecoveryRetryCount = 0
	job.SearchRetryCount = 3
	if got := interruptedSearchRecoveryAction(job, task); got != "abandon" {
		t.Fatalf("exhausted search action = %q", got)
	}
	unsafe := *task
	unsafe.Spec.Command = workflow.CommandProcess
	if got := interruptedSearchRecoveryAction(job, &unsafe); got != "" {
		t.Fatalf("unsafe process action = %q", got)
	}
}
func TestAutoReviewSettlesDefinitiveModelNoMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-MusicOps-Site") {
		case "dicmusic":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example & Test Live","artist":"Tribute Band","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3,"remasterTitle":"Live"}]}]}}`)
		case "open.cd":
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
		default:
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	calls := atomic.Int32{}
	application.setReviewCompleter(reviewCaller{calls: &calls, body: []byte(`{"decision":"review","reason":"not the same recording"}`)})
	application.reviews = reviewai.NewCache(1)
	post(t, application, "/v1/search", searchBody("album-auto-settle", server.URL, []string{"open.cd", "dicmusic", "orpheus"}, false))
	waitJob(t, application, "search:album-auto-settle", "needs_review")
	stats := application.autoReviewPass(context.Background())
	if stats.Candidates != 1 || stats.Settled != 1 || stats.Held != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	job, err := application.flows.Job("search:album-auto-settle")
	if err != nil || job == nil || job.Stage != "skipped" || job.ReviewKind != "" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}
