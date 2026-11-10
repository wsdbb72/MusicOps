package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

func searchEvidence() []EvidenceInput {
	return []EvidenceInput{{ID: "search-evidence", Kind: "catalog", Source: "redacted", Payload: []byte("observed candidates")}}
}

func fourSites(overrides map[string]searchdecision.SiteReport) []searchdecision.SiteReport {
	rows := make([]searchdecision.SiteReport, 0, len(searchdecision.TrackerSites))
	for _, site := range searchdecision.TrackerSites {
		row := searchdecision.SiteReport{Site: site, Status: "NO_RESOURCE", Attempted: true}
		if override, ok := overrides[site]; ok {
			override.Site = site
			row = override
		}
		rows = append(rows, row)
	}
	return rows
}

func matchedReport() searchdecision.Report {
	return searchdecision.Report{
		Scene: "playlist", IdentityConfirmed: true, SelectedSite: "redacted", SelectedCandidateID: "torrent-1",
		Sites: fourSites(map[string]searchdecision.SiteReport{"redacted": {Status: "OK", Attempted: true, CandidateCount: 1, Exact: true, ObservedTitle: "Kind of Blue", ExpectedTitle: "Kind of Blue", Seeders: 4, SeedersKnown: true}}),
	}
}

func openSearch(t *testing.T) (*Repository, *taskrepo.Repository, taskrepo.Token) {
	t.Helper()
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("identify", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	return repo, tasks, begin(t, tasks, "search:album1", "identify")
}

func TestSearchGateQueuesARequestAndRejectsADirectDownload(t *testing.T) {
	repo, tasks, token := openSearch(t)
	matched := matchedReport()
	download := Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "downloading", TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &matched}
	if _, err := repo.Complete("download-now", token, download); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	limited := matched
	limited.Sites = fourSites(map[string]searchdecision.SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})
	fallback := download
	fallback.NextStage = "fallback"
	fallback.Search = &limited
	if _, err := repo.Complete("fallback-now", token, fallback); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	ai := download
	ai.NextStage = "requesting"
	ai.Evidence = []EvidenceInput{{ID: "ai-only", Kind: "ai", Source: "assistant", Payload: []byte("alias")}}
	if _, err := repo.Complete("ai-download", token, ai); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	request := download
	request.NextStage = "requesting"
	request.Evidence = searchEvidence()
	request.Search = &matched
	result, err := repo.Complete("search", token, request)
	if err != nil || result.Job.Stage != "completed" || result.Job.SearchAction != searchdecision.ActionQueueRequest || len(result.Children) != 1 {
		t.Fatal(result, err)
	}
	if result.Children[0].Stage != "requesting" || result.Children[0].SearchAction != searchdecision.ActionQueueRequest || result.Search == nil || result.Search.Action != searchdecision.ActionQueueRequest {
		t.Fatal(result.Children, result.Search)
	}
	if result.Children[0].Message != "检索命中，等待获取；尚未下载、尚未入库" {
		t.Fatal(result.Children[0].Message)
	}
	replay, err := repo.Complete("search", token, request)
	if err != nil || !replay.Replayed || replay.Children[0].ID != result.Children[0].ID {
		t.Fatal(replay, err)
	}
	process, err := tasks.Get(result.Children[0].ID)
	if err != nil || process == nil || process.State != "queued" || process.Command != cmdProcess {
		t.Fatal(process, err)
	}
	if _, err = tasks.Finish("bare", taskrepo.Token{ID: process.ID, Owner: "owner", Fence: 1}, taskrepo.Completion{State: "completed", NowNS: 5000}, nil); !errors.Is(err, taskrepo.ErrWorkflowOwned) && !errors.Is(err, taskrepo.ErrStale) {
		t.Fatal(err)
	}
}

