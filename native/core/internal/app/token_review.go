package app

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const tokenReviewRetryLimit = 3

type tokenReviewDisposition string

const (
	tokenReviewRetry    tokenReviewDisposition = "retry"
	tokenReviewFallback tokenReviewDisposition = "fallback"
	tokenReviewSkip     tokenReviewDisposition = "skip"
	tokenReviewWait     tokenReviewDisposition = "wait"
)

// tokenReviewDecision is deliberately independent of the AI model. Token
// checkpoints describe external effects, so a model cannot authorize another
// spend. Safe pre-spend failures get a bounded retry; an uncertain request is
// reconciled from balance evidence; everything that cannot be proved safe is
// moved to the configured streaming fallback or settled as skipped.
func tokenReviewDecisionFor(job workflow.Job, fallbackUsable bool) tokenReviewDisposition {
	class := strings.TrimSpace(job.ErrorClass)
	if class == "" || class == "detail" {
		normalized := candidateReadFailureClassText(job.Message, nil)
		if normalized != "detail" {
			class = normalized
		}
	}
	if tokenReviewRetrySafe(class) && job.TokenRetryCount < tokenReviewRetryLimit {
		return tokenReviewRetry
	}
	if fallbackUsable {
		return tokenReviewFallback
	}
	if class == "REQUEST_UNKNOWN" || class == "REQUESTING" || class == "UNKNOWN" || class == "token_uncertain" {
		// A background balance reconciler owns this state. It expires the
		// uncertain request without ever replaying usetoken, then calls this
		// resolver again with the terminal ledger verdict.
		return tokenReviewWait
	}
	return tokenReviewSkip
}

func tokenReviewRetrySafe(class string) bool {
	switch class {
	case "account_token_hold", "rate_limited", "token_pending",
		"balance_unknown", "balance_stale", "network", "parse", "read_error", "ledger_rejected":
		return true
	default:
		return false
	}
}

func tokenReviewFallbackUsable(job workflow.Job, plan SearchPlan) bool {
	if isNeteasePlaylistFallback(job.BatchID, plan) {
		return true
	}
	if !plan.StreamingEnabled {
		return false
	}
	switch job.Scene {
	case "playlist", "expand", "upgrade", "fill":
		return true
	default:
		return false
	}
}

func (a *App) tokenReviewPlan(job workflow.Job) SearchPlan {
	if a == nil || a.tasks == nil || job.ActiveTaskID == "" {
		return SearchPlan{}
	}
	raw, found, err := a.tasks.Data(job.ActiveTaskID, "arguments", 0)
	if err != nil || !found {
		return SearchPlan{}
	}
	var plan SearchPlan
	if json.Unmarshal(raw, &plan) != nil {
		return SearchPlan{}
	}
	return plan
}

func tokenReviewFallbackReason(class string) string {
	switch class {
	case "token_budget":
		return "令牌预算不足，未重复请求令牌；已自动改用流媒体回退"
	case "token_spent", "SPEND_CONFIRMED":
		return "该候选的令牌已确认占用或获取结果不可恢复；已自动改用流媒体回退，未重复使用令牌"
	case "REQUEST_UNKNOWN", "token_uncertain":
		return "令牌请求结果不明确；已自动改用流媒体回退，未重复使用令牌"
	default:
		return "令牌获取无法继续安全重试；已自动改用流媒体回退，未重复使用令牌"
	}
}

// reviewTokenCheckpoint is the automatic counterpart to a human pressing
// "AI review" on a token checkpoint. It never sends usetoken and never invokes
// the language model.
func (a *App) reviewTokenCheckpoint(w http.ResponseWriter, requestID, id string, nowNS int64, job workflow.Job) {
	plan := a.tokenReviewPlan(job)
	class := strings.TrimSpace(job.ErrorClass)
	if class == "" || class == "detail" {
		if normalized := candidateReadFailureClassText(job.Message, nil); normalized != "detail" {
			job.ErrorClass = normalized
		}
	}
	decision := tokenReviewDecisionFor(job, tokenReviewFallbackUsable(job, plan))
	switch decision {
	case tokenReviewRetry:
		result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "retry"}, nowNS)
		a.writeReviewResult(w, result, err, "ai")
	case tokenReviewFallback:
		result, err := a.flows.RequeueTokenFallback(requestID, id, nowNS, tokenReviewFallbackReason(job.ErrorClass))
		a.writeReviewResult(w, result, err, "ai")
	case tokenReviewWait:
		// This is a valid terminal response for the batch UI: the request was
		// accepted and deliberately held for balance evidence. HTTP 409 made a
		// healthy reconciliation appear as dozens of submission failures.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "held": true, "code": "TOKEN_RECONCILING",
			"message": "正在用余额变化核对令牌是否消耗；不会重复使用令牌，到期后会自动回退或跳过",
		})
	default:
		result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "skip"}, nowNS)
		a.writeReviewResult(w, result, err, "ai")
	}
}
