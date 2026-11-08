package releaseidentity

import (
	"encoding/json"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// Summary counts offline classifications. It is not a download success rate.
type Summary struct {
	Items           int `json:"items"`
	Confirmed       int `json:"confirmed"`
	Unresolved      int `json:"unresolved"`
	Conflict        int `json:"conflict"`
	IsolatedFailure int `json:"isolated_failure"`
	Deferred        int `json:"deferred"`
	NotLookedUp     int `json:"not_looked_up"`
	CacheHits       int `json:"cache_hits"`
	BudgetSpent     int `json:"budget_spent"`
	Groups          int `json:"groups"`
}

// Item is one track plus the observation already gathered for its release.
// Observations are not fetched here.
type Item struct {
	Query       Query
	Observation *Observation
}

type cacheEntry struct {
	Result    Result `json:"result"`
	ExpiresNS int64  `json:"expires_ns"`
}

// Catalogue spends one lookup budget per release group and stores that group
// verdict in the existing embedded engine. Each track is still matched on its
// own; a sibling that is absent from the observation is not confirmed.
type Catalogue struct {
	engine *dataengine.Engine
	Budget int
	TTL    time.Duration
	spent  int
	halt   bool
}

func New(engine *dataengine.Engine, budget int, ttl time.Duration) *Catalogue {
	if budget < 0 {
		budget = 0
	}
	return &Catalogue{engine: engine, Budget: budget, TTL: ttl}
}

func (c *Catalogue) Resolve(now time.Time, items []Item) ([]Result, Summary, error) {
	results := make([]Result, len(items))
	summary := Summary{Items: len(items)}
	if c == nil || c.engine == nil || now.IsZero() {
		return nil, Summary{}, dataengine.ErrInvalidRequest
	}
	groups := map[string][]int{}
	order := make([]string, 0)
	for i, item := range items {
		key, ok := groupKey(item.Query)
		if !ok {
			key = "track:" + digestText(strictText(item.Query.Title)+"\x1f"+artistKey(item.Query.Artists)+"\x1f"+formatCount(i))
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	summary.Groups = len(order)
	for _, key := range order {
		indexes := groups[key]
		base, hit, err := c.groupDecision(now, key, items, indexes)
		if err != nil {
			return nil, Summary{}, err
		}
		if hit {
			summary.CacheHits += len(indexes)
		}
		for offset, index := range indexes {
			results[index] = applyTrack(base, items[index])
			if offset > 0 {
				results[index].Reused = true
			}
			if hit {
				results[index].CacheHit = true
			}
			switch results[index].Status {
			case "confirmed":
				summary.Confirmed++
			case "unresolved":
				summary.Unresolved++
			case "conflict":
				summary.Conflict++
			case "isolated_failure":
				summary.IsolatedFailure++
			case "deferred":
				summary.Deferred++
			case "not_looked_up":
				summary.NotLookedUp++
			default:
				return nil, Summary{}, dataengine.ErrInvalidRequest
			}
		}
	}
	summary.BudgetSpent = c.spent
	return results, summary, nil
}

func (c *Catalogue) groupDecision(now time.Time, key string, items []Item, indexes []int) (Result, bool, error) {
	if cached, ok, err := c.read(key, now); err != nil {
		return Result{}, false, err
	} else if ok {
		return cached, true, nil
	}
	if c.halt || c.spent >= c.Budget {
		return Result{Status: "deferred", Reason: "rate_budget"}, false, nil
	}
	c.spent++
	decision := sharedObservation(items, indexes)
	if decision.Status == "isolated_failure" && decision.FailureClass == "rate_limited" {
		c.halt = true
	}
	if decision.ProductionVerified || decision.Status == "confirmed" {
		return Result{}, false, dataengine.ErrInvalidRequest
	}
	if err := c.write(key, decision, now); err != nil {
		return Result{}, false, err
	}
	return decision, false, nil
}

func sharedObservation(items []Item, indexes []int) Result {
	first := items[indexes[0]].Observation
	for _, index := range indexes[1:] {
		if !sameObservation(first, items[index].Observation) {
			return Result{Status: "conflict", Reason: "mixed_observations"}
		}
	}
	return observationLevel(items[indexes[0]].Query, first)
}

func sameObservation(left, right *Observation) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	encodedLeft, errLeft := json.Marshal(left)
	encodedRight, errRight := json.Marshal(right)
	return errLeft == nil && errRight == nil && string(encodedLeft) == string(encodedRight)
}

func applyTrack(base Result, item Item) Result {
	if base.Status != "ready" {
		return base
	}
	if item.Observation == nil || !trackMatches(item.Query, *item.Observation) {
		return Result{Status: "unresolved", Reason: "track_not_on_release"}
	}
	return Result{Status: "confirmed", Reason: "track_release_agreed", ReleaseID: base.ReleaseID, IdentityConfirmed: true}
}

func cacheKey(key string) []byte { return []byte("release-cache/" + key) }

func (c *Catalogue) read(key string, now time.Time) (Result, bool, error) {
	var entry cacheEntry
	var found bool
	err := c.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(cacheKey(key))
		if err != nil || !ok {
			return err
		}
		if json.Unmarshal(raw, &entry) != nil || entry.ExpiresNS <= 0 {
			return dataengine.ErrInvalidRequest
		}
		found = entry.ExpiresNS > now.UnixNano()
		return nil
	})
	if err != nil || !found {
		return Result{}, false, err
	}
	if entry.Result.ProductionVerified || (entry.Result.IdentityConfirmed && entry.Result.ReleaseID == "") {
		return Result{}, false, dataengine.ErrInvalidRequest
	}
	return entry.Result, true, nil
}

func (c *Catalogue) write(key string, result Result, now time.Time) error {
	if c.TTL <= 0 {
		return nil
	}
	payload, err := json.Marshal(cacheEntry{Result: result, ExpiresNS: now.Add(c.TTL).UnixNano()})
	if err != nil {
		return err
	}
	var current []byte
	var exists bool
	err = c.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(cacheKey(key))
		current, exists = raw, ok
		return err
	})
	if err != nil {
		return err
	}
	condition := dataengine.Condition{Key: cacheKey(key), Absent: !exists, Value: current}
	applied, err := c.engine.CompareAndSwap([]dataengine.Condition{condition}, []dataengine.Mutation{{Key: cacheKey(key), Value: payload}})
	if err != nil {
		return err
	}
	if !applied {
		return dataengine.ErrInvalidRequest
	}
	return nil
}
