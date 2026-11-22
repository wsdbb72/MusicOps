package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// reviewCandidateView is one stored search hit the reviewer can pick. It is
// built from the stored observation evidence only: the candidate was observed
// by the search executor, never invented by the review page.
type reviewCandidateView struct {
	Site        string   `json:"site"`
	CandidateID string   `json:"candidate_id"`
	Title       string   `json:"title"`
	Artists     []string `json:"artists"`
	Seeders     *int     `json:"seeders,omitempty"`
	GroupID     string   `json:"group_id,omitempty"`
	Year        int      `json:"year,omitempty"`
	Edition     string   `json:"edition,omitempty"`
	Format      string   `json:"format,omitempty"`
	Quality     string   `json:"quality,omitempty"`
	Exact       bool     `json:"exact"`
	Available   bool     `json:"available"`
	Free        bool     `json:"free,omitempty"`
	rank        candidateQualityRank
}

type reviewSiteView struct {
	Site           string `json:"site"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	CandidateCount int    `json:"candidate_count"`
	Exact          bool   `json:"exact"`
	Limited        bool   `json:"limited"`
}

// reviewObservations reads the stored per-site observations for one job. It
// returns the observations keyed by site plus the display rows. Evidence that
// is not a search observation (for example a torrent receipt) is skipped, not
// forced into the shape. Exactness is decided by the same summarizeSite the
// executor ran, using the stored search plan.
func (a *App) reviewObservations(inspection workflow.Inspection, plan SearchPlan) (map[string]trackersearch.Observation, []reviewSiteView, []reviewCandidateView, error) {
	observations := map[string]trackersearch.Observation{}
	for _, item := range inspection.Evidence {
		if item.Kind != "api" && item.Kind != "website" {
			continue
		}
		if !isTrackerSite(item.Source) {
			continue
		}
		payload, found, err := a.flows.EvidenceData(item.ID)
		if err != nil || !found {
			return nil, nil, nil, err
		}
		var observation trackersearch.Observation
		if err := json.Unmarshal(payload, &observation); err != nil || observation.Status == "" || observation.Source != item.Source {
			continue
		}
		// The evidence list is in digest order, not time order, and a
		// retried search leaves one observation per attempt: keep the
		// latest per site by the time the executor stamped on it.
		if current, ok := observations[item.Source]; ok && !observedAfter(observation, current) {
			continue
		}
		observations[item.Source] = observation
	}
	enabled := map[string]bool{}
	for _, site := range a.effectiveEnabledSites(plan) {
		enabled[site] = true
	}
	sites := make([]reviewSiteView, 0, len(searchdecision.TrackerSites))
	candidates := []reviewCandidateView{}
	for _, site := range searchdecision.TrackerSites {
		observation, ok := observations[site]
		if !ok {
			status := "DISABLED"
			if enabled[site] {
				// Enabled but never searched: the job predates the setting and
				// has not been retried yet.
				status = "NOT_SEARCHED"
			}
			sites = append(sites, reviewSiteView{Site: site, Status: status})
			continue
		}
		row, _, _ := summarizeSite(plan, site, observation)
		sites = append(sites, reviewSiteView{Site: site, Status: row.Status, Reason: observation.Reason, CandidateCount: row.CandidateCount, Exact: row.Exact, Limited: row.Limited})
		for _, candidate := range observation.Candidates {
			hit := releaseHit(candidate)
			if hit.Title == "" {
				continue
			}
			quality, available := candidateQuality(plan, site, candidate)
			match := reviewmatch.CompareSource(site, plan.ExpectedTitle, plan.ExpectedArtist, hit)
			candidates = append(candidates, reviewCandidateView{
				Site: site, CandidateID: candidate.CandidateID, Title: hit.Title, Artists: hit.Artists,
				Seeders: candidate.Seeders, GroupID: hit.GroupID, Year: hit.Year, Edition: hit.Edition,
				Format: hit.Format, Quality: candidateQualityLabel(quality, candidate), Exact: match.Pass,
				Available: available, Free: quality.Free > 0, rank: quality,
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Exact != candidates[j].Exact {
			return candidates[i].Exact
		}
		if candidates[i].Available != candidates[j].Available {
			return candidates[i].Available
		}
		if order := compareCandidateQuality(candidates[i].rank, candidates[j].rank); order != 0 {
			return order > 0
		}
		return candidates[i].Title < candidates[j].Title
	})
	if len(candidates) > 100 {
		candidates = candidates[:100]
	}
	return observations, sites, candidates, nil
}

// observedAfter reports whether a was observed after b. An empty stamp
// counts as the oldest, so an unstamped observation never replaces a
// stamped one.
func observedAfter(a, b trackersearch.Observation) bool {
	at, err := time.Parse(time.RFC3339Nano, a.ObservedAt)
	if err != nil {
		return false
	}
	bt, err := time.Parse(time.RFC3339Nano, b.ObservedAt)
	if err != nil {
		return true
	}
	return at.After(bt)
}

func isTrackerSite(site string) bool {
	for _, name := range searchdecision.TrackerSites {
		if name == site {
			return true
		}
	}
	return false
}

// reviewData returns everything the review page needs for one job: the job,
// what the machine blocked on, the stored observations and candidates, and
// which decisions are available with the reason each unavailable one cannot
// run.
func (a *App) reviewData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inspection, err := a.flows.Inspect(id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, workflow.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	actions, err := a.flows.ReviewActions(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
		return
	}
	var plan SearchPlan
	if inspection.Task != nil {
		if raw, found, err := a.tasks.Data(inspection.Task.ID, "arguments", 0); err == nil && found {
			_ = json.Unmarshal(raw, &plan)
		}
	}
	_, sites, candidates, err := a.reviewObservations(inspection, plan)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	expectedTitle, expectedArtist := inspection.Job.Label, ""
	if plan.ExpectedTitle != "" {
		expectedTitle, expectedArtist = plan.ExpectedTitle, plan.ExpectedArtist
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "job": publicJob(inspection.Job), "review_kind": inspection.Job.ReviewKind,
		"message": inspection.Job.Message, "expected_title": expectedTitle, "expected_artist": expectedArtist,
		"sites": sites, "candidates": candidates, "actions": actions,
		"ai_review_available": a.aiReviewEligible(inspection.Job),
	})
}

// rebuildReviewReport rebuilds the search report from the stored plan and the
// stored observations, with the user's selection applied. It classifies each
// site with the same summarizeSite the executor ran, so the rebuilt report is
// the report the executor would have built from the same observations.
func (a *App) rebuildReviewReport(job workflow.Job, plan SearchPlan, observations map[string]trackersearch.Observation, site, candidateID string) searchdecision.Report {
	report := searchdecision.Report{
		Scene: job.Scene, IdentityConfirmed: plan.Identity.IdentityConfirmed,
		StreamingEnabled: plan.StreamingEnabled, RetryCount: job.SearchRetryCount, Query: plan.Query,
		Sites:        make([]searchdecision.SiteReport, 0, len(searchdecision.TrackerSites)),
		SelectedSite: site, SelectedCandidateID: candidateID,
	}
	enabled := map[string]bool{}
	for _, name := range a.effectiveEnabledSites(plan) {
		enabled[name] = true
	}
	for _, name := range searchdecision.TrackerSites {
		observation, ok := observations[name]
		if !ok {
			status := "NOT_SEARCHED"
			if !enabled[name] {
				status = "DISABLED"
			}
			report.Sites = append(report.Sites, searchdecision.SiteReport{Site: name, Status: status, ExpectedTitle: plan.ExpectedTitle})
			continue
		}
		row, _, _ := summarizeSite(plan, name, observation)
		report.Sites = append(report.Sites, row)
	}
	return report
}

// reviewJob applies one human decision to a blocked or failed job.
func (a *App) reviewJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action      string `json:"action"`
		Site        string `json:"site"`
		CandidateID string `json:"candidate_id"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch body.Action {
	case "accept", "retry", "skip", "ai":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	id := r.PathValue("id")
	nowNS := time.Now().UnixNano()
	requestID := "review-" + id + "-" + body.Action + "-" + strconv.FormatInt(nowNS, 10)
	if body.Action == "ai" {
		a.reviewWithAI(w, r, id, requestID, nowNS)
		return
	}
	if body.Action == "accept" {
		a.reviewAccept(w, id, requestID, body.Site, body.CandidateID, nowNS)
		return
	}
	result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: body.Action}, nowNS)
	a.writeReviewResult(w, result, err, body.Action)
}

