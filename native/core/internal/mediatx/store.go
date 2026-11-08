// Package mediatx publishes one synthetic file into an isolated library root.
// Bytes are copied, checked, and renamed inside that root. The receipt never
// records a site download or a production verification.
package mediatx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const maxBytes = 1 << 20

var (
	ErrConflict = errors.New("media transaction conflict")
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`)
)

// Roots are four directories under one parent. They are not a user library.
type Roots struct {
	Parent   string
	Incoming string
	Work     string
	Library  string
	Backup   string
}

// Request is one fixture publication. Synthetic must be true.
type Request struct {
	RequestID string
	Name      string
	Content   []byte
	Replace   bool
	Synthetic bool
}

// Result is the durable outcome. Downloaded and ProductionVerified stay false.
type Result struct {
	RequestID          string `json:"request_id"`
	Name               string `json:"name"`
	State              string `json:"state"`
	SHA256             string `json:"sha256"`
	Size               int    `json:"size"`
	Replayed           bool   `json:"replayed"`
	Synthetic          bool   `json:"synthetic"`
	Replace            bool   `json:"replace"`
	Downloaded         bool   `json:"downloaded"`
	ProductionVerified bool   `json:"production_verified"`
}

type record struct {
	RequestID    string `json:"request_id"`
	Name         string `json:"name"`
	SHA256       string `json:"sha256"`
	Size         int    `json:"size"`
	State        string `json:"state"`
	Replace      bool   `json:"replace"`
	Synthetic    bool   `json:"synthetic"`
	HeldPrevious bool   `json:"held_previous,omitempty"`
}

type Store struct {
	engine *dataengine.Engine
	roots  Roots
}

func OpenRoots(parent string) (Roots, error) {
	parent, err := filepath.Abs(parent)
	if err != nil {
		return Roots{}, err
	}
	roots := Roots{
		Parent: parent, Incoming: filepath.Join(parent, "incoming"), Work: filepath.Join(parent, "work"),
		Library: filepath.Join(parent, "library"), Backup: filepath.Join(parent, "backup"),
	}
	for _, dir := range []string{roots.Incoming, roots.Work, roots.Library, roots.Backup} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return Roots{}, err
		}
		if err := rejectReparse(dir); err != nil {
			return Roots{}, err
		}
	}
	return roots, nil
}

func New(engine *dataengine.Engine, roots Roots) (*Store, error) {
	if engine == nil || roots.Parent == "" {
		return nil, dataengine.ErrInvalidRequest
	}
	return &Store{engine: engine, roots: roots}, nil
}

func (s *Store) Stage(input Request) (Result, error) {
	if s == nil || !input.Synthetic || !idPattern.MatchString(input.RequestID) || !namePattern.MatchString(input.Name) || len(input.Content) == 0 || len(input.Content) > maxBytes {
		return Result{}, dataengine.ErrInvalidRequest
	}
	sum := hashBytes(input.Content)
	if existing, raw, found, err := s.read(input.RequestID); err != nil {
		return Result{}, err
	} else if found {
		if existing.SHA256 != sum || existing.Name != input.Name || existing.Replace != input.Replace || !existing.Synthetic {
			return Result{}, ErrConflict
		}
		return resultFrom(existing, true), nil
	} else if len(raw) != 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	incoming := s.incomingPath(input.RequestID)
	staging := s.stagingPath(input.RequestID)
	if err := s.contain(incoming, staging); err != nil {
		return Result{}, err
	}
	if err := writeSame(incoming, input.Content); err != nil {
		return Result{}, err
	}
	if err := copySame(incoming, staging); err != nil {
		return Result{}, err
	}
	if got, err := hashFile(staging); err != nil || got != sum {
		return Result{}, ErrConflict
	}
	rec := record{RequestID: input.RequestID, Name: input.Name, SHA256: sum, Size: len(input.Content), State: "staged", Replace: input.Replace, Synthetic: true}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return Result{}, err
	}
	ok, err := s.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: key(input.RequestID), Absent: true}},
		[]dataengine.Mutation{{Key: key(input.RequestID), Value: encoded}},
	)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return s.Stage(input)
	}
	return resultFrom(rec, false), nil
}

func (s *Store) Publish(requestID string) (Result, error) {
	rec, raw, found, err := s.read(requestID)
	if err != nil || !found || !rec.Synthetic {
		if err == nil {
			err = dataengine.ErrInvalidRequest
		}
		return Result{}, err
	}
	if rec.State == "published" {
		if err := s.verifyLibrary(rec); err != nil {
			return Result{}, err
		}
		return resultFrom(rec, true), nil
	}
	if rec.State != "staged" {
		return Result{}, dataengine.ErrInvalidRequest
	}
	staging := s.stagingPath(requestID)
	if got, err := hashFile(staging); err != nil || got != rec.SHA256 {
		return Result{}, ErrConflict
	}
	dest := s.libraryPath(rec.Name)
	temp := filepath.Join(s.roots.Library, ".musicops-"+requestID+".publish")
	hold := filepath.Join(s.roots.Library, ".musicops-"+requestID+".restore")
	if err := s.contain(dest, temp, hold); err != nil {
		return Result{}, err
	}
	if err := copySame(staging, temp); err != nil {
		return Result{}, err
	}
	previous, err := hashFile(dest)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if rec.Replace {
			os.Remove(temp)
			return Result{}, dataengine.ErrInvalidRequest
		}
	case err != nil:
		os.Remove(temp)
		return Result{}, err
	case previous == rec.SHA256:
		os.Remove(temp)
	default:
		if !rec.Replace {
			os.Remove(temp)
			return Result{}, ErrConflict
		}
		backup := s.backupPath(requestID, rec.Name)
		if err := s.contain(backup); err != nil {
			return Result{}, err
		}
		if err := copySame(dest, backup); err != nil {
			return Result{}, err
		}
		if err := os.Rename(dest, hold); err != nil {
			return Result{}, err
		}
		rec.HeldPrevious = true
	}
	if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(temp, dest); err != nil {
			if rec.HeldPrevious {
				_ = os.Rename(hold, dest)
			}
			return Result{}, err
		}
	}
	if err := syncDir(s.roots.Library); err != nil {
		return Result{}, err
	}
	if err := s.verifyLibrary(rec); err != nil {
		return Result{}, err
	}
	rec.State = "published"
	return s.commit(rec, raw)
}

func (s *Store) Rollback(requestID string) (Result, error) {
	rec, raw, found, err := s.read(requestID)
	if err != nil || !found || !rec.Synthetic {
		if err == nil {
			err = dataengine.ErrInvalidRequest
		}
		return Result{}, err
	}
	if rec.State == "rolled_back" {
		return resultFrom(rec, true), nil
	}
	if rec.State != "staged" && rec.State != "published" {
		return Result{}, dataengine.ErrInvalidRequest
	}
	dest := s.libraryPath(rec.Name)
	hold := filepath.Join(s.roots.Library, ".musicops-"+requestID+".restore")
	backup := s.backupPath(requestID, rec.Name)
	switch {
	case fileExists(hold):
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		if err := os.Rename(hold, dest); err != nil {
			return Result{}, err
		}
	case fileExists(backup):
		if err := copySame(backup, dest); err != nil {
			return Result{}, err
		}
	case rec.State == "published":
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
	}
	_ = os.Remove(s.stagingPath(requestID))
	_ = os.Remove(filepath.Join(s.roots.Library, ".musicops-"+requestID+".publish"))
	if _, err := os.Stat(s.incomingPath(requestID)); err != nil {
		return Result{}, ErrConflict
	}
	rec.State = "rolled_back"
	return s.commit(rec, raw)
}

func (s *Store) Get(requestID string) (Result, bool, error) {
	rec, _, found, err := s.read(requestID)
	if err != nil || !found {
		return Result{}, false, err
	}
	return resultFrom(rec, false), true, nil
}

func (s *Store) commit(rec record, raw []byte) (Result, error) {
	encoded, err := json.Marshal(rec)
	if err != nil {
		return Result{}, err
	}
	ok, err := s.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: key(rec.RequestID), Value: raw}},
		[]dataengine.Mutation{{Key: key(rec.RequestID), Value: encoded}},
	)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		current, _, found, err := s.read(rec.RequestID)
		if err != nil || !found || current.State != rec.State || current.SHA256 != rec.SHA256 {
			if err == nil {
				err = ErrConflict
			}
			return Result{}, err
		}
		return resultFrom(current, true), nil
	}
	return resultFrom(rec, false), nil
}

func (s *Store) verifyLibrary(rec record) error {
	got, err := hashFile(s.libraryPath(rec.Name))
	if err != nil || got != rec.SHA256 {
		return ErrConflict
	}
	return nil
}

func (s *Store) read(id string) (record, []byte, bool, error) {
	var rec record
	var raw []byte
	var found bool
	err := s.engine.View(func(v *dataengine.View) error {
		value, ok, err := v.Get(key(id))
		if err != nil || !ok {
			return err
		}
		if json.Unmarshal(value, &rec) != nil || rec.RequestID != id || !rec.Synthetic || rec.SHA256 == "" {
			return dataengine.ErrInvalidRequest
		}
		raw = bytes.Clone(value)
		found = true
		return nil
	})
	return rec, raw, found, err
}

func resultFrom(rec record, replayed bool) Result {
	return Result{
		RequestID: rec.RequestID, Name: rec.Name, State: rec.State, SHA256: rec.SHA256, Size: rec.Size,
		Replayed: replayed, Synthetic: true, Replace: rec.Replace,
	}
}

func (s *Store) contain(paths ...string) error {
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.roots.Parent, abs)
		if err != nil || escapes(rel) {
			return dataengine.ErrInvalidRequest
		}
		if err := rejectReparse(abs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) incomingPath(id string) string { return filepath.Join(s.roots.Incoming, id+".bin") }
func (s *Store) stagingPath(id string) string {
	return filepath.Join(s.roots.Work, id, "staging.bin")
}
func (s *Store) libraryPath(name string) string { return filepath.Join(s.roots.Library, name) }
func (s *Store) backupPath(id, name string) string {
	return filepath.Join(s.roots.Backup, id, name)
}

func escapes(rel string) bool {
	rel = filepath.Clean(rel)
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

func key(id string) []byte { return []byte("media/tx/" + id) }

func writeSame(path string, content []byte) error {
	current, err := os.ReadFile(path)
	if err == nil {
		if bytes.Equal(current, content) {
			return nil
		}
		return ErrConflict
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return writeSame(path, content)
		}
		return err
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		os.Remove(path)
		if writeErr != nil {
			return writeErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	return syncDir(filepath.Dir(path))
}

func copySame(source, dest string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return writeSame(dest, content)
}

func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return hashBytes(content), nil
}

func fileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func syncDir(dir string) error {
	// Windows FlushFileBuffers rejects directories. The file itself is already
	// synced, so a directory flush is not required for the publication to be durable.
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func rejectReparse(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return dataengine.ErrInvalidRequest
	}
	return nil
}
