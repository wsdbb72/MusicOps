package workflow

import (
	"errors"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

const downloadTestHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const downloadTestMeta = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func downloadInput() DownloadInput {
	return DownloadInput{
		Plan:          []byte(`{"root_job_id":"album1","job_id":"process:search:album1","site":"dicmusic","resource_id":"11","infohash":"` + downloadTestHash + `","free_confirmed":true}`),
		Site:          "dicmusic",
		ResourceID:    "11",
		InfoHash:      downloadTestHash,
		FreeConfirmed: true,
	}
}

// downloadReviewJob leaves a process job in needs_review with the given
// ReviewKind, after the acquire executor saved a torrent receipt (or not,
// when withReceipt is false). It mirrors TestReviewAcceptRefusesProcessJob.
func downloadReviewJob(t *testing.T, repo *Repository, tasks *taskrepo.Repository, kind string, withReceipt bool) {
	t.Helper()
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
		JobID: "process:search:album1", Edge: edgeTerminal, NextStage: "needs_review", ReviewKind: kind,
		TaskState: "blocked", Message: "尚未下载", NowNS: 6000,
	}
	if withReceipt {
		receipt := []byte(`{"site":"dicmusic","resource_id":"11","infohash":"` + downloadTestHash + `","metainfo_sha256":"` + downloadTestMeta + `","path":"/tmp/x.torrent"}`)
		complete.Evidence = []EvidenceInput{{ID: "receipt-1", Kind: "api", Source: "dicmusic", Payload: receipt}}
	}
	if _, err := repo.Complete("process-block", token, complete); err != nil {
		t.Fatal(err)
	}
}

func TestStartDownloadQueuesDownloadTask(t *testing.T) {
	repo, tasks := fixture(t)
	downloadReviewJob(t, repo, tasks, "candidate", true)
	result, err := repo.StartDownload("start-download", "process:search:album1", downloadInput(), 7000)
	if err != nil || result.Job == nil || result.Job.Stage != "downloading" || result.Job.ActiveTaskID != "download:process:search:album1" || result.Job.ReviewKind != "" {
		t.Fatalf("%+v %v", result.Job, err)
	}
	old, err := tasks.Get("process:search:album1")
	if err != nil || old == nil || old.State != "completed" {
		t.Fatalf("%+v %v", old, err)
	}
	download, err := tasks.Get("download:process:search:album1")
	if err != nil || download == nil || download.State != "queued" || download.Command != cmdDownload || download.Lane != "aux" || download.WorkflowJobID != "process:search:album1" {
		t.Fatalf("%+v %v", download, err)
	}
	payload, found, err := tasks.Data(download.ID, "arguments", 0)
	if err != nil || !found || string(payload) != string(downloadInput().Plan) {
		t.Fatal(found, err, string(payload))
	}
}

func TestStartDownloadRejectsNonCandidateKind(t *testing.T) {
	repo, tasks := fixture(t)
	downloadReviewJob(t, repo, tasks, "token", true)
	if _, err := repo.StartDownload("start-download", "process:search:album1", downloadInput(), 7000); err == nil {
		t.Fatal("a token-review job has no confirmed free download to start")
	}
	job, err := repo.Job("process:search:album1")
	if err != nil || job == nil || job.Stage != "needs_review" {
		t.Fatalf("%+v %v", job, err)
	}
}

func TestStartDownloadRejectsSearchJob(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	if _, err := repo.StartDownload("start-download", "search:album1", downloadInput(), 5000); err == nil {
		t.Fatal("a search job must not start a download")
	}
}

func TestStartDownloadRejectsWrongStage(t *testing.T) {
	repo, _ := fixture(t)
	startPlaylist(t, repo)
	if _, err := repo.StartDownload("start-download", "album1", downloadInput(), 2000); err == nil {
		t.Fatal("a job that is not blocked for review must not start a download")
	}
	if _, err := repo.StartDownload("start-download", "missing-job", downloadInput(), 2000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job error = %v, want ErrNotFound", err)
	}
}

func TestStartDownloadRejectsMissingReceipt(t *testing.T) {
	repo, tasks := fixture(t)
	// A streaming-fallback job also ends in needs_review/candidate, but it
	// never saved a torrent: there is nothing to download.
	downloadReviewJob(t, repo, tasks, "candidate", false)
	if _, err := repo.StartDownload("start-download", "process:search:album1", downloadInput(), 7000); err == nil {
		t.Fatal("a job with no saved torrent receipt must not start a download")
	}
	job, err := repo.Job("process:search:album1")
	if err != nil || job == nil || job.Stage != "needs_review" {
		t.Fatalf("%+v %v", job, err)
	}
}

