package app

import (
	"testing"

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
