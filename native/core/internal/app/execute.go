package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// SearchPlan is the durable, secret-free description of one tracker read.
type SearchPlan struct {
	RootJobID string `json:"root_job_id"`
	// Scene is the workflow scene this plan was created for.
	Scene string `json:"scene,omitempty"`
	// PlaylistImport marks a job created from a fetched playlist. Scene
	// alone cannot: "playlist" is also the default scene of the ordinary
	// search form.
	PlaylistImport bool `json:"playlist_import,omitempty"`
	// PlaylistMode: "all" starts every track right after the list is read;
	// empty parks the list so the page can show it and send a selection.
	NeteaseTrackOnly    bool                 `json:"netease_track_only,omitempty"`
	NeteaseOnly         bool                 `json:"netease_only,omitempty"`
	NeteaseSongID       int64                `json:"netease_song_id,omitempty"`
	NeteaseAlbumID      int64                `json:"netease_album_id,omitempty"`
	PlaylistTrack       string               `json:"playlist_track,omitempty"`
	ExpectedDuration    int                  `json:"expected_duration,omitempty"`
	PlaylistMode        string               `json:"playlist_mode,omitempty"`
	AlbumArtist         string               `json:"album_artist,omitempty"`
	Query               string               `json:"query"`
	ExpectedTitle       string               `json:"expected_title"`
	ExpectedArtist      string               `json:"expected_artist,omitempty"`
	EnabledSites        []string             `json:"enabled_sites"`
	StreamingEnabled    bool                 `json:"streaming_enabled"`
	Synthetic           bool                 `json:"synthetic"`
	BaseURL             string               `json:"base_url,omitempty"`
	Budget              int                  `json:"budget,omitempty"`
	SafetyMargin        int                  `json:"safety_margin,omitempty"`
	SelectedSite        string               `json:"selected_site,omitempty"`
	SelectedCandidateID string               `json:"selected_candidate_id,omitempty"`
	LibraryTracks       []localWorkflowTrack `json:"library_tracks,omitempty"`
	// AcquireCandidates is the durable, ranked fallback order for the
	// selected release. The first entry is always the selected candidate; if
	// its detail is unavailable before any token request, acquire can move to
	// the next eligible torrent without re-running the search.
	AcquireCandidates []AcquireCandidate `json:"acquire_candidates,omitempty"`
	Identity          Identity           `json:"identity"`
}

