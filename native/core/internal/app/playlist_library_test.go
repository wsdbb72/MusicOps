package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
func TestPlaylistLibraryReusesListingWithinCacheTTL(t *testing.T) {
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
		RequestID: "playlist-list-cache", RootID: "playlist-list-root", Track: 1,
		Path: file, Library: libraryDir, Format: "flac", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	application := &App{engine: engine, now: func() time.Time { return now }}
	_ = application.playlistLibrary()
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	// A second call inside the TTL must use the listing snapshot rather than
	// reopening the now-closed catalog.
	library := application.playlistLibrary()
	if !library.hasTrack("Artist", "Album", "Song") {
		t.Fatal("playlist listing cache was not reused")
	}
}

func TestPlaylistLibraryUsesCachedMetadataWithoutOpeningAudio(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	libraryDir := filepath.Join(t.TempDir(), "music")
	file := filepath.Join(libraryDir, "Artist", "Album", "01 Song.flac")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	// Deliberately not a FLAC payload. A cache hit must be able to satisfy the
	// audit without opening the file body; a cache miss would parse this and
	// report an unknown album state instead.
	if err := os.WriteFile(file, []byte("cache-hit-not-a-flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := audiorelease.IndexExisting(engine, audiorelease.Existing{
		RequestID: "playlist-cached-metadata", RootID: "playlist-cached-album", Track: 1,
		Path: file, Library: libraryDir, Format: "flac", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	entries, err := audiorelease.ListIndexed(engine)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	scanFiles := map[string]cachedLibraryScan{
		"Artist/Album/01 Song.flac": {
			Size: info.Size(), ModNS: info.ModTime().UnixNano(), MetadataVersion: libraryAutomationMetadataVersion,
			Artists: []string{"Artist"}, Album: "Album", Title: "Song", Duration: 180, TrackTotal: 1,
		},
	}
	index := indexPlaylistLibraryWithMetadata(engine, entries, libraryDir, scanFiles)
	// Closing the catalogue proves both path resolution and unchanged-state
	// checks came from the immutable listing plus the stat snapshot.
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if state := index.albumCompletionState("Artist", "Album"); state != playlistAlbumComplete {
		t.Fatalf("album state without FLAC read = %v", state)
	}
	rows := make([]playlistTrack, 4085)
	for i := range rows {
		rows[i] = playlistTrack{Artist: "Artist", Album: "Album", Title: "Song", Duration: "180", NeteaseSongID: 1}
	}
	present, err := playlistPresenceForIndexContext(context.Background(), index, rows)
	if err != nil || len(present) != len(rows) {
		t.Fatalf("presence len=%d err=%v", len(present), err)
	}
	for i, found := range present {
		if !found {
			t.Fatalf("row %d was not found", i)
		}
	}
	// A changed size/mtime invalidates the cache instead of trusting stale tags.
	if err := os.WriteFile(file, []byte("changed-and-still-not-flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := indexPlaylistLibraryWithMetadata(engine, entries, libraryDir, scanFiles)
	if state := stale.albumCompletionState("Artist", "Album"); state == playlistAlbumComplete {
		t.Fatal("changed file kept a complete album state from stale scan metadata")
	}
}

func TestPlaylistAuditCacheSurvivesRestart(t *testing.T) {
	vaultKey(t)
	root := t.TempDir()
	state := filepath.Join(root, "state")
	backup := filepath.Join(root, "backups")
	library := filepath.Join(root, "music")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	application, err := Open(state, backup, "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	incoming := application.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "#EXTM3U\n#PLAYLIST:Audit cache\n#EXTINF:180,Artist - Song\n#MUSICOPS-ALBUM:Album\n/media/song.flac\n"
	if err := os.WriteFile(filepath.Join(incoming, "playlist-cache-test.m3u"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	first := getJSON(t, application, "/v1/playlists/tracks?job=cache-test&summary=1")
	if first["cached"] != false || first["total"] != float64(1) {
		t.Fatal(first)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(state, backup, "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second := getJSON(t, reopened, "/v1/playlists/tracks?job=cache-test&summary=1")
	if second["cached"] != true || second["total"] != float64(1) {
		t.Fatalf("restart did not reuse the durable audit result: %v", second)
	}
	summary, _ := second["summary"].(map[string]any)
	if summary["missing"] != float64(1) {
		t.Fatal(second)
	}
	changedManifest := "#EXTM3U\n#PLAYLIST:Audit cache\n#EXTINF:181,Artist - Changed Song\n#MUSICOPS-ALBUM:Album\n/media/changed.flac\n"
	if err := os.WriteFile(filepath.Join(incoming, "playlist-cache-test.m3u"), []byte(changedManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	third := getJSON(t, reopened, "/v1/playlists/tracks?job=cache-test&summary=1")
	if third["cached"] != false {
		t.Fatalf("changed playlist content reused a stale audit: %v", third)
	}
}

func TestPlaylistAuditSummaryDoesNotInlineTracks(t *testing.T) {
	application := newApp(t)
	incoming := application.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "#EXTM3U\n#PLAYLIST:Audit\n#EXTINF:180,Artist - Song\n#MUSICOPS-ALBUM:Album\n/media/song.flac\n"
	if err := os.WriteFile(filepath.Join(incoming, "playlist-audit-summary.m3u"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	body := getJSON(t, application, "/v1/playlists/tracks?job=audit-summary&summary=1")
	if body["ok"] != true || body["total"] != float64(1) || body["tracks"] != nil {
		t.Fatal(body)
	}
	summary, _ := body["summary"].(map[string]any)
	if summary["total"] != float64(1) || summary["missing"] != float64(1) || summary["completion_needed"] != float64(1) {
		t.Fatal(summary)
	}
}
