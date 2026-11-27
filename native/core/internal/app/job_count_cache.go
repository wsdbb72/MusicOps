package app

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// jobCountCache keeps the expensive root-family rollup out of the UI polling
// path. The desktop asks for counts every few seconds, while a full workflow
// snapshot walks every historical root. Mutations invalidate this cache; a
// short floor coalesces bursts from an actively progressing workflow without
// hiding terminal changes for more than one refresh interval.
const jobCountCacheRefreshInterval = 10 * time.Second

type jobCountCache struct {
	mu         sync.Mutex
	refreshMu  sync.Mutex
	values     map[string]int
	computedAt time.Time
	dirty      bool
	epoch      uint64
}

func (c *jobCountCache) invalidate() {
	c.mu.Lock()
	c.dirty = true
	c.epoch++
	c.mu.Unlock()
}

// load returns the cached rollup and recomputes only when the cache is empty,
// dirty, and older than the coalescing interval. If Pebble changes while a
// rollup is being built, the newer epoch keeps the cache dirty for the next
// pass instead of publishing stale data as clean.
func (c *jobCountCache) load(now time.Time, compute func() (map[string]int, error)) (map[string]int, error) {
	c.mu.Lock()
	if len(c.values) > 0 && (!c.dirty || now.Sub(c.computedAt) < jobCountCacheRefreshInterval) {
		result := cloneJobCounts(c.values)
		c.mu.Unlock()
		return result, nil
	}
	c.mu.Unlock()

	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	if len(c.values) > 0 && (!c.dirty || now.Sub(c.computedAt) < jobCountCacheRefreshInterval) {
		result := cloneJobCounts(c.values)
		c.mu.Unlock()
		return result, nil
	}
	epoch := c.epoch
	stale := cloneJobCounts(c.values)
	c.mu.Unlock()

	values, err := compute()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.computedAt = now
	if err != nil {
		// A stale rollup is more useful to the live UI than an empty counters
		// panel. Health still reports the underlying store failure separately.
		if len(stale) > 0 {
			return stale, nil
		}
		return nil, err
	}
	c.values = cloneJobCounts(values)
	if c.epoch == epoch {
		c.dirty = false
	}
	return cloneJobCounts(c.values), nil
}

func cloneJobCounts(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string]int, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

// observeNativeChanges invalidates the rollup only for state that can change a
// root's classification. Task progress rewrites the task document frequently;
// treating every write as a count change would recreate the polling I/O this
// cache exists to remove.
func (a *App) observeNativeChanges(conditions []dataengine.Condition, mutations []dataengine.Mutation) {
	if workflowCountsChanged(conditions, mutations) {
		a.jobCountRollup.invalidate()
	}
}

func workflowCountsChanged(conditions []dataengine.Condition, mutations []dataengine.Mutation) bool {
	oldTasks := make(map[string]taskrepo.Task)
	for _, condition := range conditions {
		if condition.Absent || !strings.HasPrefix(string(condition.Key), "task/") {
			continue
		}
		var task taskrepo.Task
		if json.Unmarshal(condition.Value, &task) == nil {
			oldTasks[string(condition.Key)] = task
		}
	}
	for _, mutation := range mutations {
		key := string(mutation.Key)
		if strings.HasPrefix(key, "workflow-job/") {
			return true
		}
		if mutation.Delete || !strings.HasPrefix(key, "task/") {
			continue
		}
		var next taskrepo.Task
		if json.Unmarshal(mutation.Value, &next) != nil {
			continue
		}
		old, found := oldTasks[key]
		if !found || old.State != next.State || old.Lane != next.Lane || old.WorkflowJobID != next.WorkflowJobID {
			return true
		}
	}
	return false
}
