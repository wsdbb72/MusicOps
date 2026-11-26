package audiorelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestStampVerifiedPresencesUpgradesLegacyRecord(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	library := filepath.Join(t.TempDir(), "music")
	path := filepath.Join(library, "Artist", "Album", "01 Song.flac")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("verified legacy audio")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	root, request := "stamp-root", "stamp-request"
	rec := record{
		RequestID: request, RootID: root, Path: path, SHA256: hex.EncodeToString(sum[:]), Format: "flac",
		State: "published", Verified: true, LibraryPublished: true, Library: library,
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := engine.CompareAndSwap(nil, []dataengine.Mutation{
		{Key: releaseKey(request), Value: raw},
		{Key: albumKey(root, 1), Value: raw},
	})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	entries, err := ListIndexed(engine)
	if err != nil || len(entries) != 1 || entries[0].HasIndexedStat() {
		t.Fatal(entries, err)
	}
	if err := StampVerifiedPresences(engine, []VerifiedPresence{{RequestID: request, RootID: root, Track: 1}}); err != nil {
		t.Fatal(err)
	}
	entries, err = ListIndexed(engine)
	if err != nil || len(entries) != 1 || !entries[0].HasIndexedStat() || !entries[0].PresenceUnchanged() {
		t.Fatal(entries, err)
	}
	if err := os.WriteFile(path, []byte("changed audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if entries[0].PresenceUnchanged() {
		t.Fatal("changed legacy file was stamped as unchanged")
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

func TestSnapshotBatchDerivesFormatWhenCallerLeavesItEmpty(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	library := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(library, "Artist", "Album", "01 Track.FLAC")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := IndexSnapshots(engine, []Existing{{RequestID: "derive-format", RootID: "derive-album", Track: 1, Path: path, Library: library, MetadataOnly: true}}); err != nil || added != 1 {
		t.Fatalf("added=%d err=%v", added, err)
	}
	entries, err := ListIndexed(engine)
	if err != nil || len(entries) != 1 || entries[0].Format != "flac" {
		t.Fatalf("entries=%+v err=%v", entries, err)
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

func TestPruneMissingIndexedKeepsVerifiedReleaseReceipt(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(104))
	released, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil {
		t.Fatal(err)
	}
	path, found, err := PathFor(engine, released.RequestID)
	if err != nil || !found {
		t.Fatal(path, found, err)
	}
	hold := filepath.Join(t.TempDir(), "song.wav")
	if err := os.Rename(path, hold); err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneMissingIndexed(engine, library)
	if err != nil || pruned != 0 {
		t.Fatalf("verified release was pruned: pruned=%d err=%v", pruned, err)
	}
	if _, found, err := PathFor(engine, released.RequestID); err != nil || !found {
		t.Fatal("verified receipt disappeared", found, err)
	}
	if _, err := Verify(engine, released.RequestID); err == nil {
		t.Fatal("missing verified file was reported as present")
	}
	if err := os.Rename(hold, path); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverPublished(context.Background(), engine, RecoverInput{
		RequestID: released.RequestID, RootID: released.RootID, Track: 1,
		SHA256: released.SHA256, Format: released.Format, Origin: released.Origin,
		SourceName: released.SourceName, Library: library,
	})
	if err != nil || recovered.SHA256 != released.SHA256 {
		t.Fatal(recovered, err)
	}
}

func TestMetadataIndexDoesNotOverwriteVerifiedRelease(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(112))
	released, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(library, "Artist", "Album", "01 Title.mp3")
	if err := os.WriteFile(metadataPath, []byte("metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := IndexExisting(engine, Existing{
		RequestID: "lib-metadata-protect", RootID: released.RootID, Track: 1,
		Path: metadataPath, Library: library, Format: "mp3", SourceName: "reindex", MetadataOnly: true,
	})
	if err != nil || !added {
		t.Fatal(added, err)
	}
	if _, err := Verify(engine, released.RequestID); err != nil {
		t.Fatal("metadata index displaced verified release", err)
	}
}

func TestMetadataIndexDoesNotDuplicateVerifiedPhysicalPath(t *testing.T) {
	engine, incoming, library := openRelease(t)
	writeSource(t, incoming, "song.wav", wav(128))
	released, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil {
		t.Fatal(err)
	}
	path, found, err := PathFor(engine, released.RequestID)
	if err != nil || !found {
		t.Fatal(path, found, err)
	}
	added, err := IndexExisting(engine, Existing{
		RequestID: "lib-same-path", RootID: released.RootID, Track: 1,
		Path: path, Library: library, Format: released.Format, SourceName: "reindex", MetadataOnly: true,
	})
	if err != nil || added {
		t.Fatalf("same physical path was added twice: added=%v err=%v", added, err)
	}
	entries, err := ListIndexed(engine)
	if err != nil || len(entries) != 1 || entries[0].RequestID != released.RequestID {
		t.Fatal(entries, err)
	}
	if _, err := Verify(engine, released.RequestID); err != nil {
		t.Fatal(err)
	}
}

func TestIndexExistingRejectsMissingNonMetadataFile(t *testing.T) {
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	library := t.TempDir()
	missing := filepath.Join(library, "missing.flac")
	if _, err := IndexExisting(engine, Existing{
		RequestID: "lib-missing", RootID: "root-missing", Track: 1,
		Path: missing, Library: library, Format: "flac",
	}); !os.IsNotExist(err) {
		t.Fatalf("missing file error = %v", err)
	}
	if _, found, err := PathFor(engine, "lib-missing"); err != nil || found {
		t.Fatal("missing file was indexed", found, err)
	}
}
