package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	if err != nil || got != 6750 {
		t.Fatalf("usage=%d err=%v; want the complete managed footprint (6750)", got, err)
	}
}

func TestAudioCacheRootsCountLegacyAndConfiguredActiveTreesOnce(t *testing.T) {
	application := newApp(t)
	work := t.TempDir()
	if _, err := application.saveLibrarySettings(librarySettings{WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	roots := application.audioCacheRoots(filepath.Join(work, "incoming"))
	want := map[string]bool{
		filepath.Join(application.Dir, "incoming"):  false,
		filepath.Join(application.Dir, "downloads"): false,
		filepath.Join(application.Dir, "seeds"):     false,
		filepath.Join(work, "incoming"):             false,
		filepath.Join(work, "downloads"):            false,
		filepath.Join(work, "seeds"):                false,
	}
	for _, root := range roots {
		key := filepath.Clean(root)
		if _, ok := want[key]; !ok {
			t.Fatalf("unexpected cache root %s in %v", root, roots)
		}
		if want[key] {
			t.Fatalf("cache root counted twice: %s", root)
		}
		want[key] = true
	}
	for root, seen := range want {
		if !seen {
			t.Fatalf("missing cache root %s from %v", root, roots)
		}
	}
}

func TestAudioCacheUsageSnapshotReportsGlobalSizeAndLimit(t *testing.T) {
	application := newApp(t)
	path := filepath.Join(application.Dir, "incoming", "snapshot-copy.flac")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, 1234), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "4096")
	used, limit, roots, err := application.audioCacheUsageSnapshot()
	if err != nil || used != 1234 || limit != 4096 || len(roots) == 0 {
		t.Fatalf("used=%d limit=%d roots=%v err=%v", used, limit, roots, err)
	}
}

func TestAudioCacheUsageRootsDeduplicatesNestedRoots(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "incoming")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "stream-one.flac"), make([]byte, 321), 0o600); err != nil {
		t.Fatal(err)
	}
	used, err := audioCacheUsageRoots([]string{root, nested})
	if err != nil || used != 321 {
		t.Fatalf("usage=%d err=%v; nested roots were counted more than once", used, err)
	}
}

func TestAudioCacheCleanupOnlyRemovesEmptyQuarantineDirectories(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "quarantine", "merge-empty", "artist", "album")
	protected := filepath.Join(root, "quarantine", "merge-keep", "artist", "album")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(protected, "unique.flac")
	if err := os.WriteFile(keep, []byte("unique"), 0o600); err != nil {
		t.Fatal(err)
	}
	pruneEmptyCacheParents([]string{filepath.Join(root, "quarantine")}, map[string]struct{}{empty: {}})
	if _, err := os.Stat(filepath.Join(root, "quarantine", "merge-empty")); !os.IsNotExist(err) {
		t.Fatalf("empty quarantine tree survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("quarantined recording was removed: %v", err)
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

func TestReserveAudioCacheUsesOneGlobalQuotaAcrossLegacyAndWorkTrees(t *testing.T) {
	application := newApp(t)
	work := t.TempDir()
	if _, err := application.saveLibrarySettings(librarySettings{WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	write := func(path string, size int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := filepath.Join(application.Dir, "incoming", "legacy-copy.flac")
	current := filepath.Join(work, "incoming", "current-copy.flac")
	quarantine := filepath.Join(application.Dir, "quarantine", "merge-old", "loser.flac")
	write(legacy, 600)
	write(current, 300)
	write(quarantine, 200)
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "800")
	if release, err := application.reserveAudioCache(filepath.Join(work, "incoming"), 100); err == nil {
		release()
		t.Fatal("global cache quota ignored files in another managed root")
	} else if !isTemporaryAudioCacheError(err) {
		t.Fatalf("capacity error must be retryable, got %T: %v", err, err)
	}
	for _, path := range []string{legacy, current, quarantine} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected cache/quarantine file was deleted: %s: %v", path, err)
		}
	}
}

func TestQuarantineCopyIsRecoveryStorageOutsideActiveCacheQuota(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	relative := filepath.Join("Artist", "Album", "01 Song.flac")
	source := filepath.Join(library, relative)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 600)
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "500")
	receipt := mergeReceipt{}
	err := application.quarantineFile(&receipt, duplicateFile{
		RequestID: "source-copy", File: relative, LibraryDir: library, SHA256: hex.EncodeToString(sum[:]),
	}, filepath.Join(application.Dir, "quarantine", "merge-limit"))
	if err != nil {
		t.Fatalf("recovery quarantine was blocked by the active download-cache quota: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source was not moved into recovery quarantine: %v", err)
	}
	if len(receipt.Moved) != 1 {
		t.Fatalf("quarantine receipt was not recorded: %+v", receipt.Moved)
	}
}

