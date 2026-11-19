package audiorelease

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func TestMetadataIndexReplacesFormatVersionAndPrunesMissingPath(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	library := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(library, "01 Track.wav")
	flac := filepath.Join(library, "01 Track.flac")
	if err := os.WriteFile(wav, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flac, []byte("flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := "alb-version-test"
	oldID, newID := "lib-wav-version", "lib-flac-version"
	if added, err := IndexExisting(engine, Existing{
		RequestID: oldID, RootID: root, Track: 1, Path: wav, Library: library,
		Format: "wav", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	if added, err := IndexExisting(engine, Existing{
		RequestID: newID, RootID: root, Track: 1, Path: flac, Library: library,
		Format: "flac", SourceName: "wav-convert",
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	entries, err := ListIndexed(engine)
	if err != nil || len(entries) != 1 || entries[0].RequestID != newID || entries[0].Format != "flac" {
		t.Fatal(entries, err)
	}
	if _, found, err := PathFor(engine, oldID); err != nil || found {
		t.Fatal("old format release key was not replaced", found, err)
	}
	if err := os.Remove(flac); err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneMissingIndexed(engine, library)
	if err != nil || pruned != 1 {
		t.Fatal(pruned, err)
	}
	if entries, err = ListIndexed(engine); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestSnapshotBatchAcceptsEveryIndexedAudioExtension(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	library := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	formats := []string{"flac", "mp3", "wav", "aac", "alac", "aiff", "m4a", "ogg", "opus", "ape", "wv", "wma", "mka", "dsf", "dff"}
	items := make([]Existing, 0, len(formats))
	for _, format := range formats {
		path := filepath.Join(library, "01 Track."+format)
		if err := os.WriteFile(path, []byte(format), 0o600); err != nil {
			t.Fatal(err)
		}
		items = append(items, Existing{
			RequestID: "lib-index-" + format, RootID: "alb-index-" + format, Track: 1, Path: path,
			Library: library, Format: format, SourceName: "reindex", MetadataOnly: true,
		})
	}
	if added, err := IndexSnapshots(engine, items); err != nil || added != len(items) {
		t.Fatalf("added=%d err=%v", added, err)
	}
	entries, err := ListIndexed(engine)
	if err != nil || len(entries) != len(items) {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
}

func TestSnapshotBatchUsesPrimaryTrackMapping(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	library := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(library, "01 Track.flac")
	if err := os.WriteFile(path, []byte("flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	items := []Existing{{
		RequestID: "lib-snapshot", RootID: "alb-snapshot", Track: 1, Path: path,
		Library: library, Format: "flac", SourceName: "reindex", MetadataOnly: true,
	}}
	if added, err := IndexSnapshots(engine, items); err != nil || added != 1 {
		t.Fatal(added, err)
	}
	primaryRaw, found, err := readRaw(engine, albumKey("alb-snapshot", 1))
	if err != nil || !found || len(primaryRaw) == 0 {
		t.Fatal("primary mapping missing", found, err)
	}
}

func TestAPEToWAVConversionReplacesTheSameLogicalTrack(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	library := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	ape := filepath.Join(library, "07 Same Song.ape")
	wav := filepath.Join(library, "07 Same Song.wav")
	if err := os.WriteFile(ape, []byte("ape"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := IndexExisting(engine, Existing{
		RequestID: "lib-ape-source", RootID: "alb-ape-convert", Track: 7, Path: ape,
		Library: library, SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	if err := os.WriteFile(wav, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := IndexExisting(engine, Existing{
		RequestID: "lib-wav-result", RootID: "alb-ape-convert", Track: 7, Path: wav,
		Library: library, SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	entries, err := ListIndexed(engine)
	if err != nil || len(entries) != 1 || entries[0].RequestID != "lib-wav-result" || entries[0].Format != "wav" {
		t.Fatal(entries, err)
	}
}
