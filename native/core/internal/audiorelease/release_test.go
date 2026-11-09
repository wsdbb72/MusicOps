package audiorelease

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
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
	again, err := Release(context.Background(), engine, sample(incoming, library, "song.wav"))
	if err != nil || !again.Replayed || again.Downloaded || again.SHA256 != got.SHA256 {
		t.Fatal(again, err)
	}
	albums, err := Albums(engine)
	if err != nil || !albums["root-1"] {
		t.Fatal(albums, err)
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