// AcquireCandidate is a secret-free tracker candidate retained beside the
// search plan. CandidateID is the site-qualified id already used by the
// tracker observations; the slice order is the persisted ranking.
type AcquireCandidate struct {
	Site        string   `json:"site"`
	CandidateID string   `json:"candidate_id"`
	Title       string   `json:"title,omitempty"`
	Artists     []string `json:"artists,omitempty"`
	Quality     string   `json:"quality,omitempty"`
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
	normalizePlaylistAlbumPlan(&plan)
	token := execution.Token()
	// A playlist job has no album to identify: read the list it was created
	// with, record it as evidence and park for review. Running the album
	// lookup here was what made a freshly created playlist job fail with
	// "invalid data request".
	if plan.PlaylistImport {
		return a.identifyPlaylist(ctx, execution, plan, token)
	}
	if len(plan.LibraryTracks) > 0 && !plan.Identity.IdentityConfirmed {
		if err := a.confirmSelectedLibrary(ctx, &plan); err == nil {
			payload, err = json.Marshal(plan)
			if err != nil {
				return scheduler.Outcome{}, err
			}
		}
	}
	// Old NetEase playlist rows predate the confirmed platform evidence now
	// stored at enqueue time. Recover their durable song/album ids from the
	// ordered M3U (or one authenticated playlist refresh) and let the search
	// proceed automatically; media identity is still checked again at acquire.
	if !plan.Identity.IdentityConfirmed {
		if task, taskErr := a.tasks.Get(token.ID); taskErr == nil && task != nil {
			if job, jobErr := a.flows.Job(task.WorkflowJobID); jobErr == nil && job != nil && a.recoverNeteasePlaylistIDs(ctx, job.BatchID, &plan) {
				if !plan.Identity.IdentityConfirmed {
					identityPayload, _ := json.Marshal(map[string]any{
						"source": "netease", "title": plan.PlaylistTrack, "artist": plan.ExpectedArtist,
						"album": plan.ExpectedTitle, "netease_song_id": plan.NeteaseSongID,
						"netease_album_id": plan.NeteaseAlbumID,
					})
					plan.Identity = Identity{
						ID: evidenceID(token.ID, token.Fence, "playlist-identity"), Kind: "api", Source: "netease",
						SubjectID:         fmt.Sprintf("netease:%d:%d", plan.NeteaseSongID, plan.NeteaseAlbumID),
						TracklistVerified: true, IdentityConfirmed: true,
						Payload: string(identityPayload),
					}
				}
				payload, err = json.Marshal(plan)
				if err != nil {
					return scheduler.Outcome{}, err
				}
			}
		}
	}
	evidence := plan.evidence()
	// A playlist track job has no catalogue identity to attach; its row
	// evidence was written when the job started, so passing the empty one
	// would make the complete call fail validation (empty evidence id) and
	// the job would die with "invalid data request" instead of parking.
	evidenceInputs := []workflow.EvidenceInput{}
	if evidence.ID != "" {
		evidenceInputs = append(evidenceInputs, evidence)
	}
	// A bare album name is the normal entry point of the search page and it
	// carries no confirmed catalogue identity. Asking the workflow for the
	// search edge in that state is rejected (the edge requires confirmed
	// evidence), the scheduler then treats the rejection as an unknown
	// external effect and interrupts the task, and the job lands in
	// recovery_required with "执行已退出". Identity is therefore an explicit
	// human checkpoint: unconfirmed evidence parks the job in
	// needs_review/metadata before any site is read. Automatic catalogue
	// identification is a later batch and will take this same checkpoint.
	if !plan.Identity.IdentityConfirmed {
		complete := workflow.Complete{
			Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "metadata",
			TaskState: "blocked", ErrorClass: "IDENTITY_UNCONFIRMED",
			Evidence: evidenceInputs, Result: payload,
			Message: "身份证据未确认，已转入核对；未检索、未下载、未入库", NowNS: a.now().UnixNano(),
		}
		if _, err = a.flows.Complete(intentID("identify", token.ID, token.Fence), token, complete); err != nil {
			return scheduler.Outcome{}, err
		}
		return scheduler.Outcome{State: "blocked", Message: complete.Message}, nil
	}
	_, err = a.flows.Complete(intentID("identify", token.ID, token.Fence), token, workflow.Complete{
		Edge: workflow.EdgeQueueSearch, TaskState: "completed", Evidence: evidenceInputs,
		Result: payload, Message: "身份证据已接收，等待四站只读检索；尚未下载", NowNS: a.now().UnixNano(),
	})
	if err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: "已交给检索"}, nil
}

func (a *App) search(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	outcome, err := a.searchOnce(ctx, execution, payload)
	if err != nil && errors.Is(err, taskrepo.ErrStale) {
		token := execution.Token()
		requestID := intentID("replay-search", token.ID, token.Fence)
		if _, replayErr := a.flows.ReplayStaleSearch(requestID, token, a.now().UnixNano(), "只读检索遇到本地并发冲突，已自动重排；未下载、未入库"); replayErr == nil {
			return scheduler.Outcome{}, scheduler.ErrRequeued
		}
	}
	return outcome, err
}

