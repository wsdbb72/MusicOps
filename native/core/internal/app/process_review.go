package app

import (
	"net/http"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const processCandidateReviewRetryLimit = 2

type processReviewDisposition string

const (
	processReviewRetry    processReviewDisposition = "retry"
	processReviewFallback processReviewDisposition = "fallback"
	processReviewSkip     processReviewDisposition = "skip"
)

// processReviewDecisionFor bounds ordinary process-release checkpoints. A
// fallback child uses SearchRetryCount because that durable counter survives
// stage changes for process tasks; treating only ProcessRetryCount as the
// budget made decode failures leave review every 30 seconds forever.
func processReviewDecisionFor(job workflow.Job, fallbackUsable, alreadyFallback bool) processReviewDisposition {
	retries := job.ProcessRetryCount
	if alreadyFallback && job.SearchRetryCount > retries {
		retries = job.SearchRetryCount
	}
	if retries < processCandidateReviewRetryLimit {
		return processReviewRetry
	}
	if alreadyFallback {
		return processReviewSkip
	}
	if fallbackUsable {
		return processReviewFallback
	}
	return processReviewSkip
}

func (a *App) processCandidateUsesFallback(job workflow.Job) bool {
	if job.SearchAction == searchdecision.ActionFallback {
		return true
	}
	if a == nil || a.flows == nil || job.ParentJobID == "" {
		return false
	}
	parent, err := a.flows.Job(job.ParentJobID)
	return err == nil && parent != nil && parent.SearchAction == searchdecision.ActionFallback
}

func (a *App) reviewProcessCandidateCheckpoint(w http.ResponseWriter, requestID, id string, nowNS int64, job workflow.Job) {
	plan := a.tokenReviewPlan(job)
	fallbackUsable := tokenReviewFallbackUsable(job, plan)
	alreadyFallback := a.processCandidateUsesFallback(job)
	decision := processReviewDecisionFor(job, fallbackUsable, alreadyFallback)
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
