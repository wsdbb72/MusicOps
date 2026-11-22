package workflow

import "testing"

func TestInterruptedProcessCanFallbackWithoutReplaying(t *testing.T) {
	repo, tasks, processID := startNeteaseFallbackProcess(t, "lib-interrupted-fallback")
	held := begin(t, tasks, processID, "aux")
	if _, err := repo.Interrupt("interrupt-fallback", held, 5000, "old process exited"); err != nil {
		t.Fatal(err)
	}
	result, err := repo.RequeueTokenFallback("recover-fallback", processID, 6000, "switch to streaming")
	if err != nil || result.Job == nil || result.Job.Stage != "fallback" || result.Job.ReviewKind != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	task, err := tasks.Get(processID)
	if err != nil || task == nil || task.State != "queued" || task.Owner != "" || task.Execution != "" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func TestInterruptedProcessCanBeAbandonedWithoutReplaying(t *testing.T) {
	repo, tasks, processID := startNeteaseFallbackProcess(t, "lib-interrupted-abandon")
	held := begin(t, tasks, processID, "aux")
	if _, err := repo.Interrupt("interrupt-abandon", held, 5000, "old process exited"); err != nil {
		t.Fatal(err)
	}
	result, err := repo.AbandonInterrupted("abandon-interrupted", processID, 6000, "no fallback configured")
	if err != nil || result.Job == nil || result.Job.Stage != "skipped" || result.Job.ReviewKind != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	task, err := tasks.Get(processID)
	if err != nil || task == nil || task.State != "cancelled" || task.Owner != "" || task.Execution != "finished" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}
