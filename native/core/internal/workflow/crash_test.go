package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

func TestWorkflowHandoffSurvivesUnclosedProcess(t *testing.T) {
	if directory := os.Getenv("MUSICOPS_WORKFLOW_CRASH_DIR"); directory != "" {
		engine, err := dataengine.Open(directory)
		if err != nil {
			panic(err)
		}
		tasks := taskrepo.New(engine)
		repo := New(engine, tasks)
		if _, err = repo.Start("start", playlist(1000)); err != nil {
			panic(err)
		}
		claim, err := tasks.Claim("claim", "identify", "crash-owner", 2000)
		if err != nil || claim.Task == nil {
			panic(err)
		}
		token := taskrepo.Token{ID: claim.Task.ID, Owner: claim.Task.Owner, Fence: claim.Task.Fence}
		if _, err = tasks.Begin("begin", token, 2100); err != nil {
			panic(err)
		}
		if _, err = repo.Complete("handoff", token, handoff(3000)); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	directory := filepath.Join(t.TempDir(), "data")
	child := exec.Command(os.Args[0], "-test.run=^TestWorkflowHandoffSurvivesUnclosedProcess$")
	child.Env = append(os.Environ(), "MUSICOPS_WORKFLOW_CRASH_DIR="+directory)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	engine, err := dataengine.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	tasks := taskrepo.New(engine)
	repo := New(engine, tasks)
	inspected, err := repo.Inspect("album1")
	if err != nil {
		t.Fatal(err)
	}
	if err = closedError(inspected); err != nil {
		t.Fatal(err)
	}
	token := taskrepo.Token{ID: "album1", Owner: "crash-owner", Fence: inspected.Task.Fence}
	replay, err := repo.Complete("handoff", token, handoff(3000))
	if err != nil || !replay.Replayed || replay.Job.Stage != "completed" || len(replay.Children) != 1 {
		t.Fatal(replay, err)
	}
	body, found, err := repo.EvidenceData("evidence-1")
	if err != nil || !found || string(body) != "tracklist" {
		t.Fatal(found, err, body)
	}
}
