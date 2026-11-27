package app

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

func TestJobCountCacheCoalescesDirtyPolls(t *testing.T) {
	start := time.Unix(1000, 0)
	var cache jobCountCache
	var calls int
	compute := func() (map[string]int, error) {
		calls++
		return map[string]int{"total": calls, "queued": calls - 1}, nil
	}

	first, err := cache.load(start, compute)
	if err != nil || first["total"] != 1 || calls != 1 {
		t.Fatalf("first=%v calls=%d err=%v", first, calls, err)
	}
	cache.invalidate()
	coalesced, err := cache.load(start.Add(3*time.Second), compute)
	if err != nil || coalesced["total"] != 1 || calls != 1 {
		t.Fatalf("coalesced=%v calls=%d err=%v", coalesced, calls, err)
	}
	refreshed, err := cache.load(start.Add(jobCountCacheRefreshInterval), compute)
	if err != nil || refreshed["total"] != 2 || calls != 2 {
		t.Fatalf("refreshed=%v calls=%d err=%v", refreshed, calls, err)
	}
}

func TestJobCountCacheConcurrentMissComputesOnce(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var cache jobCountCache
	var calls atomic.Int32
	compute := func() (map[string]int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return map[string]int{"total": 7}, nil
	}

	const callers = 8
	results := make(chan map[string]int, callers)
	var wait sync.WaitGroup
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, err := cache.load(time.Unix(1000, 0), compute)
			if err != nil {
				t.Error(err)
				return
			}
			results <- value
		}()
	}
	<-started
	close(release)
	wait.Wait()
	close(results)
	for value := range results {
		if value["total"] != 7 {
			t.Fatalf("unexpected value %v", value)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("compute called %d times", calls.Load())
	}
}

func TestWorkflowCountsChangedIgnoresTaskProgressButTracksState(t *testing.T) {
	oldTask := taskrepo.Task{Spec: taskrepo.Spec{ID: "task", Lane: "aux"}, State: "running", WorkflowJobID: "job"}
	next := oldTask
	next.Progress = 90
	next.Message = "still working"
	oldRaw, _ := json.Marshal(oldTask)
	nextRaw, _ := json.Marshal(next)
	conditions := []dataengine.Condition{{Key: []byte("task/hash"), Value: oldRaw}}
	mutations := []dataengine.Mutation{{Key: []byte("task/hash"), Value: nextRaw}}
	if workflowCountsChanged(conditions, mutations) {
		t.Fatal("progress-only update invalidated counts")
	}

	next.State = "completed"
	nextRaw, _ = json.Marshal(next)
	mutations[0].Value = nextRaw
	if !workflowCountsChanged(conditions, mutations) {
		t.Fatal("terminal task update did not invalidate counts")
	}
	if !workflowCountsChanged(nil, []dataengine.Mutation{{Key: []byte("workflow-job/hash"), Value: []byte("{}")}}) {
		t.Fatal("workflow job update did not invalidate counts")
	}
}
