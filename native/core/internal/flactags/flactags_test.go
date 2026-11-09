package flactags

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
}

// genFLAC generates a real 1-second 440 Hz sine FLAC with ffmpeg.
func genFLAC(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "flac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate %s: %v %s", path, err, out)
	}
	return path
}

// pcmHash decodes the file to raw s16le PCM with ffmpeg and returns its SHA-256.
func pcmHash(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "s16le", "-acodec", "pcm_s16le", "-")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg decode %s: %v", path, err)
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:])
}

// ffprobeTags returns the format tags ffprobe reports for path.
func ffprobeTags(t *testing.T, path string) map[string]string {
	t.Helper()
	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var probe struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("ffprobe JSON %s: %v", path, err)
	}
	return probe.Format.Tags
}

// blockTypes returns the metadata block types in order.
func blockTypes(t *testing.T, data []byte) []byte {
	t.Helper()
	pf, err := parseFLAC(data)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]byte, len(pf.blocks))
	for i, b := range pf.blocks {
		types[i] = b.typ
	}
	return types
}

// stripCommentBlock returns data with the VORBIS_COMMENT block removed
// (a FLAC with no comment block to write into).
func stripCommentBlock(t *testing.T, data []byte) []byte {
	t.Helper()
	pf, err := parseFLAC(data)
	if err != nil {
		t.Fatal(err)
	}
	if pf.commentIdx < 0 {
		return data
	}
	keptBlocks := make([]block, 0, len(pf.blocks))
	for i, b := range pf.blocks {
		if i != pf.commentIdx {
			keptBlocks = append(keptBlocks, b)
		}
	}
	var out bytes.Buffer
	out.WriteString(flacMagic)
	for i, b := range keptBlocks {
		if err := writeBlockHeader(&out, b.typ, len(b.data), i == len(keptBlocks)-1); err != nil {
			t.Fatal(err)
		}
		out.Write(b.data)
	}
	out.Write(pf.audio)
	return out.Bytes()
}

func TestVorbisCommentEncodeDecode(t *testing.T) {
	entries := map[string][]string{
		"TITLE":  {"Song Title"},
		"ARTIST": {"First Artist", "Second Artist"},
		"DATE":   {"2024-05-06"},
	}
	encoded, err := EncodeVorbisComment("TestVendor", entries)
	if err != nil {
		t.Fatal(err)
	}
	vendor, decoded, err := DecodeVorbisComment(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if vendor != "TestVendor" {
		t.Fatalf("vendor = %q, want TestVendor", vendor)
	}
	if len(decoded["ARTIST"]) != 2 || decoded["ARTIST"][0] != "First Artist" || decoded["ARTIST"][1] != "Second Artist" {
		t.Fatalf("ARTIST = %v", decoded["ARTIST"])
	}
	if decoded["TITLE"][0] != "Song Title" || decoded["DATE"][0] != "2024-05-06" {
		t.Fatalf("decoded = %v", decoded)
	}
	// Encoding is deterministic (sorted keys).
	again, err := EncodeVorbisComment("TestVendor", entries)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, again) {
		t.Fatal("EncodeVorbisComment is not deterministic")
	}
	// Lower-case keys are canonicalized on decode.
	encoded, err = EncodeVorbisComment("v", map[string][]string{"title": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, decoded, err := DecodeVorbisComment(encoded); err != nil || decoded["TITLE"][0] != "x" {
		t.Fatalf("lower-case key: decoded=%v err=%v", decoded, err)
	}
}

func TestVorbisCommentDecodeRejectsBadInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":              {},
		"truncated vendor":   {0x05, 0x00, 0x00, 0x00, 'a', 'b'},
		"invalid utf8":       append([]byte{0x01, 0x00, 0x00, 0x00, 'v', 0x01, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00}, []byte("A=\xff")...),
		"entry without =":    {0x01, 0x00, 0x00, 0x00, 'v', 0x01, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00, 'A', 'B', 'C'},
		"overrun length":     {0x01, 0x00, 0x00, 0x00, 'v', 0x01, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff},
		"trailing bytes":     {0x01, 0x00, 0x00, 0x00, 'v', 0x00, 0x00, 0x00, 0x00, 0x00},
		"bad key (= in key)": func() []byte { b, _ := EncodeVorbisComment("v", map[string][]string{"A=B": {"x"}}); return b }(),
	}
	for name, data := range cases {
		if name == "bad key (= in key)" && data != nil {
			// EncodeVorbisComment rejects the bad key, so data is nil;
			// build the bad payload by hand instead.
			data = []byte{0x01, 0x00, 0x00, 0x00, 'v', 0x01, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00, 'A', '=', 'B', '=', 'x'}
		}
		if _, _, err := DecodeVorbisComment(data); err == nil {
			t.Errorf("%s: DecodeVorbisComment accepted invalid input", name)
		}
	}
	if _, err := EncodeVorbisComment("v", map[string][]string{"TITLE": {"bad \xff"}}); err == nil {
		t.Error("EncodeVorbisComment accepted invalid UTF-8 value")
	}
	if _, err := EncodeVorbisComment("bad \xff", nil); err == nil {
		t.Error("EncodeVorbisComment accepted invalid UTF-8 vendor")
	}
}

