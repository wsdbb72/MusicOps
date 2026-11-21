package audiorelease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

func TestWAVPublishesOnceAndKeepsTheSource(t *testing.T) {
	engine, incoming, library := openRelease(t)
	body := wav(16)
	writeSource(t, incoming, "song.wav", body)
	got, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil || got.Downloaded || !got.LibraryPublished || !got.Verified || !got.SourcePreserved || got.ProductionVerified || got.Replayed || got.Format != "wav" {
		t.Fatal(got, err)
	}
	source, err := os.ReadFile(filepath.Join(incoming, "song.wav"))
	if err != nil || !bytes.Equal(source, body) {
		t.Fatal("source changed", err)
	}
	published := filepath.Join(library, "Artist", "Album", "01 Title.wav")
	file, err := os.ReadFile(published)
	if err != nil || !bytes.Equal(file, body) {
		t.Fatal(err, len(file))
	}
	entries, err := ListIndexed(engine)
	if err != nil || len(entries) != 1 || !entries[0].HasIndexedStat() {
		t.Fatal(entries, err)
	}
	again, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil || !again.Replayed || again.Downloaded || again.SHA256 != got.SHA256 {
		t.Fatal(again, err)
	}
	albums, err := Albums(engine)
	if err != nil || !albums["root-1"] {
		t.Fatal(albums, err)
	}
}

func TestExistingIdenticalLibraryFileCanBeAdoptedByAnotherRoot(t *testing.T) {
	engine, incoming, library := openRelease(t)
	body := wav(48)
	writeSource(t, incoming, "song.wav", body)
	first := sample(incoming, library, "song.wav")
	if _, err := Release(context.Background(), engine, first); err != nil {
		t.Fatal(err)
	}
	second := sample(incoming, library, "song.wav")
	second.RequestID, second.RootID = "rel-2", "root-2"
	got, err := Release(context.Background(), engine, second)
	if err != nil || got.LibraryPublished != true || got.SHA256 == "" {
		t.Fatal(got, err)
	}
	published := filepath.Join(library, "Artist", "Album", "01 Title.wav")
	if data, err := os.ReadFile(published); err != nil || !bytes.Equal(data, body) {
		t.Fatal("identical destination changed", err)
	}

	writeSource(t, incoming, "song.wav", wav(64))
	third := sample(incoming, library, "song.wav")
	third.RequestID, third.RootID = "rel-3", "root-3"
	if _, err := Release(context.Background(), engine, third); !errors.Is(err, ErrConflict) {
		t.Fatal("different destination bytes were overwritten or adopted", err)
	}
}

