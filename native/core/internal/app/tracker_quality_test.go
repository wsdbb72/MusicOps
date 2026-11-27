package app

import (
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
)

func intPointer(value int) *int { return &value }

func TestCandidateQualityPrefersHiResLosslessThenFreeThenSeeders(t *testing.T) {
	plan := SearchPlan{Budget: 2}
	hiRes := trackersearch.Candidate{Title: "Album [24bit 96kHz FLAC]", Media: map[string]any{"format": "FLAC", "encoding": "24bit Lossless"}, Seeders: intPointer(2), SiteFlags: map[string]any{"canUseToken": true}}
	cd := trackersearch.Candidate{Title: "Album [FLAC]", Media: map[string]any{"format": "FLAC", "encoding": "Lossless"}, Seeders: intPointer(20), SiteFlags: map[string]any{"canUseToken": true}}
	mp3 := trackersearch.Candidate{Title: "Album [MP3 320]", Media: map[string]any{"format": "MP3", "encoding": "320"}, Seeders: intPointer(50)}
	hiRank, hiOK := candidateQuality(plan, "dicmusic", hiRes)
	cdRank, cdOK := candidateQuality(plan, "open.cd", cd)
	mp3Rank, mp3OK := candidateQuality(plan, "open.cd", mp3)
	if !hiOK || !cdOK || !mp3OK || compareCandidateQuality(hiRank, cdRank) <= 0 || compareCandidateQuality(cdRank, mp3Rank) <= 0 {
		t.Fatalf("quality order wrong: hires=%+v/%v cd=%+v/%v mp3=%+v/%v", hiRank, hiOK, cdRank, cdOK, mp3Rank, mp3OK)
	}
	free := trackersearch.Candidate{Media: map[string]any{"format": "FLAC", "encoding": "Lossless"}, Seeders: intPointer(1), SiteFlags: map[string]any{"canUseToken": true, "isPersonalFreeleech": true}}
	paid := trackersearch.Candidate{Media: map[string]any{"format": "FLAC", "encoding": "Lossless"}, Seeders: intPointer(99), SiteFlags: map[string]any{"canUseToken": true}}
	freeRank, _ := candidateQuality(plan, "dicmusic", free)
	paidRank, _ := candidateQuality(plan, "dicmusic", paid)
	if compareCandidateQuality(freeRank, paidRank) <= 0 {
		t.Fatalf("free FL should win before seeders at equal quality: free=%+v paid=%+v", freeRank, paidRank)
	}
}

func TestCandidateQualitySkipsUnavailableTokenCandidateAndChoosesNext(t *testing.T) {
	paidHiRes := trackersearch.Candidate{CandidateID: "dicmusic:1", Media: map[string]any{"format": "FLAC", "encoding": "24bit Lossless"}, Seeders: intPointer(4), SiteFlags: map[string]any{"canUseToken": true}}
	if _, ok := candidateQuality(SearchPlan{Budget: 0}, "dicmusic", paidHiRes); !ok {
		t.Fatal("search cannot discard an FL candidate before its detail proves personal-free status")
	}
	noToken := trackersearch.Candidate{Media: map[string]any{"format": "FLAC", "encoding": "24bit Lossless"}, Seeders: intPointer(4), SiteFlags: map[string]any{"canUseToken": false}}
	if _, ok := candidateQuality(SearchPlan{Budget: 5}, "orpheus", noToken); ok {
		t.Fatal("orpheus candidate without FL token button was selected")
	}
	next := identityPick{site: "open.cd", id: "open.cd:2", hit: reviewmatch.Hit{Title: "Album", Artists: []string{"Artist"}, Format: "FLAC"}, quality: candidateQualityRank{Lossless: 1, BitDepth: 16, SampleRate: 44100, Seeders: 8}}
	if site, id := selectIdentity([]identityPick{next}); site != "open.cd" || id != "open.cd:2" {
		t.Fatalf("next candidate not selected: %s %s", site, id)
	}
}

func TestCandidateQualityDoesNotInventMissingTokenFlag(t *testing.T) {
	plan := SearchPlan{Budget: 3, SafetyMargin: 0}
	candidate := trackersearch.Candidate{Media: map[string]any{"format": "FLAC", "encoding": "Lossless"}, Seeders: intPointer(2)}
	if _, ok := candidateQuality(plan, "dicmusic", candidate); !ok {
		t.Fatal("an unreported canUseToken flag was treated as false")
	}
	if _, ok := candidateQuality(plan, "orpheus", candidate); !ok {
		t.Fatal("missing token availability evidence should not discard the candidate")
	}
}

func TestSelectIdentityChoosesHighestQualityAcrossSites(t *testing.T) {
	hit := reviewmatch.Hit{Title: "Album", Artists: []string{"Artist"}, Format: "FLAC"}
	picks := []identityPick{
		{site: "open.cd", id: "open.cd:1", hit: hit, quality: candidateQualityRank{Lossless: 1, BitDepth: 16, SampleRate: 44100, Seeders: 30}},
		{site: "dicmusic", id: "dicmusic:9", hit: hit, quality: candidateQualityRank{Lossless: 1, BitDepth: 24, SampleRate: 96000, Seeders: 2}},
		{site: "orpheus", id: "orpheus:8", hit: hit, quality: candidateQualityRank{Lossless: 1, BitDepth: 16, SampleRate: 44100, Seeders: 60}},
	}
	if site, id := selectIdentity(picks); site != "dicmusic" || id != "dicmusic:9" {
		t.Fatalf("selected %s %s", site, id)
	}
}

func TestSummarizeSiteCandidatesCarriesRetryAfter(t *testing.T) {
	row, _, _ := summarizeSiteCandidates(SearchPlan{ExpectedTitle: "Album"}, "orpheus", trackersearch.Observation{Status: "RATE_LIMITED", RetryAfterSec: 2400})
	if row.Status != "RATE_LIMITED" || row.RetryAfterSec != 2400 {
		t.Fatalf("row=%+v", row)
	}
}
