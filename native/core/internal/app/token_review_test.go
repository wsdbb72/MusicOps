package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestTokenReviewDecisionNeverReplaysAnUncertainSpend(t *testing.T) {
	cases := []struct {
		name        string
		class       string
		retries     uint8
		fallback    bool
		message     string
		disposition tokenReviewDisposition
	}{
		{name: "account hold retries", class: "account_token_hold", disposition: tokenReviewRetry},
		{name: "transient read retries", class: "network", disposition: tokenReviewRetry},
		{name: "detail rate limit retries", class: "detail", message: "detail response HTTP 429 is rate limited for 22 seconds", disposition: tokenReviewRetry},
		{name: "retry limit falls back", class: "account_token_hold", retries: tokenReviewRetryLimit, fallback: true, disposition: tokenReviewFallback},
		{name: "retry limit settles", class: "account_token_hold", retries: tokenReviewRetryLimit, disposition: tokenReviewSkip},
		{name: "unknown waits for balance proof", class: "REQUEST_UNKNOWN", disposition: tokenReviewWait},
		{name: "unknown uses streaming", class: "REQUEST_UNKNOWN", fallback: true, disposition: tokenReviewFallback},
		{name: "spent candidate settles", class: "token_spent", disposition: tokenReviewSkip},
		{name: "spent candidate falls back", class: "token_spent", fallback: true, disposition: tokenReviewFallback},
		{name: "budget never downloads normally", class: "token_budget", fallback: true, disposition: tokenReviewFallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenReviewDecisionFor(workflow.Job{ErrorClass: tc.class, Message: tc.message, TokenRetryCount: tc.retries}, tc.fallback)
			if got != tc.disposition {
				t.Fatalf("decision=%s want=%s", got, tc.disposition)
			}
		})
	}
}

func TestTokenReviewFallbackUsableForPlaylistAndStreamingLibraryWork(t *testing.T) {
	if !tokenReviewFallbackUsable(workflow.Job{BatchID: "pl-netease-1", Scene: "expand"}, SearchPlan{}) {
		t.Fatal("NetEase playlist must always retain its source fallback")
	}
	if !tokenReviewFallbackUsable(workflow.Job{Scene: "upgrade"}, SearchPlan{StreamingEnabled: true}) {
		t.Fatal("streaming-enabled library upgrade must be able to fall back")
	}
	if tokenReviewFallbackUsable(workflow.Job{Scene: "upgrade"}, SearchPlan{}) {
		t.Fatal("a plan without streaming must not claim a fallback")
	}
	if tokenReviewFallbackUsable(workflow.Job{Scene: "other"}, SearchPlan{StreamingEnabled: true}) {
		t.Fatal("an unrelated scene must not be routed into album fallback")
	}
}