func TestTransientSearchDefersWithoutStreamingOrASecondJob(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := matchedReport()
	report.SelectedSite = ""
	report.SelectedCandidateID = ""
	report.StreamingEnabled = true
	report.Sites = fourSites(map[string]searchdecision.SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})
	early := Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "fallback", TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &report}
	if _, err := repo.Complete("stream", token, early); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	result, err := repo.Complete("retry", token, Complete{JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &report})
	if err != nil || result.Job.Stage != "searching" || result.Job.SearchRetryCount != 1 || result.Job.SearchRetryAfterSec != 600 || len(result.Children) != 0 || len(result.Followups) != 1 {
		t.Fatal(result, err)
	}
	if result.Followups[0].ID != "stage:search:album1:retry:1" || result.Followups[0].State != "queued" || result.Followups[0].Command != cmdSearch {
		t.Fatal(result.Followups)
	}
	if result.Job.ProcessingJobID != "" || result.Search == nil || result.Search.StreamingDeferred != true {
		t.Fatal(result.Job, result.Search)
	}
	replay, err := repo.Complete("retry", token, Complete{JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &report})
	if err != nil || !replay.Replayed || replay.Job.SearchRetryCount != 1 {
		t.Fatal(replay, err)
	}
	next := begin(t, tasks, "stage:search:album1:retry:1", "identify")
	if next.ID != "stage:search:album1:retry:1" {
		t.Fatal(next)
	}
	report.RetryCount = 1
	second, err := repo.Complete("retry-2", next, Complete{JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: 5000, Search: &report})
	if err != nil || second.Job.SearchRetryCount != 2 || second.Job.SearchRetryAfterSec != 1800 || second.Followups[0].ID != "stage:search:album1:retry:2" {
		t.Fatal(second, err)
	}
}

func TestAuthAndDisabledSitesDoNotOpenStreaming(t *testing.T) {
	repo, _, token := openSearch(t)
	auth := matchedReport()
	auth.StreamingEnabled = true
	auth.SelectedSite = ""
	auth.SelectedCandidateID = ""
	auth.Sites = fourSites(map[string]searchdecision.SiteReport{"dicmusic": {Status: "AUTH_FAILED", Attempted: true}})
	review := Complete{JobID: "search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "search", TaskState: "blocked", NowNS: 4000, Evidence: searchEvidence(), Search: &auth}
	result, err := repo.Complete("auth", token, review)
	if err != nil || result.Job.Stage != "needs_review" || result.Job.ReviewKind != "search" || result.Job.SearchAction != searchdecision.ActionReviewSearch || len(result.Children) != 0 {
		t.Fatal(result, err)
	}
	if result.Search == nil || result.Search.Reason != "auth_blocked" || result.Search.Classes["dicmusic"] != searchdecision.ClassAuth {
		t.Fatal(result.Search)
	}
	repo, _, token = openSearch(t)
	disabled := auth
	disabled.Sites = fourSites(map[string]searchdecision.SiteReport{
		"open.cd": {Status: "DISABLED"}, "dicmusic": {Status: "DISABLED"}, "orpheus": {Status: "DISABLED"}, "redacted": {Status: "DISABLED"},
	})
	fallback := Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "fallback", TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &disabled}
	if _, err = repo.Complete("disabled-stream", token, fallback); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	skipped := Complete{JobID: "search:album1", Edge: edgeTerminal, NextStage: "completed", TaskState: "completed", NowNS: 4000, Search: &disabled}
	if _, err = repo.Complete("mark-success", token, skipped); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	job, err := repo.Job("search:album1")
	if err != nil || job.Stage != "searching" {
		t.Fatal(job, err)
	}
}

func TestFallbackReviewRetriesTheStreamingWalk(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := matchedReport()
	report.StreamingEnabled = true
	report.ExplicitFallback = true
	report.SelectedSite = ""
	report.SelectedCandidateID = ""
	report.Sites = fourSites(map[string]searchdecision.SiteReport{"orpheus": {Status: "AUTH_REQUIRED", Attempted: true}})
	queued, err := repo.Complete("confirmed", token, Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "fallback", TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &report})
	if err != nil || len(queued.Children) != 1 || queued.Children[0].Stage != "fallback" {
		t.Fatal(queued, err)
	}
	childID := queued.Children[0].ID
	held := begin(t, tasks, childID, "aux")
	if held.ID != childID {
		t.Fatal(held.ID)
	}
	if _, err = repo.Complete("fallback-block", held, Complete{JobID: childID, Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: "candidate", TaskState: "blocked", Message: "回退需要核对；尚未下载、尚未入库", NowNS: 5000}); err != nil {
		t.Fatal(err)
	}
	actions, err := repo.ReviewActions(childID)
	if err != nil {
		t.Fatal(err)
	}
	allowed := false
	for _, action := range actions {
		if action.Action == "retry" && action.Allowed {
			allowed = true
		}
		if action.Action == "retry" && !action.Allowed && strings.Contains(action.Reason, "尚未接入") {
			t.Fatal(action)
		}
	}
	if !allowed {
		t.Fatal(actions)
	}
	result, err := repo.Review("review-fallback", childID, ReviewDecision{Action: "retry"}, 6000)
	if err != nil || result.Job == nil || result.Job.Stage != "fallback" {
		t.Fatalf("%+v %v", result.Job, err)
	}
	retried, err := tasks.Get(childID)
	if err != nil || retried == nil || retried.State != "queued" {
		t.Fatalf("%+v %v", retried, err)
	}
}

