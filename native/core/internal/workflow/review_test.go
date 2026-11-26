package workflow

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// candidateReport classifies as review_candidate: two qualified sites with
// exact matches and no single selection, so the machine blocks for a human
// choice. The report is the same shape the search executor builds.
func candidateReport(retryCount uint8) *searchdecision.Report {
	return &searchdecision.Report{
		Scene: "playlist", IdentityConfirmed: true, RetryCount: retryCount, Query: "Artist Album",
		Sites: []searchdecision.SiteReport{
			{Site: "open.cd", Status: "NO_RESOURCE", Attempted: true},
			{Site: "dicmusic", Status: "OK", Attempted: true, CandidateCount: 1, Exact: true, Seeders: 3, SeedersKnown: true, ObservedTitle: "Example & Test", ExpectedTitle: "Example & Test"},
			{Site: "orpheus", Status: "NO_RESOURCE", Attempted: true},
			{Site: "redacted", Status: "OK", Attempted: true, CandidateCount: 1, Exact: true, Seeders: 2, SeedersKnown: true, ObservedTitle: "Example & Test", ExpectedTitle: "Example & Test"},
		},
	}
}

func searchReviewJob(t *testing.T, repo *Repository, tasks *taskrepo.Repository, retryCount uint8) {
	t.Helper()
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	token = begin(t, tasks, "search:album1", "identify")
	complete := Complete{
		JobID: "search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "candidate",
		TaskState: "blocked", Message: "候选需要核对；尚未下载", Search: candidateReport(retryCount), NowNS: 4000,
	}
	if _, err := repo.Complete("search-block", token, complete); err != nil {
		t.Fatal(err)
	}
}

func TestReplayStaleSearchKeepsSameWorkflowAndBoundsRetries(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff-replay", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	for attempt := uint8(0); attempt < 3; attempt++ {
		claim, err := tasks.Claim(fmt.Sprintf("replay-claim-%d", attempt), "identify", fmt.Sprintf("owner-%d", attempt), int64(3100)+int64(attempt))
		if err != nil || claim.Task == nil {
			t.Fatalf("claim attempt %d: %+v %v", attempt, claim, err)
		}
		token = taskrepo.Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
		if _, err := tasks.Begin(fmt.Sprintf("replay-begin-%d", attempt), token, int64(3101)+int64(attempt)); err != nil {
			t.Fatalf("begin attempt %d: %v", attempt, err)
		}
		if _, err := repo.ReplayStaleSearch(fmt.Sprintf("replay-search-%d", attempt), token, int64(3102)+int64(attempt), "本地 CAS 冲突"); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		job, err := repo.Job("search:album1")
		if err != nil || job == nil || job.ActiveTaskID != "search:album1" || job.RecoveryRetryCount != attempt+1 || job.Stage != "searching" {
			t.Fatalf("attempt %d job=%+v err=%v", attempt, job, err)
		}
	}
	claim, err := tasks.Claim("replay-claim-exhausted", "identify", "owner-exhausted", 3300)
	if err != nil || claim.Task == nil {
		t.Fatalf("claim exhausted: %+v %v", claim, err)
	}
	token = taskrepo.Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
	if _, err := tasks.Begin("replay-begin-exhausted", token, 3301); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReplayStaleSearch("replay-search-exhausted", token, 3302, "本地 CAS 冲突"); err == nil {
		t.Fatal("stale replay exceeded its bounded retry budget")
	}
}