func (a *App) reviewWithAI(w http.ResponseWriter, r *http.Request, id, requestID string, nowNS int64) {
	inspection, err := a.flows.Inspect(id)
	if err == nil && inspection.Job.TaskType == "search" && inspection.Job.Stage == "completed" && inspection.Job.ProcessingJobID != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_processed": true})
		return
	}
	if err != nil || inspection.Job.Stage != "needs_review" || !a.aiReviewEligible(inspection.Job) {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	if inspection.Job.ReviewKind == "interrupted" {
		result, recoverErr := a.flows.RecoverInterrupted(requestID, id, nowNS)
		if errors.Is(recoverErr, workflow.ErrStale) && canResetInterruptedSearch(inspection.Job, inspection.Task) {
			// Automatic recovery is bounded. Once that budget is exhausted, the
			// AI review path may start one explicit read-only search cycle; this
			// still refuses token/download/acquire recovery.
			result, recoverErr = a.flows.Review(requestID+"-fresh", id, workflow.ReviewDecision{Action: "retry"}, nowNS)
		}
		a.writeReviewResult(w, result, recoverErr, "ai")
		return
	}
	if inspection.Job.ReviewKind == "token" {
		// aiReviewEligible admits only token checkpoints proven to have made no token download request.
		// Replay that hold through the ordinary bounded review path so acquire
		// re-checks the balance and enters the NetEase fallback when the account
		// still cannot fund an FL request. Unknown or consumed token requests
		// never reach this branch.
		result, retryErr := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "retry"}, nowNS)
		a.writeReviewResult(w, result, retryErr, "ai")
		return
	}
	if inspection.Job.TaskType != "search" {
		// Album expansion, metadata and interrupted jobs do not have a tracker
		// candidate to compare. Use the original identity and current source
		// policy to replay the job automatically.
		result, retryErr := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "retry"}, nowNS)
		a.writeReviewResult(w, result, retryErr, "ai")
		return
	}
	if inspection.Task == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	raw, found, err := a.tasks.Data(inspection.Task.ID, "arguments", 0)
	plan, planErr := decodePlan(raw)
	if err != nil || !found || planErr != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	observations, _, _, err := a.reviewObservations(inspection, plan)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	if a.reviewSearchFromEvidence(w, requestID, id, nowNS, inspection.Job, plan, observations) {
		return
	}
	model := a.reviewCompleter()
	if model == nil || a.reviews == nil {
		if inspection.Job.ReviewKind == "candidate" && isNeteasePlaylistFallback(inspection.Job.BatchID, plan) {
			a.acceptNeteasePlaylistFallback(w, id, requestID, nowNS, inspection.Job, plan, observations)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "AI_UNAVAILABLE", "message": "请先启用 AI 并保存 API 密钥"})
		return
	}
	pool := []reviewai.Candidate{}
	sites := map[string]string{}
	for _, site := range searchdecision.TrackerSites {
		obs := observations[site]
		_, _, items := summarizeSite(plan, site, obs)
		pool = append(pool, items...)
		if obs.Status != "OK" {
			continue
		}
		for _, hit := range obs.Candidates {
			if site == "open.cd" || hit.Seeders != nil && *hit.Seeders > 0 {
				sites[hit.CandidateID] = site
			}
		}
	}
	pool = boundedReviewCandidates(pool, plan.ExpectedTitle, plan.ExpectedArtist)
	advice := a.reviews.Advise(r.Context(), model, reviewai.Request{ExpectedTitle: plan.ExpectedTitle, ExpectedArtist: plan.ExpectedArtist, Candidates: pool})
	if site := sites[advice.CandidateID]; advice.Pass && site != "" && inspection.Job.ReviewKind == "candidate" {
		a.reviewAccept(w, id, requestID, site, advice.CandidateID, nowNS)
		return
	}
	if inspection.Job.ReviewKind == "candidate" && isNeteasePlaylistFallback(inspection.Job.BatchID, plan) {
		a.acceptNeteasePlaylistFallback(w, id, requestID, nowNS, inspection.Job, plan, observations)
		return
	}
	if advice.Class == "infrastructure" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "AI_CALL_FAILED", "reason": advice.Reason, "message": "AI 调用失败，候选仍待核对"})
		return
	}
	writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "AI_NO_CONFIRMED_MATCH", "reason": advice.Reason, "message": "AI 未能核实可下载的发行版"})
}