func TestUpgradeCanAtomicallyReplaceTheSameLibraryPath(t *testing.T) {
	engine, incoming, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(folder, "01 Title.wav")
	oldBody := wav(16)
	writeSource(t, folder, "01 Title.wav", oldBody)
	newBody := wav(64)
	writeSource(t, incoming, "song.wav", newBody)
	request := sample(incoming, library, "song.wav")
	request.RequestID, request.RootID = "rel-replace", "root-replace"
	request.ReplacePath = dest
	got, err := Release(context.Background(), engine, request)
	if err != nil || !got.LibraryPublished || !got.Verified {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	body, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(body, newBody) {
		t.Fatalf("replacement bytes=%d err=%v", len(body), err)
	}
	if backups, _ := filepath.Glob(filepath.Join(folder, ".musicops-rel-replace.replaced")); len(backups) != 0 {
		t.Fatalf("replacement backup was not committed: %v", backups)
	}
}

func TestUpgradeRejectsAnUnauthorizedReplacementPath(t *testing.T) {
	engine, incoming, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(folder, "01 Title.wav")
	current := wav(32)
	writeSource(t, folder, filepath.Base(dest), current)
	other := filepath.Join(folder, "other.wav")
	otherBody := wav(16)
	writeSource(t, folder, filepath.Base(other), otherBody)
	writeSource(t, incoming, "song.wav", wav(64))
	request := sample(incoming, library, "song.wav")
	request.RequestID, request.RootID = "rel-replace-denied", "root-replace-denied"
	request.ReplacePath = other
	if _, err := Release(context.Background(), engine, request); !errors.Is(err, ErrConflict) {
		t.Fatalf("unauthorized replacement error=%v", err)
	}
	if body, err := os.ReadFile(dest); err != nil || !bytes.Equal(body, current) {
		t.Fatalf("authorized library file changed: %d %v", len(body), err)
	}
	if body, err := os.ReadFile(other); err != nil || !bytes.Equal(body, otherBody) {
		t.Fatalf("unrelated file changed: %d %v", len(body), err)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	recoveryDenied := request
	recoveryDenied.RequestID = "rel-recovery-denied"
	backup := replacementBackupPath(dest, recoveryDenied.RequestID)
	writeSource(t, folder, filepath.Base(backup), current)
	if _, err := Release(context.Background(), engine, recoveryDenied); !errors.Is(err, ErrConflict) {
		t.Fatalf("unauthorized recovery error=%v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("unauthorized recovery created destination: %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("unauthorized recovery consumed backup: %v", err)
	}
}

func TestReleaseRecoversReplacementBackupBeforePublishing(t *testing.T) {
	engine, incoming, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(folder, "01 Title.wav")
	request := sample(incoming, library, "song.wav")
	request.RequestID, request.RootID = "rel-recover", "root-recover"
	request.ReplacePath = dest
	writeSource(t, folder, filepath.Base(replacementBackupPath(dest, request.RequestID)), wav(16))
	newBody := wav(64)
	writeSource(t, incoming, "song.wav", newBody)
	if _, err := Release(context.Background(), engine, request); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(dest); err != nil || !bytes.Equal(body, newBody) {
		t.Fatalf("recovered destination=%d err=%v", len(body), err)
	}
	if _, err := os.Stat(replacementBackupPath(dest, request.RequestID)); !os.IsNotExist(err) {
		t.Fatalf("recovery backup remains: %v", err)
	}
}

func TestReplacementRecoveryKeepsBackupUntilCASCommits(t *testing.T) {
	engine, incoming, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(folder, "01 Title.wav")
	writeSource(t, incoming, "old.wav", wav(16))
	first := sample(incoming, library, "old.wav")
	first.RequestID, first.RootID = "rel-cas-first", "root-cas-conflict"
	if _, err := Release(context.Background(), engine, first); err != nil {
		t.Fatal(err)
	}
	newBody := wav(64)
	writeSource(t, incoming, "new.wav", newBody)
	second := sample(incoming, library, "new.wav")
	second.RequestID, second.RootID = "rel-cas-second", "root-cas-conflict"
	second.ReplacePath = dest
	original := wav(128)
	writeSource(t, folder, filepath.Base(dest), newBody)
	backup := replacementBackupPath(dest, second.RequestID)
	writeSource(t, folder, filepath.Base(backup), original)
	if _, err := Release(context.Background(), engine, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS conflict error=%v", err)
	}
	if body, err := os.ReadFile(dest); err != nil || !bytes.Equal(body, original) {
		t.Fatalf("original was not restored: bytes=%d err=%v", len(body), err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("recovered backup remains: %v", err)
	}
}

func TestCommittedReplayMismatchPreservesBackup(t *testing.T) {
	engine, incoming, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(folder, "01 Title.wav")
	writeSource(t, folder, filepath.Base(dest), wav(16))
	writeSource(t, incoming, "song.wav", wav(64))
	request := sample(incoming, library, "song.wav")
	request.RequestID, request.RootID = "rel-replay-mismatch", "root-replay-mismatch"
	request.ReplacePath = dest
	if _, err := Release(context.Background(), engine, request); err != nil {
		t.Fatal(err)
	}
	backup := replacementBackupPath(dest, request.RequestID)
	writeSource(t, folder, filepath.Base(backup), wav(16))
	writeSource(t, folder, filepath.Base(dest), wav(32))
	if _, err := Release(context.Background(), engine, request); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched replay error=%v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup was deleted despite mismatch: %v", err)
	}
}
func TestReleaseReplayCleansCommittedReplacementBackup(t *testing.T) {
	engine, incoming, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(folder, "01 Title.wav")
	writeSource(t, folder, filepath.Base(dest), wav(16))
	body := wav(64)
	writeSource(t, incoming, "song.wav", body)
	request := sample(incoming, library, "song.wav")
	request.RequestID, request.RootID = "rel-cleanup", "root-cleanup"
	request.ReplacePath = dest
	if _, err := Release(context.Background(), engine, request); err != nil {
		t.Fatal(err)
	}
	backup := replacementBackupPath(dest, request.RequestID)
	writeSource(t, folder, filepath.Base(backup), wav(16))
	replayed, err := Release(context.Background(), engine, request)
	if err != nil || !replayed.Replayed {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("committed backup remains: %v", err)
	}
}

func TestReplayCommittedReplacementRestoresBackupWhenTargetIsMissing(t *testing.T) {
	folder := t.TempDir()
	source := filepath.Join(folder, "source.wav")
	dest := filepath.Join(folder, "track.wav")
	backup := filepath.Join(folder, "track.replaced")
	original := wav(16)
	replacement := wav(64)
	writeSource(t, folder, filepath.Base(source), replacement)
	writeSource(t, folder, filepath.Base(backup), original)
	sum := sha256.Sum256(replacement)
	digest := hex.EncodeToString(sum[:])
	rec := record{
		RequestID: "rel-cas-replay", RootID: "root-cas-replay", Path: dest, SHA256: digest,
		State: "published", Verified: true, LibraryPublished: true,
	}
	committed := false
	restore := func() error {
		return restoreReplacedFile(dest, backup, digest)
	}
	commit := func() {
		committed = true
		_ = os.Remove(backup)
	}
	if _, err := replayCommittedReplacement(rec, source, dest, commit, restore); err == nil {
		t.Fatal("missing committed target was accepted")
	}
	if committed {
		t.Fatal("backup was committed before the target verified")
	}
	if body, err := os.ReadFile(dest); err != nil || !bytes.Equal(body, original) {
		t.Fatalf("original was not restored: bytes=%d err=%v", len(body), err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("restored backup remains: %v", err)
	}
}

func TestReplacementRollbackRestoresOriginalBytes(t *testing.T) {
	folder := t.TempDir()
	dest := filepath.Join(folder, "track.flac")
	backup := filepath.Join(folder, "track.flac.replaced")
	original := []byte("original lossy flac")
	replacement := []byte("new lossless flac")
	writeSource(t, folder, filepath.Base(dest), replacement)
	writeSource(t, folder, filepath.Base(backup), original)
	sum := sha256.Sum256(replacement)
	if err := restoreReplacedFile(dest, backup, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(body, original) {
		t.Fatalf("restored=%q err=%v", body, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup still exists: %v", err)
	}
}
func TestReplacementRollbackDoesNotOverwriteConcurrentPublish(t *testing.T) {
	folder := t.TempDir()
	dest := filepath.Join(folder, "track.flac")
	backup := filepath.Join(folder, "track.flac.replaced")
	original := []byte("original lossy flac")
	replacement := []byte("replacement lossless flac")
	concurrent := []byte("concurrent publish")
	writeSource(t, folder, filepath.Base(dest), concurrent)
	writeSource(t, folder, filepath.Base(backup), original)
	sum := sha256.Sum256(replacement)
	err := restoreReplacedFile(dest, backup, hex.EncodeToString(sum[:]))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	body, readErr := os.ReadFile(dest)
	if readErr != nil || !bytes.Equal(body, concurrent) {
		t.Fatalf("concurrent publish was overwritten: bytes=%q err=%v", body, readErr)
	}
	body, readErr = os.ReadFile(backup)
	if readErr != nil || !bytes.Equal(body, original) {
		t.Fatalf("backup was not preserved: bytes=%q err=%v", body, readErr)
	}
}

func TestReleasePathLockSerializesSameDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Artist", "Album", "01 Title.wav")
	unlockFirst := lockReleasePath(path)
	acquired := make(chan struct{})
	go func() {
		unlockSecond := lockReleasePath(path)
		close(acquired)
		unlockSecond()
	}()
	select {
	case <-acquired:
		unlockFirst()
		t.Fatal("second release entered while the first held the same destination")
	case <-time.After(150 * time.Millisecond):
	}
	unlockFirst()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second release did not acquire after the first released the destination")
	}
}

func TestConcurrentReleasesDoNotInterleaveAfterDestinationScan(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "first.wav", wav(24))
	writeSource(t, incoming, "second.wav", wav(72))
	first := sample(incoming, library, "first.wav")
	first.RequestID, first.RootID = "rel-concurrent-first", "root-concurrent-first"
	second := sample(incoming, library, "second.wav")
	second.RequestID, second.RootID = "rel-concurrent-second", "root-concurrent-second"

	firstEntered := make(chan struct{})
	var scanCalls atomic.Int32
	var unblockOnce sync.Once
	unblock := make(chan struct{})
	stop := func() { unblockOnce.Do(func() { close(unblock) }) }
	defer stop()
	releaseAfterPathScan = func() {
		if scanCalls.Add(1) == 1 {
			close(firstEntered)
			<-unblock
		}
	}
	defer func() { releaseAfterPathScan = nil }()

	type outcome struct {
		result Result
		err    error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		result, err := Release(context.Background(), engine, first)
		firstDone <- outcome{result: result, err: err}
	}()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first release did not reach the destination scan")
	}

	secondDone := make(chan outcome, 1)
	go func() {
		result, err := Release(context.Background(), engine, second)
		secondDone <- outcome{result: result, err: err}
	}()
	select {
	case got := <-secondDone:
		stop()
		t.Fatalf("second release passed the path lock before the first committed: result=%+v err=%v", got.result, got.err)
	case <-time.After(200 * time.Millisecond):
	}
	stop()
	firstOutcome := <-firstDone
	secondOutcome := <-secondDone
	if firstOutcome.err != nil || !firstOutcome.result.LibraryPublished {
		t.Fatalf("first release result=%+v err=%v", firstOutcome.result, firstOutcome.err)
	}
	if !errors.Is(secondOutcome.err, ErrConflict) {
		t.Fatalf("second release must conflict with the first destination, got result=%+v err=%v", secondOutcome.result, secondOutcome.err)
	}
	dest := filepath.Join(library, "Artist", "Album", "01 Title.wav")
	if body, err := os.ReadFile(dest); err != nil || !bytes.Equal(body, wav(24)) {
		t.Fatalf("first destination was overwritten: bytes=%d err=%v", len(body), err)
	}
}

func TestReleaseConsumesBackupAfterHardLinkRollbackExit(t *testing.T) {
	engine, incoming, library := openRelease(t)
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(folder, "01 Title.wav")
	original := wav(16)
	writeSource(t, folder, filepath.Base(dest), original)
	request := sample(incoming, library, "song.wav")
	request.RequestID, request.RootID = "rel-hardlink-recovery", "root-hardlink-recovery"
	request.ReplacePath = dest
	backup := replacementBackupPath(dest, request.RequestID)
	if err := os.Link(dest, backup); err != nil {
		t.Fatal(err)
	}
	newBody := wav(80)
	writeSource(t, incoming, "song.wav", newBody)
	if _, err := Release(context.Background(), engine, request); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(dest); err != nil || !bytes.Equal(body, newBody) {
		t.Fatalf("destination bytes=%d err=%v", len(body), err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("redundant restored backup remains: %v", err)
	}
}

func TestReplacementRollbackDoesNotOverwriteAfterIdentityCheck(t *testing.T) {
	folder := t.TempDir()
	dest := filepath.Join(folder, "track.flac")
	backup := filepath.Join(folder, "track.flac.replaced")
	original := []byte("original lossy flac")
	replacement := []byte("replacement lossless flac")
	concurrent := []byte("concurrent publish after remove")
	writeSource(t, folder, filepath.Base(dest), replacement)
	writeSource(t, folder, filepath.Base(backup), original)
	restoreAfterTargetRemoved = func() {
		if err := os.WriteFile(dest, concurrent, 0o600); err != nil {
			t.Errorf("write concurrent publish: %v", err)
		}
	}
	defer func() { restoreAfterTargetRemoved = nil }()
	sum := sha256.Sum256(replacement)
	err := restoreReplacedFile(dest, backup, hex.EncodeToString(sum[:]))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if body, readErr := os.ReadFile(dest); readErr != nil || !bytes.Equal(body, concurrent) {
		t.Fatalf("new target was overwritten: bytes=%q err=%v", body, readErr)
	}
	if body, readErr := os.ReadFile(backup); readErr != nil || !bytes.Equal(body, original) {
		t.Fatalf("backup was not preserved: bytes=%q err=%v", body, readErr)
	}
}

func TestTorrentAndFixtureNeverPublish(t *testing.T) {
	engine, incoming, library := openRelease(t)
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"album.torrent", []byte("d8:announce4:http4:infod4:name4:teste"), "torrent_rejected"},
		{"song.wav", append([]byte("d4:infod"), wav(8)...), "torrent_rejected"},
		{"song.wav", append([]byte("synthetic-fixture"), wav(8)...), "fixture_rejected"},
		{"song.wav", []byte("musicops.fixture.v1"), "fixture_rejected"},
		{"song.wav", wav(8)[:20], "audio_rejected"},
		{"notes.txt", wav(8), "audio_rejected"},
	}
	for _, item := range cases {
		writeSource(t, incoming, item.name, item.body)
		got, err := Release(context.Background(), engine, sample(incoming, library, item.name))
		if !errors.Is(err, ErrRejected) || got.Downloaded || got.LibraryPublished || got.Class != item.want {
			t.Fatal(item.name, got, err)
		}
	}
	entries, err := os.ReadDir(library)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	albums, err := Albums(engine)
	if err != nil || len(albums) != 0 {
		t.Fatal(albums, err)
	}
}

func TestDecoderFailureDoesNotPublish(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(8))
	input := sample(incoming, library, "song.wav")
	input.Decoder = func(context.Context, string) error { return errors.New("decode failed") }
	got, err := Release(context.Background(), engine, input)
	if !errors.Is(err, ErrRejected) || got.Downloaded || got.Class != "decode_failed" {
		t.Fatal(got, err)
	}
	if _, err = os.Stat(filepath.Join(library, "Artist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestMissingFileAndEscapingPath(t *testing.T) {
	engine, incoming, library := openRelease(t)
	got, err := Release(context.Background(), engine, sample(incoming, library, "missing.wav"))
	if !errors.Is(err, ErrMissing) || got.Downloaded || got.Class != "audio_missing" {
		t.Fatal(got, err)
	}
	input := sample(incoming, library, "../song.wav")
	if _, err = Release(context.Background(), engine, input); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	input = sample(library, library, "song.wav")
	if _, err = Release(context.Background(), engine, input); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
}

func TestFLACWithoutDecoderUsesStructuralCheck(t *testing.T) {
	engine, incoming, library := openRelease(t)
	body := []byte("fLaC")
	body = append(body, 0x80, 0, 0, 34)
	body = append(body, bytes.Repeat([]byte{0}, 10)...)
	body = append(body, 0x0a, 0, 0)
	body = append(body, bytes.Repeat([]byte{0}, 21)...)
	body = append(body, 0xFF, 0xF8, 0x00)
	writeSource(t, incoming, "song.flac", body)
	input := sample(incoming, library, "song.flac")
	// No decoder on the host: the structural check alone accepts the file,
	// and the receipt says so instead of claiming a full decode. This is the
	// same rule WAV already had; FLAC no longer fails with tool_missing.
	got, err := Release(context.Background(), engine, input)
	if err != nil || got.Downloaded || !got.LibraryPublished || got.DecodeState != "structural" || got.Format != "flac" {
		t.Fatal(got, err)
	}
	// A decoder that rejects the file still rejects it: structural acceptance
	// is not a free pass for a bad decode.
	writeSource(t, incoming, "other.flac", body)
	reject := sample(incoming, library, "other.flac")
	reject.RequestID = "rel-2"
	reject.Track = 2
	reject.Decoder = func(context.Context, string) error { return errors.New("not a flac frame") }
	got, err = Release(context.Background(), engine, reject)
	if !errors.Is(err, ErrRejected) || got.Downloaded || got.Class != "decode_failed" {
		t.Fatal(got, err)
	}
}

func TestSourceRemovedAfterPublishReturnsReceipt(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(16))
	req := sample(incoming, library, "song.wav")
	got, err := Release(context.Background(), engine, req)
	if err != nil || !got.SourcePreserved {
		t.Fatal(got, err)
	}
	if err := os.Remove(filepath.Join(incoming, "song.wav")); err != nil {
		t.Fatal(err)
	}
	// The file and the record are committed. A retry after the source was
	// moved returns the receipt with SourcePreserved false, never an error.
	again, err := Release(context.Background(), engine, req)
	if err != nil || !again.Replayed || again.SourcePreserved || again.SHA256 != got.SHA256 {
		t.Fatal(again, err)
	}
}

func TestVerifyChecksLibraryFile(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(16))
	got, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(engine, "rel-1")
	if err != nil || verified.SHA256 != got.SHA256 || verified.Format != "wav" {
		t.Fatal(verified, err)
	}
	if _, err := Verify(engine, "rel-missing"); !errors.Is(err, ErrMissing) {
		t.Fatal("missing record verified", err)
	}
	// A library file that no longer hashes to the stored digest fails.
	if err := os.Remove(filepath.Join(library, "Artist", "Album", "01 Title.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(engine, "rel-1"); err == nil {
		t.Fatal("corrupt library verified")
	}
}

func TestSystemDecoderAcceptsWAVWhenPresent(t *testing.T) {
	decoder := SystemDecoder()
	if decoder == nil {
		t.Skip("ffmpeg and ffprobe are not installed")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatal(err)
	}
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(32))
	input := sample(incoming, library, "song.wav")
	input.Decoder = decoder
	got, err := Release(context.Background(), engine, input)
	if err != nil || got.DecodeState != "decoded" || got.Downloaded || got.ProductionVerified {
		t.Fatal(got, err)
	}
}

