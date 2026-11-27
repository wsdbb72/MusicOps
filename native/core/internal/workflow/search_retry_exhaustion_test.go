package workflow

import (
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
)

func TestTransientSearchRetryBudgetEndsInStreamingFallback(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := matchedReport()
	report.StreamingEnabled = true
	report.SelectedSite = ""
	report.SelectedCandidateID = ""
	report.Sites = fourSites(map[string]searchdecision.SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})

	current := token
	for retry := uint8(1); retry <= 3; retry++ {
		report.RetryCount = retry - 1
		result, err := repo.Complete("bounded-retry-"+string(rune('0'+retry)), current, Complete{JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: int64(4000) + int64(retry)*1000, Search: &report})
		if err != nil || result.Job.SearchRetryCount != retry || len(result.Followups) != 1 {
			t.Fatalf("retry %d: %+v err=%v", retry, result, err)
		}
		current = begin(t, tasks, result.Followups[0].ID, "identify")
	}

	report.RetryCount = 3
	final, err := repo.Complete("bounded-retry-exhausted", current, Complete{
		JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "fallback", TaskState: "completed", NowNS: 9000, Evidence: searchEvidence(), Search: &report,
	})
	if err != nil || len(final.Children) != 1 || final.Children[0].Stage != "fallback" || final.Search == nil || final.Search.Action != searchdecision.ActionFallback || final.Search.Reason != "transient_fallback" {
		t.Fatalf("exhausted retry must create the single bounded streaming fallback: %+v err=%v", final, err)
	}
}

func TestDeferredSearchKeepsOriginalRetryDeadline(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := matchedReport()
	report.SelectedSite = ""
	report.SelectedCandidateID = ""
	report.StreamingEnabled = true
	report.Sites = fourSites(map[string]searchdecision.SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})
	firstNow := int64(1000 * time.Second)
	first, err := repo.Complete("initial-backoff", token, Complete{
		JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: firstNow, Search: &report,
	})
	if err != nil || len(first.Followups) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	followup := begin(t, tasks, first.Followups[0].ID, "identify")
	dueNS := firstNow + int64(time.Duration(first.Job.SearchRetryAfterSec)*time.Second)
	deferred, err := repo.DeferSearchUntil("recover-early", followup, int64(1200*time.Second), dueNS, "检索退避尚未到期，已释放执行线程")
	if err != nil {
		t.Fatal(err)
	}
	if deferred.Job.SearchRetryAfterSec != 400 || len(deferred.Followups) != 0 {
		t.Fatalf("deferred job=%+v", deferred.Job)
	}
	task, err := tasks.Get(followup.ID)
	if err != nil || task == nil || task.State != "queued" || task.NotBeforeNS != dueNS || task.Owner != "" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func TestRateLimitRetryUsesSeparateExtendedBudget(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := matchedReport()
	report.SourceFallback = true
	report.StreamingEnabled = true
	report.SelectedSite = ""
	report.SelectedCandidateID = ""
	report.Sites = fourSites(map[string]searchdecision.SiteReport{"orpheus": {Status: "RATE_LIMITED", Attempted: true, RetryAfterSec: 3600}})

	current := token
	for retry := uint8(1); retry <= 3; retry++ {
		report.RetryCount = retry - 1
		result, err := repo.Complete("rate-limit-budget-"+string(rune('0'+retry)), current, Complete{
			JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: int64(4000) + int64(retry)*1000, Search: &report,
		})
		if err != nil || result.Job.SearchRetryCount != retry || len(result.Followups) != 1 {
			t.Fatalf("retry %d: %+v err=%v", retry, result, err)
		}
		current = begin(t, tasks, result.Followups[0].ID, "identify")
	}

	report.RetryCount = 3
	result, err := repo.Complete("rate-limit-extended", current, Complete{
		JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: 9000, Search: &report,
	})
	if err != nil || result.Job == nil || len(result.Followups) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Job.SearchRetryCount != 3 || result.Job.RateLimitRetryCount != 1 || result.Job.SearchRetryAfterSec != 3600 || result.Job.SearchAction != searchdecision.ActionRetry {
		t.Fatalf("job=%+v", result.Job)
	}
}
