package workflow

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

func fixture(t *testing.T) (*Repository, *taskrepo.Repository) {
	t.Helper()
	engine, err := dataengine.Open(t.TempDir() + "/data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	tasks := taskrepo.New(engine)
	return New(engine, tasks), tasks
}

func playlist(now int64) Start {
	return Start{JobID: "album1", BatchID: "batch1", Scene: "playlist", Stage: "identifying", Label: "示例", SourceTrackIDs: []string{"track-1", "track-2"}, LibraryFileCount: 10, MatchedCount: 1, Payload: []byte("synthetic tracks"), NowNS: now}
}

func confirmed(id string, payload []byte) EvidenceInput {
	return EvidenceInput{ID: id, Kind: "api", Source: "musicbrainz", SubjectID: "release1", TracklistVerified: true, IdentityConfirmed: true, Payload: payload}
}

func startPlaylist(t *testing.T, repo *Repository) {
	t.Helper()
	result, err := repo.Start("start", playlist(1000))
	if err != nil || result.Job == nil || result.Job.Stage != "identifying" || result.Job.SourceTrackCount != 2 || result.Job.LibraryFileCount != 10 || result.Job.MatchedCount != 1 {
		t.Fatalf("%+v %v", result.Job, err)
	}
}

func begin(t *testing.T, tasks *taskrepo.Repository, id, lane string) taskrepo.Token {
	t.Helper()
	claim, err := tasks.Claim("claim-"+id, lane, "owner", 2000)
	if err != nil || claim.Task == nil {
		t.Fatalf("%+v %v", claim, err)
	}
	token := taskrepo.Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
	if _, err = tasks.Begin("begin-"+id, token, 2100); err != nil {
		t.Fatal(err)
	}
	return token
}

func handoff(now int64) Complete {
	return Complete{JobID: "album1", Edge: edgeQueueSearch, TaskState: "completed", Message: "识别完成", Result: []byte("identity"), Evidence: []EvidenceInput{confirmed("evidence-1", []byte("tracklist"))}, NowNS: now}
}

func closedError(in Inspection) error {
	switch in.Job.Stage {
	case "identifying", "snapshotting":
		if in.Job.SearchJobID != "" || len(in.Children) != 0 || len(in.Evidence) != 0 {
			return errors.New("successor or evidence visible before stage commit")
		}
		if in.Task != nil && (in.Task.State == "completed" || in.Task.State == "cancelled") {
			return errors.New("task reached a terminal state without the workflow")
		}
	case "completed":
		if in.Job.SearchJobID == "" || len(in.Children) != 1 || len(in.Evidence) == 0 || in.Task == nil || in.Task.State != "completed" || len(in.ChildTasks) != 1 {
			return errors.New("incomplete commit")
		}
		if in.Children[0].ID != in.Job.SearchJobID || in.ChildTasks[0].ID != in.Children[0].ActiveTaskID {
			return errors.New("successor edge does not match the task")
		}
		if in.Children[0].SourceTrackCount != in.Job.SourceTrackCount || in.Children[0].LibraryFileCount != in.Job.LibraryFileCount {
			return errors.New("playlist and library counts were collapsed")
		}
	case "cancelled":
		if len(in.Children) != 0 || in.Job.SearchJobID != "" {
			return errors.New("cancel created a successor")
		}
	}
	return nil
}

func TestStageEvidenceAndSuccessorCommitTogether(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	result, err := repo.Complete("handoff", token, handoff(3000))
	if err != nil || result.Replayed || result.Job.Stage != "completed" || len(result.Children) != 1 || result.Children[0].Stage != "searching" {
		t.Fatal(result, err)
	}
	body, found, err := repo.EvidenceData("evidence-1")
	if err != nil || !found || !bytes.Equal(body, []byte("tracklist")) {
		t.Fatal(found, err)
	}
	replay, err := repo.Complete("handoff", token, handoff(3000))
	if err != nil || !replay.Replayed || len(replay.Children) != 1 {
		t.Fatal(replay, err)
	}
	children, err := repo.Children("album1", nil, 20)
	if err != nil || len(children.Jobs) != 1 {
		t.Fatal(children, err)
	}
	search, err := tasks.Get("search:album1")
	if err != nil || search == nil || search.State != "queued" || search.Command != cmdSearch || search.WorkflowJobID != "search:album1" {
		t.Fatal(search, err)
	}
	inspected, err := repo.Inspect("album1")
	if err != nil {
		t.Fatal(err)
	}
	if err = closedError(inspected); err != nil {
		t.Fatal(err)
	}
	counts, err := tasks.Counts()
	if err != nil || counts.States["completed"] != 1 || counts.States["queued"] != 1 {
		t.Fatal(counts, err)
	}
}

func TestReplayRejectsADifferentIntent(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	changed := handoff(3000)
	changed.Evidence[0].Payload = []byte("other tracklist")
	if _, err := repo.Complete("handoff", token, changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	children, _ := repo.Children("album1", nil, 20)
	if len(children.Jobs) != 1 {
		t.Fatal("conflict duplicated a successor")
	}
}

func TestSuccessorCollisionRollsBackTheWholeStage(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	if _, err := tasks.Enqueue("seed", taskrepo.Spec{ID: "search:album1", Command: "identify_metadata", Lane: "identify", Priority: 1, CreatedNS: 1500}, []byte("occupied")); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, "album1", "identify")
	_, err := repo.Complete("collision", token, handoff(3000))
	if !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	job, _ := repo.Job("album1")
	task, _ := tasks.Get("album1")
	item, _ := repo.Evidence("evidence-1")
	if job.Stage != "identifying" || task.State != "running" || item != nil || job.SearchJobID != "" {
		t.Fatalf("partial commit job=%+v task=%+v evidence=%+v", job, task, item)
	}
	status, err := repo.Request("collision")
	if err != nil || status.State != "REJECTED" {
		t.Fatal(status, err)
	}
}

func TestEvidenceConflictRollsBackTheWholeStage(t *testing.T) {
	repo, tasks := fixture(t)
	started := playlist(1000)
	started.Evidence = []EvidenceInput{confirmed("evidence-1", []byte("original"))}
	if _, err := repo.Start("start", started); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, "album1", "identify")
	_, err := repo.Complete("collision", token, handoff(3000))
	if !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	job, _ := repo.Job("album1")
	task, _ := tasks.Get("album1")
	if job.Stage != "identifying" || task.State != "running" {
		t.Fatalf("%+v %+v", job, task)
	}
}

