package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

type quarantinePruneFixture struct {
	quarantine string
	keeperPath string
	mergeID    string
}

func seedQuarantinePruneFixture(t *testing.T, application *App, playlistTitle, quarantineTitle string) quarantinePruneFixture {
	t.Helper()
	return seedQuarantinePruneFixtureWithKeeperTitle(t, application, playlistTitle, quarantineTitle, playlistTitle)
}

func seedQuarantinePruneFixtureWithKeeperTitle(t *testing.T, application *App, playlistTitle, quarantineTitle, keeperTitle string) quarantinePruneFixture {
	t.Helper()
	incoming := application.workPath("incoming")
	slug := strings.ReplaceAll(strings.ToLower(playlistTitle), " ", "-")
	keeperSource := filepath.Join(incoming, "quarantine-keeper-"+slug+".wav")
	writeToneWAV(t, keeperSource, 440)
	keeper := releaseLibraryFile(t, application, filepath.Base(keeperSource), "quarantine-keeper-"+slug, "Artist", "Album", keeperTitle, 1)

	playlistDir := application.savedPlaylistsDir()
	if err := os.MkdirAll(playlistDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "#PLAYLIST:Cache\n#MUSICOPS-ALBUM:Album\n#MUSICOPS-NETEASE:1:1\n#EXTINF:1,Artist - " + playlistTitle + "\n.\n"

	if err := os.WriteFile(filepath.Join(playlistDir, slug+".m3u"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	mergeID := "merge-cache-prune-" + slug
	quarantine := filepath.Join(application.Dir, "quarantine", mergeID, "Artist", "Album", "02 "+quarantineTitle+".wav")
	if err := os.MkdirAll(filepath.Dir(quarantine), 0o700); err != nil {
		t.Fatal(err)
	}
	body := pcmWAV(660)
	if err := os.WriteFile(quarantine, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	receipt := mergeReceipt{
		MergeID: mergeID, GroupID: "group-" + mergeID, Keep: keeper.File,
		Moved:     []mergeMove{{RequestID: "quarantine-loser", From: filepath.Join(application.Dir, "library-other", "Artist", "Album", "02 "+quarantineTitle+".wav"), Quarantine: quarantine, SHA256: hex.EncodeToString(sum[:])}},
		CreatedNS: time.Now().Add(-2 * time.Hour).UnixNano(),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: mergeReceiptKey(mergeID), Absent: true}},
		[]dataengine.Mutation{{Key: mergeReceiptKey(mergeID), Value: raw}},
	); err != nil {
		t.Fatal(err)
	}
	return quarantinePruneFixture{
		quarantine: quarantine,
		keeperPath: filepath.Join(keeper.LibraryDir, filepath.FromSlash(keeper.File)),
		mergeID:    mergeID,
	}
}

func TestSuccessfulQuarantineIsReclaimedAfterPlaylistAndHashProof(t *testing.T) {
	application := newApp(t)
	fixture := seedQuarantinePruneFixture(t, application, "Song", "Song")
	t.Setenv("MUSICOPS_AUDIO_CACHE_BYTES", "1")

	now := time.Now()
	freed, removed, err := application.pruneAudioCacheAll(time.Hour, now)
	if err != nil || removed == 0 || freed <= 0 {
		t.Fatalf("freed=%d removed=%d err=%v", freed, removed, err)
	}
	if _, err := os.Stat(fixture.quarantine); !os.IsNotExist(err) {
		t.Fatalf("successful quarantine survived: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(filepath.Dir(fixture.quarantine)))); !os.IsNotExist(err) {
		t.Fatalf("empty quarantine merge directory survived: %v", err)
	}
}

func TestQuarantineMoveForMissingPlaylistRowIsPreserved(t *testing.T) {
	application := newApp(t)
	fixture := seedQuarantinePruneFixtureWithKeeperTitle(t, application, "Missing Song", "Missing Song", "Different Keeper")
	audioCacheMu.Lock()
	_, _, err := application.pruneSuccessfulQuarantineLocked(time.Hour, time.Now(), 1)
	audioCacheMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.quarantine); err != nil {
		t.Fatalf("quarantine that could restore missing playlist row was deleted: %v", err)
	}
}

func TestQuarantineReclaimStopsOnceTheRequestedBytesAreFreed(t *testing.T) {
	application := newApp(t)
	first := seedQuarantinePruneFixture(t, application, "First Song", "First Song")
	second := seedQuarantinePruneFixture(t, application, "Second Song", "Second Song")
	info, err := os.Stat(first.quarantine)
	if err != nil {
		t.Fatal(err)
	}
	audioCacheMu.Lock()
	freed, removed, err := application.pruneSuccessfulQuarantineLocked(time.Hour, time.Now(), info.Size())
	audioCacheMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if freed < info.Size() || removed != 1 {
		t.Fatalf("freed=%d removed=%d want exactly the first receipt", freed, removed)
	}
	if _, err := os.Stat(first.quarantine); !os.IsNotExist(err) {
		t.Fatalf("first proven quarantine survived: %v", err)
	}
	if _, err := os.Stat(second.quarantine); err != nil {
		t.Fatalf("cleanup continued scanning after reaching the reclaim target: %v", err)
	}
}

func TestOldSuccessfulQuarantineOutlivesActiveTaskReference(t *testing.T) {
	application := newApp(t)
	fixture := seedQuarantinePruneFixture(t, application, "Song", "Song")
	if _, err := application.tasks.Enqueue("quarantine-old-active", taskrepo.Spec{
		ID: "quarantine-loser", Command: "fixture", Lane: "download", Priority: 1, CreatedNS: time.Now().UnixNano(),
	}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	key := mergeReceiptKey(fixture.mergeID)
	var oldRaw []byte
	found := false
	err := application.engine.View(func(v *dataengine.View) error {
		var readErr error
		oldRaw, found, readErr = v.Get(key)
		return readErr
	})
	if err != nil || !found {
		t.Fatalf("read receipt: found=%v err=%v", found, err)
	}
	var receipt mergeReceipt
	if err := json.Unmarshal(oldRaw, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.CreatedNS = time.Now().Add(-7 * time.Hour).UnixNano()
	newRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := application.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: key, Value: oldRaw}},
		[]dataengine.Mutation{{Key: key, Value: newRaw}},
	); err != nil || !ok {
		t.Fatalf("update receipt: ok=%v err=%v", ok, err)
	}
	audioCacheMu.Lock()
	_, removed, err := application.pruneSuccessfulQuarantineLocked(time.Hour, time.Now(), 1)
	audioCacheMu.Unlock()
	if err != nil || removed == 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(fixture.quarantine); !os.IsNotExist(err) {
		t.Fatalf("old successful quarantine survived: %v", err)
	}
}

func TestQuarantineHashOrKeeperMismatchIsPreserved(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, application *App, fixture quarantinePruneFixture)
	}{
		{name: "hash mismatch", mutate: func(t *testing.T, _ *App, fixture quarantinePruneFixture) {
			if err := os.WriteFile(fixture.quarantine, pcmWAV(880), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "keeper missing", mutate: func(t *testing.T, _ *App, fixture quarantinePruneFixture) {
			if err := os.Remove(fixture.keeperPath); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			application := newApp(t)
			fixture := seedQuarantinePruneFixture(t, application, "Song", "Song")
			test.mutate(t, application, fixture)
			audioCacheMu.Lock()
			_, _, err := application.pruneSuccessfulQuarantineLocked(time.Hour, time.Now(), 1)
			audioCacheMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(fixture.quarantine); err != nil {
				t.Fatalf("unproven quarantine was deleted: %v", err)
			}
		})
	}
}

func TestActiveTaskReferencePreservesSuccessfulQuarantine(t *testing.T) {
	application := newApp(t)
	fixture := seedQuarantinePruneFixture(t, application, "Song", "Song")
	if _, err := application.tasks.Enqueue("quarantine-active", taskrepo.Spec{
		ID: "quarantine-loser", Command: "fixture", Lane: "download", Priority: 1, CreatedNS: time.Now().UnixNano(),
	}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	audioCacheMu.Lock()
	_, _, err := application.pruneSuccessfulQuarantineLocked(time.Hour, time.Now(), 1)
	audioCacheMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.quarantine); err != nil {
		t.Fatalf("active-task quarantine was deleted: %v", err)
	}
}