func (a *App) searchOnce(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	plan, err := decodePlan(payload)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	normalizePlaylistAlbumPlan(&plan)
	token := execution.Token()
	// A playlist job has no album to identify: read the list it was created
	// with, record it as evidence and park for review. Running the album
	// lookup here was what made a freshly created playlist job fail with
	// "invalid data request".
	if plan.PlaylistImport {
		return a.identifyPlaylist(ctx, execution, plan, token)
	}
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
	if err = a.waitIfPaused(ctx); err != nil {
		return scheduler.Outcome{}, err
	}
	report, evidence, adviseClass, err := a.observe(ctx, execution, *job, &plan, token)
	if errors.Is(err, scheduler.ErrCancelled) {
		return scheduler.Outcome{}, a.stop(execution)
	}
	if err != nil {
		return scheduler.Outcome{}, err
	}
	// A cancel requested while the last site was being read must still
	// be honoured before the search completes: observe() only
	// checkpoints before each site, so with a single site a cancel
	// landing during its HTTP call would otherwise be recorded as
	// "completed with cancel request" instead of cancelling the job.
	// A pause requested during the read likewise takes effect here.
	// cancelled() routes an observed cancel through a.stop(), so the
	// job lands in needs_review/interrupted (task recovery_required)
	// — the designed outcome for a search cancelled mid-flight.
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	neteaseSource := isNeteasePlaylistFallback(job.BatchID, plan) && report.SelectedSite == ""
	if neteaseSource {
		// The model has already reviewed any eligible real tracker hits in
		// observe. An unresolved comparison uses the source catalogue rather
		// than turning the model's guess into download authorization.
		markNeteaseSourceFallback(&report)
		evidence = append(evidence, workflow.EvidenceInput{
			ID: evidenceID(token.ID, token.Fence, "netease"), Kind: "api", Source: "netease", SubjectID: plan.RootJobID, Payload: []byte(plan.Identity.Payload),
		})
	} else if plan.StreamingEnabled && report.SelectedSite == "" && (len(plan.LibraryTracks) > 0 ||
		plan.PlaylistTrack != "" && normalizeKey(plan.PlaylistTrack) == normalizeKey(plan.ExpectedTitle)) {
		report.ExplicitFallback = true
	}
	decision, err := searchdecision.Decide(report)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if alias, ok := a.researchAlias(ctx, *job, plan, decision); ok {
		decision.Action = searchdecision.ActionResearch
		plan.Query = alias
		if encoded, err := json.Marshal(plan); err == nil {
			payload = encoded
		}
	}
	complete, err := completion(decision)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if decision.Action == searchdecision.ActionResearch && job.AIResearchCount == 0 && strings.TrimSpace(plan.Query) != "" {
		complete.Message = "AI 找到别名，只用于重新检索；尚未下载，也不能据此通过核对"
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
		return scheduler.Outcome{}, fmt.Errorf("complete search workflow: %w", err)
	}
	if complete.Edge != workflow.EdgeDefer && complete.Edge != workflow.EdgeQueueProcess {
		// The search secret stays while the job tree can still use it: a job
		// in needs_review or failed can be retried, and the retry re-runs the
		// search with the same cookie. It is forgotten only when the search
		// is over for good. A skip also forgets it in the review handler; an
		// accept keeps it, because the process child still needs the session
		// cookie to read the torrent detail and fetch the torrent.
		if complete.NextStage == "skipped" || complete.NextStage == "cancelled" {
			a.forget(plan.RootJobID)
		}
	}
	return scheduler.Outcome{State: complete.TaskState, Message: complete.Message}, nil
}

// markNeteaseSourceFallback turns a playlist row into the explicit fallback
// state the decision engine requires. Older plans may lack the later
// streaming/identity flags; the durable NetEase batch and playlist row still
// prove the source, so those flags are raised rather than stranding the task.
func markNeteaseSourceFallback(report *searchdecision.Report) {
	if report == nil {
		return
	}
	report.SourceFallback = true
	report.StreamingEnabled = true
	report.IdentityConfirmed = true
}

