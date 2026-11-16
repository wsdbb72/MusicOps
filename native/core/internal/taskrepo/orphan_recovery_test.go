package taskrepo

import (
	"encoding/json"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func TestOrphanScanClaimReleaseAndRecover(t *testing.T) {
	r, _ := fixture(t)
	enqueue(t, r, "queued", "identify")
	claim, err := r.Claim("claim-queued", "identify", "old-owner", 2000)
	if err != nil || claim.Task == nil {
		t.Fatal(claim, err)
	}
	orphans, err := r.RunningOwnedByOther("new-owner")
	if err != nil || len(orphans) != 1 || orphans[0].ID != "queued" || orphans[0].Execution != "claimed" {
		t.Fatalf("orphans=%+v err=%v", orphans, err)
	}
	if current, err := r.RunningOwnedByOther("old-owner"); err != nil || len(current) != 0 {
		t.Fatalf("current owner's live claim treated as orphan: %+v %v", current, err)
	}
	token := Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
	if _, err := r.ReleaseClaim("release-orphan", token, 2100); err != nil {
		t.Fatal(err)
	}
	state(t, r, "queued", "queued")

	enqueue(t, r, "started", "aux")
	claim, err = r.Claim("claim-started", "aux", "old-owner-2", 3000)
	if err != nil || claim.Task == nil {
		t.Fatal(claim, err)
	}
	token = Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
	if _, err := r.Begin("begin-started", token, 3100); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Interrupt("interrupt-started", token, 3200, "old process gone"); err != nil {
		t.Fatal(err)
	}
	state(t, r, "started", "recovery_required")
	domain := func(*dataengine.View, *Task, *Task, []Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		return nil, nil, nil, nil
	}
	if _, err := r.ControlOwned("recover-started", "started", "recover", 3300, nil, domain); err != nil {
		t.Fatal(err)
	}
	recovered := state(t, r, "started", "queued")
	if recovered.Owner != "" || recovered.Execution != "" || recovered.ErrorClass != "" {
		t.Fatal(recovered)
	}
}
