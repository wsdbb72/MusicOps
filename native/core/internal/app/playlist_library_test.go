package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func TestPlaylistLibraryReusesListedFileStateWithoutAnotherDatabaseRead(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	libraryDir := filepath.Join(t.TempDir(), "music")
	file := filepath.Join(libraryDir, "Artist", "Album", "01 Song.flac")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := audiorelease.IndexExisting(engine, audiorelease.Existing{
		RequestID: "playlist-cache-entry", RootID: "playlist-cache-root", Track: 1,
		Path: file, Library: libraryDir, Format: "flac", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	entries, err := audiorelease.ListIndexed(engine)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	index := indexPlaylistLibrary(engine, entries)
	// The per-track state belongs to the listed entry. Closing the database
	// proves the playlist check does not issue one Pebble read per track.
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if !index.hasTrack("Artist", "Album", "Song") {
		t.Fatal("listed library entry was not reusable after the database closed")
	}
	if err := os.WriteFile(file, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := indexPlaylistLibrary(engine, entries)
	if changed.hasTrack("Artist", "Album", "Song") {
		t.Fatal("a changed file was treated as unchanged")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	removed := indexPlaylistLibrary(engine, entries)
	if removed.hasTrack("Artist", "Album", "Song") {
		t.Fatal("a removed file was treated as present")
	}
}
