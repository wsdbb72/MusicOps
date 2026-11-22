package app

import (
	"net/http"
	"testing"
)

func TestInterruptedLibraryProcessReviewsConvergeSafely(t *testing.T) {
	tests := []struct {
		name      string
		streaming bool
		wantStage string
		wantTask  string
	}{
		{name: "streaming fallback", streaming: true, wantStage: "fallback", wantTask: "queued"},
		{name: "skip without fallback", streaming: false, wantStage: "skipped", wantTask: "cancelled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application, _, processID := seedProcessReviewJob(t, test.streaming, "interrupted", "RECOVERY_REQUIRED")
			before := inspect(t, application, processID)
			if before.Job.Stage != "needs_review" || before.Job.ReviewKind != "interrupted" || before.Task == nil || before.Task.State != "recovery_required" {
				t.Fatalf("seed=%+v task=%+v", before.Job, before.Task)
			}
			if !application.aiReviewEligible(before.Job) {
				t.Fatal("interrupted process should be eligible for deterministic convergence")
			}
			recorder := postCode(t, application, "/v1/jobs/"+processID+"/review", map[string]any{"action": "ai"})
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
			}
			after := inspect(t, application, processID)
			if after.Job.Stage != test.wantStage || after.Job.ReviewKind != "" || after.Task == nil || after.Task.State != test.wantTask {
				t.Fatalf("job=%+v task=%+v", after.Job, after.Task)
			}
		})
	}
}
