package app

import (
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
)

func TestNeteasePlaylistFallbackKeepsOriginalPlatform(t *testing.T) {
	plan := SearchPlan{PlaylistTrack: "夏恋慕"}
	if !isNeteasePlaylistFallback("pl-netease-590536461-batch", plan) ||
		isNeteasePlaylistFallback("pl-qqmusic-123-batch", plan) ||
		isNeteasePlaylistFallback("pl-netease-590536461-batch", SearchPlan{}) {
		t.Fatal("fallback source did not reflect the playlist origin")
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
}