func TestExplicitFallbackIsQueuedOnlyAfterConfirmation(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := matchedReport()
	report.StreamingEnabled = true
	report.ExplicitFallback = true
	report.SelectedSite = ""
	report.SelectedCandidateID = ""
	report.Sites = fourSites(map[string]searchdecision.SiteReport{"orpheus": {Status: "AUTH_REQUIRED", Attempted: true}})
	result, err := repo.Complete("confirmed", token, Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "fallback", TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &report})
	if err != nil || len(result.Children) != 1 || result.Children[0].Stage != "fallback" || result.Children[0].SearchAction != searchdecision.ActionFallback {
		t.Fatal(result, err)
	}
	if result.Children[0].Message != "已排队流媒体回退；尚未下载、尚未入库" || result.Search == nil || result.Search.Reason != "review_confirmed_fallback" {
		t.Fatal(result.Children[0].Message, result.Search)
	}
	task, err := tasks.Get(result.Children[0].ID)
	if err != nil || task == nil || task.State != "queued" || task.Lane != "aux" {
		t.Fatal(task, err)
	}
}

func TestAliasResearchDefersAndDoesNotConfirmANewIdentity(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := matchedReport()
	report.SelectedSite = ""
	report.SelectedCandidateID = ""
	report.AIEnabled = true
	report.AIGrounded = true
	report.AICandidates = 1
	report.Query = "Blue"
	report.AIProposedQuery = "Kind of Blue"
	report.Sites = fourSites(nil)
	result, err := repo.Complete("research", token, Complete{JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: 4000, Search: &report})
	if err != nil || result.Job.Stage != "searching" || result.Job.AIResearchCount != 1 || result.Job.SearchAction != searchdecision.ActionResearch {
		t.Fatal(result, err)
	}
	if result.Followups[0].ID != "stage:search:album1:research:1" || result.Job.Message != "使用目录别名重新检索；别名未确认新身份" {
		t.Fatal(result.Followups, result.Job.Message)
	}
	evidence, err := repo.EvidenceFor("search:album1", nil, 20)
	if err != nil || len(evidence.Evidence) != 0 {
		t.Fatal(evidence, err)
	}
	next := begin(t, tasks, result.Followups[0].ID, "identify")
	report.AIResearchCount = 1
	if _, err = repo.Complete("again", next, Complete{JobID: "search:album1", Edge: edgeDefer, TaskState: "completed", NowNS: 5000, Search: &report}); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
}

func TestExhaustedTransientSearchRecoversWithoutADownload(t *testing.T) {
	repo, tasks, token := openSearch(t)
	report := searchdecision.Report{
		Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true, RetryCount: 3,
		Sites: fourSites(map[string]searchdecision.SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}}),
	}
	early := report
	early.RetryCount = 2
	if _, err := repo.Complete("too-soon", token, Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "fallback", TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &early}); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	result, err := repo.Complete("recover", token, Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "fallback", TaskState: "completed", NowNS: 4000, Evidence: searchEvidence(), Search: &report})
	if err != nil || len(result.Children) != 1 || result.Children[0].Stage != "fallback" || result.Children[0].SearchAction != searchdecision.ActionFallback {
		t.Fatal(result, err)
	}
	if result.Children[0].Message != "已排队流媒体回退；尚未下载、尚未入库" || result.Search == nil || result.Search.Reason != "transient_recovered" {
		t.Fatal(result.Children[0].Message, result.Search)
	}
	task, err := tasks.Get(result.Children[0].ID)
	if err != nil || task == nil || task.State != "queued" {
		t.Fatal(task, err)
	}
}