func TestSystemDecoderIgnoresNonfatalCoverWarning(t *testing.T) {
	decoder := SystemDecoder()
	if decoder == nil {
		t.Skip("ffmpeg and ffprobe are not installed")
	}
	engine, incoming, library := openRelease(t)
	sourceWAV := filepath.Join(incoming, "source.wav")
	if err := os.WriteFile(sourceWAV, wav(64), 0o600); err != nil {
		t.Fatal(err)
	}
	flacPath := filepath.Join(incoming, "song.flac")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", sourceWAV, "-c:a", "flac", flacPath)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate: %v %s", err, out)
	}
	audio, err := os.ReadFile(flacPath)
	if err != nil {
		t.Fatal(err)
	}
	// A valid signature with an impossible chunk length makes ffmpeg emit a
	// recoverable cover warning, while -xerror still proves the FLAC frames.
	brokenCover := append([]byte("\x89PNG\r\n\x1a\n\x7f\xff\xff\xffIHDR"), bytes.Repeat([]byte{0}, 32)...)
	audio, err = flactags.EmbedFrontCover(audio, brokenCover)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flacPath, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	verify := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-xerror", "-err_detect", "explode", "-threads", "1", "-i", flacPath, "-map", "0:a:0", "-f", "null", "-")
	stderr, err := verify.CombinedOutput()
	if err != nil {
		t.Fatalf("strict decode failed: %v %s", err, stderr)
	}
	if !bytes.Contains(stderr, []byte("chunk too big")) {
		t.Skipf("this ffmpeg build did not emit the expected recoverable cover warning: %s", stderr)
	}
	input := sample(incoming, library, "song.flac")
	input.Decoder = decoder
	got, err := Release(context.Background(), engine, input)
	if err != nil || got.DecodeState != "decoded" || got.Class != "" {
		t.Fatal(got, err)
	}
}