func (a *App) observe(ctx context.Context, execution *scheduler.Execution, job workflow.Job, plan *SearchPlan, token taskrepo.Token) (searchdecision.Report, []workflow.EvidenceInput, string, error) {
	// A re-search must never inherit a prior attempt's fallback order when
	// this attempt produces no confirmed candidate.
	plan.AcquireCandidates = nil
	enabled := map[string]bool{}
	if !plan.NeteaseOnly || !isNeteasePlaylistFallback(job.BatchID, *plan) {
		for _, site := range a.effectiveEnabledSites(*plan) {
			enabled[site] = true
		}
	}
	report := searchdecision.Report{
		Scene: job.Scene, IdentityConfirmed: plan.Identity.IdentityConfirmed, StreamingEnabled: plan.StreamingEnabled,
		RetryCount: job.SearchRetryCount, AIResearchCount: job.AIResearchCount, Query: plan.Query, Sites: make([]searchdecision.SiteReport, 0, 4),
	}
	solverURL, solverToken := a.solverTarget()
	evidence := []workflow.EvidenceInput{}
	var picks []identityPick
	var allPicks []identityPick
	var pool []reviewai.Candidate
	seededSites := map[string]string{}
	for _, site := range searchdecision.TrackerSites {
		row := searchdecision.SiteReport{Site: site, ExpectedTitle: plan.ExpectedTitle}
		if !enabled[site] {
			row.Status = "DISABLED"
			report.Sites = append(report.Sites, row)
			continue
		}
		phase := "search_" + strings.ReplaceAll(site, ".", "_")
		if err := execution.Checkpoint(ctx, phase, 0, site); err != nil {
			return report, nil, "", fmt.Errorf("checkpoint %s: %w", phase, err)
		}
		observation, err := a.searcher.Search(ctx, trackersearch.Query{
			Site: site, Text: plan.Query, Cookie: a.cookieFor(plan.RootJobID, site), Synthetic: plan.Synthetic, BaseURL: plan.BaseURL, SolverURL: solverURL, SolverToken: solverToken,
		})
		if err != nil {
			return report, nil, "", err
		}
		if observation.Status == "OK" && !observation.SearchComplete && observation.Pages != nil && observation.Page > 0 && *observation.Pages > observation.Page {
			next, nextErr := a.searcher.Search(ctx, trackersearch.Query{
				Site: site, Text: plan.Query, Page: observation.Page + 1, Cookie: a.cookieFor(plan.RootJobID, site), Synthetic: plan.Synthetic, BaseURL: plan.BaseURL, SolverURL: solverURL, SolverToken: solverToken,
			})
			if nextErr == nil && next.Status == "OK" {
				observation.Candidates = append(observation.Candidates, next.Candidates...)
			}
		}
		row, sitePicks, sitePool := summarizeSiteCandidates(*plan, site, observation)
		pool = append(pool, sitePool...)
		allPicks = append(allPicks, sitePicks...)
		var seeded *identityPick
		if len(sitePicks) > 0 {
			copy := sitePicks[0]
			seeded = &copy
		}
		if observation.Status == "OK" {
			for _, hit := range observation.Candidates {
				if (site == "open.cd" || hit.Seeders != nil && *hit.Seeders > 0) && hit.CandidateID != "" {
					seededSites[hit.CandidateID] = site
				}
			}
		}

		if seeded != nil {
			picks = append(picks, *seeded)
		}
		report.Sites = append(report.Sites, row)
		// Stamp the observation time when the site did not supply one, so a
		// later attempt's observation can be told apart from this one. The
		// review page reads the latest observation per site by this time.
		if observation.ObservedAt == "" {
			observation.ObservedAt = a.now().UTC().Format(time.RFC3339Nano)
		}
		body, err := json.Marshal(observation)
		if err != nil {
			return report, nil, "", err
		}
		kind := "api"
		if site == "open.cd" {
			kind = "website"
		}
		evidence = append(evidence, workflow.EvidenceInput{ID: evidenceID(token.ID, token.Fence, site), Kind: kind, Source: site, Payload: body})
	}
	chosen, chosenID := selectIdentity(picks)
	adviseClass := ""
	model := a.reviewCompleter()
	if chosen == "" && model != nil && a.reviews != nil && len(pool) > 0 {
		advice := a.reviews.Advise(ctx, model, reviewai.Request{
			ExpectedTitle: plan.ExpectedTitle, ExpectedArtist: plan.ExpectedArtist, Candidates: pool,
		})
		adviseClass = advice.Class
		if site := seededSites[advice.CandidateID]; advice.Pass && site != "" {
			chosen, chosenID = site, advice.CandidateID
		}
	}
	if chosen != "" {
		report.SelectedSite = chosen
		report.SelectedCandidateID = chosenID
		plan.AcquireCandidates = acquireCandidatesForSelection(*plan, allPicks, chosen, chosenID)
		if plan.Scene == "upgrade" {
			for _, p := range picks {
				if p.id == chosenID && localUpgradeAdmits(*plan, p.hit) {
					report.QualityImproved = true
					break
				}
			}
		}
	}
	return report, evidence, adviseClass, nil
}

// summarizeSite classifies one stored or live observation exactly the way
// the search executor does. The review path reuses it so a report rebuilt
// from stored observations classifies the same as the report the executor
// built from the same observations.
func summarizeSite(plan SearchPlan, site string, observation trackersearch.Observation) (searchdecision.SiteReport, *identityPick, []reviewai.Candidate) {
	row, picks, pool := summarizeSiteCandidates(plan, site, observation)
	if len(picks) == 0 {
		return row, nil, pool
	}
	best := picks[0]
	return row, &best, pool
}

