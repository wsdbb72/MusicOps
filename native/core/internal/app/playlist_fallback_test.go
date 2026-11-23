package app

import (
	"errors"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
)

func TestNeteasePlaylistFallbackKeepsOriginalPlatform(t *testing.T) {
	plan := SearchPlan{PlaylistTrack: "夏恋慕"}
	if !isNeteasePlaylistFallback("pl-netease-590536461-batch", plan) ||
		isNeteasePlaylistFallback("pl-qqmusic-123-batch", plan) ||
		!isNeteasePlaylistFallback("pl-netease-590536461-batch", SearchPlan{}) {
		t.Fatal("fallback source did not reflect the playlist origin")
	}
	legacy := SearchPlan{Identity: Identity{Payload: `{"artist":"Artist","album":"Album","title":"Track"}`}}
	if got := neteasePlaylistTitle(legacy); got != "Track" {
		t.Fatal(got)
	}
	order := downloadpolicy.CanonicalOrder()
	steps, err := playlistFallbackSteps(order, true, false)
	if err != nil || len(steps) < 2 || steps[0].Name != "netease" {
		t.Fatal("NetEase must be first, but remaining providers must stay available", steps, err)
	}
	exclusive, err := playlistFallbackSteps(order, true, true)
	if err != nil || len(exclusive) != 1 || exclusive[0].Name != "netease" {
		t.Fatal(exclusive, err)
	}
	normal, err := playlistFallbackSteps(order, false, false)
	if err != nil || len(normal) < 2 || normal[0].Name == "netease" {
		t.Fatal(normal, err)
	}
	album, exclusiveMode := neteaseFallbackRouting("pl-netease-590536461-batch", plan)
	if !album || exclusiveMode {
		t.Fatal("a NetEase-origin playlist must try NetEase first without disabling safe fallbacks")
	}
	explicit := plan
	explicit.NeteaseOnly = true
	album, exclusiveMode = neteaseFallbackRouting("pl-netease-590536461-batch", explicit)
	if !album || !exclusiveMode {
		t.Fatal("an explicit NetEase-only plan must stay exclusive")
	}
	album, exclusiveMode = neteaseFallbackRouting("pl-qqmusic-123-batch", plan)
	if album || exclusiveMode {
		t.Fatal("non-Netease playlist was routed through the NetEase album path")
	}
}

func TestLegacyNeteasePlanIsPromotedToSourceFallback(t *testing.T) {
	report := searchdecision.Report{
		Scene: "expand", IdentityConfirmed: false, StreamingEnabled: false,
		Sites: []searchdecision.SiteReport{
			{Site: "open.cd", Status: "NO_RESOURCE", Attempted: true}, {Site: "dicmusic", Status: "NO_RESOURCE", Attempted: true},
			{Site: "orpheus", Status: "NO_RESOURCE", Attempted: true}, {Site: "redacted", Status: "DISABLED"},
		},
	}
	markNeteaseSourceFallback(&report)
	if !report.SourceFallback || !report.IdentityConfirmed || !report.StreamingEnabled {
		t.Fatal(report)
	}
	decision, err := searchdecision.Decide(report)
	if err != nil || decision.Action != searchdecision.ActionFallback {
		t.Fatal(decision, err)
	}
}

func TestNeteaseNoSourceCanContinueTheFallbackWalk(t *testing.T) {
	err := error(&neteaseAlbumContinue{message: "no source", class: downloadpolicy.ClassNoResult})
	var keepGoing *neteaseAlbumContinue
	if !errors.As(err, &keepGoing) || keepGoing.class != downloadpolicy.ClassNoResult {
		t.Fatal("NetEase no-source result was not classified as safe to continue")
	}
}

func TestGelatoSameAlbumRemixAliasRequiresIdentityAndDuration(t *testing.T) {
	plan := SearchPlan{ExpectedArtist: "Gelato", ExpectedTitle: "野狼王的士高", PlaylistTrack: "兔子舞", ExpectedDuration: 241}
	match := streamSong{Name: "兔子舞 (Remix)", Artist: "Gelato", Album: "野狼王的士高", Duration: 241}
	if !streamTitleIdentityMatches(plan, match) {
		t.Fatal("same artist, album and duration did not admit the Gelato Remix label")
	}
	match.Duration = 340
	if streamPlaylistCardMatches(plan, match) {
		t.Fatal("duration mismatch was admitted by the Gelato alias")
	}
	match.Duration = 241
	plan.ExpectedArtist = "Another Artist"
	if streamPlaylistCardMatches(plan, match) {
		t.Fatal("different artist was admitted by the Gelato alias")
	}
}

func TestChineseDJArtistAliasStillRequiresTitleAndDuration(t *testing.T) {
	if !streamChineseDJAliasMatches("DJ奶小深", "奶小深") {
		t.Fatal("DJ prefix on a Chinese stage name was not recognized")
	}
	if streamChineseDJAliasMatches("DJ Snake", "Snake") {
		t.Fatal("Latin DJ stage name was incorrectly stripped")
	}
	plan := SearchPlan{ExpectedArtist: "DJ奶小深", ExpectedTitle: "Remix", PlaylistTrack: "爱河", ExpectedDuration: 326}
	match := streamSong{Name: "爱河", Artist: "奶小深", Album: "Remix", Duration: 326}
	if !streamPlaylistCardMatches(plan, match) {
		t.Fatal("exact DJ alias result failed title or duration identity checks")
	}
	match.Duration = 340
	if !streamArtistIdentityMatches(plan.ExpectedArtist, match.Artist) {
		t.Fatal("artist alias should be independent from duration")
	}
	if streamPlaylistCardMatches(plan, match) {
		t.Fatal("DJ alias admitted a 340-second recording for a 326-second target")
	}
}