func TestAIReviewTokenBudgetUsesStreamingFallbackWithoutReplay(t *testing.T) {
	application, rootID, processID := seedTokenReviewJob(t, true, "token_budget")
	recorder := postCode(t, application, "/v1/jobs/"+processID+"/review", map[string]any{"action": "ai"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	inspection := inspect(t, application, processID)
	if inspection.Job.Stage != "fallback" || inspection.Task == nil || inspection.Task.State != "queued" || inspection.Job.ErrorClass != "" {
		t.Fatalf("job=%+v task=%+v", inspection.Job, inspection.Task)
	}
	if inspection.Job.RootID != rootID {
		t.Fatalf("root=%s want=%s", inspection.Job.RootID, rootID)
	}
}

func TestAIReviewUnknownTokenWaitsWithoutFallback(t *testing.T) {
	application, _, processID := seedTokenReviewJob(t, false, "REQUEST_UNKNOWN")
	recorder := postCode(t, application, "/v1/jobs/"+processID+"/review", map[string]any{"action": "ai"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if json.Unmarshal(recorder.Body.Bytes(), &body) != nil || body["code"] != "TOKEN_RECONCILING" || body["held"] != true {
		t.Fatalf("body=%s", recorder.Body.String())
	}
	inspection := inspect(t, application, processID)
	if inspection.Job.Stage != "needs_review" || inspection.Job.ReviewKind != "token" || inspection.Job.TokenRetryCount != 0 {
		t.Fatalf("unknown token review changed without proof: %+v", inspection.Job)
	}
}

func seedTokenReviewJob(t *testing.T, streaming bool, class string) (*App, string, string) {
	t.Helper()
	return seedProcessReviewJob(t, streaming, "token", class)
}

func seedTokenReviewJobWithPlan(t *testing.T, streaming bool, class string, mutate func(*SearchPlan)) (*App, string, string) {
	t.Helper()
	return seedProcessReviewJobWithPlan(t, streaming, "token", class, mutate)
}

func seedProcessReviewJob(t *testing.T, streaming bool, reviewKind, class string) (*App, string, string) {
	t.Helper()
	return seedProcessReviewJobWithPlan(t, streaming, reviewKind, class, nil)
}

func seedProcessReviewJobWithPlan(t *testing.T, streaming bool, reviewKind, class string, mutate func(*SearchPlan)) (*App, string, string) {
	t.Helper()
	application := newApp(t)
	application.mu.Lock()
	application.paused = true
	application.mu.Unlock()
	now := time.Now().UnixNano()
	rootID := "process-review-" + reviewKind + "-" + strings.ToLower(strings.ReplaceAll(class, "_", "-")) + "-root"
	processID := "process:search:" + rootID
	value := SearchPlan{
		RootJobID: rootID, Query: "Artist Album", ExpectedTitle: "Album", ExpectedArtist: "Artist",
		SelectedSite: "dicmusic", SelectedCandidateID: "dicmusic:11", StreamingEnabled: streaming, Budget: 1,
		Identity: Identity{ID: "identity-" + rootID, Kind: "catalog", Source: "local-library", IdentityConfirmed: true, TracklistVerified: true, Payload: `{"title":"Album"}`},
	}
	if mutate != nil {
		mutate(&value)
	}
	plan, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.flows.Start("start-"+rootID, workflow.Start{
		JobID: rootID, BatchID: rootID, Scene: "expand", Stage: "identifying", Label: "Artist — Album", Payload: plan, NowNS: now,
	}); err != nil {
		t.Fatal(err)
	}
	token := claimAndBeginWorkflowTask(t, application, rootID, "identify", now+100)
	if _, err := application.flows.Complete("handoff-"+rootID, token, workflow.Complete{
		JobID: rootID, Edge: workflow.EdgeQueueSearch, TaskState: "completed", NowNS: now + 200,
		Evidence: []workflow.EvidenceInput{{ID: "identity-" + rootID, Kind: "catalog", Source: "local-library", SubjectID: rootID, TracklistVerified: true, IdentityConfirmed: true, Payload: []byte(`{"title":"Album"}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	token = claimAndBeginWorkflowTask(t, application, "search:"+rootID, "identify", now+300)
	report := searchdecision.Report{
		Scene: "expand", IdentityConfirmed: true, StreamingEnabled: streaming,
		SelectedSite: "dicmusic", SelectedCandidateID: "dicmusic:11",
		Sites: []searchdecision.SiteReport{{Site: "open.cd", Status: "DISABLED"}, {Site: "dicmusic", Status: "OK", Attempted: true, CandidateCount: 1, Exact: true, ObservedTitle: "Album", ExpectedTitle: "Album", Seeders: 3, SeedersKnown: true}, {Site: "orpheus", Status: "DISABLED"}, {Site: "redacted", Status: "DISABLED"}},
	}
	decision, decisionErr := searchdecision.Decide(report)
	if decisionErr != nil || decision.Action != searchdecision.ActionQueueRequest {
		t.Fatalf("test report decision=%+v err=%v", decision, decisionErr)
	}
	if _, err := application.flows.Complete("process-"+rootID, token, workflow.Complete{
		JobID: "search:" + rootID, Edge: workflow.EdgeQueueProcess, NextStage: "requesting", TaskState: "completed", NowNS: now + 400,
		Result: plan, Search: &report,
		Evidence: []workflow.EvidenceInput{{ID: "search-" + rootID, Kind: "catalog", Source: "dicmusic", SubjectID: rootID, Payload: []byte("observed")}},
	}); err != nil {
		t.Fatal(err)
	}
	token = claimAndBeginWorkflowTask(t, application, processID, "aux", now+500)
	if reviewKind == "interrupted" {
		if _, err := application.flows.Interrupt("interrupt-"+rootID, token, now+600, "处理核对测试"); err != nil {
			t.Fatal(err)
		}
		return application, rootID, processID
	}
	if _, err := application.flows.Complete("block-"+rootID, token, workflow.Complete{
		JobID: processID, Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: reviewKind, TaskState: "blocked",
		ErrorClass: class, Message: "处理核对测试", NowNS: now + 600,
	}); err != nil {
		t.Fatal(err)
	}
	return application, rootID, processID
}
