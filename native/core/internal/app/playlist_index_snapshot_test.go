package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

func TestPlaylistIndexSnapshotTrustsCatalogueWithoutStattingAudio(t *testing.T) {
	library := t.TempDir()
	path := filepath.Join(library, "Artist", "Album", "01 Song.flac")
	entry := audiorelease.Entry{
		RequestID: "snapshot-track", RootID: "snapshot-root", Track: 1,
		File: "Artist/Album/01 Song.flac", LibraryDir: library, FilePresent: true,
	}
	metadata := cachedLibraryScan{
		MetadataVersion: libraryAutomationMetadataVersion,
		Artist:          "Artist", Album: "Album", Title: "Song", Artists: []string{"Artist"},
	}
	index := indexPlaylistLibraryWithMetadata(nil, []audiorelease.Entry{entry}, library, map[string]cachedLibraryScan{
		automationCacheKey(library, path): metadata,
	})
	if index.isUnchanged(entry.RequestID) {
		t.Fatal("precise mode unexpectedly reported the missing fixture unchanged")
	}

	index.useIndexSnapshot = true
	if !index.isUnchanged(entry.RequestID) {
		t.Fatal("snapshot mode rejected a durable catalogue record")
	}
	present, err := playlistPresenceForIndexSnapshotContext(context.Background(), index, []playlistTrack{{
		Artist: "Artist", Album: "Album", Title: "Song",
	}})
	if err != nil || len(present) != 1 || !present[0] {
		t.Fatalf("snapshot presence=%v err=%v", present, err)
	}
	if got := index.recording(path); got.album != "Album" || got.title != "Song" || len(got.artists) != 1 {
		t.Fatalf("snapshot metadata did not come from the persisted cache: %+v", got)
	}
}

func TestVersion6ScanCacheBackfillsLyricsDigestWithoutDiscardingMetadata(t *testing.T) {
	application := newApp(t)
	root := t.TempDir()
	doc := libraryAutomationScanDocument{
		Version: 6,
		Root:    root,
		Files: map[string]cachedLibraryScan{
			"track": {
				Size: 123, ModNS: 456,
				MetadataVersion: libraryAutomationMetadataVersion,
				Artist:          "Artist", Album: "Album", Title: "Song", Artists: []string{"Artist"},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.casSaveJSON(libraryAutomationScanKey, raw); err != nil {
		t.Fatal(err)
	}
	loaded := application.loadLibraryAutomationScanCache(root)
	if loaded.Version != libraryAutomationScanVersion || len(loaded.Files) != 1 {
		t.Fatalf("version 6 cache was discarded: %+v", loaded)
	}
	entry := loaded.Files["track"]
	if entry.Artist != "Artist" || entry.Album != "Album" || entry.Title != "Song" || entry.LyricsDigestCheckedNS != 0 {
		t.Fatalf("compatible metadata was not preserved: %+v", entry)
	}
}

func TestPlaylistSummarySeparatesUnknownAlbumTotalsFromConfirmedGaps(t *testing.T) {
	application := newApp(t)
	library := application.libraryDir()
	trackPath := indexPlaylistTestFile(t, application, library,
		"Artist/Album/01 Song.flac", "Artist", "Album", "Song")
	application.invalidatePlaylistLibraryCache()
	incoming := application.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "#EXTM3U\n#PLAYLIST:Unknown total\n#MUSICOPS-ALBUM:Album\n#EXTINF:180,Artist - Song\n/media/song.flac\n"
	if err := os.WriteFile(filepath.Join(incoming, "playlist-audit-unknown.m3u"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	body := getJSON(t, application, "/v1/playlists/tracks?job=audit-unknown&summary=1")
	summary, _ := body["summary"].(map[string]any)
	if summary["completion_needed"] != float64(0) || summary["completion_unknown"] != float64(1) || summary["album_incomplete"] != float64(0) {
		t.Fatalf("unknown album total was misclassified as work: %v", body)
	}
	if err := os.Remove(trackPath); err != nil {
		t.Fatal(err)
	}
	exact := getJSON(t, application, "/v1/playlists/tracks?job=audit-unknown&summary=1&verify=1")
	exactSummary, _ := exact["summary"].(map[string]any)
	if exact["verification"] != "file_stat" || exactSummary["missing"] != float64(1) || exactSummary["completion_needed"] != float64(1) {
		t.Fatalf("exact verification reused the stale snapshot cache: %v", exact)
	}
}

func TestPlaylistSnapshotRepairsTruncatedFilenameWithEmbeddedFullTitle(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	fullTitle := "Phoenix（涅槃）—英雄联盟2019全球总决赛主题曲（翻自 Cailin Russo）"
	truncated := "01 Phoenix（涅槃）—英雄联盟2019全球总决赛主题曲（翻自 Cailin R.flac"
	indexPlaylistTestFile(t, application, library, "Artist/Album/"+truncated,
		"Artist", "Album", fullTitle)
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	snapshotOnly := index
	snapshotOnly.useIndexSnapshot = true
	if snapshotOnly.hasTrack("Artist", "Album", fullTitle) {
		t.Fatal("snapshot unexpectedly recovered embedded metadata that was not cached")
	}
	present, err := playlistPresenceForIndexSnapshotContext(context.Background(), index, []playlistTrack{{
		Artist: "Artist", Album: "Album", Title: fullTitle, Duration: "197", NeteaseSongID: 1,
	}})
	if err != nil || len(present) != 1 || !present[0] {
		t.Fatalf("bounded snapshot repair present=%v err=%v", present, err)
	}
}
