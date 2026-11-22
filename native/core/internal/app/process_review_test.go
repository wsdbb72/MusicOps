package app

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestProcessCandidateReviewRetriesAreBounded(t *testing.T) {
	for _, test := range []struct {
		name      string
		streaming bool
		wantStage string
		wantTask  string
	}{
		{name: "fallback after retry budget", streaming: true, wantStage: "fallback", wantTask: "queued"},
		{name: "skip after retry budget", streaming: false, wantStage: "skipped", wantTask: "completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			application, _, processID := seedProcessReviewJob(t, test.streaming, "candidate", "detail")
			base := time.Now().UnixNano()
			for attempt := 0; attempt < processCandidateReviewRetryLimit; attempt++ {
				recorder := postCode(t, application, "/v1/jobs/"+processID+"/review", map[string]any{"action": "ai"})
				if recorder.Code != http.StatusOK {
					t.Fatalf("attempt=%d code=%d body=%s", attempt, recorder.Code, recorder.Body.String())
				}
				inspection := inspect(t, application, processID)
				wantCount := uint8(attempt + 1)
				if inspection.Job.Stage != "requesting" || inspection.Job.ProcessRetryCount != wantCount || inspection.Task == nil || inspection.Task.State != "queued" {
					t.Fatalf("attempt=%d job=%+v task=%+v", attempt, inspection.Job, inspection.Task)
				}
				nowNS := base + int64((attempt+1)*1000)
				token := claimAndBeginWorkflowTask(t, application, processID+"-retry-"+strconv.Itoa(attempt), "aux", nowNS)
				if _, err := application.flows.Complete("block-process-"+processID+"-"+string(rune('a'+attempt)), token, workflow.Complete{
					JobID: processID, Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "candidate", TaskState: "blocked",
					ErrorClass: "detail", Message: "候选详情持续失败", NowNS: nowNS + 100,
				}); err != nil {
					t.Fatal(err)
				}
			}

			recorder := postCode(t, application, "/v1/jobs/"+processID+"/review", map[string]any{"action": "ai"})
			if recorder.Code != http.StatusOK {
				t.Fatalf("terminal code=%d body=%s", recorder.Code, recorder.Body.String())
			}
			inspection := inspect(t, application, processID)
			wantFallbackCount := uint8(0)
			if test.streaming {
				wantFallbackCount = 1
			}
			if inspection.Job.Stage != test.wantStage || inspection.Job.ProcessRetryCount != processCandidateReviewRetryLimit || inspection.Job.SearchRetryCount != wantFallbackCount || inspection.Task == nil || inspection.Task.State != test.wantTask {
				t.Fatalf("terminal job=%+v task=%+v", inspection.Job, inspection.Task)
			}
		})
	}
}
