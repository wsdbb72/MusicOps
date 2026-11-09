package app

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
)

// --- test-only NCM encoder (mirrors internal/ncm's test helper) ---

func testAESEncrypt(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return out
}

func testKeybox(key []byte) [256]byte {
	var box [256]byte
	for i := range box {
		box[i] = byte(i)
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (int(box[i]) + j + int(key[i%len(key)])) & 0xff
		box[i], box[j] = box[j], box[i]
	}
	return box
}

func testCryptPayload(data []byte, box [256]byte) []byte {
	out := make([]byte, len(data))
	for i, v := range data {
		j := (i + 1) & 0xff
		out[i] = v ^ box[(int(box[j])+int(box[(int(box[j])+j)&0xff]))&0xff]
	}
	return out
}

func buildTestNCM(t *testing.T, audio []byte, meta map[string]any, cover []byte) []byte {
	t.Helper()
	coreKey := []byte("hzHRAmso5kInbaxW")
	metaKey := []byte{0x23, 0x31, 0x34, 0x6c, 0x6a, 0x6b, 0x5f, 0x21, 0x5c, 0x5d, 0x26, 0x30, 0x55, 0x3c, 0x27, 0x28}
	contentKey := []byte("test-content-key")
	box := testKeybox(contentKey)
	var buf bytes.Buffer
	buf.WriteString("CTENFDAM")
	buf.Write([]byte{0, 0})
	keyPlain := append([]byte("neteasecloudmusic"), contentKey...)
	encKey := testAESEncrypt(t, keyPlain, coreKey)
	writeBlock := func(data []byte) {
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
		buf.Write(lenBuf[:])
		buf.Write(data)
	}
	xor := func(in []byte, x byte) []byte {
		out := make([]byte, len(in))
		for i, b := range in {
			out[i] = b ^ x
		}
		return out
	}
	writeBlock(xor(encKey, 0x64))
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	plain := append([]byte("music:"), metaJSON...)
	enc := testAESEncrypt(t, plain, metaKey)
	metaBlock := append([]byte("163 key(Don't modify):"), []byte(base64.StdEncoding.EncodeToString(enc))...)
	writeBlock(xor(metaBlock, 0x63))
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(audio))
	buf.Write(crcBuf[:])
	buf.Write(make([]byte, 5))
	writeBlock(cover)
	buf.Write(testCryptPayload(audio, box))
	return buf.Bytes()
}

// --- shared HTTP helpers for the media/library tests ---
// (getJSON/post/postCode already exist in review_test.go, app_test.go
// and audio_release_test.go; only the mediaHandler mux is new here.)

// mediaHandler routes the new slice-7 handlers in a test-only mux with
// the exact routes the parent will wire into http.go. The production
// Handler() does not have them yet by design (http.go is parent-owned).
func mediaHandler(application *App) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/library/ncm/decrypt", application.decryptNCM)
	mux.HandleFunc("GET /v1/library/tags", application.libraryTags)
	mux.HandleFunc("POST /v1/library/tags", application.saveLibraryTags)
	mux.HandleFunc("GET /v1/library/lyrics", application.libraryLyrics)
	mux.HandleFunc("POST /v1/library/lyrics", application.saveLibraryLyrics)
	mux.HandleFunc("GET /v1/library/duplicates", application.duplicateScan)
	mux.HandleFunc("POST /v1/library/duplicates/merge", application.duplicateMerge)
	mux.HandleFunc("POST /v1/library/duplicates/restore", application.duplicateRestore)
	mux.HandleFunc("GET /v1/library/artist-folders", application.artistFolderPlan)
	mux.HandleFunc("POST /v1/library/artist-folders", application.artistFolderRelocate)
	return mux
}

func mediaPost(t *testing.T, application *App, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mediaHandler(application).ServeHTTP(recorder, request)
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(recorder.Code, recorder.Body.String(), err)
	}
	return recorder.Code, decoded
}

func mediaGet(t *testing.T, application *App, path string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	recorder := httptest.NewRecorder()
	mediaHandler(application).ServeHTTP(recorder, request)
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(recorder.Code, recorder.Body.String(), err)
	}
	return recorder.Code, decoded
}

func genTestFLAC(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "flac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate %s: %v %s", path, err, out)
	}
	return path
}