func TestConcurrentCompletionCreatesOneSuccessor(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	var wins atomic.Int32
	var readers sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				inspected, err := repo.Inspect("album1")
				if err != nil {
					t.Error(err)
					return
				}
				if err = closedError(inspected); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	var workers sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			_, err := repo.Complete("finish-"+id, token, handoff(3000))
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrStale) {
				t.Error(err)
			}
		}(id)
	}
	workers.Wait()
	close(stop)
	readers.Wait()
	if wins.Load() != 1 {
		t.Fatal("both completions committed")
	}
	inspected, err := repo.Inspect("album1")
	if err != nil {
		t.Fatal(err)
	}
	if err = closedError(inspected); err != nil {
		t.Fatal(err)
	}
}

func TestOldFenceAndOwnerCannotAdvanceTheWorkflow(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	claim, err := tasks.Claim("claim-first", "identify", "owner", 2000)
	if err != nil || claim.Task == nil {
		t.Fatal(claim, err)
	}
	stale := taskrepo.Token{ID: "album1", Owner: "owner", Fence: claim.Task.Fence}
	if _, err = tasks.ReleaseClaim("release", stale, 2050); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, "album1", "identify")
	if token.Fence == stale.Fence {
		t.Fatal("fence did not advance")
	}
	if _, err = repo.Complete("stale-fence", stale, handoff(3000)); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	wrong := token
	wrong.Owner = "other-owner"
	if _, err = repo.Complete("wrong-owner", wrong, handoff(3000)); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	job, _ := repo.Job("album1")
	if job.Stage != "identifying" || job.SearchJobID != "" {
		t.Fatal(job)
	}
	if _, err = repo.Complete("current", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	if _, err = tasks.Finish("bare", token, taskrepo.Completion{State: "completed", NowNS: 4000}, nil); !errors.Is(err, taskrepo.ErrWorkflowOwned) {
		t.Fatal(err)
	}
}

