package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// SearchPlan is the durable, secret-free description of one tracker read.
type SearchPlan struct {
	RootJobID           string   `json:"root_job_id"`
	Query               string   `json:"query"`
	ExpectedTitle       string   `json:"expected_title"`
	ExpectedArtist      string   `json:"expected_artist,omitempty"`
	EnabledSites        []string `json:"enabled_sites"`
	StreamingEnabled    bool     `json:"streaming_enabled"`
	Synthetic           bool     `json:"synthetic"`
	BaseURL             string   `json:"base_url,omitempty"`
	Budget              int      `json:"budget,omitempty"`
	SafetyMargin        int      `json:"safety_margin,omitempty"`
	SelectedSite        string   `json:"selected_site,omitempty"`
	SelectedCandidateID string   `json:"selected_candidate_id,omitempty"`
	Identity            Identity `json:"identity"`
}

// Identity is caller-supplied catalogue evidence. AI cannot confirm it.
type Identity struct {
	ID                string `json:"evidence_id"`
	Kind              string `json:"kind"`
	Source            string `json:"source"`
	SubjectID         string `json:"subject_id,omitempty"`
	TracklistVerified bool   `json:"tracklist_verified"`
	IdentityConfirmed bool   `json:"identity_confirmed"`
	Payload           string `json:"payload"`
}

func (a *App) identify(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	plan, err := decodePlan(payload)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	token := execution.Token()
	evidence := plan.evidence()
	_, err = a.flows.Complete(intentID("identify", token.ID, token.Fence), token, workflow.Complete{
		Edge: workflow.EdgeQueueSearch, TaskState: "completed", Evidence: []workflow.EvidenceInput{evidence},
		Result: payload, Message: "身份证据已接收，等待四站只读检索；尚未下载", NowNS: a.now().UnixNano(),
	})
	if err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: "已交给检索"}, nil
}

func (a *App) search(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	plan, err := decodePlan(payload)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	token := execution.Token()
	task, err := a.tasks.Get(token.ID)
	if err != nil || task == nil {
		return scheduler.Outcome{}, errors.New("search task disappeared")
	}
	job, err := a.flows.Job(task.WorkflowJobID)
	if err != nil || job == nil {
		return scheduler.Outcome{}, errors.New("search job disappeared")
	}
	if wait := RetryRemaining(job.UpdatedNS, job.SearchRetryAfterSec, a.now()); wait > 0 {
		if err = a.wait(ctx, wait); err != nil {
			return scheduler.Outcome{}, err
		}
		if err = a.cancelled(ctx, execution); err != nil {
			return scheduler.Outcome{}, err
		}
	}
	report, evidence, adviseClass, err := a.observe(ctx, execution, *job, plan, token.ID)
	if errors.Is(err, scheduler.ErrCancelled) {
		return scheduler.Outcome{}, a.stop(execution)
	}
	if err != nil {
		return scheduler.Outcome{}, err
	}
	decision, err := searchdecision.Decide(report)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	complete, err := completion(decision)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if adviseClass == "infrastructure" && (decision.Action == searchdecision.ActionReviewSearch || decision.Action == searchdecision.ActionReviewCandidate) {
		complete.Message = "检索服务暂时没有完成自动核对；尚未下载"
	}
	complete.Evidence = evidence
	complete.Result = payload
	if complete.Edge == workflow.EdgeQueueProcess {
		plan.SelectedSite = report.SelectedSite
		plan.SelectedCandidateID = report.SelectedCandidateID
		if enriched, err := json.Marshal(plan); err == nil {
			complete.Result = enriched
		}
	}
	complete.Search = &report
	complete.NowNS = a.now().UnixNano()
	if _, err = a.flows.Complete(intentID("search", token.ID, token.Fence), token, complete); err != nil {
		return scheduler.Outcome{}, err
	}
	if complete.Edge != workflow.EdgeDefer && complete.Edge != workflow.EdgeQueueProcess {
		a.forget(plan.RootJobID)
	}
	return scheduler.Outcome{State: complete.TaskState, Message: complete.Message}, nil
}