// summarizeSiteCandidates keeps every eligible torrent for the site while
// reporting only its highest-ranked candidate in the site summary. The full
// list is persisted later so an unavailable detail does not force an
// immediate source-platform fallback.
func summarizeSiteCandidates(plan SearchPlan, site string, observation trackersearch.Observation) (searchdecision.SiteReport, []identityPick, []reviewai.Candidate) {
	row := searchdecision.SiteReport{Site: site, ExpectedTitle: plan.ExpectedTitle}
	row.Status = observation.Status
	row.Attempted = true
	row.CandidateCount = len(observation.Candidates)
	row.Limited = observation.Status == "OK" && !observation.SearchComplete
	picks := make([]identityPick, 0, len(observation.Candidates))
	pool := make([]reviewai.Candidate, 0, len(observation.Candidates))
	for _, candidate := range observation.Candidates {
		hit := releaseHit(candidate)
		pool = append(pool, reviewai.Candidate{ID: hit.ID, Title: hit.Title, Artists: hit.Artists, Aliases: hit.Aliases, Edition: hit.Edition, GroupID: hit.GroupID, Format: hit.Format, Year: hit.Year})
		if !localUpgradeAdmits(plan, hit) || !reviewmatch.CompareSource(site, plan.ExpectedTitle, plan.ExpectedArtist, hit).Pass {
			continue
		}
		row.Exact = true
		quality, available := candidateQuality(plan, site, candidate)
		if !available || candidate.CandidateID == "" {
			continue
		}
		picks = append(picks, identityPick{site: site, id: candidate.CandidateID, hit: hit, quality: quality, qualityLabel: candidateQualityLabel(quality, candidate), seedersKnown: candidate.Seeders != nil})
	}
	sort.SliceStable(picks, func(i, j int) bool {
		return compareCandidateQuality(picks[i].quality, picks[j].quality) > 0
	})
	if len(picks) > 0 {
		row.ObservedTitle = picks[0].hit.Title
		if picks[0].seedersKnown {
			row.Seeders = picks[0].quality.Seeders
			row.SeedersKnown = true
		}
	}
	return row, picks, pool
}

func acquireCandidatesForSelection(plan SearchPlan, picks []identityPick, selectedSite, selectedID string) []AcquireCandidate {
	if selectedSite == "" || selectedID == "" {
		return nil
	}
	selected := -1
	for i := range picks {
		if picks[i].site == selectedSite && picks[i].id == selectedID {
			selected = i
			break
		}
	}
	if selected < 0 {
		return []AcquireCandidate{{Site: selectedSite, CandidateID: selectedID}}
	}
	chosen := picks[selected]
	candidates := make([]identityPick, 0, len(picks))
	for _, pick := range picks {
		if sameReleaseForPlan(plan, chosen.hit, pick.hit) {
			candidates = append(candidates, pick)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].site == selectedSite && candidates[i].id == selectedID {
			return true
		}
		if candidates[j].site == selectedSite && candidates[j].id == selectedID {
			return false
		}
		return compareCandidateQuality(candidates[i].quality, candidates[j].quality) > 0
	})
	out := make([]AcquireCandidate, 0, len(candidates))
	seen := map[string]bool{}
	for _, pick := range candidates {
		key := pick.site + "\x00" + pick.id
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, AcquireCandidate{
			Site: pick.site, CandidateID: pick.id, Title: pick.hit.Title,
			Artists: append([]string(nil), pick.hit.Artists...), Quality: pick.qualityLabel,
		})
		if len(out) == 32 {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, AcquireCandidate{Site: selectedSite, CandidateID: selectedID, Title: chosen.hit.Title, Artists: append([]string(nil), chosen.hit.Artists...), Quality: chosen.qualityLabel})
	}
	return out
}

func sameReleaseForPlan(plan SearchPlan, left, right reviewmatch.Hit) bool {
	if reviewmatch.Same(left, right) {
		return true
	}
	// open.cd names releases "Artist - Album" while the Gazelle sites use the
	// album title. Both rows already passed CompareSource, so compare the
	// edition, artist and year evidence against the same expected title.
	left.Title, right.Title = plan.ExpectedTitle, plan.ExpectedTitle
	return reviewmatch.Same(left, right)
}

func acquireCandidatesFromObservations(plan SearchPlan, observations map[string]trackersearch.Observation, selectedSite, selectedID string) []AcquireCandidate {
	picks := []identityPick{}
	for _, site := range searchdecision.TrackerSites {
		_, sitePicks, _ := summarizeSiteCandidates(plan, site, observations[site])
		picks = append(picks, sitePicks...)
	}
	return acquireCandidatesForSelection(plan, picks, selectedSite, selectedID)
}

