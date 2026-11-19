package app

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	apeHeaderReadLimit = 8192
	apeTagReadLimit    = 16 << 20
	maxAPETagItems     = 1024
)

type apeMetadata struct {
	Duration float64
	Tags     map[string]string
}

// probeAPEAudio is a bounded metadata fallback for Monkey's Audio files that
// FFmpeg cannot decode (notably format version 4.11). It reads only the fixed
// descriptor/header and the APEv2 footer/items; it never decodes or rewrites
// the audio. A valid MAC header is required so this path cannot bless an
// arbitrary file merely because it has an .ape extension.
func probeAPEAudio(path string) (apeMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return apeMetadata{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return apeMetadata{}, err
	}
	if info.Size() < 76 {
		return apeMetadata{}, errors.New("APE 文件过短")
	}
	head := make([]byte, apeHeaderReadLimit)
	n, readErr := io.ReadFull(file, head)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return apeMetadata{}, readErr
	}
	head = head[:n]
	duration, err := parseAPEHeader(head)
	if err != nil {
		return apeMetadata{}, err
	}
	// Tags are optional metadata. A damaged or oversized tag must not hide a
	// valid MAC header and make the audio file permanently unscannable.
	tags, _ := readAPEv2Tags(file, info.Size())
	if tags == nil {
		tags = map[string]string{}
	}
	return apeMetadata{Duration: duration, Tags: tags}, nil
}

func parseAPEHeader(data []byte) (float64, error) {
	if len(data) < 76 || string(data[:4]) != "MAC " {
		return 0, errors.New("不是 Monkey's Audio 文件")
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	descriptor := int(binary.LittleEndian.Uint32(data[8:12]))
	// Version 3.98 introduced the descriptor and the current header layout.
	// Older files use a different layout and are left to the normal decoder.
	if version < 3980 || descriptor < 52 || descriptor > 4096 || len(data) < descriptor+24 {
		return 0, fmt.Errorf("不支持 APE 头版本 %d.%02d", version/1000, (version%1000)/10)
	}
	header := data[descriptor : descriptor+24]
	compression := binary.LittleEndian.Uint16(header[0:2])
	blocksPerFrame := binary.LittleEndian.Uint32(header[4:8])
	finalFrameBlocks := binary.LittleEndian.Uint32(header[8:12])
	totalFrames := binary.LittleEndian.Uint32(header[12:16])
	bitsPerSample := binary.LittleEndian.Uint16(header[16:18])
	channels := binary.LittleEndian.Uint16(header[18:20])
	sampleRate := binary.LittleEndian.Uint32(header[20:24])
	if compression < 1000 || compression > 5000 || blocksPerFrame == 0 || finalFrameBlocks == 0 || totalFrames == 0 ||
		channels == 0 || channels > 32 || sampleRate < 8000 || sampleRate > 384000 ||
		(bitsPerSample != 8 && bitsPerSample != 16 && bitsPerSample != 24 && bitsPerSample != 32) {
		return 0, errors.New("APE 头字段不合法")
	}
	samples := (uint64(totalFrames)-1)*uint64(blocksPerFrame) + uint64(finalFrameBlocks)
	if samples == 0 || samples > uint64(sampleRate)*86400 {
		return 0, errors.New("APE 时长字段不合法")
	}
	return float64(samples) / float64(sampleRate), nil
}

func readAPEv2Tags(reader io.ReaderAt, fileSize int64) (map[string]string, error) {
	tags := map[string]string{}
	end := fileSize
	if end >= 128 {
		id3v1 := make([]byte, 3)
		if _, err := reader.ReadAt(id3v1, end-128); err == nil && bytes.Equal(id3v1, []byte("TAG")) {
			end -= 128
		}
	}
	if end < 32 {
		return tags, nil
	}
	footer := make([]byte, 32)
	if _, err := reader.ReadAt(footer, end-32); err != nil {
		return nil, err
	}
	if string(footer[:8]) != "APETAGEX" {
		return tags, nil
	}
	version := binary.LittleEndian.Uint32(footer[8:12])
	tagSize := int64(binary.LittleEndian.Uint32(footer[12:16]))
	count := binary.LittleEndian.Uint32(footer[16:20])
	if (version != 1000 && version != 2000) || tagSize < 32 || tagSize > apeTagReadLimit || tagSize > end {
		return nil, errors.New("APEv2 尾部字段不合法")
	}
	itemsStart := end - tagSize
	itemsLen := tagSize - 32
	if itemsLen == 0 {
		return tags, nil
	}
	raw := make([]byte, int(itemsLen))
	if _, err := reader.ReadAt(raw, itemsStart); err != nil {
		return nil, err
	}
	pos := 0
	for itemIndex := uint32(0); itemIndex < count && itemIndex < maxAPETagItems; itemIndex++ {
		if pos+8 > len(raw) {
			break
		}
		valueSize := int(binary.LittleEndian.Uint32(raw[pos : pos+4]))
		flags := binary.LittleEndian.Uint32(raw[pos+4 : pos+8])
		pos += 8
		keyEnd := bytes.IndexByte(raw[pos:], 0)
		if keyEnd <= 0 || keyEnd > 256 {
			break
		}
		key := strings.ToLower(string(raw[pos : pos+keyEnd]))
		pos += keyEnd + 1
		if valueSize < 0 || valueSize > len(raw)-pos {
			break
		}
		value := raw[pos : pos+valueSize]
		pos += valueSize
		// Item flags bits 1-2: 0 text, 1 binary, 2 external locator.
		if (flags>>1)&3 != 0 || !usefulAPETagKey(key) {
			continue
		}
		text := strings.TrimSpace(strings.ToValidUTF8(string(value), "\uFFFD"))
		if len(text) > 4096 {
			text = text[:4096]
		}
		if text != "" {
			tags[key] = text
		}
	}
	return tags, nil
}

func usefulAPETagKey(key string) bool {
	switch key {
	case "artist", "artists", "album", "title", "album artist", "albumartist", "album_artist",
		"date", "year", "genre", "track", "tracknumber", "disc", "discnumber", "comment":
		return true
	default:
		return false
	}
}