func (a *App) observe(ctx context.Context, execution *scheduler.Execution, job workflow.Job, plan SearchPlan, taskID string) (searchdecision.Report, []workflow.EvidenceInput, string, error) {
	enabled := map[string]bool{}
	for _, site := range plan.EnabledSites {
		enabled[site] = true
	}
	report := searchdecision.Report{
		Scene: job.Scene, IdentityConfirmed: plan.Identity.IdentityConfirmed, StreamingEnabled: plan.StreamingEnabled,
		RetryCount: job.SearchRetryCount, Query: plan.Query, Sites: make([]searchdecision.SiteReport, 0, 4),
	}
	evidence := []workflow.EvidenceInput{}
	var picks []identityPick
	var pool []reviewai.Candidate
	for _, site := range searchdecision.TrackerSites {
		row := searchdecision.SiteReport{Site: site, ExpectedTitle: plan.ExpectedTitle}
		if !enabled[site] {
			row.Status = "DISABLED"
			report.Sites = append(report.Sites, row)
			continue
		}
		phase := "search_" + strings.ReplaceAll(site, ".", "_")
		if err := execution.Checkpoint(ctx, phase, 0, site); err != nil {
			return report, nil, "", err
		}
		observation, err := a.searcher.Search(ctx, trackersearch.Query{
			Site: site, Text: plan.Query, Cookie: a.secret(plan.RootJobID), Synthetic: plan.Synthetic, BaseURL: plan.BaseURL,
		})
		if err != nil {
			return report, nil, "", err
		}
		if observation.Status == "OK" && !observation.SearchComplete && observation.Pages != nil && observation.Page > 0 && *observation.Pages > observation.Page {
			next, nextErr := a.searcher.Search(ctx, trackersearch.Query{
				Site: site, Text: plan.Query, Page: observation.Page + 1, Cookie: a.secret(plan.RootJobID), Synthetic: plan.Synthetic, BaseURL: plan.BaseURL,
			})
			if nextErr == nil && next.Status == "OK" {
				observation.Candidates = append(observation.Candidates, next.Candidates...)
			}
		}
		row.Status = observation.Status
		row.Attempted = true
		row.CandidateCount = len(observation.Candidates)
		row.Limited = observation.Status == "OK" && !observation.SearchComplete
		var seeded *identityPick
		for _, candidate := range observation.Candidates {
			hit := releaseHit(candidate)
			pool = append(pool, reviewai.Candidate{ID: hit.ID, Title: hit.Title, Artists: hit.Artists, Aliases: hit.Aliases, Edition: hit.Edition})
			if !reviewmatch.Compare(plan.ExpectedTitle, plan.ExpectedArtist, hit).Pass {
				continue
			}
			row.Exact = true
			row.ObservedTitle = candidate.Title
			if candidate.Seeders != nil {
				row.Seeders = *candidate.Seeders
				row.SeedersKnown = true
			}
			if seeded == nil && (site == "open.cd" || row.SeedersKnown && row.Seeders > 0) {
				seeded = &identityPick{site: site, id: candidate.CandidateID, hit: hit}
			}
		}
		if seeded != nil {
			picks = append(picks, *seeded)
		}
		report.Sites = append(report.Sites, row)
		body, err := json.Marshal(observation)
		if err != nil {
			return report, nil, "", err
		}
		kind := "api"
		if site == "open.cd" {
			kind = "website"
		}
		evidence = append(evidence, workflow.EvidenceInput{ID: evidenceID(taskID, site), Kind: kind, Source: site, Payload: body})
	}
	chosen, chosenID := selectIdentity(picks)
	adviseClass := ""
	if chosen == "" && a.reviewModel != nil && a.reviews != nil && len(pool) > 0 {
		advice := a.reviews.Advise(ctx, a.reviewModel, reviewai.Request{
			ExpectedTitle: plan.ExpectedTitle, ExpectedArtist: plan.ExpectedArtist, Candidates: pool,
		})
		adviseClass = advice.Class
		if advice.Pass {
			for _, item := range picks {
				if item.id == advice.CandidateID {
					chosen, chosenID = item.site, item.id
				}
			}
		}
	}
	if chosen != "" {
		report.SelectedSite = chosen
		report.SelectedCandidateID = chosenID
	}
	return report, evidence, adviseClass, nil
}

type identityPick struct {
	site string
	id   string
	hit  reviewmatch.Hit
}

func selectIdentity(picks []identityPick) (string, string) {
	if len(picks) == 0 {
		return "", ""
	}
	for _, item := range picks[1:] {
		if !reviewmatch.Same(picks[0].hit, item.hit) {
			return "", ""
		}
	}
	return picks[0].site, picks[0].id
}

