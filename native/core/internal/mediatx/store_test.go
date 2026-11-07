package mediatx

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func openStore(t *testing.T) (*Store, *dataengine.Engine, string) {
	t.Helper()
	parent := t.TempDir()
	engine, err := dataengine.Open(filepath.Join(parent, "engine"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	roots, err := OpenRoots(filepath.Join(parent, "media"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(engine, roots)
	if err != nil {
		t.Fatal(err)
	}
	return store, engine, parent
}

func fixture(id, name string, content []byte, replace bool) Request {
	return Request{RequestID: id, Name: name, Content: content, Replace: replace, Synthetic: true}
}

func TestPublishCopiesOnceAndReplaysWithoutASecondFile(t *testing.T) {
	store, engine, parent := openStore(t)
	body := []byte("synthetic-flac-bytes")
	staged, err := store.Stage(fixture("req-1", "track.flac", body, false))
	if err != nil || staged.State != "staged" || staged.Downloaded || staged.ProductionVerified || !staged.Synthetic {
		t.Fatal(staged, err)
	}
	incoming := filepath.Join(parent, "media", "incoming", "req-1.bin")
	published, err := store.Publish("req-1")
	if err != nil || published.State != "published" || published.Downloaded || published.ProductionVerified || published.SHA256 != staged.SHA256 {
		t.Fatal(published, err)
	}
	library := filepath.Join(parent, "media", "library", "track.flac")
	got, err := os.ReadFile(library)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal(got, err)
	}
	source, err := os.ReadFile(incoming)
	if err != nil || !bytes.Equal(source, body) {
		t.Fatal("source moved", source, err)
	}
	again, err := store.Publish("req-1")
	if err != nil || !again.Replayed || again.Downloaded {
		t.Fatal(again, err)
	}
	engine.Close()
	reopened, err := dataengine.Open(filepath.Join(parent, "engine"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	roots, err := OpenRoots(filepath.Join(parent, "media"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(reopened, roots)
	if err != nil {
		t.Fatal(err)
	}
	after, err := restored.Publish("req-1")
	if err != nil || !after.Replayed || after.State != "published" || after.ProductionVerified {
		t.Fatal(after, err)
	}
}

func TestRollbackKeepsTheSourceAndRestoresAReplacedFile(t *testing.T) {
	store, _, parent := openStore(t)
	original := []byte("original-fixture")
	if _, err := store.Stage(fixture("req-a", "track.flac", original, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish("req-a"); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("replacement-fixture")
	if _, err := store.Stage(fixture("req-b", "track.flac", replacement, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish("req-b"); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(parent, "media", "library", "track.flac")
	got, err := os.ReadFile(library)
	if err != nil || !bytes.Equal(got, replacement) {
		t.Fatal(got, err)
	}
	rolled, err := store.Rollback("req-b")
	if err != nil || rolled.State != "rolled_back" || rolled.Downloaded || rolled.ProductionVerified {
		t.Fatal(rolled, err)
	}
	got, err = os.ReadFile(library)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal(got, err)
	}
	source, err := os.ReadFile(filepath.Join(parent, "media", "incoming", "req-b.bin"))
	if err != nil || !bytes.Equal(source, replacement) {
		t.Fatal(source, err)
	}
	if _, err := store.Publish("req-b"); err == nil {
		t.Fatal("rolled back publication published again")
	}
}

func TestCancelBeforePublishDoesNotCreateOrDeleteTheLibraryFile(t *testing.T) {
	store, _, parent := openStore(t)
	kept := []byte("keep-me")
	library := filepath.Join(parent, "media", "library", "other.flac")
	if err := os.WriteFile(library, kept, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stage(fixture("req-c", "track.flac", []byte("not-published"), false)); err != nil {
		t.Fatal(err)
	}
	rolled, err := store.Rollback("req-c")
	if err != nil || rolled.State != "rolled_back" {
		t.Fatal(rolled, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "media", "library", "track.flac")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	got, err := os.ReadFile(library)
	if err != nil || !bytes.Equal(got, kept) {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "media", "work", "req-c", "staging.bin")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestNonSyntheticAndOutsideNamesAreRejected(t *testing.T) {
	store, _, _ := openStore(t)
	input := fixture("req-d", "track.flac", []byte("bytes"), false)
	input.Synthetic = false
	if _, err := store.Stage(input); err == nil {
		t.Fatal("non-synthetic fixture accepted")
	}
	if _, err := store.Stage(fixture("req-e", "../track.flac", []byte("bytes"), false)); err == nil {
		t.Fatal("escaped name accepted")
	}
}

func TestDirectorySyncLeavesTheWrittenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "track.flac")
	body := []byte("synthetic-fixture-audio")
	if err := writeSame(path, body); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal(got, err)
	}
}

func TestStagedIntentPublishesAfterReopen(t *testing.T) {
	store, engine, parent := openStore(t)
	body := []byte("resume-after-stop")
	if _, err := store.Stage(fixture("req-f", "resume.flac", body, false)); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	reopened, err := dataengine.Open(filepath.Join(parent, "engine"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	roots, err := OpenRoots(filepath.Join(parent, "media"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(reopened, roots)
	if err != nil {
		t.Fatal(err)
	}
	published, err := restored.Publish("req-f")
	if err != nil || published.State != "published" || published.Downloaded || published.ProductionVerified {
		t.Fatal(published, err)
	}
	got, err := os.ReadFile(filepath.Join(parent, "media", "library", "resume.flac"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal(got, err)
	}
}
