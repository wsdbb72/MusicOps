// Package dataengine is the embedded, durable KV foundation for MusicOps.
// No SQL server, CGO, network listener or Python runtime is required to use it.
package dataengine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/pebble/v2"
)

var (
	ErrClosed           = errors.New("data engine is closed")
	ErrForeignDirectory = errors.New("refuse non-MusicOps data directory")
	ErrInvalidRequest   = errors.New("invalid data request")
)

const ownerFile = "MUSICOPS-ENGINE.json"
const owner = `{"application":"MusicOps","engine":"pebble-v2","format":2}`
const SchemaVersion = 2
const maxBatchBytes = 128 << 20

type Mutation struct {
	Key    []byte `json:"key"`
	Value  []byte `json:"value,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

// Condition compares exact bytes or asserts absence, within the same serialized
// write boundary as the mutations. It is suitable for leases and reservations.
type Condition struct {
	Key    []byte `json:"key"`
	Value  []byte `json:"value,omitempty"`
	Absent bool   `json:"absent,omitempty"`
}

type Engine struct {
	db     *pebble.DB
	life   sync.RWMutex
	write  sync.Mutex
	closed bool
}

func Open(directory string) (*Engine, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(directory) == directory {
		return nil, ErrForeignDirectory
	}
	if info, err := os.Lstat(directory); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrForeignDirectory
	}
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(entries) != 0 {
		data, err := os.ReadFile(filepath.Join(directory, ownerFile))
		if err != nil || !bytes.Equal(data, []byte(owner)) {
			return nil, ErrForeignDirectory
		}
	} else {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
		if err := writeOwner(directory); err != nil {
			return nil, err
		}
	}
	db, err := pebble.Open(directory, &pebble.Options{
		CacheSize:                   8 << 20,
		MemTableSize:                4 << 20,
		MemTableStopWritesThreshold: 2,
		CompactionConcurrencyRange:  func() (int, int) { return 1, 1 },
	})
	if err != nil {
		return nil, err
	}
	engine := &Engine{db: db}
	value, closer, err := db.Get([]byte("system/schema"))
	if errors.Is(err, pebble.ErrNotFound) {
		err = db.Set([]byte("system/schema"), []byte("2"), pebble.Sync)
	} else if err == nil {
		valid := bytes.Equal(value, []byte("2"))
		closer.Close()
		if !valid {
			err = errors.New("unsupported native data schema")
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return engine, nil
}

func writeOwner(directory string) error {
	file, err := os.OpenFile(filepath.Join(directory, ownerFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write([]byte(owner))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func validKey(key []byte) bool {
	return len(key) > 0 && len(key) <= 4096 && !bytes.HasPrefix(key, []byte("system/"))
}

// CompareAndSwap commits the whole batch only if every current value matches.
// Repositories own business receipts: progress uses sequence/fence deduplication,
// while critical commands atomically store their response with their state.
// There is no second, hidden per-write receipt log at this storage layer.
func (e *Engine) CompareAndSwap(conditions []Condition, mutations []Mutation) (bool, error) {
	if len(conditions) > 10000 || len(mutations) == 0 || len(mutations) > 10000 {
		return false, ErrInvalidRequest
	}
	size := 0
	for _, condition := range conditions {
		if !validKey(condition.Key) || condition.Absent && len(condition.Value) != 0 {
			return false, ErrInvalidRequest
		}
		size += len(condition.Key) + len(condition.Value)
	}
	for _, mutation := range mutations {
		if !validKey(mutation.Key) || mutation.Delete && len(mutation.Value) != 0 {
			return false, ErrInvalidRequest
		}
		size += len(mutation.Key) + len(mutation.Value)
	}
	if size > maxBatchBytes {
		return false, ErrInvalidRequest
	}
	e.life.RLock()
	defer e.life.RUnlock()
	if e.closed {
		return false, ErrClosed
	}
	e.write.Lock()
	defer e.write.Unlock()
	for _, condition := range conditions {
		current, closer, err := e.db.Get(condition.Key)
		if err != nil && !errors.Is(err, pebble.ErrNotFound) {
			return false, err
		}
		matches := errors.Is(err, pebble.ErrNotFound) && condition.Absent
		if err == nil {
			matches = !condition.Absent && bytes.Equal(current, condition.Value)
			closer.Close()
		}
		if !matches {
			return false, nil
		}
	}
	batch := e.db.NewBatch()
	defer batch.Close()
	for _, mutation := range mutations {
		var err error
		if mutation.Delete {
			err = batch.Delete(mutation.Key, nil)
		} else {
			err = batch.Set(mutation.Key, mutation.Value, nil)
		}
		if err != nil {
			return false, err
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return false, err
	}
	return true, nil
}

// View bounds snapshot lifetime to an in-process read callback. A repository
// finishes these reads before network calls, filesystem work or conditional writes.
func (e *Engine) View(read func(*View) error) error {
	e.life.RLock()
	defer e.life.RUnlock()
	if e.closed {
		return ErrClosed
	}
	snapshot := e.db.NewSnapshot()
	defer snapshot.Close()
	return read(&View{snapshot: snapshot})
}

func (e *Engine) Checkpoint(destination string) error {
	e.life.RLock()
	defer e.life.RUnlock()
	if e.closed {
		return ErrClosed
	}
	e.write.Lock()
	defer e.write.Unlock()
	if err := e.db.Checkpoint(destination, pebble.WithFlushedWAL()); err != nil {
		return err
	}
	return writeOwner(destination)
}

func (e *Engine) Close() error {
	e.life.Lock()
	defer e.life.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return e.db.Close()
}

type View struct{ snapshot *pebble.Snapshot }
type Item struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}
type Page struct {
	Items []Item `json:"items"`
	Next  []byte `json:"next"`
}

func (v *View) Get(key []byte) ([]byte, bool, error) {
	if !validKey(key) {
		return nil, false, ErrInvalidRequest
	}
	value, closer, err := v.snapshot.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	result := bytes.Clone(value)
	closer.Close()
	return result, true, nil
}

func prefixEnd(prefix []byte) []byte {
	end := bytes.Clone(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 255 {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

func (v *View) Scan(prefix, after []byte, limit int) (Page, error) {
	page := Page{Items: make([]Item, 0)}
	if !validKey(prefix) || limit < 1 || limit > 200 || len(after) > 0 && !bytes.HasPrefix(after, prefix) {
		return page, ErrInvalidRequest
	}
	iter, err := v.snapshot.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return page, err
	}
	defer iter.Close()
	valid := iter.First()
	if len(after) > 0 {
		valid = iter.SeekGE(after)
		if valid && bytes.Equal(iter.Key(), after) {
			valid = iter.Next()
		}
	}
	pageBytes := 0
	for valid && bytes.HasPrefix(iter.Key(), prefix) {
		if bytes.HasPrefix(iter.Key(), []byte("system/")) {
			valid = iter.Next()
			continue
		}
		size := len(iter.Key()) + len(iter.Value())
		if len(page.Items) == limit || len(page.Items) > 0 && pageBytes+size > 4<<20 {
			page.Next = bytes.Clone(page.Items[len(page.Items)-1].Key)
			break
		}
		page.Items = append(page.Items, Item{Key: bytes.Clone(iter.Key()), Value: bytes.Clone(iter.Value())})
		pageBytes += size
		valid = iter.Next()
	}
	return page, iter.Error()
}