func openRelease(t *testing.T) (*dataengine.Engine, string, string) {
	t.Helper()
	root := t.TempDir()
	engine, err := dataengine.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	incoming := filepath.Join(root, "incoming")
	library := filepath.Join(root, "library-audio")
	if err = os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	return engine, incoming, library
}

func sample(incoming, library, name string) Request {
	return Request{
		RequestID: "rel-1", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "Title", Track: 1,
		SourceName: name, Incoming: incoming, Library: library,
	}
}

func writeSource(t *testing.T, incoming, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(incoming, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func wav(frames int) []byte {
	data := bytes.Repeat([]byte{0, 0}, frames)
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(4+(8+16)+(8+len(data))))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)
	return buf.Bytes()
}

func TestListReturnsEntriesSorted(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "one.wav", wav(16))
	writeSource(t, incoming, "two.wav", wav(24))
	if _, err := Release(context.Background(), engine, Request{
		RequestID: "rel-t1", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "One", Track: 1,
		SourceName: "one.wav", Incoming: incoming, Library: library,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Release(context.Background(), engine, Request{
		RequestID: "rel-t2", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "Two", Track: 2,
		SourceName: "two.wav", Incoming: incoming, Library: library,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := List(engine)
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	if entries[0].Track != 1 || entries[0].File != "Artist/Album/01 One.wav" || !entries[0].FilePresent {
		t.Fatal(entries[0])
	}
	if entries[1].Track != 2 || entries[1].File != "Artist/Album/02 Two.wav" || entries[1].Format != "wav" {
		t.Fatal(entries[1])
	}
	// A missing file stays listed with FilePresent false: the gap is shown,
	// not hidden.
	if err := os.Remove(filepath.Join(library, "Artist", "Album", "02 Two.wav")); err != nil {
		t.Fatal(err)
	}
	entries, err = List(engine)
	if err != nil || len(entries) != 2 || entries[1].FilePresent {
		t.Fatal(entries, err)
	}
}

func TestSecondTrackPublishesSeparately(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "one.wav", wav(16))
	writeSource(t, incoming, "two.wav", wav(24))
	first, err := Release(context.Background(), engine, Request{
		RequestID: "rel-t1", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "One", Track: 1,
		SourceName: "one.wav", Incoming: incoming, Library: library,
	})
	if err != nil || first.Replayed || first.Downloaded || first.Origin != "local_import" {
		t.Fatal(first, err)
	}
	second, err := Release(context.Background(), engine, Request{
		RequestID: "rel-t2", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "Two", Track: 2,
		SourceName: "two.wav", Incoming: incoming, Library: library,
	})
	if err != nil || second.Replayed || second.Downloaded || second.SHA256 == first.SHA256 {
		t.Fatal(second, err)
	}
	for _, name := range []string{"01 One.wav", "02 Two.wav"} {
		if _, err := os.Stat(filepath.Join(library, "Artist", "Album", name)); err != nil {
			t.Fatal(name, err)
		}
	}
	albums, err := Albums(engine)
	if err != nil || !albums["root-1"] {
		t.Fatal(albums, err)
	}
}

func TestOriginManualRecordedAndInvalidRejected(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(16))
	req := sample(incoming, library, "song.wav")
	req.Origin = "manual"
	got, err := Release(context.Background(), engine, req)
	if err != nil || got.Origin != "manual" || got.SourceName != "song.wav" {
		t.Fatal(got, err)
	}
	again, err := Release(context.Background(), engine, req)
	if err != nil || !again.Replayed || again.Origin != "manual" {
		t.Fatal(again, err)
	}
	bad := sample(incoming, library, "song.wav")
	bad.RequestID = "rel-bad"
	bad.Origin = "torrent"
	if _, err := Release(context.Background(), engine, bad); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal("invalid origin was accepted", err)
	}
}

