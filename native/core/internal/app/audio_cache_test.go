package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneStaleAudioCacheOnlyRemovesOldNeteaseStaging(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, size int, mod time.Time) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldSource := write("netease-old.flac", 600, now.Add(-5*time.Hour))
	oldScrape := write("scraped-netease-old.flac", 400, now.Add(-5*time.Hour))
	recent := write("netease-recent.flac", 300, now.Add(-30*time.Minute))
	manifest := write("playlist-test.m3u", 500, now.Add(-24*time.Hour))
	freed, err := pruneStaleAudioCache(dir, 700, 4*time.Hour, now)
	if err != nil || freed < 700 {
		t.Fatal(freed, err)
	}
	for _, path := range []string{oldSource, oldScrape} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("stale NetEase staging survived", path, err)
		}
	}
	for _, path := range []string{recent, manifest} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("protected cache file was removed", path, err)
		}
	}
}

func TestReserveAudioCacheDoesNotDeleteUnprotectedInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "playlist-only.m3u"), make([]byte, 600), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "500")
	if release, err := reserveAudioCache(dir, 100); err == nil {
		release()
		t.Fatal("cache accepted a write over the hard limit by deleting user input")
	}
	if _, err := os.Stat(filepath.Join(dir, "playlist-only.m3u")); err != nil {
		t.Fatal("playlist input was removed", err)
	}
}