func TestCancelBlocksSuccessorsAndPausePropagates(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		repo, tasks := fixture(t)
		startPlaylist(t, repo)
		token := begin(t, tasks, "album1", "identify")
		if _, err := tasks.Control("bare", "album1", "cancel", 2200); !errors.Is(err, taskrepo.ErrWorkflowOwned) {
			t.Fatal(err)
		}
		controlled, err := repo.Control("cancel", "album1", "cancel", 2200)
		if err != nil || !controlled.Job.CancelRequested || controlled.Job.Stage != "identifying" {
			t.Fatal(controlled, err)
		}
		result, err := repo.Complete("handoff", token, handoff(3000))
		if err != nil || result.Job.Stage != "cancelled" || len(result.Children) != 0 || len(result.Followups) != 0 {
			t.Fatal(result, err)
		}
		if result.Task.CancelOutcome != "completed_with_cancel_request" {
			t.Fatal(result.Task)
		}
		inspected, err := repo.Inspect("album1")
		if err != nil {
			t.Fatal(err)
		}
		if err = closedError(inspected); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("pause", func(t *testing.T) {
		repo, tasks := fixture(t)
		startPlaylist(t, repo)
		token := begin(t, tasks, "album1", "identify")
		controlled, err := repo.Control("pause", "album1", "pause", 2200)
		if err != nil || !controlled.Job.Paused {
			t.Fatal(controlled, err)
		}
		result, err := repo.Complete("handoff", token, handoff(3000))
		if err != nil || result.Job.Stage != "completed" || len(result.Children) != 1 || !result.Children[0].Paused {
			t.Fatal(result, err)
		}
		if len(result.Followups) != 1 || result.Followups[0].State != "paused" || !result.Followups[0].PauseRequested {
			t.Fatal(result.Followups)
		}
		claim, err := tasks.Claim("next", "identify", "owner", 4000)
		if err != nil || claim.Task != nil {
			t.Fatal("paused search task was claimed", claim, err)
		}
	})
}

func TestAliasAndAIEvidenceCannotOpenSearch(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	ai := handoff(3000)
	ai.Evidence = []EvidenceInput{{ID: "ai-1", Kind: "ai", Source: "assistant", AliasLanguage: "zh", IdentityConfirmed: true, TracklistVerified: true, Payload: []byte("alias")}}
	if _, err := repo.Complete("ai", token, ai); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	alias := handoff(3000)
	alias.Evidence = []EvidenceInput{{ID: "alias-1", Kind: "website", Source: "catalog", AliasLanguage: "ja", TracklistVerified: true, Payload: []byte("page")}}
	if _, err := repo.Complete("alias", token, alias); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	synthetic := handoff(3000)
	synthetic.Evidence = []EvidenceInput{{ID: "fixture-1", Kind: "synthetic_fixture", Source: "fixture", ProductionVerified: true, Payload: []byte("demo")}}
	if _, err := repo.Complete("synthetic", token, synthetic); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	job, _ := repo.Job("album1")
	if job.Stage != "identifying" {
		t.Fatal(job)
	}
}

