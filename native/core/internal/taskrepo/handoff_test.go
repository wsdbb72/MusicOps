package taskrepo

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCompletionAndSuccessorsAreOneReplaySafeTransaction(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "identify", "identify")
	following := []NewTask{{Spec: spec("search-a", "identify", 1), Payload: []byte("search a")}, {Spec: spec("search-b", "identify", 1), Payload: []byte("search b")}}
	result, err := r.Finish("handoff", token, Completion{State: "completed", NowNS: 2500, Payload: []byte("observed identity")}, following)
	if err != nil || len(result.Followups) != 2 {
		t.Fatal(result, err)
	}
	counts, err := r.Counts()
	if err != nil || counts.States["completed"] != 1 || counts.States["queued"] != 2 {
		t.Fatal("fan-out counters lost updates", counts, err)
	}
	children, err := r.Children("identify", nil, 200)
	if err != nil || len(children.Tasks) != 2 {
		t.Fatal(children, err)
	}
	for _, child := range children.Tasks {
		if child.ParentID != "identify" || child.State != "queued" {
			t.Fatal(child)
		}
	}
	replay, err := r.Finish("handoff", token, Completion{State: "completed", NowNS: 9999, Payload: []byte("observed identity")}, following)
	if err != nil || !replay.Replayed || len(replay.Followups) != 2 {
		t.Fatal(replay, err)
	}
	counts, _ = r.Counts()
	if counts.States["queued"] != 2 {
		t.Fatal("replay duplicated successors")
	}
}

func TestSuccessorCollisionDoesNotLeaveCompletedParent(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "parent", "identify")
	enqueue(t, r, "existing", "identify")
	_, err := r.Finish("collision", token, Completion{State: "completed", NowNS: 2500}, []NewTask{{Spec: spec("existing", "identify", 1)}})
	if !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	state(t, r, "parent", "running")
	children, _ := r.Children("parent", nil, 200)
	if len(children.Tasks) != 0 {
		t.Fatal("partial handoff")
	}
	counts, _ := r.Counts()
	if counts.States["completed"] != 0 || counts.States["running"] != 1 || counts.States["queued"] != 1 {
		t.Fatal(counts)
	}
	receipt, _ := r.Request("collision")
	if receipt.State != "REJECTED" {
		t.Fatal(receipt)
	}
}

func TestConcurrentCompletionsCannotCreateTwoSuccessorBranches(t *testing.T) {
	r, _ := fixture(t)
	token := start(t, r, "parent", "identify")
	var wins atomic.Int32
	var workers sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			_, err := r.Finish("finish-"+id, token, Completion{State: "completed", NowNS: 2500}, []NewTask{{Spec: spec(id, "identify", 1)}})
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrStale) {
				t.Error(err)
			}
		}(id)
	}
	workers.Wait()
	if wins.Load() != 1 {
		t.Fatal("multiple handoffs committed")
	}
	children, _ := r.Children("parent", nil, 200)
	if len(children.Tasks) != 1 {
		t.Fatal("orphan/duplicate branch")
	}
	counts, _ := r.Counts()
	if counts.States["completed"] != 1 || counts.States["queued"] != 1 {
		t.Fatal(counts)
	}
}

func TestPauseAndCancelRequestsApplyToSuccessorCreation(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			r, _ := fixture(t)
			token := start(t, r, "parent", "publish")
			r.Control("control", "parent", action, 2200)
			result, err := r.Finish("finished-in-flight", token, Completion{State: "completed", NowNS: 2300}, []NewTask{{Spec: spec("next", "aux", 1)}})
			if err != nil {
				t.Fatal(err)
			}
			if action == "cancel" {
				if len(result.Followups) != 0 || result.Task.CancelOutcome != "completed_with_cancel_request" {
					t.Fatal(result)
				}
			} else {
				if len(result.Followups) != 1 || result.Followups[0].State != "paused" || !result.Followups[0].PauseRequested {
					t.Fatal(result)
				}
				if claim, err := r.Claim("try-next", "aux", "owner", 2400); err != nil || claim.Task != nil {
					t.Fatal("paused successor ran")
				}
			}
		})
	}
}
