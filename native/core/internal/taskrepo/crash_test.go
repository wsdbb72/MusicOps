package taskrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func TestHandoffReceiptAndSuccessorSurviveUnclosedProcess(t *testing.T) {
	token := Token{ID: "parent", Owner: "crash-owner", Fence: 1}
	children := []NewTask{{Spec: spec("search", "identify", 1), Payload: []byte("query")}}
	if directory := os.Getenv("MUSICOPS_TASK_CRASH_DIR"); directory != "" {
		engine, err := dataengine.Open(directory)
		if err != nil {
			panic(err)
		}
		repo := New(engine)
		if _, err = repo.Enqueue("enqueue", spec("parent", "identify", 1), []byte("input")); err != nil {
			panic(err)
		}
		if _, err = repo.Claim("claim", "identify", token.Owner, 2000); err != nil {
			panic(err)
		}
		if _, err = repo.Begin("begin", token, 2100); err != nil {
			panic(err)
		}
		if _, err = repo.Finish("handoff", token, Completion{State: "completed", NowNS: 2200, Payload: []byte("identity")}, children); err != nil {
			panic(err)
		}
		os.Exit(0) // Deliberately bypass DB.Close after the synced acknowledgement.
	}
	directory := filepath.Join(t.TempDir(), "data")
	child := exec.Command(os.Args[0], "-test.run=^TestHandoffReceiptAndSuccessorSurviveUnclosedProcess$")
	child.Env = append(os.Environ(), "MUSICOPS_TASK_CRASH_DIR="+directory)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	engine, err := dataengine.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	repo := New(engine)
	state(t, repo, "parent", "completed")
	state(t, repo, "search", "queued")
	result, err := repo.Finish("handoff", token, Completion{State: "completed", NowNS: 9999, Payload: []byte("identity")}, children)
	if err != nil || !result.Replayed || len(result.Followups) != 1 {
		t.Fatal(result, err)
	}
	counts, err := repo.Counts()
	if err != nil || counts.States["completed"] != 1 || counts.States["queued"] != 1 {
		t.Fatal(counts, err)
	}
}
