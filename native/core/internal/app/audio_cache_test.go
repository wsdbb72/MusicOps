package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAudioCacheUsageCountsAudioStagingButNotManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, size int) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("user-upload.flac", 5000)
	write("playlist-test.m3u", 1000)
	write("dl-fixture.flac", 100)
	write("netease-fixture.flac", 200)
	write(filepath.Join("engine-transcode", "engine-fixture.audio"), 300)
	write(filepath.Join("engine-transcode", "engine-fixture.audio.flac"), 150)
	got, err := audioCacheUsage(dir)
	if err != nil || got != 5750 {
		t.Fatalf("usage=%d err=%v; want all audio staging (5750) without the manifest", got, err)
	}
}

func TestPruneStaleAudioCacheOnlyRemovesManagedStaging(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(rel string, size int, mod time.Time) string {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldFiles := []string{
		write("netease-old.flac", 600, now.Add(-5*time.Hour)),
		write("scraped-netease-old.flac", 400, now.Add(-5*time.Hour)),
		write(".scraped-old.tmp", 100, now.Add(-5*time.Hour)),
		write(filepath.Join("engine-transcode", "engine-old.audio"), 100, now.Add(-5*time.Hour)),
	}
	recent := write("netease-recent.flac", 300, now.Add(-30*time.Minute))
	downloadCopy := write("dl-old.flac", 300, now.Add(-24*time.Hour))
	streamCopy := write("stream-old.flac", 200, now.Add(-24*time.Hour))
	upload := write("my-recording.wav", 500, now.Add(-24*time.Hour))
	manifest := write("playlist-test.m3u", 500, now.Add(-24*time.Hour))
	freed, err := pruneStaleAudioCache(dir, 1<<20, 4*time.Hour, now)
	if err != nil || freed < 1200 {
		t.Fatal(freed, err)
	}
	for _, path := range oldFiles {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("stale reproducible staging survived", path, err)
		}
	}
	for _, path := range []string{recent, downloadCopy, streamCopy, upload, manifest} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("protected file was removed", path, err)
		}
	}
}

func TestReserveAudioCacheCountsUploadsWithoutDeletingThem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "playlist-only.m3u"), make([]byte, 600), 0o600); err != nil {
		t.Fatal(err)
	}
	upload := filepath.Join(dir, "my-recording.wav")
	if err := os.WriteFile(upload, make([]byte, 600), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "500")
	if release, err := reserveAudioCache(dir, 100); err == nil {
		release()
		t.Fatal("audio staging accepted a write over the hard limit")
	}
	for _, name := range []string{"playlist-only.m3u", "my-recording.wav"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("staging input was removed instead of pausing the write", name, err)
		}
	}
}

func TestReserveAudioCacheRejectsManagedStagingOverLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "netease-current.flac")
	if err := os.WriteFile(path, make([]byte, 600), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "500")
	if release, err := reserveAudioCache(dir, 100); err == nil {
		release()
		t.Fatal("cache accepted a write over the hard limit")
	} else if !isTemporaryAudioCacheError(err) {
		t.Fatalf("capacity error must be retryable, got %T: %v", err, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("recent managed staging was deleted instead of pausing the write", err)
	}
}

func TestAudioCacheCapacityClassificationDoesNotCatchUnrelatedErrors(t *testing.T) {
	if !isTemporaryAudioCacheError(&audioCacheCapacityError{reason: "full"}) {
		t.Fatal("capacity error was not temporary")
	}
	if isTemporaryAudioCacheError(errors.New("permission denied")) {
		t.Fatal("unrelated error was treated as a retryable cache-capacity failure")
	}
}

func TestReserveAudioCachePrunesReconstructibleStagingAboveSeventyFivePercent(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "netease-abandoned.flac")
	user := filepath.Join(dir, "my-recording.wav")
	if err := os.WriteFile(old, make([]byte, 650), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "800")
	release, err := reserveAudioCache(dir, 100)
	if err != nil {
		t.Fatalf("reservation should prune stale staging first: %v", err)
	}
	release()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale reproducible staging survived the early cleanup threshold", err)
	}
	if _, err := os.Stat(user); err != nil {
		t.Fatal("user audio was removed", err)
	}
}

func TestPublishedStreamCacheIsReclaimedOnlyWithMatchingLibraryBytes(t *testing.T) {
	application := newApp(t)
	incoming := application.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceWAV := filepath.Join(incoming, "cache-copy.wav")
	sourceFLAC := filepath.Join(incoming, "stream-cache-source.flac")
	writeToneWAV(t, sourceWAV, 440)
	writeFLACFromWAV(t, sourceWAV, sourceFLAC)
	releaseLibraryFile(t, application, filepath.Base(sourceFLAC), "cache-gc-root", "Artist", "Album", "Song", 1)

	publishedCopy := filepath.Join(incoming, "stream-published.flac")
	otherCopy := filepath.Join(incoming, "stream-other.flac")
	publishedBytes, err := os.ReadFile(sourceFLAC)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publishedCopy, publishedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherCopy, []byte("different unpublished bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	for _, path := range []string{publishedCopy, otherCopy} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	freed, removed, err := application.prunePublishedStreamAudioCache(incoming, time.Hour, now)
	if err != nil || removed != 1 || freed != int64(len(publishedBytes)) {
		t.Fatalf("freed=%d removed=%d err=%v", freed, removed, err)
	}
	if _, err := os.Stat(publishedCopy); !os.IsNotExist(err) {
		t.Fatal("published stream copy survived", err)
	}
	if _, err := os.Stat(otherCopy); err != nil {
		t.Fatal("unpublished stream copy was removed", err)
	}
}