type identityPick struct {
	site         string
	id           string
	hit          reviewmatch.Hit
	quality      candidateQualityRank
	qualityLabel string
	seedersKnown bool
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
	best := picks[0]
	for _, item := range picks[1:] {
		if compareCandidateQuality(item.quality, best.quality) > 0 {
			best = item
		}
	}
	return best.site, best.id
}

func releaseHit(candidate trackersearch.Candidate) reviewmatch.Hit {
	edition := ""
	if candidate.Release != nil {
		if value, ok := candidate.Release["edition"].(string); ok && value != "unknown" {
			edition = value
		}
	}
	format := ""
	if candidate.Media != nil {
		if value, ok := candidate.Media["format"].(string); ok {
			format = value
		}
	}
	return reviewmatch.Hit{ID: candidate.CandidateID, Title: candidate.Title, Artists: candidate.Artists, Aliases: candidate.Aliases, Edition: edition, GroupID: candidate.GroupID, Format: format, Year: releaseYear(candidate.Release)}
}

// releaseYear reads the release year the tracker record carries, or 0 when the
// record carries none. Values arrive as JSON numbers or strings.
func releaseYear(release map[string]any) int {
	if release == nil {
		return 0
	}
	switch value := release["year"].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case string:
		year, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || year < 0 {
			return 0
		}
		return year
	default:
		return 0
	}
}

func (a *App) cancelled(ctx context.Context, execution *scheduler.Execution) error {
	err := execution.Checkpoint(ctx, "tracker_read", 0, "只读检索")
	if err == nil || !errors.Is(err, scheduler.ErrCancelled) {
		if err != nil {
			return fmt.Errorf("checkpoint tracker_read: %w", err)
		}
		return nil
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
	if err := json.Unmarshal(payload, &plan); err != nil || plan.RootJobID == "" || strings.TrimSpace(plan.Query) == "" {
		return SearchPlan{}, errors.New("search plan is incomplete")
	}
	// Older playlist jobs stored the platform's complete artist credit in the
	// query. A long ensemble credit can exceed the tracker client's 300-rune
	// safety limit and previously failed before any site was searched. Reduce
	// it to the primary credited artist plus the release/track title. This is
	// only a search hint; the complete artist credit remains in ExpectedArtist
	// and is still used for identity matching.
	plan.Query = boundedTrackerQuery(plan.Query, plan.ExpectedTitle, plan.ExpectedArtist)
	return plan, nil
}

func boundedTrackerQuery(query, title, artist string) string {
	query = strings.TrimSpace(query)
	if len([]rune(query)) <= 300 {
		return query
	}
	title = strings.TrimSpace(title)
	primaryArtist := strings.TrimSpace(strings.SplitN(artist, " / ", 2)[0])
	for _, candidate := range []string{
		strings.TrimSpace(primaryArtist + " " + title),
		title,
	} {
		if candidate != "" && len([]rune(candidate)) <= 300 {
			return candidate
		}
	}
	runes := []rune(query)
	return strings.TrimSpace(string(runes[:300]))
}

func (plan SearchPlan) evidence() workflow.EvidenceInput {
	return workflow.EvidenceInput{
		ID: plan.Identity.ID, Kind: plan.Identity.Kind, Source: plan.Identity.Source, SubjectID: plan.Identity.SubjectID,
		TracklistVerified: plan.Identity.TracklistVerified, IdentityConfirmed: plan.Identity.IdentityConfirmed,
		Payload: []byte(plan.Identity.Payload),
	}
}

// evidenceID names one attempt's evidence for one site. The fence is part of
// the name: a retried task runs again, and the evidence store rejects the
// same id with different bytes, so each attempt gets its own ids and the
// earlier evidence stays as history.
func evidenceID(taskID string, fence uint64, site string) string {
	sum := sha256.Sum256([]byte(taskID + "/" + strconv.FormatUint(fence, 10) + "/" + site))
	return "obs" + hex.EncodeToString(sum[:8])
}

func intentID(kind, taskID string, fence uint64) string {
	sum := sha256.Sum256([]byte(kind + "/" + taskID + "/" + strconv.FormatUint(fence, 10)))
	return kind + "-" + hex.EncodeToString(sum[:12])
}
