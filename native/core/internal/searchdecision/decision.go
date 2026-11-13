// Package searchdecision classifies a finished private-tracker search.
// It does not call a site, spend FL, download audio, or write a library.
//
// The four sites are the source_order set required by the config schema and
// named in search_status.SITE_NAMES: open.cd, dicmusic, orpheus, redacted.
// NetEase is a separate streaming source, not one of those four.
package searchdecision

import (
	"regexp"
	"unicode/utf8"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// TrackerSites is the only legal search set. Order here is the canonical
// source_order, not a ranking of which hit to download.
var TrackerSites = [4]string{"open.cd", "dicmusic", "orpheus", "redacted"}

const StreamingSourceNetEase = "netease"

const (
	ClassNotSearched = "not_searched"
	ClassNoResult    = "no_result"
	ClassMatched     = "matched"
	ClassMismatch    = "mismatch"
	ClassAuth        = "auth"
	ClassRateLimited = "rate_limited"
	ClassForbidden   = "forbidden"
	ClassNetwork     = "network"
	ClassTransient   = "transient"
	ClassContract    = "contract"
	ClassDisabled    = "disabled"

	ActionQueueRequest    = "queue_request"
	ActionRetry           = "retry"
	ActionResearch        = "research"
	ActionReviewSearch    = "review_search"
	ActionReviewCandidate = "review_candidate"
	ActionFallback        = "fallback"
	ActionSkip            = "skip"

	maxAICandidates = 20
	maxQueryRunes   = 300
)

var retryDelaySec = [3]int{600, 1800, 3600}

var editionMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\blive\b`),
	regexp.MustCompile(`(?i)\bremix(?:ed)?\b`),
	regexp.MustCompile(`(?i)\bremaster(?:ed)?\b`),
	regexp.MustCompile(`(?i)\bdeluxe\b`),
	regexp.MustCompile(`(?i)\banniversary\b`),
	regexp.MustCompile(`(?i)\binstrumental\b`),
	regexp.MustCompile(`(?i)\bkaraoke\b`),
	regexp.MustCompile(`(?i)\bre[- ]?record(?:ed|ing)\b`),
	regexp.MustCompile(`现场`),
	regexp.MustCompile(`重制`),
	regexp.MustCompile(`重录`),
	regexp.MustCompile(`混音`),
	regexp.MustCompile(`周年`),
	regexp.MustCompile(`豪华`),
}

var candidatePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._@+-]{0,199}$`)

// SiteReport is one site's observed search outcome. Download and library
// import are not statuses here; those happen only after a later stage.
type SiteReport struct {
	Site            string `json:"site"`
	Status          string `json:"status"`
	Attempted       bool   `json:"attempted"`
	CandidateCount  int    `json:"candidate_count"`
	Exact           bool   `json:"exact"`
	EditionConflict bool   `json:"edition_conflict"`
	ObservedTitle   string `json:"observed_title,omitempty"`
	ExpectedTitle   string `json:"expected_title,omitempty"`
	Seeders         int    `json:"seeders"`
	SeedersKnown    bool   `json:"seeders_known"`
	Limited         bool   `json:"limited"`
}

// Report is a synthetic or offline description of one search attempt.
// IdentityConfirmed must already be true from catalogue evidence. An AI alias
// cannot set it.
type Report struct {
	Scene               string       `json:"scene"`
	IdentityConfirmed   bool         `json:"identity_confirmed"`
	StreamingEnabled    bool         `json:"streaming_enabled"`
	ExplicitFallback    bool         `json:"explicit_fallback"`
	QualityImproved     bool         `json:"quality_improved"`
	VersionChoice       bool         `json:"version_choice"`
	RetryCount          uint8        `json:"retry_count"`
	AIResearchCount     uint8        `json:"ai_research_count"`
	AIEnabled           bool         `json:"ai_enabled"`
	AIGrounded          bool         `json:"ai_grounded"`
	AICandidates        int          `json:"ai_candidates"`
	Query               string       `json:"query,omitempty"`
	AIProposedQuery     string       `json:"ai_proposed_query,omitempty"`
	SelectedSite        string       `json:"selected_site,omitempty"`
	SelectedCandidateID string       `json:"selected_candidate_id,omitempty"`
	Sites               []SiteReport `json:"sites"`
}

