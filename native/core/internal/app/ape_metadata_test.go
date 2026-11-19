package app

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeLegacyAPEHeaderAndTagsWithoutFFmpeg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.ape")
	header := make([]byte, 52+24+256)
	copy(header[:4], "MAC ")
	binary.LittleEndian.PutUint16(header[4:6], 4110)
	binary.LittleEndian.PutUint32(header[8:12], 52)
	binary.LittleEndian.PutUint32(header[12:16], 24)
	// APE_HEADER starts at the descriptor length and uses the >= 3.98 layout.
	binary.LittleEndian.PutUint16(header[52:54], 2000)
	binary.LittleEndian.PutUint32(header[56:60], 73728)
	binary.LittleEndian.PutUint32(header[60:64], 44100)
	binary.LittleEndian.PutUint32(header[64:68], 2)
	binary.LittleEndian.PutUint16(header[68:70], 16)
	binary.LittleEndian.PutUint16(header[70:72], 2)
	binary.LittleEndian.PutUint32(header[72:76], 44100)
	tags := makeAPEv2Tag([]apeTestTag{
		{Key: "Artist", Value: "Legacy Artist"},
		{Key: "Album Artist", Value: "Legacy Album Artist"},
		{Key: "Album", Value: "Legacy Album"},
		{Key: "Title", Value: "Legacy Song"},
		{Key: "Year", Value: "2007"},
		{Key: "Genre", Value: "Pop"},
		{Key: "Tracknumber", Value: "01"},
	})
	if err := os.WriteFile(path, append(header, tags...), 0o600); err != nil {
		t.Fatal(err)
	}

	metadata, err := probeAPEAudio(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDuration := float64(73728+44100) / 44100
	if math.Abs(metadata.Duration-wantDuration) > 0.000001 {
		t.Fatalf("duration=%v want=%v", metadata.Duration, wantDuration)
	}
	info, err := probeGenericAudio(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Codec != "ape" || info.Artist != "Legacy Artist" || info.AlbumArtist != "Legacy Album Artist" || info.Album != "Legacy Album" || info.Title != "Legacy Song" ||
		info.Date != "2007" || info.Genre != "Pop" || info.Track != "01" || !info.TagWriteUnsupported {
		t.Fatalf("info=%+v", info)
	}
	if maintenanceNeedsGenericTags(info) {
		t.Fatal("complete read-only APE tags must not trigger a rewrite loop")
	}
	if missing := genericMissingFields(info); len(missing) != 0 {
		t.Fatalf("unexpected rewrite fields: %v", missing)
	}
}

func TestProbeAPEAudioRejectsInvalidHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.ape")
	data := make([]byte, 100)
	copy(data[:4], "MAC ")
	binary.LittleEndian.PutUint16(data[4:6], 4110)
	binary.LittleEndian.PutUint32(data[8:12], 52)
	binary.LittleEndian.PutUint32(data[12:16], 24)
	binary.LittleEndian.PutUint16(data[52:54], 2000)
	binary.LittleEndian.PutUint32(data[56:60], 73728)
	binary.LittleEndian.PutUint32(data[60:64], 44100)
	binary.LittleEndian.PutUint32(data[64:68], 1)
	binary.LittleEndian.PutUint16(data[68:70], 16)
	binary.LittleEndian.PutUint16(data[70:72], 2)
	// Sample rate zero is not a valid APE stream.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := probeAPEAudio(path); err == nil {
		t.Fatal("invalid APE header was accepted")
	}
}

type apeTestTag struct {
	Key   string
	Value string
}

func makeAPEv2Tag(items []apeTestTag) []byte {
	body := make([]byte, 0, 256)
	for _, item := range items {
		value := []byte(item.Value)
		entry := make([]byte, 8)
		binary.LittleEndian.PutUint32(entry[:4], uint32(len(value)))
		body = append(body, entry...)
		body = append(body, item.Key...)
		body = append(body, 0)
		body = append(body, value...)
	}
	tagSize := uint32(len(body) + 32)
	block := func(flags uint32) []byte {
		out := make([]byte, 32)
		copy(out[:8], "APETAGEX")
		binary.LittleEndian.PutUint32(out[8:12], 2000)
		binary.LittleEndian.PutUint32(out[12:16], tagSize)
		binary.LittleEndian.PutUint32(out[16:20], uint32(len(items)))
		binary.LittleEndian.PutUint32(out[20:24], flags)
		return out
	}
	out := append(block(0xA0000000), body...)
	return append(out, block(0x80000000)...)
}

func TestRealLegacyAPEWhenConfigured(t *testing.T) {
	path := os.Getenv("MUSICOPS_REAL_APE")
	if path == "" {
		t.Skip("set MUSICOPS_REAL_APE to verify a local legacy APE")
	}
	info, err := probeGenericAudio(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Codec != "ape" || info.Duration <= 0 || info.Artist == "" || info.Album == "" || info.Title == "" || !info.TagWriteUnsupported {
		t.Fatalf("unexpected APE metadata: %+v", info)
	}
}

func TestRealAPELibraryWhenConfigured(t *testing.T) {
	root := os.Getenv("MUSICOPS_REAL_APE_DIR")
	if root == "" {
		t.Skip("set MUSICOPS_REAL_APE_DIR to scan a local APE library")
	}
	total, failed := 0, 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".ape") {
			return nil
		}
		total++
		info, err := probeGenericAudio(context.Background(), path)
		if err != nil || info.Codec != "ape" || info.Duration <= 0 {
			failed++
			t.Logf("failed %s: info=%+v err=%v", path, info, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("APE files=%d failed=%d", total, failed)
	if total == 0 {
		t.Fatal("configured APE directory contained no files")
	}
	if failed != 0 {
		t.Fatalf("%d of %d APE files still failed metadata fallback", failed, total)
	}
}

func TestProbeAPEAudioKeepsHeaderWhenTagsAreMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken-tag.ape")
	data := make([]byte, 52+24+32)
	copy(data[:4], "MAC ")
	binary.LittleEndian.PutUint16(data[4:6], 4110)
	binary.LittleEndian.PutUint32(data[8:12], 52)
	binary.LittleEndian.PutUint32(data[12:16], 24)
	binary.LittleEndian.PutUint16(data[52:54], 2000)
	binary.LittleEndian.PutUint32(data[56:60], 73728)
	binary.LittleEndian.PutUint32(data[60:64], 44100)
	binary.LittleEndian.PutUint32(data[64:68], 2)
	binary.LittleEndian.PutUint16(data[68:70], 16)
	binary.LittleEndian.PutUint16(data[70:72], 2)
	binary.LittleEndian.PutUint32(data[72:76], 44100)
	footer := data[len(data)-32:]
	copy(footer[:8], "APETAGEX")
	binary.LittleEndian.PutUint32(footer[8:12], 2000)
	binary.LittleEndian.PutUint32(footer[12:16], apeTagReadLimit+1)
	binary.LittleEndian.PutUint32(footer[16:20], 1)
	binary.LittleEndian.PutUint32(footer[20:24], 0x80000000)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, err := probeAPEAudio(path)
	if err != nil || metadata.Duration <= 0 || len(metadata.Tags) != 0 {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}
}