// syntheticFLAC builds a minimal FLAC: STREAMINFO + optional PICTURE
// block + VORBIS_COMMENT + fake audio bytes. It is enough for flactags
// to parse and rewrite, which is what the tag-preservation test needs.
func syntheticFLAC(t *testing.T, withPicture bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("fLaC")
	writeBlock := func(typ byte, data []byte, last bool) {
		header := typ & 0x7f
		if last {
			header |= 0x80
		}
		buf.Write([]byte{header, byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))})
		buf.Write(data)
	}
	writeBlock(0, make([]byte, 34), false)
	if withPicture {
		writeBlock(6, []byte("PICTURE-MARKER-BYTES"), false)
	}
	vendor := "TestVendor"
	comment := new(bytes.Buffer)
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(vendor)))
	comment.Write(n[:])
	comment.WriteString(vendor)
	binary.LittleEndian.PutUint32(n[:], 0)
	comment.Write(n[:])
	writeBlock(4, comment.Bytes(), true)
	buf.WriteString("FAKEAUDIOBYTES")
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// NCM decrypt endpoint

func TestNCMDecryptEndpoint(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	wav := writeTestWAV(t, filepath.Join(incoming, "plain.wav"))
	meta := map[string]any{"musicName": "Test Song", "artist": []any{map[string]any{"id": float64(7), "name": "Test Artist"}}, "album": "Test Album", "format": "wav", "duration": float64(1000)}
	container := buildTestNCM(t, wav, meta, []byte{0xff, 0xd8, 0xff, 0xe0})
	source := filepath.Join(incoming, "song.ncm")
	if err := os.WriteFile(source, container, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := hashFile(source)
	if err != nil {
		t.Fatal(err)
	}
	code, decoded := mediaPost(t, application, "/v1/library/ncm/decrypt", map[string]any{"source": "incoming/song.ncm"})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	if decoded["format"] != "wav" || decoded["has_cover"] != true || decoded["validation_state"] != "pending" {
		t.Fatal(decoded)
	}
	metadata, ok := decoded["metadata"].(map[string]any)
	if !ok || metadata["title"] != "Test Song" || metadata["album"] != "Test Album" {
		t.Fatal(decoded)
	}
	output, ok := decoded["output"].(string)
	if !ok || output == "" {
		t.Fatal(decoded)
	}
	if !strings.HasSuffix(output, ".wav") || strings.HasSuffix(output, ".ncm") {
		t.Fatal("output must carry the decrypted format, never .ncm:", output)
	}
	outBytes, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(outBytes, wav) {
		t.Fatal("decrypted output is not the original WAV", err)
	}
	// The source NCM is untouched.
	if after, err := hashFile(source); err != nil || after != before {
		t.Fatal("source changed during decrypt")
	}
	// A second identical request replays instead of decrypting again.
	code, decoded = mediaPost(t, application, "/v1/library/ncm/decrypt", map[string]any{"source": "incoming/song.ncm"})
	if code != http.StatusOK || decoded["replayed"] != true {
		t.Fatal(code, decoded)
	}
}

func TestNCMDecryptRejects(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	// Not an NCM container.
	plain := filepath.Join(incoming, "plain.ncm")
	if err := os.WriteFile(plain, []byte("not an ncm container at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, decoded := mediaPost(t, application, "/v1/library/ncm/decrypt", map[string]any{"source": "incoming/plain.ncm"})
	if code != http.StatusUnprocessableEntity || decoded["code"] != "NCM_DECRYPT_FAILED" {
		t.Fatal(code, decoded)
	}
	// Escape from the state directory.
	code, _ = mediaPost(t, application, "/v1/library/ncm/decrypt", map[string]any{"source": "/etc/passwd"})
	if code != http.StatusBadRequest {
		t.Fatal(code)
	}
	code, _ = mediaPost(t, application, "/v1/library/ncm/decrypt", map[string]any{"source": "../outside.ncm"})
	if code != http.StatusBadRequest {
		t.Fatal(code)
	}
	// Missing file.
	code, _ = mediaPost(t, application, "/v1/library/ncm/decrypt", map[string]any{"source": "incoming/missing.ncm"})
	if code != http.StatusBadRequest {
		t.Fatal(code)
	}
	// An existing output with different bytes is a conflict, not an overwrite.
	wav := writeTestWAV(t, filepath.Join(incoming, "plain2.wav"))
	meta := map[string]any{"musicName": "Second", "format": "wav"}
	container := buildTestNCM(t, wav, meta, nil)
	if err := os.WriteFile(filepath.Join(incoming, "song2.ncm"), container, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "song2.wav"), []byte("different bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, decoded = mediaPost(t, application, "/v1/library/ncm/decrypt", map[string]any{"source": "incoming/song2.ncm"})
	if code != http.StatusConflict || decoded["code"] != "NCM_CONFLICT" {
		t.Fatal(code, decoded)
	}
	if got, err := os.ReadFile(filepath.Join(incoming, "song2.wav")); err != nil || string(got) != "different bytes" {
		t.Fatal("existing output was overwritten")
	}
}

// ---------------------------------------------------------------------------
// FLAC tags endpoint

func TestFLACTagsReadWriteEndpoint(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	path := genTestFLAC(t, incoming, "tagged.flac")
	rel := "incoming/tagged.flac"

	code, decoded := mediaGet(t, application, "/v1/library/tags?path="+rel)
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	if decoded["ok"] != true {
		t.Fatal(decoded)
	}
	code, decoded = mediaPost(t, application, "/v1/library/tags", map[string]any{
		"path": rel,
		"tags": map[string]any{"title": "Tag Song", "artist": []string{"First Artist", "Second Artist"}, "album": "Tag Album", "tracknumber": 3, "date": "2024-05-06", "genre": "Test"},
	})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	tags, ok := decoded["tags"].(map[string]any)
	if !ok || tags["title"] != "Tag Song" || tags["tracknumber"] != float64(3) {
		t.Fatal(decoded)
	}
	artists, ok := tags["artist"].([]any)
	if !ok || len(artists) != 2 || artists[0] != "First Artist" || artists[1] != "Second Artist" {
		t.Fatal(decoded)
	}
	// The audio still fully decodes after the tag write.
	decoder := audiorelease.SystemDecoder()
	if decoder == nil {
		t.Fatal("system decoder unavailable")
	}
	if err := decoder(t.Context(), path); err != nil {
		t.Fatal("tagged FLAC did not fully decode", err)
	}
	// Deleting a field removes it.
	code, decoded = mediaPost(t, application, "/v1/library/tags", map[string]any{
		"path": rel, "tags": map[string]any{"genre": ""},
	})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	tags = decoded["tags"].(map[string]any)
	if tags["genre"] != "" {
		t.Fatal("genre was not deleted", decoded)
	}
	if tags["title"] != "Tag Song" {
		t.Fatal("unrelated field changed", decoded)
	}
}

func TestFLACTagsRejects(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	path := genTestFLAC(t, incoming, "reject.flac")
	rel := "incoming/reject.flac"
	// Unknown field.
	code, decoded := mediaPost(t, application, "/v1/library/tags", map[string]any{"path": rel, "tags": map[string]any{"bpm": "120"}})
	if code != http.StatusBadRequest {
		t.Fatal(code, decoded)
	}
	// Bad track number.
	code, decoded = mediaPost(t, application, "/v1/library/tags", map[string]any{"path": rel, "tags": map[string]any{"tracknumber": "abc"}})
	if code != http.StatusBadRequest {
		t.Fatal(code, decoded)
	}
	// Non-FLAC file.
	wav := writeTestWAV(t, filepath.Join(incoming, "tone.wav"))
	_ = wav
	code, decoded = mediaGet(t, application, "/v1/library/tags?path=incoming/tone.wav")
	if code != http.StatusBadRequest {
		t.Fatal(code, decoded)
	}
	// Escape.
	code, _ = mediaGet(t, application, "/v1/library/tags?path=/etc/passwd")
	if code != http.StatusBadRequest {
		t.Fatal(code)
	}
	// "3/12" style track numbers keep the leading integer.
	code, decoded = mediaPost(t, application, "/v1/library/tags", map[string]any{"path": rel, "tags": map[string]any{"tracknumber": "3/12"}})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	if decoded["tags"].(map[string]any)["tracknumber"] != float64(3) {
		t.Fatal(decoded)
	}
	// The file itself is unchanged by the rejected writes above except
	// for the last accepted one; the tag write preserved the audio.
	decoder := audiorelease.SystemDecoder()
	if decoder == nil {
		t.Fatal("system decoder unavailable")
	}
	if err := decoder(t.Context(), path); err != nil {
		t.Fatal(err)
	}
}

func TestFLACTagWritePreservesPictureBlock(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(incoming, "pic.flac")
	if err := os.WriteFile(path, syntheticFLAC(t, true), 0o600); err != nil {
		t.Fatal(err)
	}
	code, decoded := mediaPost(t, application, "/v1/library/tags", map[string]any{
		"path": "incoming/pic.flac", "tags": map[string]any{"title": "With Picture"},
	})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte("PICTURE-MARKER-BYTES")) {
		t.Fatal("picture block was not preserved by the tag write")
	}
	if !bytes.Contains(after, []byte("FAKEAUDIOBYTES")) {
		t.Fatal("audio bytes were not preserved by the tag write")
	}
	tags, err := flactags.ReadFile(path)
	if err != nil || tags.Title != "With Picture" {
		t.Fatal(tags, err)
	}
}

// ---------------------------------------------------------------------------
// LRC endpoint

func TestLyricsSidecarAndEmbed(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	path := genTestFLAC(t, incoming, "lyric.flac")
	rel := "incoming/lyric.flac"

	// No sidecar yet: GET reports null sidecar, no embedded lyrics.
	code, decoded := mediaGet(t, application, "/v1/library/lyrics?path="+rel)
	if code != http.StatusOK || decoded["sidecar"] != nil || decoded["embedded"] != nil {
		t.Fatal(code, decoded)
	}
	// Write with an offset: it folds into the timestamps (player_lrc).
	text := "[ti:Test Song]\n[offset:+500]\n[00:01.00] Hello\n[00:02.50] World\n"
	code, decoded = mediaPost(t, application, "/v1/library/lyrics", map[string]any{"path": rel, "text": text, "sidecar": true, "embed": true})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	if decoded["sidecar_written"] != true || decoded["embedded"] != true || decoded["lines"] != float64(2) {
		t.Fatal(decoded)
	}
	written, err := os.ReadFile(lrc.SidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	// Offset +500ms folded in: 01.00 -> 01.50, 02.50 -> 03.00.
	if !strings.Contains(string(written), "[00:01.50] Hello") || !strings.Contains(string(written), "[00:03.00] World") {
		t.Fatal("offset was not folded into the timestamps:\n" + string(written))
	}
	if strings.Contains(string(written), "[offset:") {
		t.Fatal("offset tag survived normalization:\n" + string(written))
	}
	code, decoded = mediaGet(t, application, "/v1/library/lyrics?path="+rel)
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	sidecar, ok := decoded["sidecar"].(map[string]any)
	if !ok {
		t.Fatal(decoded)
	}
	if sidecar["offset_ms"] != float64(0) {
		t.Fatal("sidecar offset was not folded", decoded)
	}
	embedded, ok := decoded["embedded"].(map[string]any)
	if !ok {
		t.Fatal("embedded lyrics missing", decoded)
	}
	embeddedText, _ := embedded["text"].(string)
	if !strings.Contains(embeddedText, "[00:01.50] Hello") {
		t.Fatal("embedded lyrics do not hold the normalized timeline", decoded)
	}
	// The audio still fully decodes after embedding.
	decoder := audiorelease.SystemDecoder()
	if decoder == nil {
		t.Fatal("system decoder unavailable")
	}
	if err := decoder(t.Context(), path); err != nil {
		t.Fatal("FLAC with embedded lyrics did not fully decode", err)
	}
	// Overwriting the sidecar with new content replaces it.
	code, decoded = mediaPost(t, application, "/v1/library/lyrics", map[string]any{"path": rel, "text": "[00:00.50] New line\n", "sidecar": true})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	written, err = os.ReadFile(lrc.SidecarPath(path))
	if err != nil || !strings.Contains(string(written), "New line") {
		t.Fatal("sidecar overwrite failed", string(written), err)
	}
}

func TestLyricsRejects(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	_ = genTestFLAC(t, incoming, "lyric2.flac")
	rel := "incoming/lyric2.flac"
	// No valid timed line.
	code, decoded := mediaPost(t, application, "/v1/library/lyrics", map[string]any{"path": rel, "text": "just plain text\nno stamps here\n", "sidecar": true})
	if code != http.StatusBadRequest || decoded["code"] != "INVALID_LRC" {
		t.Fatal(code, decoded)
	}
	// Neither target selected.
	code, _ = mediaPost(t, application, "/v1/library/lyrics", map[string]any{"path": rel, "text": "[00:01.00] Hi\n"})
	if code != http.StatusBadRequest {
		t.Fatal(code)
	}
	// Embedding into a non-FLAC file is refused honestly.
	wav := writeTestWAV(t, filepath.Join(incoming, "tone.wav"))
	_ = wav
	code, decoded = mediaPost(t, application, "/v1/library/lyrics", map[string]any{"path": "incoming/tone.wav", "text": "[00:01.00] Hi\n", "embed": true})
	if code != http.StatusUnprocessableEntity || decoded["code"] != "UNSUPPORTED_FORMAT" {
		t.Fatal(code, decoded)
	}
	// But a sidecar next to a WAV is fine.
	code, decoded = mediaPost(t, application, "/v1/library/lyrics", map[string]any{"path": "incoming/tone.wav", "text": "[00:01.00] Hi\n", "sidecar": true})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	// Escape.
	code, _ = mediaGet(t, application, "/v1/library/lyrics?path=../outside.flac")
	if code != http.StatusBadRequest {
		t.Fatal(code)
	}
}
