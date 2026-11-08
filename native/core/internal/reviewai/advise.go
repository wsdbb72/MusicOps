// Package reviewai asks a configured model to point at one real candidate.
// A pass is kept only when that candidate already satisfies reviewmatch.
// Malformed output is retried once. Timeouts stay review and are not cached as success.
package reviewai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
)

// Completer is one model call. The application supplies it only when a spend is authorized.
type Completer interface {
	Complete(context.Context, []byte) ([]byte, error)
}

// Candidate is one search hit sent to the model and checked locally afterwards.
// GroupID, Format and Year carry the release-identity evidence with the candidate
// so the model cache key and the local checks cover them.
type Candidate struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Artists []string `json:"artists,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	Edition string   `json:"edition,omitempty"`
	GroupID string   `json:"group_id,omitempty"`
	Format  string   `json:"format,omitempty"`
	Year    int      `json:"year,omitempty"`
}

// Request is the grounded input. The model does not receive permission to invent a release.
type Request struct {
	ExpectedTitle  string      `json:"expected_title"`
	ExpectedArtist string      `json:"expected_artist,omitempty"`
	Candidates     []Candidate `json:"candidates"`
}

// Result is the guarded outcome. Class separates an infrastructure failure from an identity doubt.
type Result struct {
	Pass        bool   `json:"pass"`
	CandidateID string `json:"candidate_id,omitempty"`
	Reason      string `json:"reason"`
	Class       string `json:"class"`
	Calls       int    `json:"calls"`
	CacheHit    bool   `json:"cache_hit"`
}

type verdict struct {
	Decision    string `json:"decision"`
	CandidateID string `json:"candidate_id"`
	Reason      string `json:"reason"`
}

// Cache deduplicates identical candidate sets and bounds concurrent model calls.
type Cache struct {
	slots chan struct{}
	mu    sync.Mutex
	saved map[string]Result
}

func NewCache(parallel int) *Cache {
	if parallel < 1 {
		parallel = 1
	}
	if parallel > 4 {
		parallel = 4
	}
	return &Cache{slots: make(chan struct{}, parallel), saved: map[string]Result{}}
}

// Advise calls the model at most twice. Identity failures are cached. Transport failures are not.
func (c *Cache) Advise(ctx context.Context, model Completer, request Request) Result {
	if c == nil || model == nil {
		return Result{Reason: "model_unconfigured", Class: "infrastructure"}
	}
	if len(request.Candidates) == 0 {
		return Result{Reason: "no_candidates", Class: "identity"}
	}
	key := cacheKey(request)
	c.mu.Lock()
	if saved, ok := c.saved[key]; ok {
		saved.CacheHit = true
		saved.Calls = 0
		c.mu.Unlock()
		return saved
	}
	c.mu.Unlock()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return Result{Reason: "timeout", Class: "infrastructure"}
	}
	prompt, _ := json.Marshal(request)
	calls := 0
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return Result{Reason: "timeout", Class: "infrastructure", Calls: calls}
		}
		body, err := model.Complete(ctx, prompt)
		calls++
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
				return Result{Reason: "timeout", Class: "infrastructure", Calls: calls}
			}
			return Result{Reason: "model_error", Class: "infrastructure", Calls: calls}
		}
		parsed, ok := parseVerdict(body)
		if !ok {
			continue
		}
		decided := decide(request, parsed)
		decided.Calls = calls
		if decided.Class == "identity" {
			c.mu.Lock()
			if previous, exists := c.saved[key]; exists {
				previous.CacheHit = true
				previous.Calls = 0
				c.mu.Unlock()
				return previous
			}
			c.saved[key] = decided
			c.mu.Unlock()
		}
		return decided
	}
	return Result{Reason: "malformed", Class: "infrastructure", Calls: calls}
}

func decide(request Request, parsed verdict) Result {
	if parsed.Decision != "pass" {
		return Result{Reason: "model_review", Class: "identity"}
	}
	var cited *Candidate
	var passing []reviewmatch.Hit
	for _, candidate := range request.Candidates {
		hit := reviewmatch.Hit{ID: candidate.ID, Title: candidate.Title, Artists: candidate.Artists, Aliases: candidate.Aliases, Edition: candidate.Edition, GroupID: candidate.GroupID, Format: candidate.Format, Year: candidate.Year}
		if reviewmatch.Compare(request.ExpectedTitle, request.ExpectedArtist, hit).Pass {
			passing = append(passing, hit)
		}
		if candidate.ID == parsed.CandidateID {
			copy := candidate
			cited = &copy
		}
	}
	if cited == nil {
		return Result{Reason: "unknown_candidate", Class: "identity"}
	}
	citedHit := reviewmatch.Hit{ID: cited.ID, Title: cited.Title, Artists: cited.Artists, Aliases: cited.Aliases, Edition: cited.Edition, GroupID: cited.GroupID, Format: cited.Format, Year: cited.Year}
	if !reviewmatch.Compare(request.ExpectedTitle, request.ExpectedArtist, citedHit).Pass {
		return Result{Reason: "evidence_rejected", Class: "identity"}
	}
	for _, other := range passing {
		if other.ID != cited.ID && !reviewmatch.Same(other, citedHit) {
			return Result{Reason: "ambiguous_candidates", Class: "identity"}
		}
	}
	return Result{Pass: true, CandidateID: cited.ID, Reason: "cited_evidence", Class: "identity"}
}

func parseVerdict(body []byte) (verdict, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var parsed verdict
	if err := decoder.Decode(&parsed); err != nil {
		return verdict{}, false
	}
	if parsed.Decision != "pass" && parsed.Decision != "review" {
		return verdict{}, false
	}
	return parsed, true
}

func cacheKey(request Request) string {
	cloned := request
	cloned.Candidates = append([]Candidate(nil), request.Candidates...)
	sort.Slice(cloned.Candidates, func(i, j int) bool { return cloned.Candidates[i].ID < cloned.Candidates[j].ID })
	raw, _ := json.Marshal(cloned)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
