package app

import (
	"net/http"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const processCandidateReviewRetryLimit = 2

type processReviewDisposition string

const (
	processReviewRetry    processReviewDisposition = "retry"
	processReviewFallback processReviewDisposition = "fallback"
	processReviewSkip     processReviewDisposition = "skip"
)

// processReviewDecisionFor bounds ordinary process-release checkpoints. These
// are not tracker spend results, so a small retry allowance is safe; after it
// is exhausted the job must either use the configured streaming fallback or
// settle instead of returning to the same review every cooldown cycle.
func processReviewDecisionFor(job workflow.Job, fallbackUsable bool) processReviewDisposition {
	if job.ProcessRetryCount < processCandidateReviewRetryLimit {
		return processReviewRetry
	}
	if fallbackUsable {
		return processReviewFallback
	}
	return processReviewSkip
}

func (a *App) reviewProcessCandidateCheckpoint(w http.ResponseWriter, requestID, id string, nowNS int64, job workflow.Job) {
	plan := a.tokenReviewPlan(job)
	decision := processReviewDecisionFor(job, tokenReviewFallbackUsable(job, plan))
	switch decision {
	case processReviewRetry:
		result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "retry"}, nowNS)
		a.writeReviewResult(w, result, err, "ai")
	case processReviewFallback:
		reason := "候选获取连续失败"
		if class := strings.TrimSpace(job.ErrorClass); class != "" {
			reason += "（" + class + "）"
		}
		if message := strings.TrimSpace(job.Message); message != "" {
			reason += "：" + message
		}
		reason += "；自动重试上限已用完，已切换流媒体回退"
		if runes := []rune(reason); len(runes) > 1000 {
			reason = string(runes[:1000])
		}
		result, err := a.flows.RequeueTokenFallback(requestID, id, nowNS, reason)
		a.writeReviewResult(w, result, err, "ai")
	default:
		result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "skip"}, nowNS)
		a.writeReviewResult(w, result, err, "ai")
	}
}
