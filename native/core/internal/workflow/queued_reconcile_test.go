package workflow

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

func TestReconcileQueuedSupersededKeepsOnlyCurrentWorkflowTask(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	identify := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff-for-queued-reconcile", identify, handoff(3000)); err != nil {
		t.Fatal(err)
	}

	noDomain := func(*dataengine.View, *taskrepo.Task, *taskrepo.Task, []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		return nil, nil, nil, nil
	}
	create := func(requestID, id, jobID string) {
		t.Helper()
		_, err := tasks.CreateFollowup(requestID, taskrepo.Spec{ID: id, Command: cmdSearch, Lane: "identify", Priority: 1, CreatedNS: 3000}, []byte(`{"schema":1}`), jobID, nil, noDomain)
		if err != nil {
			t.Fatal(err)
		}
	}
	create("create-orphan-queued", "stale-orphan-search", "missing-job")
	create("create-duplicate-queued", "stale-duplicate-search", "search:album1")
	currentBefore, err := tasks.Get("search:album1")
	if err != nil || currentBefore == nil {
		t.Fatalf("read current task: %+v %v", currentBefore, err)
	}

	count, err := repo.ReconcileQueuedSuperseded("queued-reconcile", 4000)
	if err != nil || count != 2 {
		t.Fatalf("reconciled=%d err=%v", count, err)
	}
	current, err := tasks.Get("search:album1")
	if err != nil || current == nil || current.State != "queued" || current.CancelRequested || current.Revision != currentBefore.Revision {
		t.Fatalf("current task was changed: %+v %v", current, err)
	}
	for _, id := range []string{"stale-orphan-search", "stale-duplicate-search"} {
		task, err := tasks.Get(id)
		if err != nil || task == nil || task.State != "cancelled" || !task.CancelRequested || task.CancelOutcome != "cancelled_before_start" {
			t.Fatalf("stale task %s was not safely cancelled: %+v %v", id, task, err)
		}
	}
	job, err := repo.Job("search:album1")
	if err != nil || job == nil || job.ActiveTaskID != "search:album1" || job.Stage != "searching" {
		t.Fatalf("current workflow changed: %+v %v", job, err)
	}
}

func TestInterruptedRecoveryHasSeparateBudgetAndManualReset(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	identify := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("identify-recovery-budget", identify, handoff(3000)); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		token := begin(t, tasks, "search-budget-"+string(rune('a'+i)), "identify")
		if _, err := repo.Interrupt("interrupt-recovery-budget-"+string(rune('a'+i)), token, int64(4000+i*100), "执行器返回错误，等待核对外部影响：invalid data request"); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if _, err := repo.RecoverInterrupted("recover-budget-"+string(rune('a'+i)), "search:album1", int64(4500+i*100)); err != nil {
				t.Fatal(err)
			}
		}
	}

	job, err := repo.Job("search:album1")
	if err != nil || job == nil || job.RecoveryRetryCount != 3 || job.SearchRetryCount != 0 || job.ReviewKind != "interrupted" {
		t.Fatalf("setup job=%+v err=%v", job, err)
	}
	if _, err := repo.RecoverInterrupted("recover-budget-exhausted", "search:album1", 6000); !errors.Is(err, ErrStale) {
		t.Fatalf("automatic recovery ignored its budget: %v", err)
	}
	actions, err := repo.ReviewActions("search:album1")
	if err != nil {
		t.Fatal(err)
	}
	manualAllowed := false
	for _, action := range actions {
		if action.Action == "retry" && action.Allowed {
			manualAllowed = true
		}
	}
	if !manualAllowed {
		t.Fatalf("safe interrupted search has no manual recovery action: %+v", actions)
	}
	recovered, err := repo.Review("manual-recover-budget", "search:album1", ReviewDecision{Action: "retry"}, 7000)
	if err != nil || recovered.Job == nil || recovered.Job.Stage != "searching" || recovered.Job.RecoveryRetryCount != 0 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.State != "queued" || task.Owner != "" || task.Execution != "" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func TestReconcileClaimedKeepsCurrentWorkflowTask(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	identify := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff-current-claim", identify, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	claim, err := tasks.Claim("claim-current-workflow", "identify", "gate-owner", 4000)
	if err != nil || claim.Task == nil || claim.Task.ID != "search:album1" {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	skip, err := repo.ReconcileClaimedSuperseded("claim-current-check", *claim.Task, 4100)
	if err != nil || skip {
		t.Fatalf("current claim skip=%v err=%v", skip, err)
	}
	task, err := tasks.Get("search:album1")
	if err != nil || task == nil || task.Owner != "gate-owner" || task.Execution != "claimed" {
		t.Fatalf("current claim changed: %+v err=%v", task, err)
	}
}

func TestReconcileClaimedCancelsDuplicateBeforeBegin(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	identify := begin(t, tasks, "album1", "identify")
	if _, err := repo.Complete("handoff-stale-claim", identify, handoff(3000)); err != nil {
		t.Fatal(err)
	}
	noDomain := func(*dataengine.View, *taskrepo.Task, *taskrepo.Task, []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		return nil, nil, nil, nil
	}
	if _, err := tasks.CreateFollowup("create-stale-claim", taskrepo.Spec{ID: "stale-search-claim", Command: cmdSearch, Lane: "identify", Priority: 0, CreatedNS: 3100}, []byte("{}"), "search:album1", nil, noDomain); err != nil {
		t.Fatal(err)
	}
	claim, err := tasks.Claim("claim-stale-workflow", "identify", "gate-owner", 4000)
	if err != nil || claim.Task == nil || claim.Task.ID != "stale-search-claim" {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	skip, err := repo.ReconcileClaimedSuperseded("claim-stale-check", *claim.Task, 4100)
	if err != nil || !skip {
		t.Fatalf("stale claim skip=%v err=%v", skip, err)
	}
	task, err := tasks.Get("stale-search-claim")
	if err != nil || task == nil || task.State != "cancelled" || task.CancelOutcome != "cancelled_before_start" {
		t.Fatalf("stale claim=%+v err=%v", task, err)
	}
	job, err := repo.Job("search:album1")
	if err != nil || job == nil || job.ActiveTaskID != "search:album1" || job.Stage != "searching" {
		t.Fatalf("job changed: %+v err=%v", job, err)
	}
}