// Decision is a classification, not a measured download or a live success rate.
type Decision struct {
	Action            string            `json:"action"`
	Reason            string            `json:"reason"`
	Classes           map[string]string `json:"classes"`
	RetryAfterSec     int               `json:"retry_after_sec,omitempty"`
	SelectedSite      string            `json:"selected_site,omitempty"`
	StreamingDeferred bool              `json:"streaming_deferred,omitempty"`
}

type classified struct {
	report    Report
	classes   map[string]string
	qualified []string
	held      bool
	mismatch  bool
	limited   bool
}

func Decide(report Report) (Decision, error) {
	classed, err := classify(report)
	if err != nil {
		return Decision{}, err
	}
	decision := decide(classed)
	decision.Classes = classed.classes
	if classed.report.StreamingEnabled && fallbackScene(classed.report.Scene) && decision.Action != ActionFallback && decision.Action != ActionQueueRequest {
		decision.StreamingDeferred = true
	}
	return decision, nil
}

func classify(report Report) (classified, error) {
	if !knownScene(report.Scene) || report.RetryCount > 3 || report.AIResearchCount > 1 || report.AICandidates < 0 || report.AICandidates > maxAICandidates {
		return classified{}, dataengine.ErrInvalidRequest
	}
	if err := validText(report.Query); err != nil || validText(report.AIProposedQuery) != nil {
		return classified{}, dataengine.ErrInvalidRequest
	}
	if report.SelectedCandidateID != "" && !candidatePattern.MatchString(report.SelectedCandidateID) {
		return classified{}, dataengine.ErrInvalidRequest
	}
	if report.SelectedSite != "" && !trackerSite(report.SelectedSite) {
		return classified{}, dataengine.ErrInvalidRequest
	}
	if len(report.Sites) != len(TrackerSites) {
		return classified{}, dataengine.ErrInvalidRequest
	}
	out := classified{report: report, classes: map[string]string{}}
	seen := map[string]bool{}
	for _, site := range report.Sites {
		if !trackerSite(site.Site) || seen[site.Site] || site.CandidateCount < 0 || site.CandidateCount > 5000 || site.Seeders < 0 {
			return classified{}, dataengine.ErrInvalidRequest
		}
		if len(site.ObservedTitle) > 500 || len(site.ExpectedTitle) > 500 {
			return classified{}, dataengine.ErrInvalidRequest
		}
		seen[site.Site] = true
		class, err := siteClass(site)
		if err != nil {
			return classified{}, err
		}
		out.classes[site.Site] = class
		if class == ClassMatched && !qualified(site) {
			out.held = true
		}
		if class == ClassMatched && qualified(site) {
			out.qualified = append(out.qualified, site.Site)
		}
		if class == ClassMismatch {
			out.mismatch = true
		}
		if site.Limited {
			out.limited = true
		}
	}
	if len(seen) != len(TrackerSites) {
		return classified{}, dataengine.ErrInvalidRequest
	}
	return out, nil
}

