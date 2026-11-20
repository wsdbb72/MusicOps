package app

import (
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
	steps, err := playlistFallbackSteps(order, true)
	if err != nil || len(steps) != 1 || steps[0].Name != "netease" {
		t.Fatal(steps, err)
	}
	normal, err := playlistFallbackSteps(order, false)
	if err != nil || len(normal) < 2 || normal[0].Name == "netease" {
		t.Fatal(normal, err)
	}
	album, exclusive := neteaseFallbackRouting("pl-netease-590536461-batch", plan)
	if !album || exclusive {
		t.Fatal("Netease-origin playlist must follow the configured provider order unless netease_only was explicit")
	}
	plan.NeteaseOnly = true
	album, exclusive = neteaseFallbackRouting("pl-netease-590536461-batch", plan)
	if !album || !exclusive {
		t.Fatal("explicit netease_only routing was not preserved")
	}
	album, exclusive = neteaseFallbackRouting("pl-qqmusic-123-batch", plan)
	if album || exclusive {
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
