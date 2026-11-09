package dataengine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func dirBytes(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func TestStartupReopenAndConcurrentConditionalWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	started := time.Now()
	engine, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	startup := time.Since(started)
	const writers = 8
	const each = 25
	var wg sync.WaitGroup
	errors := make(chan error, writers)
	for worker := 0; worker < writers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; n < each; n++ {
				key := []byte(fmt.Sprintf("profile/%d/%d", worker, n))
				applied, err := engine.CompareAndSwap([]Condition{{Key: key, Absent: true}}, []Mutation{{Key: key, Value: []byte("synthetic")}})
				if err != nil || !applied {
					errors <- fmt.Errorf("writer %d: %v applied=%v", worker, err, applied)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	if err = engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopenStarted := time.Now()
	engine, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopen := time.Since(reopenStarted)
	err = engine.View(func(v *View) error {
		value, found, err := v.Get([]byte("profile/3/7"))
		if err != nil || !found || string(value) != "synthetic" {
			return fmt.Errorf("reopen read %q found=%v %v", value, found, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Close before walking the directory: Pebble manages its files
	// (including deleting obsolete OPTIONS files) while the engine is
	// open, so a walk of the live directory can race a deletion and
	// fail (OPTIONS-000003 vanished between readdir and stat on the
	// Windows CI runner in run 42580776325). After Close, background
	// file management has stopped and the directory is stable.
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	disk, err := dirBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if disk <= 0 {
		t.Fatal("empty data directory")
	}
	// Logged sizes are one Linux run of the current engine. They are not a
	// before/after improvement and not a comparison against another database.
	t.Logf("startup=%s reopen=%s keys=%d disk_bytes=%d", startup, reopen, writers*each, disk)
}

func BenchmarkStartup(b *testing.B) {
	root := b.TempDir()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dir := filepath.Join(root, fmt.Sprintf("open-%d", i))
		engine, err := Open(dir)
		if err != nil {
			b.Fatal(err)
		}
		engine.Close()
	}
}

func BenchmarkConditionalWrite(b *testing.B) {
	engine, err := Open(filepath.Join(b.TempDir(), "data"))
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := []byte(fmt.Sprintf("bench/%d", i))
		applied, err := engine.CompareAndSwap([]Condition{{Key: key, Absent: true}}, []Mutation{{Key: key, Value: []byte("synthetic-record")}})
		if err != nil || !applied {
			b.Fatal(err, applied)
		}
	}
}

func BenchmarkConcurrentConditionalWrite(b *testing.B) {
	engine, err := Open(filepath.Join(b.TempDir(), "data"))
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n := 0
		for pb.Next() {
			n++
			key := []byte(fmt.Sprintf("parallel/%p/%d", pb, n))
			applied, err := engine.CompareAndSwap([]Condition{{Key: key, Absent: true}}, []Mutation{{Key: key, Value: []byte("synthetic-record")}})
			if err != nil || !applied {
				b.Error(err, applied)
				return
			}
		}
	})
}
