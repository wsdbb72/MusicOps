package app

import (
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const downloadFailureRecoveryDelay = 10 * time.Second

// runDownloadFailureRecovery gives old download failures one bounded replay
// after an upgrade. The replay re-enters the download executor, whose current
// failure path advances to streaming when the persisted plan permits it. The
// deterministic request id makes this a one-time migration: if the same row
// fails again, later starts do not queue it repeatedly.
func (a *App) runDownloadFailureRecovery(stop <-chan struct{}) {
	timer := time.NewTimer(downloadFailureRecoveryDelay)
	defer timer.Stop()
	select {
	case <-stop:
		return
	case <-timer.C:
	}
	_, _ = a.recoverDownloadFailures(stop)
}

func (a *App) recoverDownloadFailures(stop <-chan struct{}) (int, int) {
	if a == nil || a.flows == nil || a.tasks == nil {
		return 0, 0
	}
	scanned, requeued := 0, 0
	var cursor []byte
	for {
		select {
		case <-stop:
			return scanned, requeued
		default:
		}
		page, err := a.flows.List(cursor, 200)
		if err != nil {
			return scanned, requeued
		}
		for _, job := range page.Jobs {
			scanned++
			if job.Stage != "failed" || job.Paused || job.CancelRequested || !downloadFailureCanFallback(job.ErrorClass) {
				continue
			}
			task, err := a.tasks.Get(job.ActiveTaskID)
			if err != nil || task == nil || task.Command != workflow.CommandDownload || task.State != "failed" {
				continue
			}
			raw, found, err := a.tasks.Data(job.ID, "arguments", 0)
			if err != nil || !found {
				continue
			}
			plan, err := decodePlan(raw)
			if err != nil || !tokenReviewFallbackUsable(job, plan) {
				continue
			}
			requestID := "download-infra-fallback-" + job.ID
			result, err := a.flows.Review(requestID, job.ID, workflow.ReviewDecision{Action: "retry"}, a.now().UnixNano())
			if err == nil && result.Job != nil {
				requeued++
			}
		}
		if len(page.Next) == 0 {
			return scanned, requeued
		}
		cursor = page.Next
	}
}
