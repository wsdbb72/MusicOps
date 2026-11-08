package taskrepo

import (
	"encoding/json"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func TestCreateFollowupKeepsExistingTask(t *testing.T) {
	r, _ := fixture(t)
	empty := func(v *dataengine.View, _, _ *Task, _ []Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		return nil, nil, nil, nil
	}
	s := Spec{ID: "stage:album1:publishing", Command: "publish_album", Lane: "publish", Priority: 1, CreatedNS: 1000}
	first, err := r.CreateFollowup("followup-1", s, []byte("receipt-1"), "album1", nil, empty)
	if err != nil || first.Task == nil || first.Task.WorkflowJobID != "album1" || first.Task.State != "queued" {
		t.Fatalf("%+v %v", first, err)
	}
	// A second call with a different request id keeps the queued task instead
	// of replacing it or erroring.
	second, err := r.CreateFollowup("followup-2", s, []byte("receipt-2"), "album1", nil, empty)
	if err != nil || second.Task == nil || second.Task.ID != first.Task.ID || second.Task.State != "queued" {
		t.Fatalf("%+v %v", second, err)
	}
	// The same request id replays the original result.
	replay, err := r.CreateFollowup("followup-1", s, []byte("receipt-1"), "album1", nil, empty)
	if err != nil || !replay.Replayed {
		t.Fatalf("%+v %v", replay, err)
	}
}
