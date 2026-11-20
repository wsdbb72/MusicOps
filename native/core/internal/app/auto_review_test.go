package app

import (
	"context"
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
	if stats.Candidates != 1 || stats.Accepted != 0 || stats.Held != 1 {
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

func TestCanResetInterruptedSearchOnlyForExhaustedSafeSearch(t *testing.T) {
	job := workflow.Job{Stage: "needs_review", ReviewKind: "interrupted", RecoveryRetryCount: 3}
	task := &taskrepo.Task{Spec: taskrepo.Spec{Command: workflow.CommandSearch}, State: "recovery_required"}
	if !canResetInterruptedSearch(job, task) {
		t.Fatal("exhausted read-only search was not resettable")
	}
	unsafe := *task
	unsafe.Spec.Command = workflow.CommandProcess
	if canResetInterruptedSearch(job, &unsafe) {
		t.Fatal("unsafe process recovery was allowed")
	}
	cancelled := *task
	cancelled.CancelRequested = true
	if canResetInterruptedSearch(job, &cancelled) {
		t.Fatal("cancelled task was reset")
	}
	job.RecoveryRetryCount = 2
	if canResetInterruptedSearch(job, task) {
		t.Fatal("automatic budget was bypassed before exhaustion")
	}
}
