package app

import (
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestMaintenanceRetryPointFollowsCompletedRootIntoDescendants(t *testing.T) {
	root := workflow.Job{ID: "root", Stage: "completed", SearchJobID: "search"}
	search := workflow.Job{ID: "search", Stage: "completed", ProcessingJobID: "process"}
	process := workflow.Job{ID: "process", Stage: "failed", UpdatedNS: 300}
	jobs := map[string]workflow.Job{"root": root, "search": search, "process": process}
	latest, retry, err := maintenanceRetryPoint(root, func(id string) (*workflow.Job, error) {
		job, ok := jobs[id]
		if !ok {
			return nil, nil
		}
		return &job, nil
	})
	if err != nil || !retry || latest.ID != "process" {
		t.Fatalf("latest=%+v retry=%v err=%v", latest, retry, err)
	}
}

func TestMaintenanceRetryPointKeepsLiveOrReviewWork(t *testing.T) {
	for _, stage := range []string{"searching", "requesting", "downloading", "needs_review"} {
		root := workflow.Job{ID: "root", Stage: "completed", ProcessingJobID: "child"}
		child := workflow.Job{ID: "child", Stage: stage}
		jobs := map[string]workflow.Job{"root": root, "child": child}
		latest, retry, err := maintenanceRetryPoint(root, func(id string) (*workflow.Job, error) {
			job := jobs[id]
			return &job, nil
		})
		if err != nil || retry || latest.Stage != stage {
			t.Fatalf("stage=%s latest=%+v retry=%v err=%v", stage, latest, retry, err)
		}
	}
}

func TestMaintenanceRetryPointDoesNotReplayCompletedOrBrokenChains(t *testing.T) {
	completed := workflow.Job{ID: "root", Stage: "completed", SearchJobID: "search"}
	search := workflow.Job{ID: "search", Stage: "completed"}
	jobs := map[string]workflow.Job{"root": completed, "search": search}
	if _, retry, err := maintenanceRetryPoint(completed, func(id string) (*workflow.Job, error) {
		job, ok := jobs[id]
		if !ok {
			return nil, nil
		}
		return &job, nil
	}); err != nil || retry {
		t.Fatalf("completed chain retry=%v err=%v", retry, err)
	}
	missing := workflow.Job{ID: "root", Stage: "completed", ProcessingJobID: "missing"}
	if _, retry, err := maintenanceRetryPoint(missing, func(string) (*workflow.Job, error) { return nil, nil }); err != nil || retry {
		t.Fatalf("missing child retry=%v err=%v", retry, err)
	}
}

func TestSkippedMaintenanceRetryUsesCooldown(t *testing.T) {
	updated := time.Unix(1_000, 0)
	skipped := workflow.Job{Stage: "skipped", UpdatedNS: updated.UnixNano()}
	if !maintenanceSkippedCoolingDown(skipped, updated.Add(time.Hour)) {
		t.Fatal("a skipped maintenance task was retried during its cooldown")
	}
	if maintenanceSkippedCoolingDown(skipped, updated.Add(libraryAutomationSkippedRetry)) {
		t.Fatal("a skipped maintenance task stayed blocked after its cooldown")
	}
	if maintenanceSkippedCoolingDown(workflow.Job{Stage: "failed", UpdatedNS: skipped.UpdatedNS}, updated.Add(time.Minute)) {
		t.Fatal("a failed task was treated as a cooling-down skip")
	}
}
