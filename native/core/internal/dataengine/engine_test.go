package dataengine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func openTest(t *testing.T) *Engine {
	t.Helper()
	e, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}
func getTest(t *testing.T, e *Engine, key string) ([]byte, bool) {
	t.Helper()
	var value []byte
	var found bool
	err := e.View(func(v *View) error { var err error; value, found, err = v.Get([]byte(key)); return err })
	if err != nil {
		t.Fatal(err)
	}
	return value, found
}

func TestSyncedAtomicWriteAndConditionalRejection(t *testing.T) {
	e := openTest(t)
	writes := []Mutation{{Key: []byte("record/a"), Value: []byte("data")}, {Key: []byte("index/queued/a"), Value: []byte("a")}}
	if applied, err := e.CompareAndSwap([]Condition{{Key: []byte("record/a"), Absent: true}}, writes); err != nil || !applied {
		t.Fatalf("%v %v", applied, err)
	}
	if applied, err := e.CompareAndSwap([]Condition{{Key: []byte("record/a"), Value: []byte("other")}}, []Mutation{{Key: []byte("record/a"), Value: []byte("wrong")}}); err != nil || applied {
		t.Fatalf("%v %v", applied, err)
	}
	if value, _ := getTest(t, e, "record/a"); string(value) != "data" {
		t.Fatal("rejected batch changed data")
	}
}

func TestInvalidMutationCannotPartiallyApply(t *testing.T) {
	e := openTest(t)
	_, err := e.CompareAndSwap(nil, []Mutation{{Key: []byte("record/a"), Value: []byte("x")}, {Key: []byte("system/schema"), Value: []byte("bad")}})
	if err == nil {
		t.Fatal("reserved key accepted")
	}
	if _, exists := getTest(t, e, "record/a"); exists {
		t.Fatal("partial batch committed")
	}
}

func TestSnapshotRemainsConsistentAcrossConcurrentWriter(t *testing.T) {
	e := openTest(t)
	e.CompareAndSwap(nil, []Mutation{{Key: []byte("record/a"), Value: []byte("old")}})
	err := e.View(func(v *View) error {
		var writer sync.WaitGroup
		writer.Add(1)
		go func() {
			defer writer.Done()
			if _, err := e.CompareAndSwap(nil, []Mutation{{Key: []byte("record/a"), Value: []byte("new")}}); err != nil {
				t.Error(err)
			}
		}()
		writer.Wait()
		value, found, err := v.Get([]byte("record/a"))
		if err != nil || !found || string(value) != "old" {
			t.Fatalf("snapshot changed: %s %v", value, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := getTest(t, e, "record/a"); string(value) != "new" {
		t.Fatal("new view did not see committed write")
	}
}

func TestConcurrentWritesKeepAllRecordsAndIndexes(t *testing.T) {
	e := openTest(t)
	var writers sync.WaitGroup
	for i := 0; i < 32; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			id := fmt.Sprint(i)
			if _, err := e.CompareAndSwap(nil, []Mutation{{Key: []byte("record/" + id), Value: []byte(id)}, {Key: []byte("index/" + id), Value: []byte(id)}}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	writers.Wait()
	for i := 0; i < 32; i++ {
		for _, prefix := range []string{"record/", "index/"} {
			if value, found := getTest(t, e, prefix+fmt.Sprint(i)); !found || string(value) != fmt.Sprint(i) {
				t.Fatal("lost concurrent update")
			}
		}
	}
}

func TestBoundedCursorScanDoesNotSkipOrRepeat(t *testing.T) {
	e := openTest(t)
	mutations := make([]Mutation, 0, 451)
	for i := 0; i < 451; i++ {
		mutations = append(mutations, Mutation{Key: []byte(fmt.Sprintf("index/%04d", i)), Value: []byte("x")})
	}
	e.CompareAndSwap(nil, mutations)
	var after []byte
	seen := map[string]bool{}
	for {
		var page Page
		if err := e.View(func(v *View) error { var err error; page, err = v.Scan([]byte("index/"), after, 200); return err }); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 200 {
			t.Fatal("unbounded page")
		}
		for _, item := range page.Items {
			if seen[string(item.Key)] {
				t.Fatal("duplicate cursor item")
			}
			seen[string(item.Key)] = true
		}
		if len(page.Next) == 0 {
			break
		}
		after = page.Next
	}
	if len(seen) != 451 {
		t.Fatalf("lost records: %d", len(seen))
	}
}

func TestCheckpointCanOpenIndependentlyAfterOriginalChanges(t *testing.T) {
	e := openTest(t)
	e.CompareAndSwap(nil, []Mutation{{Key: []byte("record/a"), Value: []byte("old")}})
	destination := filepath.Join(t.TempDir(), "checkpoint")
	if err := e.Checkpoint(destination); err != nil {
		t.Fatal(err)
	}
	e.CompareAndSwap(nil, []Mutation{{Key: []byte("record/a"), Value: []byte("new")}})
	copy, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if value, _ := getTest(t, copy, "record/a"); string(value) != "old" {
		t.Fatal("checkpoint not independent")
	}
}

func TestRejectForeignDirectoryAndExclusiveOpen(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "user-file")
	os.WriteFile(file, []byte("keep"), 0600)
	if _, err := Open(dir); !errors.Is(err, ErrForeignDirectory) {
		t.Fatal(err)
	}
	if value, _ := os.ReadFile(file); string(value) != "keep" {
		t.Fatal("foreign data modified")
	}
	dir = filepath.Join(t.TempDir(), "owned")
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Open(dir); err == nil {
		second.Close()
		t.Fatal("multiple data writers opened directory")
	}
}

func TestCrashRecoveryFromSyncedWAL(t *testing.T) {
	if dir := os.Getenv("MUSICOPS_CRASH_TEST_DIR"); dir != "" {
		e, err := Open(dir)
		if err != nil {
			panic(err)
		}
		if _, err = e.CompareAndSwap(nil, []Mutation{{Key: []byte("record/crash"), Value: []byte("durable")}}); err != nil {
			panic(err)
		}
		os.Exit(0) // Intentional: bypass deferred DB.Close to exercise WAL recovery.
	}
	dir := filepath.Join(t.TempDir(), "crash")
	process := exec.Command(os.Args[0], "-test.run=^TestCrashRecoveryFromSyncedWAL$")
	process.Env = append(os.Environ(), "MUSICOPS_CRASH_TEST_DIR="+dir)
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("crash child: %v %s", err, output)
	}
	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if value, _ := getTest(t, e, "record/crash"); string(value) != "durable" {
		t.Fatal("acknowledged write lost after crash")
	}
}
