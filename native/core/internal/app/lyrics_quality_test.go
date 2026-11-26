package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaskedLyricsUseCleanExactLRCLIBFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" || r.URL.Query().Get("track_name") != "Cola" || r.URL.Query().Get("artist_name") != "Lana Del Rey" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"trackName": "Cola", "artistName": "Lana Del Rey", "albumName": "Wrong Album", "syncedLyrics": "[00:18.33]still *****", "instrumental": false},
			{"trackName": "Cola", "artistName": "Lana Del Rey", "albumName": "Paradise", "syncedLyrics": "[00:18.33]complete words\n[00:22.55]second line", "instrumental": false},
			{"trackName": "Cola (Live)", "artistName": "Lana Del Rey", "albumName": "Paradise", "syncedLyrics": "[00:18.33]not the studio title", "instrumental": false},
		})
	}))
	defer server.Close()
	application := &App{lyricsFallbackBase: server.URL + "/api/search", lyricsHTTPClient: server.Client()}
	got := application.cleanFetchedLyrics(context.Background(), "Lana Del Rey", "Paradise", "Cola", "[00:18.33]My ***** tastes like Pepsi cola")
	if !strings.Contains(got, "complete words") || hasMaskedLyrics(got) {
		t.Fatalf("fallback lyrics=%q", got)
	}
}

func TestMaskedLyricsAreNeverAcceptedWithoutCleanFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"trackName": "Song", "artistName": "Artist", "albumName": "Album", "syncedLyrics": "[00:01]still f***ing masked", "instrumental": false}})
	}))
	defer server.Close()
	application := &App{lyricsFallbackBase: server.URL, lyricsHTTPClient: server.Client()}
	if got := application.cleanFetchedLyrics(context.Background(), "Artist", "Album", "Song", "[00:01]f***ing masked"); got != "" {
		t.Fatalf("masked lyrics were accepted: %q", got)
	}
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(audio, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeLyricsSidecar(audio, "[00:01]f***ing masked"); err == nil {
		t.Fatal("masked sidecar was written")
	}
}

func TestMaskedSidecarOrEmbeddedLyricsAreRepairCandidates(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(audio, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !lyricsNeedsWrite(audio, nil) {
		t.Fatal("missing sidecar was not a repair candidate")
	}
	if err := os.WriteFile(lyricsSidecarPath(audio), []byte("[00:01]clean lyric\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !lyricsNeedsWrite(audio, nil) {
		t.Fatal("a FLAC with a clean sidecar but no embedded lyrics was not a repair candidate")
	}
	if lyricsNeedsWrite(audio, []string{"[00:01]clean lyric"}) {
		t.Fatal("clean embedded lyrics and sidecar were treated as missing")
	}
	if !lyricsNeedsWrite(audio, []string{"[00:01]still ****"}) {
		t.Fatal("masked embedded lyrics were not a repair candidate")
	}
	if err := os.WriteFile(audio+".lrc", []byte("[00:01]legacy f***ing masked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !lyricsNeedsWrite(audio, []string{"[00:01]clean lyric"}) {
		t.Fatal("a masked legacy sidecar was hidden by a clean standard sidecar")
	}
	if err := writeLyricsSidecar(audio, "[00:01]clean lyric"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(audio + ".lrc"); !os.IsNotExist(err) {
		t.Fatal("masked legacy sidecar was not removed")
	}
	mp3 := filepath.Join(dir, "song.mp3")
	if err := os.WriteFile(mp3, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lyricsSidecarPath(mp3), []byte("[00:01]clean lyric\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lyricsNeedsWrite(mp3, nil) {
		t.Fatal("a non-FLAC sidecar-only track was incorrectly marked for embedded repair")
	}
}