func TestStartDownloadRejectsCancelledJob(t *testing.T) {
	repo, tasks := fixture(t)
	searchReviewJob(t, repo, tasks, 0)
	report := candidateReport(0)
	report.SelectedSite = "dicmusic"
	report.SelectedCandidateID = "dicmusic:11"
	plan := []byte(`{"root_job_id":"album1","query":"Artist Album","selected_site":"dicmusic","selected_candidate_id":"dicmusic:11"}`)
	if _, err := repo.Review("review-accept", "search:album1", ReviewDecision{Action: "accept", Site: "dicmusic", CandidateID: "dicmusic:11", Search: report, Plan: plan}, 5000); err != nil {
		t.Fatal(err)
	}
	// A blocked task cannot be cancelled, so cancel the process job while
	// it is still requesting: the download must refuse the cancelled job.
	if _, err := repo.Control("cancel-process", "process:search:album1", "cancel", 6000); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Job("process:search:album1")
	if err != nil || job == nil || job.Stage != "cancelled" {
		t.Fatalf("%+v %v", job, err)
	}
	if _, err := repo.StartDownload("start-download", "process:search:album1", downloadInput(), 7000); err == nil {
		t.Fatal("a cancelled job must not start a download")
	}
}

func TestStartDownloadRejectsBadInput(t *testing.T) {
	repo, tasks := fixture(t)
	downloadReviewJob(t, repo, tasks, "candidate", true)
	cases := []DownloadInput{
		{Plan: nil, Site: "dicmusic", ResourceID: "11", InfoHash: downloadTestHash, FreeConfirmed: true},
		{Plan: []byte(`not json`), Site: "dicmusic", ResourceID: "11", InfoHash: downloadTestHash, FreeConfirmed: true},
		{Plan: downloadInput().Plan, Site: "dicmusic", ResourceID: "11", InfoHash: downloadTestHash, FreeConfirmed: false},
		{Plan: downloadInput().Plan, Site: "dicmusic", ResourceID: "11", InfoHash: "xyz", FreeConfirmed: true},
		{Plan: downloadInput().Plan, Site: "dicmusic", ResourceID: "0", InfoHash: downloadTestHash, FreeConfirmed: true},
		{Plan: downloadInput().Plan, Site: "dicmusic", ResourceID: "11", InfoHash: downloadTestHash, FreeConfirmed: true},
	}
	// The last case is valid on its own; it is rejected here only because
	// the site does not match the stored receipt.
	cases[5].Site = "orpheus"
	for i, in := range cases {
		if _, err := repo.StartDownload("start-download-bad", "process:search:album1", in, 7000); err == nil {
			t.Fatalf("case %d accepted, want rejection", i)
		} else if i < 5 && !errors.Is(err, dataengine.ErrInvalidRequest) && !errors.Is(err, ErrStale) {
			t.Fatalf("case %d error = %v", i, err)
		}
	}
	// A wrong-site plan is a state mismatch, not a malformed request.
	if _, err := repo.StartDownload("start-download-site", "process:search:album1", cases[5], 7000); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong-site error = %v, want ErrStale", err)
	}
}

func TestStartDownloadIdempotent(t *testing.T) {
	repo, tasks := fixture(t)
	downloadReviewJob(t, repo, tasks, "candidate", true)
	first, err := repo.StartDownload("start-download-1", "process:search:album1", downloadInput(), 7000)
	if err != nil || first.Job == nil || first.Job.Stage != "downloading" {
		t.Fatalf("%+v %v", first.Job, err)
	}
	second, err := repo.StartDownload("start-download-2", "process:search:album1", downloadInput(), 8000)
	if err != nil || second.Job == nil || !second.Replayed || second.Job.Stage != "downloading" || second.Job.ActiveTaskID != "download:process:search:album1" {
		t.Fatalf("%+v %v", second.Job, err)
	}
	download, err := tasks.Get("download:process:search:album1")
	if err != nil || download == nil || download.State != "queued" {
		t.Fatalf("%+v %v", download, err)
	}
	// The same request id replays the first commit instead of writing again.
	replay, err := repo.StartDownload("start-download-1", "process:search:album1", downloadInput(), 7000)
	if err != nil || !replay.Replayed {
		t.Fatalf("%+v %v", replay, err)
	}
}

func TestDownloadFailedJobRetriesToDownloading(t *testing.T) {
	repo, tasks := fixture(t)
	downloadReviewJob(t, repo, tasks, "candidate", true)
	if _, err := repo.StartDownload("start-download", "process:search:album1", downloadInput(), 7000); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, "download:process:search:album1", "aux")
	if _, err := repo.Complete("download-fail", token, Complete{
		JobID: "process:search:album1", Edge: edgeTerminal, NextStage: "failed",
		TaskState: "failed", Message: "qBittorrent 不可用", ErrorClass: "QBITTORENT_UNAVAILABLE", NowNS: 8000,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Job("process:search:album1")
	if err != nil || job == nil || job.Stage != "failed" {
		t.Fatalf("%+v %v", job, err)
	}
	result, err := repo.Review("review-download-retry", "process:search:album1", ReviewDecision{Action: "retry"}, 9000)
	if err != nil || result.Job == nil || result.Job.Stage != "downloading" {
		t.Fatalf("%+v %v", result.Job, err)
	}
	task, err := tasks.Get("download:process:search:album1")
	if err != nil || task == nil || task.State != "queued" {
		t.Fatalf("%+v %v", task, err)
	}
}