func releaseHit(candidate trackersearch.Candidate) reviewmatch.Hit {
	edition := ""
	if candidate.Release != nil {
		if value, ok := candidate.Release["edition"].(string); ok && value != "unknown" {
			edition = value
		}
	}
	return reviewmatch.Hit{ID: candidate.CandidateID, Title: candidate.Title, Artists: candidate.Artists, Aliases: candidate.Aliases, Edition: edition}
}

func (a *App) cancelled(ctx context.Context, execution *scheduler.Execution) error {
	err := execution.Checkpoint(ctx, "tracker_read", 0, "只读检索")
	if err == nil || !errors.Is(err, scheduler.ErrCancelled) {
		return err
	}
	return a.stop(execution)
}

func (a *App) stop(execution *scheduler.Execution) error {
	token := execution.Token()
	_, err := a.flows.Interrupt(intentID("cancel", token.ID, token.Fence), token, a.now().UnixNano(), "检索已取消")
	if err != nil && !errors.Is(err, workflow.ErrStale) {
		return err
	}
	return scheduler.ErrCancelled
}

func (a *App) wait(ctx context.Context, delay time.Duration) error {
	if a.Wait != nil {
		return a.Wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RetryRemaining is the unelapsed part of a 10/30/60 minute search backoff.
func RetryRemaining(updatedNS int64, retryAfterSec int, now time.Time) time.Duration {
	if retryAfterSec <= 0 || updatedNS <= 0 {
		return 0
	}
	due := time.Unix(0, updatedNS).Add(time.Duration(retryAfterSec) * time.Second)
	if !now.Before(due) {
		return 0
	}
	return due.Sub(now)
}

func completion(decision searchdecision.Decision) (workflow.Complete, error) {
	switch decision.Action {
	case searchdecision.ActionQueueRequest:
		return workflow.Complete{Edge: workflow.EdgeQueueProcess, NextStage: "requesting", TaskState: "completed", Message: "检索命中，已排队获取；尚未下载、尚未入库"}, nil
	case searchdecision.ActionFallback:
		return workflow.Complete{Edge: workflow.EdgeQueueProcess, NextStage: "fallback", TaskState: "completed", Message: "检索未命中，已排队流媒体回退；尚未下载、尚未入库"}, nil
	case searchdecision.ActionRetry, searchdecision.ActionResearch:
		return workflow.Complete{Edge: workflow.EdgeDefer, NextStage: "searching", TaskState: "completed", Message: "站点暂时不可用，已安排有界重试；尚未下载"}, nil
	case searchdecision.ActionReviewSearch:
		return workflow.Complete{Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "search", TaskState: "blocked", Message: "检索需要核对；尚未下载"}, nil
	case searchdecision.ActionReviewCandidate:
		return workflow.Complete{Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "candidate", TaskState: "blocked", Message: "候选需要核对；尚未下载"}, nil
	case searchdecision.ActionSkip:
		return workflow.Complete{Edge: workflow.EdgeTerminal, NextStage: "skipped", TaskState: "completed", Message: "没有可用的精确命中，未下载"}, nil
	default:
		return workflow.Complete{}, errors.New("unclassified search decision")
	}
}

func decodePlan(payload []byte) (SearchPlan, error) {
	var plan SearchPlan
	if err := json.Unmarshal(payload, &plan); err != nil || plan.RootJobID == "" || plan.Query == "" {
		return SearchPlan{}, errors.New("search plan is incomplete")
	}
	return plan, nil
}

func (plan SearchPlan) evidence() workflow.EvidenceInput {
	return workflow.EvidenceInput{
		ID: plan.Identity.ID, Kind: plan.Identity.Kind, Source: plan.Identity.Source, SubjectID: plan.Identity.SubjectID,
		TracklistVerified: plan.Identity.TracklistVerified, IdentityConfirmed: plan.Identity.IdentityConfirmed,
		Payload: []byte(plan.Identity.Payload),
	}
}

func evidenceID(taskID, site string) string {
	sum := sha256.Sum256([]byte(taskID + "/" + site))
	return "obs" + hex.EncodeToString(sum[:8])
}

func intentID(kind, taskID string, fence uint64) string {
	sum := sha256.Sum256([]byte(kind + "/" + taskID + "/" + strconv.FormatUint(fence, 10)))
	return kind + "-" + hex.EncodeToString(sum[:12])
}