const reviewAICandidateLimit = 64

func boundedReviewCandidates(pool []reviewai.Candidate, expectedTitle, expectedArtist string) []reviewai.Candidate {
	if len(pool) <= reviewAICandidateLimit {
		return pool
	}
	type rankedCandidate struct {
		candidate reviewai.Candidate
		score     int
		index     int
	}
	seen := map[string]struct{}{}
	ranked := make([]rankedCandidate, 0, len(pool))
	wantTitle := normalizeKey(expectedTitle)
	wantArtists := splitPlaylistArtists(expectedArtist)
	for index, candidate := range pool {
		if candidate.ID == "" {
			continue
		}
		if _, exists := seen[candidate.ID]; exists {
			continue
		}
		seen[candidate.ID] = struct{}{}
		score := 0
		if wantTitle != "" && normalizeKey(candidate.Title) == wantTitle {
			score += 1000
		} else if wantTitle != "" && strings.Contains(normalizeKey(candidate.Title), wantTitle) {
			score += 500
		}
		for _, alias := range candidate.Aliases {
			if wantTitle != "" && normalizeKey(alias) == wantTitle {
				score += 450
				break
			}
		}
		if len(wantArtists) > 0 && playlistArtistsMatch(wantArtists, candidate.Artists) {
			score += 250
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Format), "FLAC") {
			score += 20
		}
		ranked = append(ranked, rankedCandidate{candidate: candidate, score: score, index: index})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].index < ranked[j].index
	})
	if len(ranked) > reviewAICandidateLimit {
		ranked = ranked[:reviewAICandidateLimit]
	}
	out := make([]reviewai.Candidate, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.candidate)
	}
	return out
}

