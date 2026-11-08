package downloadpolicy

import (
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
)

func names(steps []Step) []string {
	out := make([]string, len(steps))
	for i, step := range steps {
		out[i] = step.Name
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDefaultOrderPrefersLosslessSubscriptions(t *testing.T) {
	bare, err := Plan(DefaultSources(), Track{})
	if err != nil || !equal(names(bare), []string{"tidal", "applemusic", "qqmusic", "netease", "playlist"}) {
		t.Fatal(names(bare), err)
	}
	known, err := Plan(DefaultSources(), Track{SourceKnown: true})
	if err != nil || !equal(names(known), []string{"tidal", "applemusic", "qqmusic", "netease"}) {
		t.Fatal(names(known), err)
	}
	info, err := Plan(DefaultSources(), Track{InfoKnown: true, PlaylistPlatform: "qqmusic"})
	if err != nil || !equal(names(info), []string{"tidal", "applemusic", "qqmusic", "netease"}) {
		t.Fatal(names(info), err)
	}
	labeled, err := Plan(DefaultSources(), Track{PlaylistPlatform: "qqmusic"})
	if err != nil || !equal(names(labeled), []string{"tidal", "applemusic", "qqmusic", "netease", "playlist:qqmusic"}) {
		t.Fatal(names(labeled), err)
	}
	shuffled := DefaultSources()
	shuffled[0], shuffled[5] = shuffled[5], shuffled[0]
	shuffled[2].Enabled = true
	ordered, err := Plan(shuffled, Track{})
	if err != nil || !equal(names(ordered), []string{"tidal", "applemusic", "qobuz", "qqmusic", "netease", "playlist"}) {
		t.Fatal(names(ordered), err)
	}
	lossy := DefaultSources()
	lossy[4].Quality = "mp3"
	if _, err = Plan(lossy, Track{}); err == nil {
		t.Fatal("lossy switch accepted")
	}
}

func TestStreamingStaysClosedUntilTheTrackerMissIsSafe(t *testing.T) {
	sites := make([]searchdecision.SiteReport, 4)
	for i, site := range searchdecision.TrackerSites {
		sites[i] = searchdecision.SiteReport{Site: site, Status: "NO_RESOURCE", Attempted: true}
	}
	miss, err := searchdecision.Decide(searchdecision.Report{
		Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true, Sites: sites,
	})
	if err != nil || miss.Action != searchdecision.ActionFallback {
		t.Fatal(miss, err)
	}
	if !AllowStreaming(Gate{SearchAction: miss.Action, Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true}) {
		t.Fatal("safe miss")
	}
	sites[1].Status = "RATE_LIMITED"
	limited, err := searchdecision.Decide(searchdecision.Report{
		Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true, Sites: sites,
	})
	if err != nil || limited.Action != searchdecision.ActionRetry {
		t.Fatal(limited, err)
	}
	if AllowStreaming(Gate{SearchAction: limited.Action, Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true, DownloadClass: ClassRateLimited}) {
		t.Fatal("rate limit opened streaming")
	}
	for _, class := range []string{ClassMatch, ClassAuth, ClassAuthorization, ClassNetwork, ClassDownload, ClassImport} {
		if AllowStreaming(Gate{SearchAction: searchdecision.ActionFallback, Scene: "fill", IdentityConfirmed: true, StreamingEnabled: true, DownloadClass: class}) {
			t.Fatal(class)
		}
	}
	if AllowStreaming(Gate{SearchAction: searchdecision.ActionQueueRequest, Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true}) {
		t.Fatal("exact hit opened streaming")
	}
	if AllowStreaming(Gate{SearchAction: searchdecision.ActionFallback, Scene: "playlist", IdentityConfirmed: false, StreamingEnabled: true}) {
		t.Fatal("unknown identity")
	}
	if !AllowStreaming(Gate{Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true, DownloadClass: ClassTokenBudget, TokenRecorded: true}) {
		t.Fatal("recorded token exhaustion")
	}
	if AllowStreaming(Gate{Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true, DownloadClass: ClassTokenBudget}) {
		t.Fatal("unrecorded token exhaustion")
	}
}

func TestFixtureObservationIsNotADownload(t *testing.T) {
	plan, err := Plan(DefaultSources(), Track{PlaylistPlatform: "netease"})
	if err != nil {
		t.Fatal(err)
	}
	walk, err := Apply(plan, []Outcome{
		{Source: "tidal", Class: ClassAuth},
		{Source: "applemusic", Class: ClassNoResult},
		{Source: "qqmusic", Class: ClassFixture, Synthetic: true},
	}, 0)
	if err != nil || !walk.FixtureObserved || walk.Downloaded || walk.Imported || walk.ProductionVerified || walk.FailureClass != "" || len(walk.Next) != 0 {
		t.Fatal(walk, err)
	}
	if !equal(walk.Attempts, []string{"tidal:auth", "applemusic:no_result", "qqmusic:fixture_observed"}) {
		t.Fatal(walk.Attempts)
	}
	failed, err := Apply(plan, []Outcome{
		{Source: "tidal", Class: ClassNetwork},
		{Source: "applemusic", Class: ClassDownload},
		{Source: "qqmusic", Class: ClassAuthorization},
		{Source: "netease", Class: ClassImport},
	}, 0)
	if err != nil || failed.FailureClass != ClassImport || failed.FixtureObserved || failed.Downloaded || failed.Imported || len(failed.Next) != 0 {
		t.Fatal(failed, err)
	}
	if _, err = Apply(plan, []Outcome{{Source: "tidal", Class: ClassFixture}}, 0); err != dataengine.ErrInvalidRequest {
		t.Fatal(err)
	}
}

func TestTransientStreamingRetriesThenAsksForReview(t *testing.T) {
	plan, err := Plan(DefaultSources(), Track{SourceKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := make([]Outcome, len(plan))
	for i, step := range plan {
		outcomes[i] = Outcome{Source: step.Name, Class: ClassNetwork}
	}
	first, err := Apply(plan, outcomes, 0)
	if err != nil || !first.Recoverable || first.RetryAfterSec != 600 || first.Downloaded || first.ReviewRequired || !equal(names(first.Next), names(plan)) {
		t.Fatal(first, err)
	}
	last, err := Apply(plan, outcomes, 3)
	if err != nil || last.Recoverable || !last.ReviewRequired || len(last.Next) != 0 || last.Downloaded || last.FailureClass != ClassNetwork {
		t.Fatal(last, err)
	}
}