func TestDuplicatePlaylistIDsAndSameJobAdvance(t *testing.T) {
	repo, tasks := fixture(t)
	invalid := playlist(1000)
	invalid.SourceTrackIDs = []string{"track-1", "track-1"}
	if _, err := repo.Start("dup", invalid); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	if _, err := repo.Start("repair", Start{JobID: "repair1", BatchID: "batch1", Scene: "complete", Stage: "metadata", LibraryFileCount: 4, Payload: []byte("files"), NowNS: 1000}); err != nil {
		t.Fatal(err)
	}
	token := begin(t, tasks, "repair1", "aux")
	result, err := repo.Complete("advance", token, Complete{JobID: "repair1", Edge: edgeAdvance, NextStage: "validating", TaskState: "completed", NowNS: 3000, Result: []byte("ready")})
	if err != nil || result.Job.Stage != "validating" || result.Job.ActiveTaskID != "stage:repair1:validating" || len(result.Followups) != 1 {
		t.Fatal(result, err)
	}
	next, err := tasks.Get("stage:repair1:validating")
	if err != nil || next == nil || next.State != "queued" || next.Command != cmdProcess || next.Lane != "aux" || next.WorkflowJobID != "repair1" {
		t.Fatal(next, err)
	}
}

func TestInterruptRejectsTheOldFenceAndSearchHandoff(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	result, err := repo.Interrupt("stop", token, 3000, "执行已退出，等待核对执行记录")
	if err != nil || result.Job.Stage != "needs_review" || result.Job.ReviewKind != "interrupted" || result.Job.ErrorClass != "RECOVERY_REQUIRED" {
		t.Fatal(result, err)
	}
	task, err := tasks.Get("album1")
	if err != nil || task == nil || task.State != "recovery_required" || task.Fence == token.Fence {
		t.Fatal(task, err)
	}
	if _, err = repo.Complete("late", token, handoff(4000)); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	if _, err = tasks.Interrupt("bare", token, 5000, "不要绕过工作流"); !errors.Is(err, taskrepo.ErrWorkflowOwned) {
		t.Fatal(err)
	}
	search, _ := tasks.Get("search:album1")
	if search != nil {
		t.Fatal(search)
	}
}

