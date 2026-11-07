package taskrepo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func fixture(t *testing.T) (*Repository, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	engine, err := dataengine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return New(engine), path
}
func spec(id, lane string, priority int) Spec {
	return Spec{ID: id, Command: "identify_metadata", Lane: lane, Priority: priority, CreatedNS: 1000}
}
func enqueue(t *testing.T, r *Repository, id, lane string) {
	t.Helper()
	if _, err := r.Enqueue("enqueue-"+id, spec(id, lane, 1), []byte("arguments")); err != nil {
		t.Fatal(err)
	}
}
func start(t *testing.T, r *Repository, id, lane string) Token {
	t.Helper()
	enqueue(t, r, id, lane)
	claim, err := r.Claim("claim-"+id, lane, "boot-owner", 2000)
	if err != nil || claim.Task == nil {
		t.Fatalf("%+v %v", claim, err)
	}
	token := Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
	if _, err = r.Begin("begin-"+id, token, 2100); err != nil {
		t.Fatal(err)
	}
	return token
}
func state(t *testing.T, r *Repository, id, want string) *Task {
	t.Helper()
	task, err := r.Get(id)
	if err != nil || task == nil || task.State != want {
		t.Fatalf("want %s, got %+v %v", want, task, err)
	}
	return task
}

func TestSummaryPagesCountsAndBlobDeduplication(t *testing.T) {
	r, _ := fixture(t)
	payload := bytes.Repeat([]byte("large configuration "), 50000)
	for i := 0; i < 205; i++ {
		id := fmt.Sprint(i)
		if _, err := r.Enqueue("e-"+id, spec(id, "identify", 2), payload); err != nil {
			t.Fatal(err)
		}
	}
	var cursor []byte
	seen := map[string]bool{}
	for {
		page, err := r.List("identify", "queued", cursor, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range page.Tasks {
			if seen[task.ID] {
				t.Fatal("duplicate page")
			}
			seen[task.ID] = true
			raw, _ := json.Marshal(task)
			if len(raw) > 1024 {
				t.Fatal("manifest in summary")
			}
		}
		if len(page.Next) == 0 {
			break
		}
		cursor = page.Next
	}
	if len(seen) != 205 {
		t.Fatal("missing tasks")
	}
	counts, err := r.Counts()
	if err != nil || counts.States["queued"] != 205 {
		t.Fatal(counts, err)
	}
	raw, found, err := r.Data("1", "arguments", 0)
	if err != nil || !found || !bytes.Equal(raw, payload) {
		t.Fatal("payload changed")
	}
	r.engine.View(func(v *dataengine.View) error {
		page, err := v.Scan([]byte("task-blob/"), nil, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatal("identical blob duplicated")
		}
		return nil
	})
}

func TestQueuedPauseAndCancellationBeforeBegin(t *testing.T) {
	r, _ := fixture(t)
	enqueue(t, r, "a", "identify")
	r.Control("pause", "a", "pause", 2000)
	if claim, err := r.Claim("empty", "identify", "owner", 2100); err != nil || claim.Task != nil {
		t.Fatal("paused task claimed")
	}
	r.Control("resume", "a", "resume", 2200)
	claim, err := r.Claim("claim", "identify", "owner", 2300)
	if err != nil {
		t.Fatal(err)
	}
	token := Token{ID: "a", Owner: "owner", Fence: claim.Task.Fence}
	r.Control("cancel", "a", "cancel", 2400)
	state(t, r, "a", "cancelled")
	if _, err = r.Begin("begin", token, 2500); !errors.Is(err, ErrStale) {
		t.Fatal("cancelled claim started", err)
	}
	attempt, err := r.Attempt("a", token.Fence)
	if err != nil || attempt.State != "cancelled" {
		t.Fatal(attempt, err)
	}
	counts, _ := r.Counts()
	if counts.States["cancelled"] != 1 || counts.States["running"] != 0 {
		t.Fatal(counts)
	}
}

func TestPauseAcknowledgementAndCancellationWakeup(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "a", "publish")
	point := Point{Sequence: 1, Percent: 20, Phase: "copying", NowNS: 2200}
	r.Progress(token, point)
	r.Control("pause", "a", "pause", 2300)
	if !state(t, r, "a", "running").PauseRequested {
		t.Fatal("pause missing")
	}
	reply, err := r.Progress(token, point)
	if err != nil || reply.Decision != "pause" || reply.Task.State != "paused" {
		t.Fatal(reply, err)
	}
	r.Control("cancel", "a", "cancel", 2400)
	reply, err = r.Progress(token, point)
	if err != nil || reply.Decision != "cancel" {
		t.Fatal(reply, err)
	}
	if _, err = r.Finish("finish", token, Completion{State: "cancelled", NowNS: 2500}, nil); err != nil {
		t.Fatal(err)
	}
	state(t, r, "a", "cancelled")
}

