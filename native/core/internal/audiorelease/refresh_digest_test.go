package audiorelease

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshPathDigestsUpdatesEveryReceiptForOnePath(t *testing.T) {
	engine, _, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "01 Title.wav")
	original := wav(16)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, item := range []Existing{
		{RequestID: "receipt-one", RootID: "root-one", Track: 1, Path: path, Library: library, Format: "wav"},
		{RequestID: "receipt-two", RootID: "root-two", Track: 1, Path: path, Library: library, Format: "wav"},
	} {
		if added, err := IndexExisting(engine, item); err != nil || !added {
			t.Fatalf("index %s added=%v err=%v", item.RequestID, added, err)
		}
	}

	edited := wav(64)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := RefreshPathDigests(engine, []string{path, path})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 4 {
		t.Fatalf("updated records=%d, want primary and album receipt for both identities", updated)
	}
	for _, requestID := range []string{"receipt-one", "receipt-two"} {
		if _, err := Verify(engine, requestID); err != nil {
			t.Fatalf("verify %s after path refresh: %v", requestID, err)
		}
	}
	if again, err := RefreshPathDigests(engine, []string{path}); err != nil || again != 0 {
		t.Fatalf("idempotent refresh changed=%d err=%v", again, err)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(body, edited) {
		t.Fatalf("refresh changed audio bytes: %d %v", len(body), err)
	}
}

func TestRefreshPathDigestsIgnoresUnpublishedFiles(t *testing.T) {
	engine, incoming, _ := openRelease(t)
	path := filepath.Join(incoming, "unpublished.wav")
	if err := os.WriteFile(path, wav(16), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := RefreshPathDigests(engine, []string{path})
	if err != nil || updated != 0 {
		t.Fatalf("updated=%d err=%v", updated, err)
	}
}