func TestSearchHandoffCreatesProcessTaskWithoutRunningIt(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("identify", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	searchToken := begin(t, tasks, "search:album1", "identify")
	matched := matchedReport()
	result, err := repo.Complete("search", searchToken, Complete{JobID: "search:album1", Edge: edgeQueueProcess, NextStage: "requesting", TaskState: "completed", NowNS: 4000, Result: []byte("candidates"), Evidence: []EvidenceInput{{ID: "search-evidence", Kind: "catalog", Source: "redacted", Payload: []byte("observed candidates")}}, Search: &matched})
	if err != nil || result.Job.Stage != "completed" || len(result.Children) != 1 || result.Children[0].Stage != "requesting" {
		t.Fatal(result, err)
	}
	process, err := tasks.Get(result.Children[0].ID)
	if err != nil || process == nil || process.State != "queued" || process.Command != cmdProcess || process.Lane != "aux" {
		t.Fatal(process, err)
	}
}

func TestCommandsStayUnregisteredNames(t *testing.T) {
	if len(Commands) != 8 {
		t.Fatal(Commands)
	}
	for _, command := range Commands {
		if command == "" {
			t.Fatal("blank command")
		}
	}
}

func recordRelease(t *testing.T, repo *Repository, jobID, requestID string, now int64) Result {
	t.Helper()
	receipt := []byte(`{"schema":1,"root_id":"` + jobID + `"}`)
	result, err := repo.RecordRelease(RecordReleaseInput{
		JobID: jobID, RequestID: requestID,
		Evidence: EvidenceInput{ID: "rel-" + requestID, Kind: "catalog", Source: "local-library", SubjectID: jobID, TracklistVerified: true, Payload: receipt},
		Payload:  receipt, NowNS: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func completedAlbum(t *testing.T) (*Repository, *taskrepo.Repository) {
	t.Helper()
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff", token, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Job("album1")
	if err != nil || job == nil || job.Stage != "completed" {
		t.Fatalf("%+v %v", job, err)
	}
	return repo, tasks
}

func catalogEvidence(t *testing.T, repo *Repository, jobID string) []Evidence {
	t.Helper()
	inspection, err := repo.Inspect(jobID)
	if err != nil {
		t.Fatal(err)
	}
	var out []Evidence
	for _, item := range inspection.Evidence {
		if item.Kind == "catalog" && item.Source == "local-library" {
			out = append(out, item)
		}
	}
	return out
}

func TestRecordReleasePublishesAndQueuesPublishTask(t *testing.T) {
	repo, tasks := completedAlbum(t)
	result := recordRelease(t, repo, "album1", "record-1", 4000)
	if result.Job == nil || result.Job.Stage != "publishing" {
		t.Fatalf("%+v", result.Job)
	}
	if result.Task == nil || result.Task.ID != "stage:album1:publishing" {
		t.Fatalf("%+v", result.Task)
	}
	task, err := tasks.Get("stage:album1:publishing")
	if err != nil || task == nil || task.State != "queued" || task.Command != cmdPublish || task.Lane != "publish" || task.WorkflowJobID != "album1" {
		t.Fatalf("%+v %v", task, err)
	}
	if got := catalogEvidence(t, repo, "album1"); len(got) != 1 {
		t.Fatal(got)
	}
}

func TestRecordReleaseSecondTrackAppendsEvidenceOnly(t *testing.T) {
	repo, tasks := completedAlbum(t)
	recordRelease(t, repo, "album1", "record-1", 4000)
	recordRelease(t, repo, "album1", "record-2", 4100)
	job, err := repo.Job("album1")
	if err != nil || job == nil || job.Stage != "publishing" {
		t.Fatalf("%+v %v", job, err)
	}
	task, err := tasks.Get("stage:album1:publishing")
	if err != nil || task == nil || task.State != "queued" {
		t.Fatalf("%+v %v", task, err)
	}
	if got := catalogEvidence(t, repo, "album1"); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestRecordReleaseAfterPublishStaysCompleted(t *testing.T) {
	repo, tasks := completedAlbum(t)
	recordRelease(t, repo, "album1", "record-1", 4000)
	token := begin(t, tasks, "stage:album1:publishing", "publish")
	if _, err := repo.Complete("publish-1", token, Complete{
		Edge: edgeTerminal, NextStage: "completed", TaskState: "completed", Message: "verified",
		Evidence: []EvidenceInput{{ID: "publish-ev", Kind: "catalog", Source: "local-library", SubjectID: "album1", TracklistVerified: true, Payload: []byte("receipt")}},
		NowNS:    5000,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Job("album1")
	if err != nil || job == nil || job.Stage != "completed" {
		t.Fatalf("%+v %v", job, err)
	}
	// A late ingest is recorded as evidence; the finished stage is left
	// alone instead of reopening publishing with no task to complete it.
	recordRelease(t, repo, "album1", "record-3", 6000)
	job, err = repo.Job("album1")
	if err != nil || job == nil || job.Stage != "completed" {
		t.Fatalf("late ingest reopened publishing: %+v %v", job, err)
	}
	if got := catalogEvidence(t, repo, "album1"); len(got) != 3 {
		t.Fatal(got)
	}
}

func TestRecordReleaseRejectsFailedJob(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	token := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("fail-1", token, Complete{
		Edge: edgeTerminal, NextStage: "failed", TaskState: "failed", Message: "identify failed", NowNS: 3000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordRelease(RecordReleaseInput{
		JobID: "album1", RequestID: "record-1",
		Evidence: EvidenceInput{ID: "rel-record-1", Kind: "catalog", Source: "local-library", SubjectID: "album1", TracklistVerified: true, Payload: []byte("receipt")},
		Payload:  []byte("receipt"), NowNS: 4000,
	}); err == nil {
		t.Fatal("failed job accepted a release")
	}
	task, err := tasks.Get("stage:album1:publishing")
	if err != nil || task != nil {
		t.Fatal("rejected release left a publish task", task, err)
	}
}