func decide(in classified) Decision {
	report := in.report
	if !report.IdentityConfirmed {
		return Decision{Action: ActionReviewSearch, Reason: "unknown_release"}
	}
	if report.VersionChoice && (len(in.qualified) > 0 || in.mismatch || in.held) {
		return Decision{Action: ActionReviewCandidate, Reason: "version_choice"}
	}
	selected := report.SelectedSite
	if selected == "" && len(in.qualified) == 1 {
		selected = in.qualified[0]
	}
	if selected != "" && contains(in.qualified, selected) {
		if report.SelectedCandidateID == "" {
			return Decision{Action: ActionReviewCandidate, Reason: "selection_required"}
		}
		if report.Scene == "upgrade" && !report.QualityImproved {
			return Decision{Action: ActionSkip, Reason: "upgrade_not_better"}
		}
		return Decision{Action: ActionQueueRequest, Reason: "exact_match", SelectedSite: selected}
	}
	// An explicit pick of a candidate that is not an exact match. A human
	// accepting it (or the review model passing it) names both the site and
	// the candidate, and the candidate must exist on that site. Refusing this
	// made every near-match unaccept: the accept endpoint rebuilt the report,
	// asked this function, and got "selection_not_exact" back as an invalid
	// request, so nothing could ever be downloaded from a candidate list.
	if selected != "" && report.SelectedCandidateID != "" {
		for _, site := range report.Sites {
			if site.Site != selected || !site.Attempted || site.CandidateCount == 0 {
				continue
			}
			if report.Scene == "upgrade" && !report.QualityImproved {
				return Decision{Action: ActionSkip, Reason: "upgrade_not_better"}
			}
			return Decision{Action: ActionQueueRequest, Reason: "selected_candidate", SelectedSite: selected}
		}
	}
	if selected != "" {
		return Decision{Action: ActionReviewCandidate, Reason: "selection_not_exact"}
	}
	if len(in.qualified) > 1 {
		return Decision{Action: ActionReviewCandidate, Reason: "selection_required"}
	}
	if in.held {
		return Decision{Action: ActionReviewCandidate, Reason: "needs_seeders"}
	}
	if research(report) && !in.mismatch && !in.held {
		return Decision{Action: ActionResearch, Reason: "alias_research"}
	}
	if retryable(in) {
		return Decision{Action: ActionRetry, Reason: "transient_retry", RetryAfterSec: retryDelaySec[report.RetryCount]}
	}
	if in.mismatch {
		return Decision{Action: ActionReviewCandidate, Reason: "candidate_review"}
	}
	if action, reason, ok := blocked(in); ok {
		return override(report, Decision{Action: action, Reason: reason})
	}
	if autoFallback(in) {
		return Decision{Action: ActionFallback, Reason: "sites_exhausted"}
	}
	return override(report, Decision{Action: ActionSkip, Reason: "no_match_kept"})
}

func override(report Report, decision Decision) Decision {
	if report.ExplicitFallback && report.IdentityConfirmed && report.StreamingEnabled && fallbackScene(report.Scene) {
		return Decision{Action: ActionFallback, Reason: "review_confirmed_fallback"}
	}
	return decision
}

func research(report Report) bool {
	if !report.IdentityConfirmed || !report.AIEnabled || !report.AIGrounded || report.AIResearchCount != 0 {
		return false
	}
	if report.AIProposedQuery == "" || report.AIProposedQuery == report.Query {
		return false
	}
	return true
}

func retryable(in classified) bool {
	if in.report.RetryCount >= 3 {
		return false
	}
	saw := false
	for _, site := range TrackerSites {
		switch in.classes[site] {
		case ClassRateLimited, ClassNetwork, ClassTransient:
			saw = true
		case ClassNoResult, ClassDisabled, ClassMismatch, ClassMatched:
		default:
			return false
		}
	}
	return saw
}

func blocked(in classified) (string, string, bool) {
	disabled := 0
	for _, site := range TrackerSites {
		switch in.classes[site] {
		case ClassDisabled:
			disabled++
		case ClassNotSearched:
			return ActionReviewSearch, "incomplete_search", true
		case ClassAuth:
			return ActionReviewSearch, "auth_blocked", true
		case ClassForbidden:
			return ActionReviewSearch, "access_blocked", true
		case ClassContract:
			return ActionReviewSearch, "contract_error", true
		case ClassRateLimited, ClassNetwork, ClassTransient:
			if recoverableTransient(in) {
				return ActionFallback, "transient_recovered", true
			}
			return ActionReviewSearch, "transient_exhausted", true
		case ClassNoResult:
		case ClassMatched, ClassMismatch:
			return ActionReviewSearch, "unresolved_match", true
		default:
			return ActionReviewSearch, "unclassified", true
		}
	}
	if in.limited {
		return ActionReviewSearch, "limited_results", true
	}
	if disabled == len(TrackerSites) {
		return ActionReviewSearch, "no_sites_enabled", true
	}
	return "", "", false
}

