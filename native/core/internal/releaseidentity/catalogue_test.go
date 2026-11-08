package releaseidentity

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func openCatalogue(t *testing.T, budget int) *Catalogue {
	t.Helper()
	engine, err := dataengine.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return New(engine, budget, time.Hour)
}

func trackQuery(title string, duration float64) Query {
	return Query{Album: "Kind of Blue", Edition: "", Catalogue: "CL 1355", Title: title, Artists: []string{"Miles Davis"}, Duration: duration, HasDuration: true}
}

func studioObservation() Observation {
	return Observation{
		Source: "fixture", Kind: "catalog", ReleaseID: "fixture-release-1", Album: "Kind of Blue", Catalogue: "CL 1355",
		Artists: []string{"Miles Davis"}, TracklistComplete: true,
		Tracks: []Track{
			{Title: "So What", Artists: []string{"Miles Davis"}, Duration: 562, HasDuration: true},
			{Title: "Freddie Freeloader", Artists: []string{"Miles Davis"}, Duration: 578, HasDuration: true},
		},
	}
}

func TestBatchReusesOneReleaseAndKeepsAFailureIsolated(t *testing.T) {
	catalogue := openCatalogue(t, 4)
	now := time.Unix(1_700_000_000, 0)
	observation := studioObservation()
	failed := Observation{Failed: true, FailureClass: "network"}
	items := []Item{
		{Query: trackQuery("So What", 562), Observation: &observation},
		{Query: trackQuery("Freddie Freeloader", 580), Observation: &observation},
		{Query: trackQuery("All Blues", 680), Observation: &observation},
		{Query: Query{Album: "Other", Title: "Broken", Artists: []string{"Someone"}, Duration: 100, HasDuration: true}, Observation: &failed},
	}
	results, summary, err := catalogue.Resolve(now, items)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].IdentityConfirmed || results[0].ReleaseID != "fixture-release-1" || results[0].ProductionVerified {
		t.Fatal(results[0])
	}
	if !results[1].IdentityConfirmed || !results[1].Reused || results[1].ReleaseID != results[0].ReleaseID {
		t.Fatal(results[1])
	}
	if results[2].Status != "unresolved" || results[2].IdentityConfirmed || !results[2].Reused {
		t.Fatal(results[2])
	}
	if results[3].Status != "isolated_failure" || results[3].FailureClass != "network" || results[3].IdentityConfirmed {
		t.Fatal(results[3])
	}
	if summary.Confirmed != 2 || summary.Unresolved != 1 || summary.IsolatedFailure != 1 || summary.BudgetSpent != 2 || summary.Groups != 2 {
		t.Fatalf("%+v", summary)
	}
	again, summary, err := catalogue.Resolve(now.Add(time.Minute), items)
	if err != nil || !again[0].CacheHit || !again[3].CacheHit || again[3].Status != "isolated_failure" || again[2].IdentityConfirmed || summary.BudgetSpent != 2 {
		t.Fatal(again, summary, err)
	}
}

func TestUnknownLiveAndAIDoNotBecomeConfirmed(t *testing.T) {
	catalogue := openCatalogue(t, 8)
	now := time.Unix(1_700_000_000, 0)
	missingID := studioObservation()
	missingID.ReleaseID = ""
	liveQuery := trackQuery("So What (Live)", 562)
	liveObservation := studioObservation()
	aiQuery := trackQuery("So What", 562)
	aiQuery.Album = "Alias Study"
	ai := studioObservation()
	ai.Album = aiQuery.Album
	ai.Kind = "ai"
	partialQuery := trackQuery("So What", 562)
	partialQuery.Album = "Partial"
	incomplete := studioObservation()
	incomplete.Album = partialQuery.Album
	incomplete.TracklistComplete = false
	items := []Item{
		{Query: trackQuery("So What", 562), Observation: &missingID},
		{Query: liveQuery, Observation: &liveObservation},
		{Query: aiQuery, Observation: &ai},
		{Query: partialQuery, Observation: &incomplete},
		{Query: Query{Title: "No Album", Artists: []string{"未知"}, Duration: 10, HasDuration: true}, Observation: &missingID},
	}
	results, summary, err := catalogue.Resolve(now, items)
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		if result.IdentityConfirmed || result.ReleaseID != "" || result.ProductionVerified {
			t.Fatal(i, result)
		}
	}
	if results[0].Reason != "unknown_release" || results[1].Status != "conflict" || results[2].Reason != "evidence_cannot_confirm" || results[3].Reason != "incomplete_tracklist" {
		t.Fatal(results)
	}
	if summary.Confirmed != 0 {
		t.Fatal(summary)
	}
}

func TestRateLimitDefersTheRestOfTheBatch(t *testing.T) {
	catalogue := openCatalogue(t, 5)
	now := time.Unix(1_700_000_000, 0)
	limited := Observation{Failed: true, FailureClass: "rate_limited"}
	next := studioObservation()
	results, summary, err := catalogue.Resolve(now, []Item{
		{Query: Query{Album: "One", Title: "A", Artists: []string{"Artist"}, Duration: 180, HasDuration: true}, Observation: &limited},
		{Query: trackQuery("So What", 562), Observation: &next},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].FailureClass != "rate_limited" || results[1].Status != "deferred" || results[1].IdentityConfirmed || summary.Confirmed != 0 {
		t.Fatal(results, summary)
	}
}

func TestBudgetDoesNotTurnAMissIntoSuccess(t *testing.T) {
	catalogue := openCatalogue(t, 0)
	now := time.Unix(1_700_000_000, 0)
	observation := studioObservation()
	results, _, err := catalogue.Resolve(now, []Item{{Query: trackQuery("So What", 562), Observation: &observation}})
	if err != nil || results[0].Status != "deferred" || results[0].IdentityConfirmed {
		t.Fatal(results, err)
	}
}

func TestDurationOutsideThreeSecondsDoesNotMatch(t *testing.T) {
	decision := classify(trackQuery("So What", 570), ptr(studioObservation()))
	if decision.IdentityConfirmed || decision.Reason != "track_not_on_release" {
		t.Fatal(decision)
	}
	decision = classify(trackQuery("So What", 565), ptr(studioObservation()))
	if !decision.IdentityConfirmed {
		t.Fatal(decision)
	}
}

func TestFullwidthTitleMatchesTheSameTrack(t *testing.T) {
	query := trackQuery("Ｓｏ Ｗｈａｔ", 562)
	decision := classify(query, ptr(studioObservation()))
	if !decision.IdentityConfirmed || decision.ReleaseID != "fixture-release-1" {
		t.Fatal(decision)
	}
}

func ptr(observation Observation) *Observation { return &observation }
