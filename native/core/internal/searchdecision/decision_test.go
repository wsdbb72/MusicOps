package searchdecision

import (
	"errors"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func sites(overrides map[string]SiteReport) []SiteReport {
	rows := make([]SiteReport, 0, len(TrackerSites))
	for _, site := range TrackerSites {
		row := SiteReport{Site: site, Status: "NO_RESOURCE", Attempted: true}
		if override, ok := overrides[site]; ok {
			override.Site = site
			row = override
		}
		rows = append(rows, row)
	}
	return rows
}

func base() Report {
	return Report{Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true, Sites: sites(nil)}
}

func exact(site string, seeders int, known bool) SiteReport {
	return SiteReport{Status: "OK", Attempted: true, CandidateCount: 1, Exact: true, ObservedTitle: "Kind of Blue", ExpectedTitle: "Kind of Blue", Seeders: seeders, SeedersKnown: known}
}

func TestTrackerSitesMatchTheConfiguredOrder(t *testing.T) {
	if TrackerSites != [4]string{"open.cd", "dicmusic", "orpheus", "redacted"} {
		t.Fatal(TrackerSites)
	}
	if StreamingSourceNetEase == TrackerSites[0] || StreamingSourceNetEase == "open.cd" {
		t.Fatal(StreamingSourceNetEase)
	}
}

func TestOfflineSamplesCoverSearchClassesWithoutClaimingDownloads(t *testing.T) {
	type sample struct {
		name        string
		report      Report
		action      string
		reason      string
		delay       int
		deferStream bool
	}
	redacted := exact("redacted", 4, true)
	samples := []sample{
		{name: "exact-redacted", report: func() Report {
			r := base()
			r.Scene = "complete"
			r.StreamingEnabled = false
			r.SelectedSite = "redacted"
			r.SelectedCandidateID = "torrent-1"
			r.Sites = sites(map[string]SiteReport{"redacted": redacted})
			return r
		}(), action: ActionQueueRequest, reason: "exact_match"},
		{name: "opencd-without-seeders", report: func() Report {
			r := base()
			r.Scene = "complete"
			r.StreamingEnabled = false
			r.SelectedCandidateID = "oc-1"
			r.Sites = sites(map[string]SiteReport{"open.cd": exact("open.cd", 0, true)})
			return r
		}(), action: ActionQueueRequest, reason: "exact_match"},
		{name: "dicmusic-without-seeders-is-not-a-miss", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"dicmusic": exact("dicmusic", 0, false)})
			return r
		}(), action: ActionReviewCandidate, reason: "needs_seeders", deferStream: true},
		{name: "live-marker-is-not-the-studio-release", report: func() Report {
			r := base()
			row := exact("orpheus", 2, true)
			row.ObservedTitle = "Kind of Blue (Live)"
			row.ExpectedTitle = "Kind of Blue"
			r.Sites = sites(map[string]SiteReport{"orpheus": row})
			return r
		}(), action: ActionReviewCandidate, reason: "candidate_review", deferStream: true},
		{name: "shared-live-marker-can-match", report: func() Report {
			r := base()
			r.SelectedCandidateID = "same-live"
			row := exact("open.cd", 0, false)
			row.ObservedTitle = "Live in Tokyo"
			row.ExpectedTitle = "Live in Tokyo"
			r.Sites = sites(map[string]SiteReport{"open.cd": row})
			return r
		}(), action: ActionQueueRequest, reason: "exact_match"},
		{name: "chinese-edition-marker", report: func() Report {
			r := base()
			row := exact("redacted", 2, true)
			row.ObservedTitle = "专辑 现场"
			row.ExpectedTitle = "专辑"
			row.Exact = true
			r.Sites = sites(map[string]SiteReport{"redacted": row})
			return r
		}(), action: ActionReviewCandidate, reason: "candidate_review", deferStream: true},
		{name: "rate-limit-retries-before-streaming", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})
			return r
		}(), action: ActionRetry, reason: "transient_retry", delay: 600, deferStream: true},
		{name: "second-transient-delay", report: func() Report {
			r := base()
			r.RetryCount = 1
			r.Sites = sites(map[string]SiteReport{"orpheus": {Status: "NETWORK_ERROR", Attempted: true}})
			return r
		}(), action: ActionRetry, reason: "transient_retry", delay: 1800, deferStream: true},
		{name: "third-transient-delay", report: func() Report {
			r := base()
			r.RetryCount = 2
			r.Sites = sites(map[string]SiteReport{"dicmusic": {Status: "TRANSIENT_ERROR", Attempted: true}})
			return r
		}(), action: ActionRetry, reason: "transient_retry", delay: 3600, deferStream: true},
		{name: "transient-retries-exhausted", report: func() Report {
			r := base()
			r.RetryCount = 3
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})
			return r
		}(), action: ActionFallback, reason: "transient_recovered"},
		{name: "transient-exhausted-outside-fallback-scene", report: func() Report {
			r := base()
			r.Scene = "complete"
			r.RetryCount = 3
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "NETWORK_ERROR", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "transient_exhausted"},
		{name: "auth-remains-review-after-retries", report: func() Report {
			r := base()
			r.RetryCount = 3
			r.Sites = sites(map[string]SiteReport{"dicmusic": {Status: "AUTH_FAILED", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "auth_blocked", deferStream: true},
		{name: "auth-is-not-an-empty-search", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"dicmusic": {Status: "AUTH_FAILED", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "auth_blocked", deferStream: true},
		{name: "auth-required", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"open.cd": {Status: "AUTH_REQUIRED", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "auth_blocked", deferStream: true},
		{name: "access-blocked", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"orpheus": {Status: "ACCESS_BLOCKED", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "access_blocked", deferStream: true},
		{name: "policy-blocked", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "POLICY_BLOCKED", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "access_blocked", deferStream: true},
		{name: "parse-contract", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"open.cd": {Status: "PARSE_ERROR", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "contract_error", deferStream: true},
		{name: "api-contract", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"dicmusic": {Status: "API_CONTRACT_ERROR", Attempted: true}})
			return r
		}(), action: ActionReviewSearch, reason: "contract_error", deferStream: true},
		{name: "disabled-sites-do-not-stream", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"open.cd": {Status: "DISABLED"}, "dicmusic": {Status: "DISABLED"}, "orpheus": {Status: "DISABLED"}, "redacted": {Status: "DISABLED"}})
			return r
		}(), action: ActionReviewSearch, reason: "no_sites_enabled", deferStream: true},
		{name: "clean-empty-playlist-may-stream", report: base(), action: ActionFallback, reason: "sites_exhausted"},
		{name: "clean-empty-fill-may-stream", report: func() Report { r := base(); r.Scene = "fill"; return r }(), action: ActionFallback, reason: "sites_exhausted"},
		{name: "clean-empty-album-stays", report: func() Report { r := base(); r.Scene = "complete"; return r }(), action: ActionSkip, reason: "no_match_kept"},
		{name: "limited-page-blocks-streaming", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "OK", Attempted: true, Limited: true}})
			return r
		}(), action: ActionReviewSearch, reason: "limited_results", deferStream: true},
		{name: "site-not-searched", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"orpheus": {Status: "NOT_SEARCHED"}})
			return r
		}(), action: ActionReviewSearch, reason: "incomplete_search", deferStream: true},
		{name: "version-choice", report: func() Report {
			r := base()
			r.VersionChoice = true
			r.SelectedCandidateID = "a"
			r.SelectedSite = "redacted"
			r.Sites = sites(map[string]SiteReport{"redacted": redacted})
			return r
		}(), action: ActionReviewCandidate, reason: "version_choice", deferStream: true},
		{name: "two-exact-sources-need-a-choice", report: func() Report {
			r := base()
			r.Scene = "complete"
			r.StreamingEnabled = false
			r.Sites = sites(map[string]SiteReport{"redacted": redacted, "orpheus": exact("orpheus", 3, true)})
			return r
		}(), action: ActionReviewCandidate, reason: "selection_required"},
		{name: "explicit-source-among-two", report: func() Report {
			r := base()
			r.Scene = "complete"
			r.StreamingEnabled = false
			r.SelectedSite = "orpheus"
			r.SelectedCandidateID = "orp-1"
			r.Sites = sites(map[string]SiteReport{"redacted": redacted, "orpheus": exact("orpheus", 3, true)})
			return r
		}(), action: ActionQueueRequest, reason: "exact_match"},
		{name: "grounded-alias-researches-without-confirming-identity", report: func() Report {
			r := base()
			r.AIEnabled = true
			r.AIGrounded = true
			r.AICandidates = 2
			r.Query = "Blue"
			r.AIProposedQuery = "Kind of Blue"
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})
			return r
		}(), action: ActionResearch, reason: "alias_research", deferStream: true},
		{name: "ungrounded-ai-does-not-research", report: func() Report {
			r := base()
			r.AIEnabled = true
			r.AICandidates = 2
			r.Query = "Blue"
			r.AIProposedQuery = "Kind of Blue"
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "RATE_LIMITED", Attempted: true}})
			return r
		}(), action: ActionRetry, reason: "transient_retry", delay: 600, deferStream: true},
		{name: "unknown-release-is-not-guessed", report: func() Report {
			r := base()
			r.IdentityConfirmed = false
			r.SelectedSite = "redacted"
			r.SelectedCandidateID = "torrent-1"
			r.Sites = sites(map[string]SiteReport{"redacted": redacted})
			return r
		}(), action: ActionReviewSearch, reason: "unknown_release", deferStream: true},
		{name: "upgrade-without-a-better-copy", report: func() Report {
			r := base()
			r.Scene = "upgrade"
			r.StreamingEnabled = false
			r.SelectedCandidateID = "oc-1"
			r.Sites = sites(map[string]SiteReport{"open.cd": exact("open.cd", 0, false)})
			return r
		}(), action: ActionSkip, reason: "upgrade_not_better"},
		{name: "upgrade-when-the-copy-is-better", report: func() Report {
			r := base()
			r.Scene = "upgrade"
			r.StreamingEnabled = false
			r.QualityImproved = true
			r.SelectedCandidateID = "oc-1"
			r.Sites = sites(map[string]SiteReport{"open.cd": exact("open.cd", 0, false)})
			return r
		}(), action: ActionQueueRequest, reason: "exact_match"},
		{name: "explicit-fallback-after-auth", report: func() Report {
			r := base()
			r.ExplicitFallback = true
			r.Sites = sites(map[string]SiteReport{"dicmusic": {Status: "AUTH_FAILED", Attempted: true}})
			return r
		}(), action: ActionFallback, reason: "review_confirmed_fallback"},
		{name: "non-exact-candidates", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "OK", Attempted: true, CandidateCount: 2, ObservedTitle: "Other", ExpectedTitle: "Kind of Blue"}})
			return r
		}(), action: ActionReviewCandidate, reason: "candidate_review", deferStream: true},
		{name: "rate-limit-with-a-candidate-still-retries", report: func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "OK", Attempted: true, CandidateCount: 1, ObservedTitle: "Other", ExpectedTitle: "Kind of Blue"}, "dicmusic": {Status: "NETWORK_ERROR", Attempted: true}})
			return r
		}(), action: ActionRetry, reason: "transient_retry", delay: 600, deferStream: true},
	}
	actions := map[string]int{}
	classes := map[string]bool{}
	for _, item := range samples {
		t.Run(item.name, func(t *testing.T) {
			decision, err := Decide(item.report)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != item.action || decision.Reason != item.reason || decision.RetryAfterSec != item.delay || decision.StreamingDeferred != item.deferStream {
				t.Fatalf("%+v", decision)
			}
			if decision.Action == ActionQueueRequest && decision.SelectedSite == "" {
				t.Fatal("missing selected site")
			}
			if decision.Action == ActionFallback && (decision.Reason == "downloaded" || decision.SelectedSite != "") {
				t.Fatal(decision)
			}
		})
		decision, err := Decide(item.report)
		if err != nil {
			t.Fatal(err)
		}
		actions[decision.Action]++
		for _, class := range decision.Classes {
			classes[class] = true
		}
	}
	for _, action := range []string{ActionQueueRequest, ActionRetry, ActionResearch, ActionReviewSearch, ActionReviewCandidate, ActionFallback, ActionSkip} {
		if actions[action] == 0 {
			t.Fatalf("offline sample missing action %s in %v", action, actions)
		}
	}
	for _, class := range []string{ClassNotSearched, ClassNoResult, ClassMatched, ClassMismatch, ClassAuth, ClassRateLimited, ClassForbidden, ClassNetwork, ClassTransient, ClassContract, ClassDisabled} {
		if !classes[class] {
			t.Fatalf("offline sample missing class %s", class)
		}
	}
	// actions counts are fixture coverage, not a four-site download success rate.
	if actions[ActionQueueRequest]+actions[ActionFallback] >= len(samples) {
		t.Fatal("fixture collapsed every sample into download or streaming")
	}
}

func TestRejectedReportsDoNotBecomeEmptyResults(t *testing.T) {
	cases := []Report{
		func() Report {
			r := base()
			r.Sites = append(r.Sites, SiteReport{Site: StreamingSourceNetEase, Status: "NO_RESOURCE", Attempted: true})
			return r
		}(),
		func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "DOWNLOADED", Attempted: true}})
			return r
		}(),
		func() Report {
			r := base()
			r.Sites = sites(map[string]SiteReport{"redacted": {Status: "IMPORTED", Attempted: true}})
			return r
		}(),
		func() Report { r := base(); r.AIEnabled = true; r.AICandidates = 21; return r }(),
		func() Report { r := base(); r.AIProposedQuery = "line\nbreak"; return r }(),
		func() Report { r := base(); r.Scene = "broadcast"; return r }(),
	}
	for i, report := range cases {
		_, err := Decide(report)
		if !errors.Is(err, dataengine.ErrInvalidRequest) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}