// reviewSearchFromEvidence resolves the common no-candidate search checkpoint
// without asking the model to invent a release. A transient/contract failure
// starts a fresh bounded retry cycle; an exhausted but definitive search with
// streaming enabled queues the configured fallback. This is the same decision
// shape used by the executor, rebuilt from the persisted per-site observations.
func (a *App) reviewSearchFromEvidence(w http.ResponseWriter, requestID, id string, nowNS int64, job workflow.Job, plan SearchPlan, observations map[string]trackersearch.Observation) bool {
	if job.TaskType != "search" || job.ReviewKind != "search" {
		return false
	}
	for _, observation := range observations {
		if len(observation.Candidates) > 0 {
			return false
		}
	}
	report := a.rebuildReviewReport(job, plan, observations, "", "")
	report.SourceFallback = true
	report.ExplicitFallback = true
	decision, err := searchdecision.Decide(report)
	if err != nil {
		return false
	}
	if decision.Action == searchdecision.ActionRetry || decision.Action == searchdecision.ActionReviewSearch && (decision.Reason == "transient_exhausted" || decision.Reason == "contract_error") {
		result, retryErr := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "retry"}, nowNS)
		a.writeReviewResult(w, result, retryErr, "ai")
		return true
	}
	if decision.Action != searchdecision.ActionFallback {
		return false
	}
	plan.SelectedSite, plan.SelectedCandidateID = "", ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return true
	}
	result, acceptErr := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "accept", Search: &report, Plan: encoded}, nowNS)
	a.writeReviewResult(w, result, acceptErr, "ai")
	return true
}

func (a *App) acceptNeteasePlaylistFallback(w http.ResponseWriter, id, requestID string, nowNS int64, job workflow.Job, plan SearchPlan, observations map[string]trackersearch.Observation) {
	report := a.rebuildReviewReport(job, plan, observations, "", "")
	markNeteaseSourceFallback(&report)
	report.ExplicitFallback = true
	report.SourceFallbackConfirmed = true
	decision, err := searchdecision.Decide(report)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	if decision.Action == searchdecision.ActionRetry {
		result, retryErr := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "retry"}, nowNS)
		a.writeReviewResult(w, result, retryErr, "ai")
		return
	}
	if decision.Action != searchdecision.ActionFallback {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "AI_NO_CONFIRMED_MATCH", "reason": decision.Reason, "message": "检索证据还不能安全推进；仍保留在待核对"})
		return
	}
	plan.SelectedSite, plan.SelectedCandidateID = "", ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "accept", Search: &report, Plan: encoded}, nowNS)
	a.writeReviewResult(w, result, err, "ai")
}

