package scheduler

import (
	"context"
	"errors"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T, handlers map[string]Handler, config Config) (*taskrepo.Repository, *Coordinator) {
	t.Helper()
	engine, err := dataengine.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	repo := taskrepo.New(engine)
	c, err := New(repo, handlers, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Stop(ctx)
		engine.Close()
	})
	return repo, c
}
func add(t *testing.T, r *taskrepo.Repository, id, lane, command string, priority int, payload string) {
	t.Helper()
	_, err := r.Enqueue("enqueue-"+id, taskrepo.Spec{ID: id, Command: command, Lane: lane, Priority: priority, CreatedNS: 1000}, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
}
func until(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func hasState(r *taskrepo.Repository, id, state string) bool {
	task, err := r.Get(id)
	return err == nil && task != nil && task.State == state
}

func TestSlowTaskDoesNotHoldAnEntireWaveOrAnotherLane(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := func(ctx context.Context, e *Execution, args []byte) (Outcome, error) {
		if string(args) == "slow" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			}
		}
		return Outcome{}, nil
	}
	config := DefaultConfig()
	config.Limits["identify"] = 2
	repo, c := setup(t, map[string]Handler{"controlled": handler}, config)
	add(t, repo, "slow", "identify", "controlled", 0, "slow")
	add(t, repo, "fast", "identify", "controlled", 1, "fast")
	c.Start(context.Background())
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("slow handler not started")
	}
	defer close(release)
	until(t, func() bool { return hasState(repo, "fast", "completed") })
	add(t, repo, "next", "identify", "controlled", 1, "next")
	add(t, repo, "publish", "publish", "controlled", 1, "publish")
	until(t, func() bool { return hasState(repo, "next", "completed") && hasState(repo, "publish", "completed") })
	if !hasState(repo, "slow", "running") {
		t.Fatal("slow peer was not retained")
	}
}

func TestPauseResumeAndCancelAtSafeBoundary(t *testing.T) {
	entered := make(chan struct{})
	handler := func(ctx context.Context, e *Execution, args []byte) (Outcome, error) {
		if string(args) != "paused" {
			return Outcome{}, nil
		}
		close(entered)
		for {
			if err := e.Checkpoint(ctx, "working", 50, "safe boundary"); err != nil {
				return Outcome{}, err
			}
			select {
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	repo, c := setup(t, map[string]Handler{"controlled": handler}, DefaultConfig())
	add(t, repo, "a", "identify", "controlled", 1, "paused")
	c.Start(context.Background())
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler not entered")
	}
	if _, err := repo.Control("pause", "a", "pause", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return hasState(repo, "a", "paused") })
	add(t, repo, "other", "aux", "controlled", 1, "other")
	until(t, func() bool { return hasState(repo, "other", "completed") })
	repo.Control("resume", "a", "resume", time.Now().UnixNano())
	until(t, func() bool { return hasState(repo, "a", "running") })
	repo.Control("pause-again", "a", "pause", time.Now().UnixNano())
	until(t, func() bool { return hasState(repo, "a", "paused") })
	repo.Control("cancel", "a", "cancel", time.Now().UnixNano())
	until(t, func() bool { return hasState(repo, "a", "cancelled") })
}

func TestShutdownTimeoutDoesNotReleaseStillRunningExecution(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := func(context.Context, *Execution, []byte) (Outcome, error) {
		close(entered)
		<-release
		return Outcome{}, nil
	}
	repo, c := setup(t, map[string]Handler{"controlled": handler}, DefaultConfig())
	add(t, repo, "a", "publish", "controlled", 1, "")
	c.Start(context.Background())
	<-entered
	before, _ := repo.Get("a")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := c.Stop(ctx); !errors.Is(err, ErrShutdownPending) {
		t.Fatal(err)
	}
	during, _ := repo.Get("a")
	if during.State != "running" || during.Fence != before.Fence || during.Owner != before.Owner {
		t.Fatal("timeout stole ownership")
	}
	if !c.Health().Stopping {
		t.Fatal("shutdown pending hidden")
	}
	close(release)
	done, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := c.Stop(done); err != nil {
		t.Fatal(err)
	}
	if !hasState(repo, "a", "recovery_required") {
		t.Fatal("interrupted execution pretended complete")
	}
}

func TestPanicIsIsolatedAndDoesNotStopTheQueue(t *testing.T) {
	handlers := map[string]Handler{"broken": func(context.Context, *Execution, []byte) (Outcome, error) { panic("private panic detail") }, "controlled": func(context.Context, *Execution, []byte) (Outcome, error) { return Outcome{}, nil }}
	config := DefaultConfig()
	config.Limits["identify"] = 1
	repo, c := setup(t, handlers, config)
	add(t, repo, "broken", "identify", "broken", 0, "")
	add(t, repo, "next", "identify", "controlled", 1, "")
	c.Start(context.Background())
	until(t, func() bool {
		return hasState(repo, "broken", "recovery_required") && hasState(repo, "next", "completed")
	})
	failed, _ := repo.Get("broken")
	if strings.Contains(failed.Message, "private panic") {
		t.Fatal("panic content leaked")
	}
}

func TestUnregisteredHandlerIsBlockedNotSilentlyCompleted(t *testing.T) {
	repo, c := setup(t, map[string]Handler{}, DefaultConfig())
	add(t, repo, "a", "aux", "unregistered", 1, "")
	c.Start(context.Background())
	until(t, func() bool { return hasState(repo, "a", "blocked") })
	task, _ := repo.Get("a")
	if task.ErrorClass != "UNSUPPORTED_COMMAND" {
		t.Fatal(task)
	}
}

func TestRepeatedCheckpointsDoNotWriteEveryPoll(t *testing.T) {
	handler := func(ctx context.Context, e *Execution, _ []byte) (Outcome, error) {
		for i := 0; i < 1000; i++ {
			if err := e.Checkpoint(ctx, "working", 50, ""); err != nil {
				return Outcome{}, err
			}
		}
		return Outcome{}, nil
	}
	repo, c := setup(t, map[string]Handler{"controlled": handler}, DefaultConfig())
	add(t, repo, "a", "identify", "controlled", 1, "")
	c.Start(context.Background())
	until(t, func() bool { return hasState(repo, "a", "completed") })
	task, _ := repo.Get("a")
	if task.ProgressSequence != 2 {
		t.Fatalf("unchanged checkpoint writes: %d", task.ProgressSequence)
	}
}

func TestRegisteredStageCanAtomicallyScheduleItsSuccessor(t *testing.T) {
	handlers := map[string]Handler{
		"identify": func(context.Context, *Execution, []byte) (Outcome, error) {
			return Outcome{Payload: []byte("identity evidence"), Followups: []taskrepo.NewTask{{Spec: taskrepo.Spec{ID: "search", Command: "search", Lane: "identify", Priority: 1, CreatedNS: 2000}, Payload: []byte("identified album")}}}, nil
		},
		"search": func(context.Context, *Execution, []byte) (Outcome, error) {
			return Outcome{Payload: []byte("observed search result")}, nil
		},
	}
	repo, c := setup(t, handlers, DefaultConfig())
	add(t, repo, "identify", "identify", "identify", 1, "")
	c.Start(context.Background())
	until(t, func() bool { return hasState(repo, "identify", "completed") && hasState(repo, "search", "completed") })
	child, _ := repo.Get("search")
	if child.ParentID != "identify" {
		t.Fatal("stage lineage lost")
	}
}
