package reviewai

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type script struct {
	calls atomic.Int32
	reply func(int) ([]byte, error)
}

func (s *script) Complete(ctx context.Context, _ []byte) ([]byte, error) {
	n := int(s.calls.Add(1))
	return s.reply(n)
}

func TestAdviceAcceptsOnlyCitedEvidenceAndCachesIt(t *testing.T) {
	cache := NewCache(2)
	model := &script{reply: func(int) ([]byte, error) {
		return []byte(`{"decision":"pass","candidate_id":"dic:1","reason":"alias on the record"}`), nil
	}}
	request := Request{ExpectedTitle: "Blue", ExpectedArtist: "Artist", Candidates: []Candidate{{
		ID: "dic:1", Title: "蓝", Artists: []string{"Artist"}, Aliases: []string{"Blue"},
	}}}
	first := cache.Advise(context.Background(), model, request)
	second := cache.Advise(context.Background(), model, request)
	if !first.Pass || first.CandidateID != "dic:1" || first.Calls != 1 || first.Class != "identity" || second.Calls != 0 || !second.CacheHit || !second.Pass || model.calls.Load() != 1 {
		t.Fatal(first, second, model.calls.Load())
	}
}

func TestAdviceCacheKeyCoversReleaseEvidence(t *testing.T) {
	cache := NewCache(2)
	model := &script{reply: func(int) ([]byte, error) {
		return []byte(`{"decision":"pass","candidate_id":"dic:1","reason":"alias on the record"}`), nil
	}}
	one := Request{ExpectedTitle: "Blue", ExpectedArtist: "Artist", Candidates: []Candidate{{
		ID: "dic:1", Title: "蓝", Artists: []string{"Artist"}, Aliases: []string{"Blue"}, GroupID: "g1", Format: "FLAC", Year: 2020,
	}}}
	other := Request{ExpectedTitle: "Blue", ExpectedArtist: "Artist", Candidates: []Candidate{{
		ID: "dic:1", Title: "蓝", Artists: []string{"Artist"}, Aliases: []string{"Blue"}, GroupID: "g1", Format: "FLAC", Year: 2021,
	}}}
	first := cache.Advise(context.Background(), model, one)
	if !first.Pass || first.Calls != 1 {
		t.Fatal(first)
	}
	// Only the release year differs: the cache must not serve the first verdict.
	second := cache.Advise(context.Background(), model, other)
	if second.CacheHit || second.Calls != 1 || model.calls.Load() != 2 {
		t.Fatal(second, model.calls.Load())
	}
	// The identical request is still a cache hit.
	third := cache.Advise(context.Background(), model, one)
	if !third.CacheHit || third.Calls != 0 || model.calls.Load() != 2 {
		t.Fatal(third, model.calls.Load())
	}
}

func TestAdviceRejectsHallucinationConflictAndTimeout(t *testing.T) {
	cache := NewCache(2)
	unknown := cache.Advise(context.Background(), &script{reply: func(int) ([]byte, error) {
		return []byte(`{"decision":"pass","candidate_id":"missing","reason":"invented"}`), nil
	}}, Request{ExpectedTitle: "Blue", Candidates: []Candidate{{ID: "dic:1", Title: "蓝", Aliases: []string{"Blue"}}}})
	if unknown.Pass || unknown.Class != "identity" || unknown.Reason != "unknown_candidate" {
		t.Fatal(unknown)
	}
	live := cache.Advise(context.Background(), &script{reply: func(int) ([]byte, error) {
		return []byte(`{"decision":"pass","candidate_id":"dic:2","reason":"same album"}`), nil
	}}, Request{ExpectedTitle: "Blue", Candidates: []Candidate{{ID: "dic:2", Title: "蓝 Live", Aliases: []string{"Blue"}}}})
	if live.Pass || live.Reason != "evidence_rejected" || live.Class != "identity" {
		t.Fatal(live)
	}
	ambiguous := cache.Advise(context.Background(), &script{reply: func(int) ([]byte, error) {
		return []byte(`{"decision":"pass","candidate_id":"a","reason":"first"}`), nil
	}}, Request{ExpectedTitle: "Example", Candidates: []Candidate{
		{ID: "a", Title: "Example", Artists: []string{"One"}},
		{ID: "b", Title: "Example", Artists: []string{"Two"}},
	}})
	if ambiguous.Pass || ambiguous.Reason != "ambiguous_candidates" {
		t.Fatal(ambiguous)
	}
	retries := &script{reply: func(n int) ([]byte, error) {
		if n == 1 {
			return []byte(`not-json`), nil
		}
		return []byte(`{"decision":"review","candidate_id":"dic:1","reason":"unsure"}`), nil
	}}
	recovered := cache.Advise(context.Background(), retries, Request{ExpectedTitle: "Blue", Candidates: []Candidate{{ID: "dic:1", Title: "Blue"}}})
	if recovered.Pass || recovered.Calls != 2 || recovered.Class != "identity" {
		t.Fatal(recovered)
	}
	hung := &script{reply: func(int) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	timed := Request{ExpectedTitle: "Other", Candidates: []Candidate{{ID: "z", Title: "Other"}}}
	failed := cache.Advise(ctx, hung, timed)
	if failed.Pass || failed.Class != "infrastructure" || failed.Calls != 1 {
		t.Fatal(failed)
	}
	again := cache.Advise(context.Background(), &script{reply: func(int) ([]byte, error) {
		return []byte(`{"decision":"review","candidate_id":"z","reason":"still unsure"}`), nil
	}}, timed)
	if again.CacheHit || again.Pass || again.Calls != 1 || again.Class != "identity" {
		t.Fatal(again)
	}
}