// aiReviewEligible keeps single and batch AI review on the same safe set.
// A zero-spend token hold may safely retry: the account read was held or
// rate-limited, or no reservation formed, so no token download was sent.
// Consumed, confirmed and uncertain token actions still remain explicit or
// flow through the background token reconciler. Every other review can be
// replayed from its durable evidence without inventing a release.
func (a *App) aiReviewEligible(job workflow.Job) bool {
	if a == nil || job.Stage != "needs_review" {
		return false
	}
	if job.ReviewKind == "token" {
		// These classes are emitted before a token download request exists:
		// the account read was held or rate-limited, or the reservation never
		// formed. Replaying them only re-checks the ledger and current balance;
		// consumed, released and uncertain requests remain explicit/reconciled.
		switch job.ErrorClass {
		case "account_token_hold", "rate_limited", "token_pending":
			return true
		default:
			return false
		}
	}
	if job.ReviewKind == "interrupted" {
		task, err := a.tasks.Get(job.ActiveTaskID)
		return err == nil && interruptedReviewSafe(job, task)
	}
	// Search checkpoints first replay the persisted site evidence. This can
	// retry an expired rate limit or choose the configured fallback without a
	// model. Candidate comparisons still need the configured reviewer below.
	// Metadata and album jobs use the durable retry path too.
	if job.TaskType != "search" || job.ReviewKind == "search" || job.ReviewKind == "candidate" && strings.HasPrefix(job.BatchID, "pl-netease-") {
		return true
	}
	return a.reviewCompleter() != nil && a.reviews != nil
}

func canResetInterruptedSearch(job workflow.Job, task *taskrepo.Task) bool {
	return job.ReviewKind == "interrupted" && job.RecoveryRetryCount >= 3 && !job.CancelRequested &&
		task != nil && task.State == "recovery_required" && task.Command == workflow.CommandSearch && !task.CancelRequested
}

// interruptedReviewSafe identifies the interrupted paths that can be replayed
// without spending another tracker token. Downloads and tracker acquire tasks
// remain explicit-review only until their external effect is reconciled.
func interruptedReviewSafe(job workflow.Job, task *taskrepo.Task) bool {
	if task == nil || job.CancelRequested || task.CancelRequested {
		return false
	}
	switch task.Command {
	case workflow.CommandSearch, workflow.CommandIdentify, workflow.CommandSnapshot, workflow.CommandPublish:
		return true
	case workflow.CommandProcess:
		// A NetEase playlist process task has no safe in-place replay: it may
		// already have crossed the FL token boundary. RecoverInterrupted routes
		// every such task through the source-platform fallback, which is
		// idempotent and never sends a second token or ordinary tracker download.
		return strings.HasPrefix(job.BatchID, "pl-netease-")
	default:
		return false
	}
}

func (a *App) reviewAccept(w http.ResponseWriter, id, requestID, site, candidateID string, nowNS int64) {
	inspection, err := a.flows.Inspect(id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, workflow.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if inspection.Task == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "WORKFLOW_REJECTED", "error": "job has no active task"})
		return
	}
	raw, found, err := a.tasks.Data(inspection.Task.ID, "arguments", 0)
	if err != nil || !found {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE", "error": "the stored search plan cannot be read"})
		return
	}
	var plan SearchPlan
	if err := json.Unmarshal(raw, &plan); err != nil || plan.RootJobID == "" || plan.Query == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE", "error": "the stored search plan is incomplete"})
		return
	}
	observations, _, _, err := a.reviewObservations(inspection, plan)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	report := a.rebuildReviewReport(inspection.Job, plan, observations, site, candidateID)
	plan.SelectedSite = site
	plan.SelectedCandidateID = candidateID
	enriched, err := json.Marshal(plan)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "REVIEW_UNAVAILABLE"})
		return
	}
	result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: "accept", Site: site, CandidateID: candidateID, Search: &report, Plan: enriched}, nowNS)
	a.writeReviewResult(w, result, err, "accept")
}