func recoverableTransient(in classified) bool {
	if !in.report.IdentityConfirmed || !in.report.StreamingEnabled || !fallbackScene(in.report.Scene) || in.report.RetryCount < 3 {
		return false
	}
	saw := false
	for _, site := range TrackerSites {
		switch in.classes[site] {
		case ClassRateLimited, ClassNetwork, ClassTransient:
			saw = true
		case ClassNoResult, ClassDisabled:
		default:
			return false
		}
	}
	return saw
}

func autoFallback(in classified) bool {
	if !in.report.IdentityConfirmed || !in.report.StreamingEnabled || !fallbackScene(in.report.Scene) || in.limited {
		return false
	}
	enabled := 0
	for _, site := range TrackerSites {
		switch in.classes[site] {
		case ClassDisabled:
		case ClassNoResult:
			enabled++
		default:
			return false
		}
	}
	return enabled > 0
}

func siteClass(site SiteReport) (string, error) {
	switch site.Status {
	case "DISABLED":
		if site.Attempted || site.CandidateCount != 0 || site.Exact || site.Limited || site.SeedersKnown {
			return "", dataengine.ErrInvalidRequest
		}
		return ClassDisabled, nil
	case "NOT_SEARCHED":
		if site.Attempted || site.CandidateCount != 0 || site.Exact || site.Limited {
			return "", dataengine.ErrInvalidRequest
		}
		return ClassNotSearched, nil
	case "AUTH_REQUIRED", "AUTH_FAILED":
		return attempted(site, ClassAuth)
	case "RATE_LIMITED":
		return attempted(site, ClassRateLimited)
	case "ACCESS_BLOCKED", "POLICY_BLOCKED":
		return attempted(site, ClassForbidden)
	case "NETWORK_ERROR":
		return attempted(site, ClassNetwork)
	case "TRANSIENT_ERROR":
		return attempted(site, ClassTransient)
	case "PARSE_ERROR", "API_CONTRACT_ERROR":
		return attempted(site, ClassContract)
	case "NO_RESOURCE", "NO_MATCH":
		if site.Exact || site.CandidateCount != 0 || site.Limited {
			return "", dataengine.ErrInvalidRequest
		}
		return attempted(site, ClassNoResult)
	case "OK":
		if !site.Attempted {
			return "", dataengine.ErrInvalidRequest
		}
		if site.Exact && site.CandidateCount == 0 {
			return "", dataengine.ErrInvalidRequest
		}
		conflict := site.EditionConflict || titlesConflict(site.ObservedTitle, site.ExpectedTitle)
		if conflict && site.CandidateCount > 0 {
			return ClassMismatch, nil
		}
		if site.Exact {
			return ClassMatched, nil
		}
		if site.CandidateCount > 0 {
			return ClassMismatch, nil
		}
		return ClassNoResult, nil
	default:
		// DOWNLOADED, IMPORTED and unknown codes are not search results.
		return "", dataengine.ErrInvalidRequest
	}
}

func attempted(site SiteReport, class string) (string, error) {
	if !site.Attempted || site.Exact || site.CandidateCount != 0 || site.Limited {
		return "", dataengine.ErrInvalidRequest
	}
	return class, nil
}

func qualified(site SiteReport) bool {
	if site.Site == "open.cd" {
		return true
	}
	return site.SeedersKnown && site.Seeders > 0
}

// TitlesConflict reports an edition marker that is present only on the observed title.
func TitlesConflict(observed, expected string) bool {
	return titlesConflict(observed, expected)
}

func titlesConflict(observed, expected string) bool {
	if observed == "" {
		return false
	}
	for _, marker := range editionMarkers {
		if marker.MatchString(observed) && !marker.MatchString(expected) {
			return true
		}
	}
	return false
}

func knownScene(scene string) bool {
	switch scene {
	case "playlist", "complete", "upgrade", "fill", "expand":
		return true
	default:
		return false
	}
}

func fallbackScene(scene string) bool {
	return scene == "playlist" || scene == "fill"
}

func trackerSite(site string) bool {
	return contains(TrackerSites[:], site)
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func validText(value string) error {
	if !utf8.ValidString(value) || len([]rune(value)) > maxQueryRunes {
		return dataengine.ErrInvalidRequest
	}
	for _, r := range value {
		if r < 0x20 {
			return dataengine.ErrInvalidRequest
		}
	}
	return nil
}
