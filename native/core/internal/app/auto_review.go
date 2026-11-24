package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const (
	autoReviewInterval = 30 * time.Second
	autoReviewBatch    = 40
	autoReviewCooldown = 15 * time.Minute
)

type autoReviewStats struct {
	Candidates int `json:"candidates"`
	Accepted   int `json:"accepted"`
	Settled    int `json:"settled"`
	Held       int `json:"held"`
	Failed     int `json:"failed"`
}

type autoReviewRuntime struct {
	Enabled   bool
	Active    bool
	LastRunNS int64
	NextRunNS int64
	Last      autoReviewStats
}

// runAutoReview is the in-process counterpart to the "AI review all" button.
// It only enters the same handler after aiReviewEligible has admitted the safe
// review paths. Proven zero-spend token failures move to a durable cooldown;
// uncertain token effects are handed to balance reconciliation and never
// replayed.
func (a *App) runAutoReview(stop <-chan struct{}) {
	initialDelay := 8 * time.Second
	a.setAutoReviewRuntime(func(runtime *autoReviewRuntime) {
		*runtime = autoReviewRuntime{Enabled: true, NextRunNS: a.now().Add(initialDelay).UnixNano()}
	})
	defer a.setAutoReviewRuntime(func(runtime *autoReviewRuntime) {
		runtime.Enabled = false
		runtime.Active = false
		runtime.NextRunNS = 0
	})
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	select {
	case <-stop:
		return
	case <-timer.C:
	}
	for {
		if a.pipelinePaused() {
			timer := time.NewTimer(time.Second)
			a.setAutoReviewRuntime(func(runtime *autoReviewRuntime) { runtime.NextRunNS = a.now().Add(time.Second).UnixNano() })
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		a.setAutoReviewRuntime(func(runtime *autoReviewRuntime) {
			runtime.Active = true
			runtime.NextRunNS = 0
		})
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
		stats := a.autoReviewPass(ctx)
		close(done)
		<-monitorDone
		cancel()
		nextNS := a.now().Add(autoReviewInterval).UnixNano()
		a.setAutoReviewRuntime(func(runtime *autoReviewRuntime) {
			runtime.Active = false
			runtime.LastRunNS = a.now().UnixNano()
			runtime.NextRunNS = nextNS
			runtime.Last = stats
		})
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
	cursor := a.autoReviewCursor
	for scanned := 0; scanned < 10000 && stats.Candidates < autoReviewBatch; {
		page, err := a.flows.List(cursor, 100)
		if err != nil {
			stats.Failed++
			return stats
		}
		scanned += len(page.Jobs)
		for _, job := range page.Jobs {
			if stats.Candidates >= autoReviewBatch || ctx.Err() != nil {
				return stats
			}
			a.autoReviewCursor = workflow.ListCursor(job)
			if !a.aiReviewEligible(job) || a.autoReviewCoolingDown(job.ID, a.now()) {
				continue
			}
			stats.Candidates++
			code, body := a.autoReviewJob(ctx, job.ID)
			if code == http.StatusOK {
				inspection, inspectErr := a.flows.Inspect(job.ID)
				if inspectErr != nil || inspection.Job.Stage == "needs_review" {
					// Only a decision that still leaves the job at the human
					// checkpoint belongs in the held count. A token item can now
					// move to a durable delayed retry and is not still in review.
					stats.Held++
					a.setAutoReviewCooldown(job.ID, a.now())
				} else {
					stats.Accepted++
					if job.ReviewKind == "metadata" {
						a.setAutoReviewCooldown(job.ID, a.now())
					} else {
						a.clearAutoReviewCooldown(job.ID)
					}
				}
			} else if autoReviewNoMatch(code, body) {
				requestID := "auto-skip-" + job.ID + "-" + strconv.FormatInt(a.now().UnixNano(), 10)
				result, skipErr := a.flows.Review(requestID, job.ID, workflow.ReviewDecision{Action: "skip"}, a.now().UnixNano())
				if skipErr == nil && result.Job != nil && result.Job.Stage == "skipped" {
					stats.Settled++
					a.clearAutoReviewCooldown(job.ID)
				} else {
					stats.Held++
					a.setAutoReviewCooldown(job.ID, a.now())
				}
			} else {
				stats.Failed++
				stats.Held++
				a.setAutoReviewCooldown(job.ID, a.now())
			}
		}
		if len(page.Next) == 0 {
			a.autoReviewCursor = nil
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
	request.Header.Set("X-MusicOps-Auto-Review", "1")
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

func (a *App) setAutoReviewRuntime(update func(*autoReviewRuntime)) {
	if a == nil || update == nil {
		return
	}
	a.autoReviewMu.Lock()
	defer a.autoReviewMu.Unlock()
	update(&a.autoReviewRuntime)
}

// autoReviewHealth is the small liveness snapshot exposed by healthz and the
// counts-only jobs endpoint. It lets the UI distinguish an idle background
// reviewer from a stopped one and shows when the next pass will run.
func (a *App) autoReviewHealth() map[string]any {
	if a == nil {
		return map[string]any{"enabled": false, "active": false}
	}
	a.autoReviewMu.Lock()
	runtime := a.autoReviewRuntime
	a.autoReviewMu.Unlock()
	return map[string]any{
		"enabled": runtime.Enabled, "active": runtime.Active,
		"last_run_ns": runtime.LastRunNS, "next_run_ns": runtime.NextRunNS,
		"last_result": runtime.Last,
	}
}

func autoReviewNoMatch(code int, body string) bool {
	if code != http.StatusConflict {
		return false
	}
	var failure struct {
		Code string `json:"code"`
	}
	return json.Unmarshal([]byte(body), &failure) == nil && failure.Code == "AI_NO_CONFIRMED_MATCH"
}