func TestReserveAudioCachePrunesStaleStagingInEveryManagedRoot(t *testing.T) {
	application := newApp(t)
	work := t.TempDir()
	if _, err := application.saveLibrarySettings(librarySettings{WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	paths := []string{
		filepath.Join(application.Dir, "incoming", "netease-legacy.flac"),
		filepath.Join(work, "incoming", "netease-current.flac"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 300), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "600")
	release, err := application.reserveAudioCache(filepath.Join(work, "incoming"), 100)
	if err != nil {
		t.Fatalf("global quota should reclaim stale staging in both roots: %v", err)
	}
	release()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale staging survived in managed root %s: %v", path, err)
		}
	}
}

func TestAutomationCacheCleanupScansLegacyAndConfiguredRoots(t *testing.T) {
	application := newApp(t)
	work := t.TempDir()
	if _, err := application.saveLibrarySettings(librarySettings{WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	oldTime := now.Add(-2 * time.Hour)
	paths := []string{
		filepath.Join(application.Dir, "incoming", "netease-legacy.flac"),
		filepath.Join(work, "incoming", "netease-current.flac"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 300), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "600")
	freed, files, err := application.pruneAudioCacheAll(time.Hour, now)
	if err != nil || freed != 600 || files != 2 {
		t.Fatalf("freed=%d files=%d err=%v", freed, files, err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale staging survived automation in %s: %v", path, err)
		}
	}
}

func TestAudioCacheReclaimNeedTargetsHeadroomBelowTheLimit(t *testing.T) {
	const limit = int64(1 << 30)
	if got := audioCacheReclaimNeed(700<<20, limit); got != 0 {
		t.Fatalf("need below target = %d, want 0", got)
	}
	if got, want := audioCacheReclaimNeed(900<<20, limit), int64(388<<20); got != want {
		t.Fatalf("need at high-water mark = %d, want %d", got, want)
	}
	maxInt := int64(^uint64(0) >> 1)
	if got := audioCacheReclaimNeed(maxInt, maxInt); got <= 0 || got > maxInt {
		t.Fatalf("near-int64 need overflowed: got %d", got)
	}
	if got := audioCacheReclaimNeed(2*limit, limit); got != 3*limit/2 {
		t.Fatalf("need at twice the limit = %d, want half-quota headroom %d", got, 3*limit/2)
	}
}

func TestAudioCacheQuotaExcludesRecoveryQuarantine(t *testing.T) {
	application := newApp(t)
	quarantine := filepath.Join(application.Dir, "quarantine", "merge-test", "track.flac")
	if err := os.MkdirAll(filepath.Dir(quarantine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quarantine, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	used, _, roots, err := application.audioCacheUsageSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if used != 0 {
		t.Fatalf("recovery quarantine was counted as active cache: used=%d", used)
	}
	for _, root := range roots {
		if strings.EqualFold(filepath.Base(root), "quarantine") {
			t.Fatalf("active cache roots include recovery quarantine: %v", roots)
		}
	}
}

func TestPublishedCacheReclaimStopsAtTheRequestedNeed(t *testing.T) {
	application := newApp(t)
	incoming := application.workPath("incoming")
	sourceWAV := filepath.Join(incoming, "bounded-cache-source.wav")
	sourceFLAC := filepath.Join(incoming, "bounded-cache-source.flac")
	writeToneWAV(t, sourceWAV, 987)
	writeFLACFromWAV(t, sourceWAV, sourceFLAC)
	releaseLibraryFile(t, application, filepath.Base(sourceFLAC), "bounded-cache-root", "Artist", "Album", "Bounded Song", 1)
	entries, err := audiorelease.List(application.engine)
	if err != nil || len(entries) == 0 {
		t.Fatal(entries, err)
	}
	body, err := os.ReadFile(filepath.Join(entries[0].LibraryDir, filepath.FromSlash(entries[0].File)))
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(incoming, "stream-00-bounded.flac")
	second := filepath.Join(incoming, "stream-01-bounded.flac")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	for _, path := range []string{first, second} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	audioCacheMu.Lock()
	freed, removed, err := application.prunePublishedAudioCacheRootsLocked([]string{incoming}, time.Hour, now, int64(len(body)))
	audioCacheMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != int64(len(body)) {
		t.Fatalf("freed=%d removed=%d want one bounded cache copy", freed, removed)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("first published copy survived: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("cleanup continued after reaching need: %v", err)
	}
}
func TestAudioCacheTorrentReservationsPreventConcurrentOversubscription(t *testing.T) {
	application := newApp(t)
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(firstDir, "partial.bin"), make([]byte, 250), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "800")
	t.Cleanup(func() {
		application.clearAudioCacheDownload("first")
		application.clearAudioCacheDownload("second")
	})
	if err := application.reserveAudioCacheDownload("first", firstDir, 500); err != nil {
		t.Fatal(err)
	}
	if err := application.reserveAudioCacheDownload("first", firstDir, 500); err != nil {
		t.Fatalf("updating the same reservation double-counted itself: %v", err)
	}
	if err := application.reserveAudioCacheDownload("second", secondDir, 100); err == nil || !isTemporaryAudioCacheError(err) {
		t.Fatalf("second torrent reservation error=%T %v, want capacity refusal", err, err)
	}
	application.clearAudioCacheDownload("first")
	if err := application.reserveAudioCacheDownload("second", secondDir, 100); err != nil {
		t.Fatalf("reservation did not become available after release: %v", err)
	}
}
func TestAudioCacheTorrentReservationDoesNotCountDownloadedBytesTwice(t *testing.T) {
	application := newApp(t)
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(firstDir, "partial.bin"), make([]byte, 250), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "1000")
	t.Cleanup(func() {
		application.clearAudioCacheDownload("first")
		application.clearAudioCacheDownload("second")
	})
	if err := application.reserveAudioCacheDownload("first", firstDir, 500); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstDir, "progress.bin"), make([]byte, 350), 0o600); err != nil {
		t.Fatal(err)
	}
	application.updateAudioCacheDownloadRemaining("first", 150)
	if err := application.reserveAudioCacheDownload("second", secondDir, 100); err != nil {
		t.Fatalf("downloaded bytes were counted both as usage and remaining reservation: %v", err)
	}
}

func TestAudioCacheAdmissionReclaimsRecentPublishedCopy(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	sourceWAV := filepath.Join(t.TempDir(), "recent-cache-source.wav")
	sourceFLAC := filepath.Join(incoming, "recent-cache-source.flac")
	writeToneWAV(t, sourceWAV, 440)
	writeFLACFromWAV(t, sourceWAV, sourceFLAC)
	releaseLibraryFile(t, application, filepath.Base(sourceFLAC), "recent-cache-root", "Artist", "First Album", "Recent Cache Song", 1)
	entries, err := audiorelease.List(application.engine)
	if err != nil || len(entries) == 0 {
		t.Fatal(entries, err)
	}
	body, err := os.ReadFile(filepath.Join(entries[0].LibraryDir, filepath.FromSlash(entries[0].File)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sourceFLAC, filepath.Join(t.TempDir(), "source-outside-cache.flac")); err != nil {
		t.Fatal(err)
	}
	cacheCopy := filepath.Join(incoming, "stream-recent-published.flac")
	if err := os.WriteFile(cacheCopy, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", strconv.Itoa(len(body)+32))
	release, err := application.reserveAudioCache(incoming, 64)
	if err != nil {
		t.Fatalf("capacity pressure did not reclaim a recent published copy: %v", err)
	}
	defer release()
	if _, err := os.Stat(cacheCopy); !os.IsNotExist(err) {
		t.Fatalf("recent published cache copy survived pressure admission: %v", err)
	}
}

func TestExpiredDownloadCachePrunesOnlyCompleteInactiveTaskTree(t *testing.T) {
	application := newApp(t)
	downloads := application.workPath("downloads")
	seeds := application.workPath("seeds")
	oldDir := filepath.Join(downloads, "process-old-task")
	recentDir := filepath.Join(seeds, "process-recent-task")
	protectedDir := filepath.Join(seeds, "process-protected-task")
	abandonedDir := filepath.Join(seeds, "process-abandoned-task")
	for _, dir := range []string{oldDir, recentDir, protectedDir, abandonedDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "audio.flac"), make([]byte, 400), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldCheck := cacheRemovalPath(oldDir)
	abandonedCheck := cacheRemovalPath(abandonedDir)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	for _, dir := range []string{oldDir, protectedDir} {
		path := filepath.Join(dir, "audio.flac")
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	abandoned := now.Add(-80 * time.Hour)
	if err := os.Chtimes(filepath.Join(abandonedDir, "audio.flac"), abandoned, abandoned); err != nil {
		t.Fatal(err)
	}
	audioCacheMu.Lock()
	freed, files, err := application.pruneExpiredDownloadCacheLocked([]string{downloads, seeds}, time.Hour, now, 800, func(path string) bool {
		return samePath(path, protectedDir) || samePath(path, abandonedDir)
	})
	audioCacheMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if freed != 800 || files != 2 {
		t.Fatalf("freed=%d files=%d", freed, files)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, dir := range []string{oldCheck, abandonedCheck} {
		for {
			_, statErr := os.Stat(dir)
			if os.IsNotExist(statErr) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expired task cache %s survived: %v", dir, statErr)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	for _, dir := range []string{recentDir, protectedDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("protected cache %s was removed: %v", dir, err)
		}
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

func TestPublishedAudioCacheReclaimsTorrentAlbumCopies(t *testing.T) {
	application := newApp(t)
	incoming := application.workPath("incoming")
	sourceWAV := filepath.Join(incoming, "download-cache-source.wav")
	sourceFLAC := filepath.Join(incoming, "download-cache-source.flac")
	writeToneWAV(t, sourceWAV, 523)
	writeFLACFromWAV(t, sourceWAV, sourceFLAC)
	releaseLibraryFile(t, application, filepath.Base(sourceFLAC), "cache-download-root", "Artist", "Album", "Song", 1)
	body, err := os.ReadFile(sourceFLAC)
	if err != nil {
		// Release moves the source into the library; read the published copy.
		entries, listErr := audiorelease.List(application.engine)
		if listErr != nil || len(entries) == 0 {
			t.Fatal(err, listErr)
		}
		body, err = os.ReadFile(filepath.Join(entries[0].LibraryDir, filepath.FromSlash(entries[0].File)))
	}
	if err != nil {
		t.Fatal(err)
	}
	downloadRoot := application.workPath("downloads")
	cachePath := filepath.Join(downloadRoot, "process-search-cache-download", "Artist - Album", "01 - Song.flac")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(cachePath, old, old); err != nil {
		t.Fatal(err)
	}
	freed, removed, err := application.prunePublishedStreamAudioCacheAll(time.Hour, now)
	if err != nil || removed != 1 || freed != int64(len(body)) {
		t.Fatalf("freed=%d removed=%d err=%v", freed, removed, err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("published torrent album copy survived: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(cachePath))); !os.IsNotExist(err) {
		t.Fatalf("empty torrent cache directories survived: %v", err)
	}
}

func TestActiveTaskReferenceProtectsPublishedCacheCopy(t *testing.T) {
	application := newApp(t)
	incoming := application.workPath("incoming")
	sourceWAV := filepath.Join(incoming, "active-cache-source.wav")
	sourceFLAC := filepath.Join(incoming, "active-cache-source.flac")
	writeToneWAV(t, sourceWAV, 659)
	writeFLACFromWAV(t, sourceWAV, sourceFLAC)
	releaseLibraryFile(t, application, filepath.Base(sourceFLAC), "cache-active-root", "Artist", "Album", "Song", 1)
	entries, err := audiorelease.List(application.engine)
	if err != nil || len(entries) == 0 {
		t.Fatal(err, entries)
	}
	body, err := os.ReadFile(filepath.Join(entries[0].LibraryDir, filepath.FromSlash(entries[0].File)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cachePath := filepath.Join(application.workPath("downloads"), "process-search-cache-active-long", "Album", "01 Song.flac")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(cachePath, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := application.tasks.Enqueue("cache-active-task", taskrepo.Spec{
		ID: "process:search:cache-active-long", Command: "fixture", Lane: "download", Priority: 1, CreatedNS: now.UnixNano(),
	}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	_, removed, err := application.prunePublishedStreamAudioCacheAll(time.Hour, now)
	if err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("active task cache copy was removed: %v", err)
	}
}

func TestActiveQBittorrentSeedProtectsPublishedCacheCopy(t *testing.T) {
	application := newApp(t)
	incoming := application.workPath("incoming")
	sourceWAV := filepath.Join(incoming, "qbt-seed-source.wav")
	sourceFLAC := filepath.Join(incoming, "qbt-seed-source.flac")
	writeToneWAV(t, sourceWAV, 784)
	writeFLACFromWAV(t, sourceWAV, sourceFLAC)
	releaseLibraryFile(t, application, filepath.Base(sourceFLAC), "cache-qbt-root", "Artist", "Album", "Seed Song", 1)
	entries, err := audiorelease.List(application.engine)
	if err != nil || len(entries) == 0 {
		t.Fatal(entries, err)
	}
	body, err := os.ReadFile(filepath.Join(entries[0].LibraryDir, filepath.FromSlash(entries[0].File)))
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(application.workPath("downloads"), "process-search-active-seed", "Album", "01 Seed Song.flac")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(cachePath, old, old); err != nil {
		t.Fatal(err)
	}
	fake := newFakeQBT(t, map[string][]byte{}, 0)
	fake.mu.Lock()
	fake.row = &fakeQBTorrent{
		hash: "0123456789abcdef0123456789abcdef01234567", savePath: filepath.Dir(cachePath),
		contentPath: filepath.Dir(cachePath), tags: qbittorrent.ProjectTag, started: true,
	}
	fake.mu.Unlock()
	post(t, application, "/v1/settings/qbittorrent", map[string]any{
		"base_url": fake.url(), "username": "user", "password": "pass",
	})
	_, removed, err := application.prunePublishedStreamAudioCacheAll(time.Hour, now)
	if err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("qBittorrent-active cache copy was removed: %v", err)
	}
}

func TestRealStreamCacheNameIsProtectedByActiveWorkflowName(t *testing.T) {
	application, rootID, _ := seedTokenReviewJob(t, true, "account_token_hold")
	incoming := application.workPath("incoming")
	sourceWAV := filepath.Join(incoming, "real-stream-cache-source.wav")
	sourceFLAC := filepath.Join(incoming, "real-stream-cache-source.flac")
	writeToneWAV(t, sourceWAV, 880)
	writeFLACFromWAV(t, sourceWAV, sourceFLAC)
	releaseLibraryFile(t, application, filepath.Base(sourceFLAC), "cache-real-stream-root", "Artist", "Album", "Stream Song", 1)
	entries, err := audiorelease.List(application.engine)
	if err != nil || len(entries) == 0 {
		t.Fatal(entries, err)
	}
	body, err := os.ReadFile(filepath.Join(entries[0].LibraryDir, filepath.FromSlash(entries[0].File)))
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(incoming, "stream-"+safeStreamID(rootID)+"-0123456789abcdef0123456789abcdef-kugou.flac")
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(cachePath, old, old); err != nil {
		t.Fatal(err)
	}
	_, removed, err := application.prunePublishedStreamAudioCacheAll(time.Hour, now)
	if err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("active stream cache with production filename was removed: %v", err)
	}
}

func TestActiveTaskProtectsStaleNeteaseStagingFromMaintenancePrune(t *testing.T) {
	application := newApp(t)
	now := time.Now()
	taskID := "process:search:netease-active-staging"
	path := filepath.Join(application.workPath("incoming"), "process-search-netease-active-staging", "netease-current.flac")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := application.tasks.Enqueue("active-staging-task", taskrepo.Spec{
		ID: taskID, Command: "fixture", Lane: "aux", Priority: 1, CreatedNS: now.UnixNano(),
	}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "800")
	freed, files, err := application.pruneAudioCacheAll(time.Hour, now)
	if err != nil || freed != 0 || files != 0 {
		t.Fatalf("freed=%d files=%d err=%v", freed, files, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active NetEase staging was removed: %v", err)
	}
}
