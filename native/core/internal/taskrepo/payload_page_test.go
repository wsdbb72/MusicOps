package taskrepo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func TestLargePayloadStaysOffTheListPage(t *testing.T) {
	repo, _ := fixture(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	if len(payload) != 1<<20 {
		t.Fatal(len(payload))
	}
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 8; i++ {
		if _, err := repo.Enqueue(fmt.Sprintf("enqueue-%d", i), spec(fmt.Sprintf("blob-%d", i), "aux", 1), payload); err != nil {
			t.Fatal(err)
		}
	}
	page, err := repo.List("aux", "queued", nil, 200)
	if err != nil || len(page.Tasks) != 8 {
		t.Fatal(len(page.Tasks), err)
	}
	for _, task := range page.Tasks {
		if len(task.PayloadHash) != 64 {
			t.Fatal(task)
		}
		encoded, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, payload[:64]) || len(encoded) > 4096 {
			t.Fatalf("list row is %d bytes and carries payload", len(encoded))
		}
		body, found, err := repo.Data(task.ID, "arguments", 0)
		if err != nil || !found || !bytes.Equal(body, payload) {
			t.Fatalf("payload read %d found=%v %v", len(body), found, err)
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// Heap numbers describe this process after the synthetic writes. They are
	// not an improvement over a previous engine and not a live download metric.
	t.Logf("payload_bytes=%d listed_tasks=%d heap_alloc=%d total_alloc_delta=%d", len(payload), len(page.Tasks), after.HeapAlloc, after.TotalAlloc-before.TotalAlloc)
}

func TestPackReusesTheCompressorWithoutChangingBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1024)
	firstHash, first, err := pack(payload)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, second, err := pack(payload)
	if err != nil || firstHash != secondHash || !bytes.Equal(first, second) {
		t.Fatal(err, firstHash == secondHash, len(first), len(second))
	}
	opened, err := unpack(firstHash, first)
	if err != nil || !bytes.Equal(opened, payload) {
		t.Fatal(err, len(opened))
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, compressed, err := pack(payload)
			if err != nil || hash != firstHash || !bytes.Equal(compressed, first) {
				t.Errorf("concurrent pack %v", err)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkPackSmallPayload(b *testing.B) {
	payload := bytes.Repeat([]byte("synthetic-page "), 64)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := pack(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnqueueSmallTask(b *testing.B) {
	engine, err := dataengine.Open(filepath.Join(b.TempDir(), "data"))
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close()
	repo := New(engine)
	payload := bytes.Repeat([]byte("synthetic-page "), 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("item-%d", i)
		if _, err := repo.Enqueue("enqueue-"+id, spec(id, "aux", 1), payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListQueuedPage(b *testing.B) {
	engine, err := dataengine.Open(filepath.Join(b.TempDir(), "data"))
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close()
	repo := New(engine)
	payload := bytes.Repeat([]byte("synthetic-page "), 64)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("seed-%d", i)
		if _, err := repo.Enqueue("seed-"+id, spec(id, "aux", 1), payload); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := repo.List("aux", "queued", nil, 50)
		if err != nil || len(page.Tasks) != 50 {
			b.Fatal(len(page.Tasks), err)
		}
	}
}
