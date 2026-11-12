package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
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
}

type reviewSiteView struct {
	Site           string `json:"site"`
	Status         string `json:"status"`
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
	sites := make([]reviewSiteView, 0, len(searchdecision.TrackerSites))
	candidates := []reviewCandidateView{}
	for _, site := range searchdecision.TrackerSites {
		observation, ok := observations[site]
		if !ok {
			sites = append(sites, reviewSiteView{Site: site, Status: "DISABLED"})
			continue
		}
		row, _, _ := summarizeSite(plan, site, observation)
		sites = append(sites, reviewSiteView{Site: site, Status: row.Status, CandidateCount: row.CandidateCount, Exact: row.Exact, Limited: row.Limited})
		for _, candidate := range observation.Candidates {
			hit := releaseHit(candidate)
			if hit.Title == "" {
				continue
			}
			candidates = append(candidates, reviewCandidateView{Site: site, CandidateID: candidate.CandidateID, Title: hit.Title, Artists: hit.Artists, Seeders: candidate.Seeders, GroupID: hit.GroupID, Year: hit.Year, Edition: hit.Edition, Format: hit.Format})
		}
		if len(candidates) > 100 {
			candidates = candidates[:100]
		}
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
	for _, name := range searchdecision.TrackerSites {
		observation, ok := observations[name]
		if !ok {
			report.Sites = append(report.Sites, searchdecision.SiteReport{Site: name, Status: "DISABLED", ExpectedTitle: plan.ExpectedTitle})
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
	case "accept", "retry", "skip":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	id := r.PathValue("id")
	nowNS := time.Now().UnixNano()
	requestID := "review-" + id + "-" + body.Action + "-" + strconv.FormatInt(nowNS, 10)
	if body.Action == "accept" {
		a.reviewAccept(w, id, requestID, body.Site, body.CandidateID, nowNS)
		return
	}
	result, err := a.flows.Review(requestID, id, workflow.ReviewDecision{Action: body.Action}, nowNS)
	a.writeReviewResult(w, result, err, body.Action)
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
