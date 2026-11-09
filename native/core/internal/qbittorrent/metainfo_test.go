package qbittorrent

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestParseMetainfoV1Single(t *testing.T) {
	data := buildV1Single()
	meta, err := ParseMetainfo(data)
	if err != nil {
		t.Fatalf("ParseMetainfo: %v", err)
	}
	if meta.Type != "v1" || meta.Name != "song.flac" {
		t.Fatalf("meta = %+v", meta)
	}
	// Expected infohash: SHA-1 of the canonical info dict bytes.
	info := map[string]any{
		"length":       int64(100),
		"name":         "song.flac",
		"piece length": int64(16384),
		"pieces":       make([]byte, 20),
	}
	sum := sha1.Sum(benc(info))
	if meta.InfoHashV1 != hex.EncodeToString(sum[:]) {
		t.Fatalf("InfoHashV1 = %s, want %s", meta.InfoHashV1, hex.EncodeToString(sum[:]))
	}
	if meta.InfoHashV2 != "" {
		t.Fatalf("InfoHashV2 = %q, want empty", meta.InfoHashV2)
	}
	if len(meta.Files) != 1 || meta.Files[0].Path != "song.flac" || meta.Files[0].Length != 100 {
		t.Fatalf("files = %+v", meta.Files)
	}
	if meta.TotalSize != 100 || meta.PieceLength != 16384 {
		t.Fatalf("total=%d pieceLength=%d", meta.TotalSize, meta.PieceLength)
	}
	whole := sha256.Sum256(data)
	if meta.MetainfoSHA256 != hex.EncodeToString(whole[:]) {
		t.Fatalf("MetainfoSHA256 = %s", meta.MetainfoSHA256)
	}
}

func TestParseMetainfoV1MultiAndPadding(t *testing.T) {
	meta, err := ParseMetainfo(buildV1Multi())
	if err != nil {
		t.Fatalf("ParseMetainfo multi: %v", err)
	}
	if meta.Type != "v1" || len(meta.Files) != 2 {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Files[0].Path != "album/01.flac" || meta.Files[1].Path != "album/02.flac" {
		t.Fatalf("files = %+v", meta.Files)
	}
	if meta.TotalSize != 300 {
		t.Fatalf("total = %d, want 300", meta.TotalSize)
	}

	padded, err := ParseMetainfo(buildV1MultiWithPadding())
	if err != nil {
		t.Fatalf("ParseMetainfo padded: %v", err)
	}
	if len(padded.Files) != 3 || !padded.Files[2].Padding {
		t.Fatalf("padded files = %+v, want third file padding", padded.Files)
	}
	if padded.TotalSize != 16684 {
		t.Fatalf("padded total = %d, want 16684", padded.TotalSize)
	}
	nonPadding := 0
	for _, f := range padded.Files {
		if !f.Padding {
			nonPadding++
		}
	}
	if nonPadding != 2 {
		t.Fatalf("non-padding files = %d, want 2", nonPadding)
	}
}

func TestParseMetainfoV2(t *testing.T) {
	data := buildV2()
	meta, err := ParseMetainfo(data)
	if err != nil {
		t.Fatalf("ParseMetainfo v2: %v", err)
	}
	if meta.Type != "v2" || meta.InfoHashV1 != "" || meta.InfoHashV2 == "" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(meta.Files) != 1 || meta.Files[0].Path != "song.flac" || meta.Files[0].Length != 50 {
		t.Fatalf("files = %+v", meta.Files)
	}
	if meta.Files[0].PiecesRoot != strings.Repeat("07", 32) {
		t.Fatalf("pieces root = %q", meta.Files[0].PiecesRoot)
	}
}

func TestParseMetainfoHybrid(t *testing.T) {
	meta, err := ParseMetainfo(buildHybrid())
	if err != nil {
		t.Fatalf("ParseMetainfo hybrid: %v", err)
	}
	if meta.Type != "hybrid" || meta.InfoHashV1 == "" || meta.InfoHashV2 == "" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(meta.Files) != 2 || meta.Files[0].Path != "album/01.flac" {
		t.Fatalf("files = %+v", meta.Files)
	}
}

func TestParseMetainfoRejectsBadInput(t *testing.T) {
	valid := buildV1Single()

	// Unordered root keys: "name" before "info".
	info := "d6:lengthi100e4:name9:song.flac12:piece lengthi16384e6:pieces20:" +
		string(make([]byte, 20)) + "e"
	unordered := []byte("d4:name9:song.flac4:info" + info + "e")

	// Non-canonical integer: leading zero in the piece length.
	nonCanonical := bytes.Replace(valid, []byte("i16384e"), []byte("i016384e"), 1)

	// Negative zero.
	negZero := bytes.Replace(valid, []byte("i100e"), []byte("i-0e"), 1)

	cases := map[string][]byte{
		"empty":                 {},
		"truncated":             valid[:len(valid)-3],
		"not a dict":            []byte("l4:infodee"),
		"unordered keys":        unordered,
		"non-canonical integer": nonCanonical,
		"negative zero":         negZero,
		"trailing garbage":      append(append([]byte{}, valid...), 'x'),
	}
	for name, data := range cases {
		if _, err := ParseMetainfo(data); !errors.Is(err, ErrInvalidMetainfo) {
			t.Errorf("%s: error = %v, want ErrInvalidMetainfo", name, err)
		}
	}

	// Piece count mismatch: pieces too short for the declared lengths.
	badPieces := map[string]any{
		"length":       int64(100),
		"name":         "song.flac",
		"piece length": int64(16384),
		"pieces":       make([]byte, 40),
	}
	if _, err := ParseMetainfo(benc(map[string]any{"info": badPieces})); !errors.Is(err, ErrInvalidMetainfo) {
		t.Errorf("piece mismatch: error = %v, want ErrInvalidMetainfo", err)
	}

	// Symlink file blocked.
	symlink := map[string]any{
		"files": []any{
			map[string]any{"length": int64(10), "path": []any{"link"}, "attr": []byte("l")},
		},
		"name":         "album",
		"piece length": int64(16384),
		"pieces":       make([]byte, 20),
	}
	if _, err := ParseMetainfo(benc(map[string]any{"info": symlink})); !errors.Is(err, ErrInvalidMetainfo) {
		t.Errorf("symlink: error = %v, want ErrInvalidMetainfo", err)
	}
}
