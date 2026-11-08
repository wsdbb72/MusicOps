package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

func TestMissingHandlerDoesNotCompleteTheWorkflow(t *testing.T) {
	repo, tasks := fixture(t)
	startPlaylist(t, repo)
	coordinator, err := scheduler.New(tasks, map[string]scheduler.Handler{}, scheduler.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.SetWorkflowHooks(scheduler.WorkflowHooks{
		Block: func(requestID string, token taskrepo.Token, nowNS int64) error {
			_, err := repo.BlockUnregistered(requestID, token, nowNS)
			return err
		},
		Interrupt: func(requestID string, token taskrepo.Token, nowNS int64, reason string) error {
			_, err := repo.Interrupt(requestID, token, nowNS, reason)
			return err
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		coordinator.Stop(ctx)
	})
	deadline := time.Now().Add(10 * time.Second)
	var job *Job
	for time.Now().Before(deadline) {
		job, err = repo.Job("album1")
		if err == nil && job != nil && job.Stage == "needs_review" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job == nil || job.Stage != "needs_review" || job.ErrorClass != "UNSUPPORTED_COMMAND" || job.SearchJobID != "" {
		t.Fatal(job, err)
	}
	task, err := tasks.Get("album1")
	if err != nil || task == nil || task.State != "blocked" || task.ErrorClass != "UNSUPPORTED_COMMAND" {
		t.Fatal(task, err)
	}
	search, err := tasks.Get("search:album1")
	if err != nil || search != nil {
		t.Fatal("missing handler created a search task", search, err)
	}
	children, err := repo.Children("album1", nil, 20)
	if err != nil || len(children.Jobs) != 0 {
		t.Fatal(children, err)
	}
}