func TestReadWriteRoundTripRealFLAC(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := genFLAC(t, dir, "song.flac")

	beforeHash := pcmHash(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforePF, err := parseFLAC(before)
	if err != nil {
		t.Fatal(err)
	}
	beforeAudio := append([]byte(nil), beforePF.audio...)

	tags := Tags{
		Title:       "Round Trip Song",
		Artists:     []string{"First Artist", "Second Artist"},
		Album:       "Test Album",
		AlbumArtist: "Album Artist",
		TrackNumber: 3,
		Date:        "2024-05-06",
		Genre:       "Test Genre",
		Lyrics:      "[00:01.00] first line\n[00:05.50] second line\n",
	}
	if err := Write(path, tags); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read back through our own reader.
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got.Title != tags.Title || got.Album != tags.Album || got.AlbumArtist != tags.AlbumArtist ||
		got.TrackNumber != 3 || got.Date != tags.Date || got.Genre != tags.Genre || got.Lyrics != tags.Lyrics {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Artists) != 2 || got.Artists[0] != "First Artist" || got.Artists[1] != "Second Artist" {
		t.Fatalf("Artists = %v, want multi-value", got.Artists)
	}
	if got.Artist != "First Artist" {
		t.Fatalf("Artist = %q, want First Artist", got.Artist)
	}
	if got.StreamInfo.SampleRate == 0 || got.StreamInfo.Channels == 0 || got.StreamInfo.TotalSamples == 0 {
		t.Fatalf("StreamInfo not populated: %+v", got.StreamInfo)
	}
	if len(got.Raw["ARTIST"]) != 2 {
		t.Fatalf("Raw ARTIST = %v", got.Raw["ARTIST"])
	}

	// Verify with ffprobe that a third-party reader sees the same tags.
	// ffprobe 8.x reports some keys upper-case (TITLE) and others
	// lower-case (track), so normalize keys before comparing; multi-value
	// ARTIST comes back joined with ";".
	probeRaw := ffprobeTags(t, path)
	probe := map[string]string{}
	for k, v := range probeRaw {
		probe[strings.ToLower(k)] = v
	}
	checks := map[string]string{
		"title":  "Round Trip Song",
		"artist": "First Artist;Second Artist",
		"album":  "Test Album",
		"date":   "2024-05-06",
		"genre":  "Test Genre",
	}
	for k, want := range checks {
		if probe[k] != want {
			t.Errorf("ffprobe %s = %q, want %q (all tags: %v)", k, probe[k], want, probeRaw)
		}
	}
	if probe["lyrics"] != tags.Lyrics {
		t.Errorf("ffprobe lyrics = %q, want embedded LYRICS", probe["lyrics"])
	}
	if probe["track"] != "3" {
		t.Errorf("ffprobe track = %q, want 3", probe["track"])
	}

	// Audio must be byte-identical: same decoded PCM and same raw audio tail.
	if gotHash := pcmHash(t, path); gotHash != beforeHash {
		t.Errorf("decoded PCM changed: before %s after %s", beforeHash, gotHash)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterPF, err := parseFLAC(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterPF.audio, beforeAudio) {
		t.Error("audio bytes after the metadata chain changed")
	}
}

func TestWriteInsertsCommentBlockWhenAbsent(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := genFLAC(t, dir, "nocmt.flac")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stripped := stripCommentBlock(t, orig)
	if pf, err := parseFLAC(stripped); err != nil || pf.commentIdx >= 0 {
		t.Fatalf("strip failed: commentIdx=%d err=%v", pf.commentIdx, err)
	}
	strippedPath := filepath.Join(dir, "stripped.flac")
	if err := os.WriteFile(strippedPath, stripped, 0644); err != nil {
		t.Fatal(err)
	}
	beforeHash := pcmHash(t, strippedPath)

	if err := Write(strippedPath, Tags{Title: "Inserted", Artist: "Someone", TrackNumber: 1}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ReadFile(strippedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Inserted" || got.Artist != "Someone" || got.TrackNumber != 1 {
		t.Fatalf("inserted tags not read back: %+v", got)
	}
	types := blockTypes(t, mustRead(t, strippedPath))
	if len(types) < 2 || types[0] != blockStreamInfo || types[1] != blockVorbisComment {
		t.Errorf("comment block not inserted after STREAMINFO: types %v", types)
	}
	if gotHash := pcmHash(t, strippedPath); gotHash != beforeHash {
		t.Errorf("decoded PCM changed after insertion: %s -> %s", beforeHash, gotHash)
	}
}

func TestWritePreservesPictureBlock(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	cover := filepath.Join(dir, "cover.png")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=16x16", "-frames:v", "1", cover)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg cover: %v %s", err, out)
	}
	base := genFLAC(t, dir, "base.flac")
	withPic := filepath.Join(dir, "withpic.flac")
	cmd = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-i", base, "-i", cover, "-map", "0:a", "-map", "1", "-c", "copy",
		"-metadata:s:v", "title=Album cover", "-disposition:v", "attached_pic", withPic)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg attach picture: %v %s", err, out)
	}
	before, err := os.ReadFile(withPic)
	if err != nil {
		t.Fatal(err)
	}
	beforePF, err := parseFLAC(before)
	if err != nil {
		t.Fatal(err)
	}
	var beforePic []byte
	for _, b := range beforePF.blocks {
		if b.typ == blockPicture {
			beforePic = b.data
		}
	}
	if beforePic == nil {
		t.Fatal("test setup: no PICTURE block in generated file")
	}
	beforeHash := pcmHash(t, withPic)

	if err := Write(withPic, Tags{Title: "With Picture"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	after, err := os.ReadFile(withPic)
	if err != nil {
		t.Fatal(err)
	}
	afterPF, err := parseFLAC(after)
	if err != nil {
		t.Fatal(err)
	}
	var afterPic []byte
	for _, b := range afterPF.blocks {
		if b.typ == blockPicture {
			afterPic = b.data
		}
	}
	if !bytes.Equal(afterPic, beforePic) {
		t.Error("PICTURE block was not preserved byte-for-byte")
	}
	if !bytes.Equal(afterPF.audio, beforePF.audio) {
		t.Error("audio bytes changed")
	}
	if gotHash := pcmHash(t, withPic); gotHash != beforeHash {
		t.Error("decoded PCM changed after picture-preserving write")
	}
	got, err := ReadFile(withPic)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "With Picture" {
		t.Fatalf("Title = %q", got.Title)
	}
}

func TestWriteToRefusesExistingDestination(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := genFLAC(t, dir, "src.flac")
	dst := filepath.Join(dir, "dst.flac")
	if err := os.WriteFile(dst, []byte("different file"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteTo(src, dst, Tags{Title: "x"}); err == nil {
		t.Error("WriteTo overwrote an existing different file")
	}
	if got, _ := os.ReadFile(dst); string(got) != "different file" {
		t.Error("existing destination was modified")
	}
	// A fresh destination works and leaves the source untouched.
	fresh := filepath.Join(dir, "fresh.flac")
	srcBefore, _ := os.ReadFile(src)
	if err := WriteTo(src, fresh, Tags{Title: "Copied"}); err != nil {
		t.Fatalf("WriteTo fresh dst: %v", err)
	}
	if got, err := ReadFile(fresh); err != nil || got.Title != "Copied" {
		t.Fatalf("fresh dst tags: %+v err=%v", got, err)
	}
	if srcAfter, _ := os.ReadFile(src); !bytes.Equal(srcAfter, srcBefore) {
		t.Error("WriteTo modified the source file")
	}
	// WriteBytes on its own never touches disk.
	out, err := WriteBytes(srcBefore, Tags{Title: "InMemory"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadBytes(out)
	if err != nil || got.Title != "InMemory" {
		t.Fatalf("WriteBytes round-trip: %+v err=%v", got, err)
	}
}

func TestReadRejectsCorruptFLAC(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	good := genFLAC(t, dir, "good.flac")
	goodBytes, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"not flac":          []byte("NOPE not a flac file at all"),
		"empty":             {},
		"magic only":        []byte("fLaC"),
		"truncated header":  goodBytes[:6],
		"truncated block":   append(append([]byte(nil), goodBytes[:10]...), goodBytes[10:20]...),
		"bad magic":         append([]byte("fLaX"), goodBytes[4:]...),
		"block length lies": func() []byte { b := append([]byte(nil), goodBytes...); b[7] = 0xff; return b }(),
	}
	// Invalid UTF-8 inside a comment entry.
	cases["invalid utf8 comment"] = func() []byte {
		// Take a good file, replace its comment block payload with bad UTF-8.
		pf, err := parseFLAC(goodBytes)
		if err != nil {
			t.Fatal(err)
		}
		if pf.commentIdx < 0 {
			t.Fatal("test setup: generated FLAC has no comment block")
		}
		// Rebuild with a hand-made comment block containing invalid UTF-8.
		payload := []byte{0x01, 0x00, 0x00, 0x00, 'v'}
		payload = append(payload, 0x01, 0x00, 0x00, 0x00)
		payload = append(payload, 0x05, 0x00, 0x00, 0x00)
		payload = append(payload, 'T', '=', 0xff, 'x', 'y')
		blocks := append([]block(nil), pf.blocks...)
		blocks[pf.commentIdx] = block{typ: blockVorbisComment, data: payload}
		var out bytes.Buffer
		out.WriteString(flacMagic)
		for i, b := range blocks {
			if err := writeBlockHeader(&out, b.typ, len(b.data), i == len(blocks)-1); err != nil {
				t.Fatal(err)
			}
			out.Write(b.data)
		}
		out.Write(pf.audio)
		return out.Bytes()
	}()
	for name, data := range cases {
		if _, err := ReadBytes(data); err == nil {
			t.Errorf("%s: ReadBytes accepted corrupt FLAC", name)
		}
		if _, err := WriteBytes(data, Tags{Title: "x"}); err == nil {
			t.Errorf("%s: WriteBytes accepted corrupt FLAC", name)
		}
	}
	if _, err := ReadFile(filepath.Join(dir, "does-not-exist.flac")); err == nil {
		t.Error("ReadFile accepted a missing file")
	}
	if err := Write(filepath.Join(dir, "does-not-exist.flac"), Tags{}); err == nil {
		t.Error("Write accepted a missing file")
	}
	if err := Write(good, Tags{TrackNumber: -1}); err == nil {
		t.Error("Write accepted a negative track number")
	}
}

func TestReadPreservesRawMapAndTrackTotal(t *testing.T) {
	// "3/12" track numbers and unknown keys survive via Raw.
	data, err := WriteBytes(mustMinimalFLAC(t), Tags{
		Raw: map[string][]string{
			"TRACKNUMBER": {"3/12"},
			"COMMENT":     {"a note"},
			"TITLE":       {"T"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrackNumber != 3 {
		t.Fatalf("TrackNumber = %d, want 3", got.TrackNumber)
	}
	if got.Raw["TRACKNUMBER"][0] != "3/12" || got.Raw["COMMENT"][0] != "a note" {
		t.Fatalf("Raw = %v", got.Raw)
	}
}

// mustMinimalFLAC builds a tiny valid FLAC: STREAMINFO (silence-ish
// values) + no audio, purely for structure-level tests.
func mustMinimalFLAC(t *testing.T) []byte {
	t.Helper()
	si := make([]byte, streamInfoSize)
	// Sample rate 44100, 2 channels, 16 bps, 0 total samples.
	si[10] = 0x0a
	si[11] = 0xc4
	si[12] = 0x42
	si[13] = 0xf0
	si[14] = 0x00
	si[15] = 0x00
	si[16] = 0x00
	si[17] = 0x00
	var out bytes.Buffer
	out.WriteString(flacMagic)
	if err := writeBlockHeader(&out, blockStreamInfo, streamInfoSize, true); err != nil {
		t.Fatal(err)
	}
	out.Write(si)
	return out.Bytes()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
