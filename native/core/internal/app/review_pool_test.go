package app

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
)

func TestBoundedReviewCandidatesKeepsBestMatchesWithinPromptBudget(t *testing.T) {
	pool := make([]reviewai.Candidate, 0, 160)
	for i := 0; i < 159; i++ {
		pool = append(pool, reviewai.Candidate{ID: "noise-" + strconv.Itoa(i), Title: "Unrelated", Artists: []string{"Other"}, Format: "AAC"})
	}
	pool = append(pool, reviewai.Candidate{ID: "exact", Title: "Blue", Artists: []string{"Artist"}, Format: "FLAC"})
	got := boundedReviewCandidates(pool, "Blue", "Artist")
	if len(got) != reviewAICandidateLimit || got[0].ID != "exact" {
		t.Fatalf("bounded=%d first=%+v", len(got), got[0])
	}
	encoded, err := json.Marshal(reviewai.Request{ExpectedTitle: "Blue", ExpectedArtist: "Artist", Candidates: got})
	if err != nil || len(encoded) >= 100000 {
		t.Fatalf("bounded prompt bytes=%d err=%v", len(encoded), err)
	}
	seen := map[string]struct{}{}
	for _, candidate := range got {
		if _, exists := seen[candidate.ID]; exists {
			t.Fatalf("duplicate candidate %s", candidate.ID)
		}
		seen[candidate.ID] = struct{}{}
	}
}