func TestCASFailureRemovesOrphanFile(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(16))
	ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{{Key: albumKey("root-1", 1), Absent: true}},
		[]dataengine.Mutation{{Key: albumKey("root-1", 1), Value: []byte("seed")}},
	)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	_, err = Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if !errors.Is(err, ErrConflict) {
		t.Fatal("expected conflict", err)
	}
	dest := filepath.Join(library, "Artist", "Album", "01 Title.wav")
	if _, err := os.Lstat(dest); err == nil {
		t.Fatal("orphaned library file left behind", dest)
	}
}

func TestRelocateUpdatesRecordAndRefusesChangedBytes(t *testing.T) {
	engine, incoming, library := openRelease(t)
	body := wav(16)
	writeSource(t, incoming, "song.wav", body)
	got, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil || !got.LibraryPublished {
		t.Fatal(got, err)
	}
	published := filepath.Join(library, "Artist", "Album", "01 Title.wav")
	moved := filepath.Join(library, "Artist", "Album", "01 Title (moved).wav")
	if err := os.Rename(published, moved); err != nil {
		t.Fatal(err)
	}
	if err := Relocate(engine, "rel-1", moved); err != nil {
		t.Fatal(err)
	}
	entries, err := List(engine)
	if err != nil || len(entries) != 1 || entries[0].File != "Artist/Album/01 Title (moved).wav" {
		t.Fatal(entries, err)
	}
	// Verify still attests the moved file.
	if _, err := Verify(engine, "rel-1"); err != nil {
		t.Fatal(err)
	}
	// A destination holding different bytes is refused.
	other := filepath.Join(library, "Artist", "Album", "01 Title (other).wav")
	if err := os.WriteFile(other, []byte("different bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Relocate(engine, "rel-1", other); err == nil {
		t.Fatal("relocate to different bytes accepted")
	}
	// A missing record is refused.
	if err := Relocate(engine, "rel-missing", moved); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
}

func TestRealFLACPublishes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available: " + err.Error())
	}
	engine, incoming, library := openRelease(t)
	flac := filepath.Join(incoming, "real.flac")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "flac", flac)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate: %v %s", err, out)
	}
	got, err := Release(context.Background(), engine, sample(incoming, library, "real.flac"))
	if err != nil || !got.LibraryPublished || got.Format != "flac" {
		t.Fatal(got, err)
	}
	// The same file with a rewritten tag block still publishes: the
	// metadata chain is longer than STREAMINFO alone.
	if _, err := os.Stat(filepath.Join(library, "Artist", "Album", "01 Title.flac")); err != nil {
		t.Fatal(err)
	}
}