func TestLateProgressCannotOverwriteControlsOrCompletion(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "a", "identify")
	for i := 1; i <= 3; i++ {
		if _, err := r.Progress(token, Point{Sequence: uint64(i), Percent: float64(i * 10), Phase: "working", NowNS: int64(2200 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	r.Control("pause", "a", "pause", 2300)
	old := Point{Sequence: 1, Percent: 10, Phase: "working", NowNS: 2201}
	reply, err := r.Progress(token, old)
	if err != nil || reply.Task.Progress != 30 || !reply.Task.PauseRequested {
		t.Fatal(reply, err)
	}
	r.Control("resume", "a", "resume", 2400)
	if _, err = r.Finish("finish", token, Completion{State: "completed", NowNS: 2500}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Progress(token, old); !errors.Is(err, ErrStale) {
		t.Fatal("late callback changed terminal state")
	}
}

func TestProgressReplayDoesNotCreateCommandReceipts(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "a", "aux")
	point := Point{Sequence: 1, Percent: 10, Phase: "reading", NowNS: 2200}
	first, err := r.Progress(token, point)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := r.Progress(token, point)
		if err != nil || again.Task.Revision != first.Task.Revision {
			t.Fatal("duplicate progress wrote state", err)
		}
	}
	point.Message = "different"
	if _, err = r.Progress(token, point); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("sequence conflict accepted")
	}
	r.engine.View(func(v *dataengine.View) error {
		page, err := v.Scan([]byte("task-operation/"), nil, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 3 {
			t.Fatalf("poll receipts: %d", len(page.Items))
		}
		return nil
	})
}

func TestRejectedReceiptSurvivesRestartAndCannotChangeOutcome(t *testing.T) {
	r, path := fixture(t)
	enqueue(t, r, "a", "identify")
	if _, err := r.Control("rejected", "a", "retry", 2000); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	claim, _ := r.Claim("claim", "identify", "owner", 2100)
	token := Token{ID: "a", Owner: "owner", Fence: claim.Task.Fence}
	r.Begin("begin", token, 2200)
	r.Finish("failed", token, Completion{State: "failed", NowNS: 2300}, nil)
	r.engine.Close()
	engine, err := dataengine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	r = New(engine)
	if _, err = r.Control("rejected", "a", "retry", 2400); !errors.Is(err, ErrStale) {
		t.Fatal("rejection changed")
	}
	status, _ := r.Request("rejected")
	if status.State != "REJECTED" {
		t.Fatal(status)
	}
	if _, err = r.Control("fresh", "a", "retry", 2500); err != nil {
		t.Fatal(err)
	}
	replay, err := r.Enqueue("enqueue-a", spec("a", "identify", 1), []byte("arguments"))
	if err != nil || !replay.Replayed {
		t.Fatal(err)
	}
	if state(t, r, "a", "queued").Revision == replay.Task.Revision {
		t.Fatal("historical receipt rewrote current state")
	}
	if _, err = r.Control("fresh", "a", "cancel", 2600); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("request conflict accepted")
	}
}

func TestResultsAndFencingSurviveRetry(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "a", "transcode")
	old := []byte(`{"legacy":NaN,"result":"preserve raw bytes"}`)
	if _, err := r.Finish("failed", token, Completion{State: "failed", NowNS: 2200, Payload: old}, nil); err != nil {
		t.Fatal(err)
	}
	r.Control("retry", "a", "retry", 2300)
	raw, found, err := r.Data("a", "result", token.Fence)
	if err != nil || !found || !bytes.Equal(raw, old) {
		t.Fatal("history lost", err)
	}
	claim, _ := r.Claim("next", "transcode", "other", 2400)
	next := Token{ID: "a", Owner: "other", Fence: claim.Task.Fence}
	if next.Fence <= token.Fence {
		t.Fatal("fence reused")
	}
	r.Begin("next-begin", next, 2500)
	if _, err = r.Finish("late", token, Completion{State: "completed", NowNS: 2600}, nil); !errors.Is(err, ErrStale) {
		t.Fatal("stale executor committed")
	}
	if _, err = r.Finish("done", next, Completion{State: "completed", NowNS: 2700, Payload: []byte("new")}, nil); err != nil {
		t.Fatal(err)
	}
	raw, found, err = r.Data("a", "result", 0)
	if err != nil || !found || string(raw) != "new" {
		t.Fatal(err)
	}
}

