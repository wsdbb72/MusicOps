package workflow

import (
	"fmt"
	"testing"

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
