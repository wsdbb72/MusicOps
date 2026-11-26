package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

func TestReindexRepairsReceiptsAfterAnUnrecordedMetadataEdit(t *testing.T) {
	application := newApp(t)
	library := application.libraryDir()
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	source := genTestFLAC(t, incoming, "shared.flac")
	if err := flactags.Write(source, flactags.Tags{Title: "Shared", Artists: []string{"Artist"}, Album: "Album", TrackNumber: 1}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ requestID, rootID string }{
		{"reindex-primary", "reindex-root-one"},
		{"reindex-alias", "reindex-root-two"},
	} {
		if _, err := audiorelease.Release(t.Context(), application.engine, audiorelease.Request{
			RequestID: item.requestID, RootID: item.rootID, Artist: "Artist", Album: "Album", Title: "Shared", Track: 1,
			SourceName: "shared.flac", Incoming: incoming, Library: library,
		}); err != nil {
			t.Fatalf("release %s: %v", item.requestID, err)
		}
	}
	path := filepath.Join(library, "Artist", "Album", "01 Shared.flac")
	// Simulate the pre-fix write path: the audio changes, but the publication
	// receipts keep their old digest and size/time snapshot.
	if err := flactags.Write(path, flactags.Tags{Raw: map[string][]string{
		"TITLE": {"Shared"}, "ARTIST": {"Artist"}, "ALBUM": {"Album"}, "TRACKNUMBER": {"1"},
		"GENRE": {strings.Repeat("x", 128)},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := application.indexLibraryTree(t.Context(), library); err != nil {
		t.Fatal(err)
	}
	for _, requestID := range []string{"reindex-primary", "reindex-alias"} {
		if _, err := audiorelease.Verify(application.engine, requestID); err != nil {
			t.Fatalf("verify %s after reindex repair: %v", requestID, err)
		}
	}
}