func TestRecordPublished(t *testing.T) {
	engine, _, library := openRelease(t)
	placed := filepath.Join(library, "Artist", "Album", "01 Title.wav")
	if err := os.MkdirAll(filepath.Dir(placed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, wav(32), 0o600); err != nil {
		t.Fatal(err)
	}
	input := PublishedInput{
		RequestID: "pub-1", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "Title", Track: 1,
		Path: placed, Library: library,
	}
	got, err := RecordPublished(context.Background(), engine, input)
	if err != nil || !got.LibraryPublished || got.Format != "wav" || got.Origin != "controlled_audio" {
		t.Fatal(got, err)
	}
	entries, err := List(engine)
	if err != nil || len(entries) != 1 || entries[0].RequestID != "pub-1" {
		t.Fatal(entries, err)
	}
	// A second call with the same request ID replays the stored receipt.
	replayed, err := RecordPublished(context.Background(), engine, input)
	if err != nil || !replayed.Replayed || replayed.SHA256 != got.SHA256 {
		t.Fatal(replayed, err)
	}
	// A different request for the same (root, track) slot is a conflict,
	// and a file that is not the organised library file for the track is
	// refused outright.
	if _, err := RecordPublished(context.Background(), engine, PublishedInput{
		RequestID: "pub-2", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "Title", Track: 1,
		Path: placed, Library: library,
	}); err == nil {
		t.Fatal("same slot accepted twice")
	}
	other := filepath.Join(library, "Artist", "Album", "99 Other.wav")
	if err := os.WriteFile(other, wav(32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordPublished(context.Background(), engine, PublishedInput{
		RequestID: "pub-3", RootID: "root-1", Artist: "Artist", Album: "Album", Title: "Title", Track: 1,
		Path: other, Library: library,
	}); err == nil {
		t.Fatal("unorganised path accepted")
	}
}

// The job list asks only about the roots it is about to show. Looking each one
// up by prefix must agree with the full scan and must not report a root that
// was never published.
func TestPublishedRootsChecksOnlyTheGivenRoots(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(16))
	if _, err := Release(context.Background(), engine, sample(incoming, library, "song.wav")); err != nil {
		t.Fatal(err)
	}
	roots, err := PublishedRoots(engine, []string{"root-1", "root-2", "", "root-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !roots["root-1"] || roots["root-2"] || len(roots) != 1 {
		t.Fatal(roots)
	}
	empty, err := PublishedRoots(engine, nil)
	if err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
}
func TestFolderArtistCanGroupCompilationTracks(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(24))
	request := sample(incoming, library, "song.wav")
	request.FolderArtist = "群星"
	request.Album = "最新热歌慢摇"
	if _, err := Release(context.Background(), engine, request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(library, "群星", "最新热歌慢摇", "01 Title.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(library, "Artist", "最新热歌慢摇")); !os.IsNotExist(err) {
		t.Fatal("track artist folder was used instead of the compilation artist")
	}
}
