package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

const (
	autoReviewInterval = 30 * time.Second
	autoReviewBatch    = 40
	autoReviewCooldown = 15 * time.Minute
)

type autoReviewStats struct {
	Candidates int
	Accepted   int
	Held       int
}

// runAutoReview is the in-process counterpart to the "AI review all" button.
// It only enters the same handler after aiReviewEligible has admitted the safe
// review paths. A proven zero-spend account hold may retry without issuing a
// tracker request; consumed and uncertain token requests remain excluded.
func (a *App) runAutoReview(stop <-chan struct{}) {
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case <-stop:
		return
	case <-timer.C:
	}
	for {
		if a.pipelinePaused() {
			timer := time.NewTimer(time.Second)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		done := make(chan struct{})
		monitorDone := make(chan struct{})
		go func() {
			defer close(monitorDone)
			select {
			case <-stop:
				cancel()
			case <-done:
			}
		}()
		_ = a.autoReviewPass(ctx)
		close(done)
		<-monitorDone
		cancel()
		timer := time.NewTimer(autoReviewInterval)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *App) autoReviewPass(ctx context.Context) autoReviewStats {
	stats := autoReviewStats{}
	if a == nil || a.flows == nil {
		return stats
	}
	var cursor []byte
	for scanned := 0; scanned < 10000 && stats.Candidates < autoReviewBatch; {
		page, err := a.flows.List(cursor, 100)
		if err != nil || len(page.Jobs) == 0 {
			break
		}
		scanned += len(page.Jobs)
		for _, job := range page.Jobs {
			if stats.Candidates >= autoReviewBatch {
				break
			}
			if !a.aiReviewEligible(job) || a.autoReviewCoolingDown(job.ID, a.now()) {
				continue
			}
			stats.Candidates++
			code, _ := a.autoReviewJob(ctx, job.ID)
			if code == http.StatusOK {
				inspection, inspectErr := a.flows.Inspect(job.ID)
				if job.TaskType != "search" || job.ReviewKind == "metadata" || inspectErr == nil && inspection.Job.Stage == "needs_review" {
					// A retry can legitimately return to a metadata checkpoint.
					// Do not hammer the same decision every tick.
					stats.Held++
					a.setAutoReviewCooldown(job.ID, a.now())
				} else {
					stats.Accepted++
					a.clearAutoReviewCooldown(job.ID)
				}
			} else {
				stats.Held++
				a.setAutoReviewCooldown(job.ID, a.now())
			}
		}
		if len(page.Next) == 0 {
			break
		}
		cursor = page.Next
	}
	return stats
}

func (a *App) autoReviewJob(ctx context.Context, id string) (int, string) {
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+id+"/review", strings.NewReader(`{"action":"ai"}`))
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.SetPathValue("id", id)
	recorder := httptest.NewRecorder()
	a.reviewJob(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func (a *App) autoReviewCoolingDown(id string, now time.Time) bool {
	a.autoReviewMu.Lock()
	defer a.autoReviewMu.Unlock()
	retryAt := a.autoReviewRetryAt[id]
	return !retryAt.IsZero() && now.Before(retryAt)
}

func (a *App) setAutoReviewCooldown(id string, now time.Time) {
	a.autoReviewMu.Lock()
	defer a.autoReviewMu.Unlock()
	if a.autoReviewRetryAt == nil {
		a.autoReviewRetryAt = map[string]time.Time{}
	}
	a.autoReviewRetryAt[id] = now.Add(autoReviewCooldown)
}

func (a *App) clearAutoReviewCooldown(id string) {
	a.autoReviewMu.Lock()
	defer a.autoReviewMu.Unlock()
	delete(a.autoReviewRetryAt, id)
}