func TestUncertainExecutionIsNotRequeuedByTimer(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "a", "publish")
	if _, err := r.Interrupt("closed", token, 2200, "owned process exited"); err != nil {
		t.Fatal(err)
	}
	if state(t, r, "a", "recovery_required").Fence <= token.Fence {
		t.Fatal("fence not invalidated")
	}
	if _, err := r.Control("retry", "a", "retry", 999999999); !errors.Is(err, ErrStale) {
		t.Fatal("uncertain effects retried")
	}
	if claim, err := r.Claim("later", "publish", "other", 999999999); err != nil || claim.Task != nil {
		t.Fatal("elapsed time stole execution")
	}
	if _, err := r.Finish("late", token, Completion{State: "completed", NowNS: 2300}, nil); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
}

func TestConcurrentClaimsKeepCountersAndOwnershipConsistent(t *testing.T) {
	r, _ := fixture(t)
	enqueue(t, r, "a", "identify")
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := r.Claim(fmt.Sprint("claim-", i), "identify", fmt.Sprint("owner-", i), 2000)
			if err != nil {
				t.Error(err)
			} else if result.Task != nil {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("multiple claims", winners.Load())
	}
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint("many-", i)
			enqueue(t, r, id, "aux")
			claim, err := r.Claim("c-"+id, "aux", id, 3000)
			if err != nil || claim.Task == nil {
				t.Error(err)
				return
			}
			token := Token{ID: claim.Task.ID, Owner: id, Fence: claim.Task.Fence}
			if _, err = r.Begin("b-"+id, token, 3100); err != nil {
				t.Error(err)
				return
			}
			if _, err = r.Finish("f-"+id, token, Completion{State: "completed", NowNS: 3200}, nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	counts, err := r.Counts()
	if err != nil || counts.States["completed"] != 32 || counts.States["running"] != 1 || counts.States["queued"] != 0 {
		t.Fatal(counts, err)
	}
}

func TestClockRollbackDoesNotBreakPauseOrRecentOrder(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "a", "identify")
	if _, err := r.Control("pause", "a", "pause", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Progress(token, Point{Sequence: 1, Percent: 10, Phase: "working", NowNS: 2}); err != nil {
		t.Fatal(err)
	}
	if state(t, r, "a", "paused").UpdatedNS < 2100 {
		t.Fatal("clock rollback reordered state")
	}
	page, err := r.Recent(nil, 200)
	if err != nil || len(page.Tasks) != 1 || page.Tasks[0].ID != "a" {
		t.Fatal(err)
	}
}