func (a *App) writeReviewResult(w http.ResponseWriter, result workflow.Result, err error, action string) {
	if err != nil {
		status, code := http.StatusInternalServerError, "JOBS_UNAVAILABLE"
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			status, code = http.StatusNotFound, "NOT_FOUND"
		case errors.Is(err, dataengine.ErrInvalidRequest):
			status, code = http.StatusBadRequest, "INVALID_REQUEST"
		case errors.Is(err, workflow.ErrStale):
			status, code = http.StatusConflict, "WORKFLOW_REJECTED"
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": code, "error": err.Error()})
		return
	}
	job := result.Job
	if job == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
		return
	}
	// The search is over for good after a skip, so the per-search cookie and
	// the download authenticators can go. An accept keeps them: the process
	// child still needs the session cookie to read the torrent detail and
	// fetch the torrent, and the download authenticators with it. A retry
	// keeps the cookie for the same reason. Secrets that are never reviewed
	// stay in memory until the process restarts; they are never persisted.
	// An identification job holds the same session secrets and a skip ends
	// it just as finally.
	if action == "skip" && (job.TaskType == "search" || job.TaskType == "identification") {
		a.forget(job.RootID)
	}
	children := []jobView{}
	for _, child := range result.Children {
		children = append(children, publicJob(child))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": publicJob(*job), "stage": job.Stage, "children": children})
}

// batchJobs applies one control action to a list of jobs and reports the
// outcome per job. A job that refuses the action is reported, not silently
// skipped: the batch itself still returns 200 with the per-job results.
func (a *App) batchJobs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobIDs []string `json:"job_ids"`
		Action string   `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch body.Action {
	case "pause", "resume", "cancel", "retry":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if len(body.JobIDs) < 1 || len(body.JobIDs) > 50 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "batch needs 1 to 50 jobs"})
		return
	}
	seen := map[string]bool{}
	results := make([]map[string]any, 0, len(body.JobIDs))
	succeeded := 0
	for _, id := range body.JobIDs {
		if id == "" || seen[id] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "empty or duplicate job id"})
			return
		}
		seen[id] = true
		nowNS := time.Now().UnixNano()
		var job *workflow.Job
		var err error
		if body.Action == "retry" {
			result, reviewErr := a.flows.Review("batch-retry-"+id+"-"+strconv.FormatInt(nowNS, 10), id, workflow.ReviewDecision{Action: "retry"}, nowNS)
			err = reviewErr
			if err == nil {
				job = result.Job
			}
		} else {
			result, controlErr := a.flows.Control("batch-"+body.Action+"-"+id+"-"+strconv.FormatInt(nowNS, 10), id, body.Action, nowNS)
			err = controlErr
			if err == nil {
				job = result.Job
			}
		}
		if err != nil {
			code := "JOBS_UNAVAILABLE"
			switch {
			case errors.Is(err, workflow.ErrNotFound):
				code = "NOT_FOUND"
			case errors.Is(err, dataengine.ErrInvalidRequest):
				code = "INVALID_REQUEST"
			case errors.Is(err, workflow.ErrStale):
				code = "WORKFLOW_REJECTED"
			}
			results = append(results, map[string]any{"job_id": id, "ok": false, "code": code, "error": err.Error()})
			continue
		}
		succeeded++
		stage := ""
		if job != nil {
			stage = job.Stage
		}
		results = append(results, map[string]any{"job_id": id, "ok": true, "stage": stage})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": body.Action, "results": results, "succeeded": succeeded, "failed": len(results) - succeeded})
}

// preflightSite checks one site's login. The check is read-only: it never
// requests a download, spends a token or writes anything. A cookie supplied
// in the call is used for this one call only and is never stored; when the
// call supplies none, the saved cookie for the site is used instead.
func (a *App) preflightSite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Site      string `json:"site"`
		Cookie    string `json:"cookie"`
		Synthetic bool   `json:"synthetic"`
		BaseURL   string `json:"base_url"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Cookie, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if !isTrackerSite(body.Site) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "unknown site: " + body.Site})
		return
	}
	cookie, err := a.resolvePreflightCookie(body.Site, body.Cookie)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "no saved cookie for this site; paste a cookie or save one first"})
		return
	}
	if body.BaseURL != "" && !body.Synthetic {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "LIVE_SITE_NOT_CALLED"})
		return
	}
	solverURL, solverToken := a.solverTarget()
	observation, err := a.searcher.Preflight(r.Context(), trackersearch.Query{
		Site: body.Site, Cookie: cookie, Synthetic: body.Synthetic, BaseURL: body.BaseURL,
		// The login check is a read-only GET, so it can use the same browser
		// fallback as a search when the site answers with a challenge.
		SolverURL: solverURL, SolverToken: solverToken,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	balance := any(nil)
	if observation.TokenBalance != nil {
		balance = *observation.TokenBalance
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "site": body.Site, "status": observation.Status, "authenticated": observation.Authenticated,
		"token_balance": balance, "error_class": observation.ErrorClass, "reason": observation.Reason,
	})
}