func TestRequeueTokenFallbackClosesBlockedNeteaseProcess(t *testing.T) {
	repo, tasks := fixture(t)
	if _, err := repo.Start("start-token-fallback", Start{
		JobID: "album1", BatchID: "pl-netease-590536461-test", Scene: "expand", Stage: "identifying",
		Label: "Artist - Album", Payload: []byte("{}"), NowNS: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff-token-fallback", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	token = begin(t, tasks, "search:album1", "identify")
	blockReport := candidateReport(0)
	blockReport.Scene = "expand"
	if _, err := repo.Complete("block-search", token, Complete{
		JobID: "search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "candidate",
		TaskState: "blocked", Message: "候选待核对", Search: blockReport, NowNS: 4000,
	}); err != nil {
		t.Fatal(err)
	}
	report := candidateReport(0)
	report.Scene = "expand"
	report.SelectedSite = "dicmusic"
	report.SelectedCandidateID = "dicmusic:11"
	if _, err := repo.Review("accept-process", "search:album1", ReviewDecision{
		Action: "accept", Site: "dicmusic", CandidateID: "dicmusic:11", Search: report,
		Plan: []byte(`{"root_job_id":"album1","query":"Artist Album","selected_site":"dicmusic","selected_candidate_id":"dicmusic:11"}`),
	}, 5000); err != nil {
		t.Fatal(err)
	}
	token = begin(t, tasks, "process:search:album1", "aux")
	if _, err := repo.Complete("block-token", token, Complete{
		JobID: "process:search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "token",
		TaskState: "blocked", ErrorClass: "REQUEST_UNKNOWN", Message: "令牌待对账", NowNS: 6000,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := repo.RequeueTokenFallback("token-fallback-test", "process:search:album1", 7000, "令牌结果不明，改用网易云回退")
	if err != nil || result.Job == nil || result.Job.Stage != "fallback" || result.Job.ReviewKind != "" || result.Job.ErrorClass != "" {
		t.Fatalf("job=%+v err=%v", result.Job, err)
	}
	queued, err := tasks.Get("process:search:album1")
	if err != nil || queued == nil || queued.State != "queued" || queued.Owner != "" {
		t.Fatalf("task=%+v err=%v", queued, err)
	}
}

func TestControlCancelsBlockedReviewNodeWithoutExternalReplay(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	result, err := repo.Control("cancel-blocked-review", "search:album1", "cancel", 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "cancelled" || result.Job.ReviewKind != "" {
		t.Fatalf("job=%+v err=%v", result.Job, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "cancelled" || !task.CancelRequested || task.CancelOutcome != "cancelled_after_blocked" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}
func TestReviewAcceptQueuesProcessWithSelectedCandidate(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	actions, err := repo.ReviewActions("search:album1")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if !action.Allowed {
			t.Fatal(action)
		}
	}
	report := candidateReport(0)
	report.SelectedSite = "dicmusic"
	report.SelectedCandidateID = "dicmusic:11"
	plan := []byte(`{"root_job_id":"album1","query":"Artist Album","selected_site":"dicmusic","selected_candidate_id":"dicmusic:11"}`)
	result, err := repo.Review("review-accept", "search:album1", ReviewDecision{Action: "accept", Site: "dicmusic", CandidateID: "dicmusic:11", Search: report, Plan: plan}, 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "completed" || result.Job.SearchAction != "queue_request" || result.Job.ProcessingJobID != "process:search:album1" {
		t.Fatalf("%+v %v", result.Job, err)
	}
	if len(result.Children) != 1 || result.Children[0].ID != "process:search:album1" || result.Children[0].Stage != "requesting" {
		t.Fatal(result.Children)
	}
	child, err := tasks.Get("process:search:album1")
	if err != nil || child == nil || child.State != "queued" || child.Command != cmdProcess || child.WorkflowJobID != "process:search:album1" {
		t.Fatalf("%+v %v", child, err)
	}
	payload, found, err := tasks.Data(child.ID, "arguments", 0)
	if err != nil || !found || string(payload) != string(plan) {
		t.Fatal(found, err, string(payload))
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "completed" {
		t.Fatalf("%+v %v", task, err)
	}
}

func TestReviewAcceptRejectsSelectionOutsideEvidence(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	report := candidateReport(0)
	report.SelectedSite = "orpheus"
	report.SelectedCandidateID = "orpheus:99"
	_, err := repo.Review("review-bad-accept", "search:album1", ReviewDecision{Action: "accept", Site: "orpheus", CandidateID: "orpheus:99", Search: report, Plan: []byte("{}")}, 5000)
	if err == nil {
		t.Fatal("an unobserved candidate must not classify as an exact match")
	}
	job, err := repo.Job("search:album1")
	if err != nil || job == nil || job.Stage != "needs_review" {
		t.Fatalf("%+v %v", job, err)
	}
}

// A process job in needs_review cannot be accepted: its candidate was
// already selected when it was queued. Accepting it would queue a second
// process job under it; retry or skip are the way out.
func TestReviewAcceptRefusesProcessJob(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	report := candidateReport(0)
	report.SelectedSite = "dicmusic"
	report.SelectedCandidateID = "dicmusic:11"
	plan := []byte(`{"root_job_id":"album1","query":"Artist Album","selected_site":"dicmusic","selected_candidate_id":"dicmusic:11"}`)
	if _, err := repo.Review("review-accept", "search:album1", ReviewDecision{Action: "accept", Site: "dicmusic", CandidateID: "dicmusic:11", Search: report, Plan: plan}, 5000); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, "process:search:album1", "aux")
	complete := Complete{
		JobID: "process:search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "candidate",
		TaskState: "blocked", Message: "尚未下载", NowNS: 6000,
	}
	if _, err := repo.Complete("process-block", token, complete); err != nil {
		t.Fatal(err)
	}
	actions, err := repo.ReviewActions("process:search:album1")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Action == "accept" && action.Allowed {
			t.Fatal("accept must not be offered on a process job", actions)
		}
	}
	_, err = repo.Review("review-process-accept", "process:search:album1", ReviewDecision{Action: "accept", Site: "dicmusic", CandidateID: "dicmusic:11", Search: report, Plan: plan}, 7000)
	if err == nil {
		t.Fatal("accept on a process job must be refused")
	}
	job, err := repo.Job("process:search:album1")
	if err != nil || job == nil || job.Stage != "needs_review" {
		t.Fatalf("%+v %v", job, err)
	}
}

func TestReviewRetryRequeuesSearchAndCountsIt(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	result, err := repo.Review("review-retry", "search:album1", ReviewDecision{Action: "retry"}, 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "searching" || result.Job.SearchRetryCount != 1 || result.Job.ReviewKind != "" {
		t.Fatalf("%+v %v", result.Job, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "queued" || task.Owner != "" {
		t.Fatalf("%+v %v", task, err)
	}
}

func TestReviewRetryPersistsBackgroundCooldown(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	retryAt := int64(30000)
	result, err := repo.Review("review-delayed-retry", "search:album1", ReviewDecision{Action: "retry", RetryAfterNS: retryAt}, 5000)
	if err != nil || result.Job == nil || result.Job.RetryAfterNS != retryAt || result.Job.Stage != "searching" {
		t.Fatalf("job=%+v err=%v", result.Job, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "queued" || task.NotBeforeNS != retryAt {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	before, err := tasks.Claim("claim-before-cooldown", "identify", "owner", retryAt-1)
	if err != nil || before.Task != nil {
		t.Fatalf("task was claimable before cooldown: %+v err=%v", before.Task, err)
	}
	after, err := tasks.Claim("claim-at-cooldown", "identify", "owner", retryAt)
	if err != nil || after.Task == nil || after.Task.ID != "search:album1" {
		t.Fatalf("task was not claimable after cooldown: %+v err=%v", after.Task, err)
	}
}

func TestReviewSkipAbandonsSearch(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	result, err := repo.Review("review-skip", "search:album1", ReviewDecision{Action: "skip"}, 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "skipped" || result.Job.SearchAction != "skip" {
		t.Fatalf("%+v %v", result.Job, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "completed" {
		t.Fatalf("%+v %v", task, err)
	}
}

func TestReviewRetryStartsAFreshCycleAfterThreeSearchRetries(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	for count := uint8(0); count < 3; count++ {
		token = begin(t, tasks, fmt.Sprintf("defer-%d", count), "identify")
		report := &searchdecision.Report{
			Scene: "playlist", IdentityConfirmed: true, RetryCount: count, Query: "Artist Album",
			Sites: []searchdecision.SiteReport{
				{Site: "open.cd", Status: "RATE_LIMITED", Attempted: true},
				{Site: "dicmusic", Status: "NO_RESOURCE", Attempted: true},
				{Site: "orpheus", Status: "NO_RESOURCE", Attempted: true},
				{Site: "redacted", Status: "NO_RESOURCE", Attempted: true},
			},
		}
		if _, err := repo.Complete(fmt.Sprintf("search-defer-%d", count), token, Complete{Edge: edgeDefer, NextStage: "searching", TaskState: "completed", Message: "重试", Search: report, NowNS: 4000 + int64(count)}); err != nil {
			t.Fatal(err)
		}
	}
	token = begin(t, tasks, "search:album1", "identify")
	if _, err := repo.Complete("search-block", token, Complete{JobID: "search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "candidate", TaskState: "blocked", Message: "候选需要核对", Search: candidateReport(3), NowNS: 9000}); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Job("search:album1")
	if err != nil || job == nil || job.SearchRetryCount != 3 {
		t.Fatalf("%+v %v", job, err)
	}
	actions, err := repo.ReviewActions("search:album1")
	if err != nil {
		t.Fatal(err)
	}
	retryAllowed := false
	for _, action := range actions {
		if action.Action == "retry" && action.Allowed {
			retryAllowed = true
		}
	}
	if !retryAllowed {
		t.Fatal("a reviewed retry must start a fresh bounded search cycle")
	}
	result, err := repo.Review("review-reset", "search:album1", ReviewDecision{Action: "retry"}, 9100)
	if err != nil || result.Job == nil || result.Job.Stage != "searching" || result.Job.SearchRetryCount != 0 {
		t.Fatalf("reviewed retry did not reset the retry cycle: %+v err=%v", result.Job, err)
	}
}

func TestReviewInterruptedSearchCanRetry(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	token = begin(t, tasks, "search:album1", "identify")
	if _, err := repo.Interrupt("interrupt-search", token, 4000, "检索已中断"); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Job("search:album1")
	if err != nil || job == nil || job.Stage != "needs_review" || job.ReviewKind != "interrupted" {
		t.Fatalf("%+v %v", job, err)
	}
	actions, err := repo.ReviewActions("search:album1")
	if err != nil {
		t.Fatal(err)
	}
	retryAllowed := false
	for _, action := range actions {
		if action.Action == "retry" {
			retryAllowed = action.Allowed
		} else if action.Allowed {
			t.Fatalf("unsafe interrupted action was allowed: %+v", action)
		}
	}
	if !retryAllowed {
		t.Fatalf("read-only search has no recovery action: %+v", actions)
	}
	result, err := repo.Review("review-interrupted", "search:album1", ReviewDecision{Action: "retry"}, 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "searching" || result.Job.RecoveryRetryCount != 0 {
		t.Fatalf("manual recovery failed: %+v %v", result, err)
	}
	if _, err := repo.Review("review-interrupted-skip", "search:album1", ReviewDecision{Action: "skip"}, 5000); err == nil {
		t.Fatal("an interrupted job cannot be skipped without reconciliation")
	}
}

func TestAbandonInterruptedSearchAfterAutomaticBudget(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff-abandon-search", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	token = begin(t, tasks, "search:album1", "identify")
	if _, err := repo.Interrupt("interrupt-abandon-search-0", token, 4000, "检索已中断"); err != nil {
		t.Fatal(err)
	}

	// Abandonment must not bypass the automatic recovery budget.
	if _, err := repo.AbandonInterrupted("abandon-search-early", "search:album1", 4100, "too early"); !errors.Is(err, ErrStale) {
		t.Fatalf("early abandon error = %v, want ErrStale", err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		result, err := repo.RecoverInterrupted(fmt.Sprintf("recover-abandon-search-%d", attempt), "search:album1", int64(4100+attempt*200))
		if err != nil || result.Job == nil || result.Job.RecoveryRetryCount != uint8(attempt) {
			t.Fatalf("recovery %d: result=%+v err=%v", attempt, result.Job, err)
		}
		claim, claimErr := tasks.Claim(fmt.Sprintf("claim-abandon-search-%d", attempt), "identify", fmt.Sprintf("owner-abandon-search-%d", attempt), int64(4210+attempt*200))
		if claimErr != nil || claim.Task == nil || claim.Task.ID != "search:album1" {
			t.Fatalf("claim %d: %+v %v", attempt, claim, claimErr)
		}
		token = taskrepo.Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
		if _, beginErr := tasks.Begin(fmt.Sprintf("begin-abandon-search-%d", attempt), token, int64(4220+attempt*200)); beginErr != nil {
			t.Fatalf("begin %d: %v", attempt, beginErr)
		}
		if _, err := repo.Interrupt(fmt.Sprintf("interrupt-abandon-search-%d", attempt), token, int64(4300+attempt*200), "检索再次中断"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.RecoverInterrupted("recover-abandon-search-exhausted", "search:album1", 5000); !errors.Is(err, ErrStale) {
		t.Fatalf("exhausted recovery error = %v, want ErrStale", err)
	}
	result, err := repo.AbandonInterrupted("abandon-search-exhausted", "search:album1", 5100, "自动恢复预算已用尽")
	if err != nil || result.Job == nil || result.Job.Stage != "skipped" || result.Job.ReviewKind != "" {
		t.Fatalf("job=%+v err=%v", result.Job, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "cancelled" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func TestReviewFailedJobRetries(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	token = begin(t, tasks, "search:album1", "identify")
	report := &searchdecision.Report{
		Scene: "playlist", IdentityConfirmed: true, RetryCount: 0, Query: "Artist Album",
		Sites: []searchdecision.SiteReport{
			{Site: "open.cd", Status: "NO_RESOURCE", Attempted: true},
			{Site: "dicmusic", Status: "NO_RESOURCE", Attempted: true},
			{Site: "orpheus", Status: "NO_RESOURCE", Attempted: true},
			{Site: "redacted", Status: "NO_RESOURCE", Attempted: true},
		},
	}
	if _, err := repo.Complete("search-fail", token, Complete{JobID: "search:album1", Edge: edgeTerminal, NextStage: "failed", TaskState: "failed", Message: "检索失败", Search: report, NowNS: 4000}); err != nil {
		t.Fatal(err)
	}
	result, err := repo.Review("review-failed", "search:album1", ReviewDecision{Action: "retry"}, 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "searching" {
		t.Fatalf("%+v %v", result.Job, err)
	}
}
func TestReviewMetadataRetryRunsIdentificationAgain(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("metadata-review", token, Complete{
		JobID: "album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "metadata",
		TaskState: "blocked", Message: "身份证据未确认", NowNS: 3000,
	}); err != nil {
		t.Fatal(err)
	}
	actions, err := repo.ReviewActions("album1")
	if err != nil {
		t.Fatal(err)
	}
	allowed := false
	for _, action := range actions {
		if action.Action == "retry" && action.Allowed {
			allowed = true
		}
	}
	if !allowed {
		t.Fatal("metadata retry was not offered", actions)
	}
	result, err := repo.Review("metadata-retry", "album1", ReviewDecision{Action: "retry"}, 4000)
	if err != nil || result.Job == nil || result.Job.Stage != "identifying" {
		t.Fatalf("%+v %v", result.Job, err)
	}
	if task, err := tasks.Get("album1"); err != nil || task == nil || task.State != "queued" {
		t.Fatalf("%+v %v", task, err)
	}
}

func TestInterruptedCancellationFinalizesWithoutPendingReview(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("cancel-handoff", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	token = begin(t, tasks, "search:album1", "identify")
	if _, err := repo.Control("cancel-running", "search:album1", "cancel", 3500); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Interrupt("cancel-interrupt", token, 4000, "进程退出；取消请求待确认"); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Job("search:album1")
	if err != nil || job == nil || !job.CancelRequested || job.Stage != "needs_review" {
		t.Fatalf("interrupted cancellation was not preserved: %+v err=%v", job, err)
	}
	result, err := repo.Control("cancel-finalize", "search:album1", "cancel", 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "cancelled" || !result.Job.CancelRequested {
		t.Fatalf("cancellation did not finalize: %+v err=%v", result.Job, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "cancelled" || task.CancelOutcome != "cancelled_after_interrupt" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func TestReviewRetryCountsTemporaryStreamingFallback(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	report := candidateReport(0)
	report.StreamingEnabled = true
	report.SourceFallback = true
	report.ExplicitFallback = true
	report.SourceFallbackConfirmed = true
	for i := range report.Sites {
		report.Sites[i] = searchdecision.SiteReport{Site: searchdecision.TrackerSites[i], Status: "NO_RESOURCE", Attempted: true}
	}
	plan := []byte(`{"root_job_id":"album1","query":"Artist Album"}`)
	accepted, err := repo.Review("fallback-accept", "search:album1", ReviewDecision{Action: "accept", Search: report, Plan: plan}, 5000)
	if err != nil || accepted.Children == nil || len(accepted.Children) != 1 || accepted.Children[0].Stage != "fallback" {
		t.Fatalf("accepted=%+v err=%v", accepted, err)
	}
	token := begin(t, tasks, "process:search:album1", "aux")
	if _, err := repo.Complete("fallback-temporary", token, Complete{
		JobID: "process:search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "candidate",
		TaskState: "blocked", Message: "缓存暂时不可用", NowNS: 6000,
	}); err != nil {
		t.Fatal(err)
	}
	retried, err := repo.Review("fallback-retry", "process:search:album1", ReviewDecision{Action: "retry"}, 7000)
	if err != nil || retried.Job == nil || retried.Job.Stage != "fallback" || retried.Job.SearchRetryCount != 1 {
		t.Fatalf("retried=%+v err=%v", retried.Job, err)
	}
}

func TestReviewRetriesFailedTaskUnderCompletedRoot(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	publishID := "stage:album1:publishing"
	if _, err := tasks.Enqueue("enqueue-publish-failed", taskrepo.Spec{
		ID: publishID, Command: cmdPublish, Lane: "publish", Priority: 1, CreatedNS: 1500,
	}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, publishID, "publish")
	if _, err := tasks.Finish("finish-publish-failed", token, taskrepo.Completion{
		State: "failed", Message: "library record did not verify", ErrorClass: "VERIFY_FAILED", NowNS: 3000,
	}, nil); err != nil {
		t.Fatal(err)
	}

	var condition dataengine.Condition
	var writes []dataengine.Mutation
	if err := repo.engine.View(func(view *dataengine.View) error {
		job, raw, err := readJob(view, "album1")
		if err != nil {
			return err
		}
		previous := append([]byte(nil), raw...)
		next := *job
		next.Stage = "completed"
		next.ActiveTaskID = publishID
		next.Message = "已入库并完成发布校验"
		updated, err := bump(next, 4000, true)
		if err != nil {
			return err
		}
		condition, writes, err = putJob(previous, updated)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ok, err := repo.engine.CompareAndSwap([]dataengine.Condition{condition}, writes)
	if err != nil || !ok {
		t.Fatalf("write completed root: ok=%v err=%v", ok, err)
	}

	actions, err := repo.ReviewActions("album1")
	if err != nil {
		t.Fatal(err)
	}
	allowed := false
	for _, action := range actions {
		if action.Action == "retry" {
			allowed = action.Allowed
		}
	}
	if !allowed {
		t.Fatalf("completed root with a failed active task did not offer retry: %+v", actions)
	}
	result, err := repo.Review("review-completed-failed", "album1", ReviewDecision{Action: "retry"}, 5000)
	if err != nil || result.Job == nil || result.Job.Stage != "publishing" {
		t.Fatalf("job=%+v err=%v", result.Job, err)
	}
	task, err := tasks.Get(publishID)
	if err != nil || task == nil || task.State != "queued" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}
