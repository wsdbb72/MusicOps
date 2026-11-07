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
	if err != nil || !got.Downloaded || !got.LibraryPublished || !got.Verified || !got.SourcePreserved || got.ProductionVerified || got.Replayed || got.Format != "wav" {
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
	if err != nil || !again.Replayed || !again.Downloaded || again.SHA256 != got.SHA256 {
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

func TestFLACRequiresADecoder(t *testing.T) {
	engine, incoming, library := openRelease(t)
	body := []byte("fLaC")
	body = append(body, 0x80, 0, 0, 34)
	body = append(body, bytes.Repeat([]byte{0}, 10)...)
	body = append(body, 0x0a, 0, 0)
	body = append(body, bytes.Repeat([]byte{0}, 21)...)
	body = append(body, 0xFF, 0xF8, 0x00)
	writeSource(t, incoming, "song.flac", body)
	input := sample(incoming, library, "song.flac")
	got, err := Release(context.Background(), engine, input)
	if !errors.Is(err, ErrTool) || got.Downloaded || got.Class != "tool_missing" {
		t.Fatal(got, err)
	}
	input.Decoder = func(context.Context, string) error { return errors.New("not a flac frame") }
	got, err = Release(context.Background(), engine, input)
	if !errors.Is(err, ErrRejected) || got.Downloaded || got.Class != "decode_failed" {
		t.Fatal(got, err)
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
	if err != nil || got.DecodeState != "decoded" || !got.Downloaded || got.ProductionVerified {
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
