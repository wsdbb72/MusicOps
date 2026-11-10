package wavflac

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func buildWAV(t *testing.T, channels, bits, rate int, samples []int32, extra []byte, extensible bool) []byte {
	t.Helper()
	bps := bits / 8
	data := make([]byte, len(samples)*bps)
	for i, sample := range samples {
		off := i * bps
		switch bits {
		case 8:
			data[off] = byte(int(sample) + 128)
		case 16:
			binary.LittleEndian.PutUint16(data[off:], uint16(int16(sample)))
		case 24:
			u := uint32(sample)
			data[off] = byte(u)
			data[off+1] = byte(u >> 8)
			data[off+2] = byte(u >> 16)
		case 32:
			binary.LittleEndian.PutUint32(data[off:], uint32(sample))
		}
	}
	formatSize := 16
	if extensible {
		formatSize = 40
	}
	body := make([]byte, 0, 12+8+formatSize+len(extra)+8+len(data))
	body = append(body, []byte("RIFF")...)
	body = append(body, 0, 0, 0, 0)
	body = append(body, []byte("WAVE")...)
	body = append(body, []byte("fmt ")...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(formatSize))
	body = append(body, size[:]...)
	fmtChunk := make([]byte, formatSize)
	if extensible {
		binary.LittleEndian.PutUint16(fmtChunk[0:], 0xFFFE)
	} else {
		binary.LittleEndian.PutUint16(fmtChunk[0:], 1)
	}
	binary.LittleEndian.PutUint16(fmtChunk[2:], uint16(channels))
	binary.LittleEndian.PutUint32(fmtChunk[4:], uint32(rate))
	align := channels * bps
	binary.LittleEndian.PutUint32(fmtChunk[8:], uint32(rate*align))
	binary.LittleEndian.PutUint16(fmtChunk[12:], uint16(align))
	binary.LittleEndian.PutUint16(fmtChunk[14:], uint16(bits))
	if extensible {
		binary.LittleEndian.PutUint16(fmtChunk[16:], 22)
		binary.LittleEndian.PutUint16(fmtChunk[18:], uint16(bits))
		copy(fmtChunk[24:], pcmGUID)
	}
	body = append(body, fmtChunk...)
	body = append(body, extra...)
	body = append(body, []byte("data")...)
	binary.LittleEndian.PutUint32(size[:], uint32(len(data)))
	body = append(body, size[:]...)
	body = append(body, data...)
	binary.LittleEndian.PutUint32(body[4:], uint32(len(body)-8))
	return body
}

func tone(channels, count int) []int32 {
	out := make([]int32, count*channels)
	for i := 0; i < count; i++ {
		for ch := 0; ch < channels; ch++ {
			out[i*channels+ch] = int32((i*17 + ch*1000) % 10000)
		}
	}
	return out
}

func TestConvertKeepsPCMAcrossWidthsAndChannels(t *testing.T) {
	cases := []struct {
		channels, bits, rate int
		extensible           bool
	}{
		{1, 16, 44100, false},
		{2, 16, 48000, false},
		{2, 24, 96000, false},
		{6, 24, 48000, true},
		{2, 32, 22050, true},
		{1, 8, 8000, false},
	}
	for _, item := range cases {
		dir := t.TempDir()
		src := filepath.Join(dir, "Artist", "Album", "01 Opening.wav")
		if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
			t.Fatal(err)
		}
		samples := tone(item.channels, 5000)
		body := buildWAV(t, item.channels, item.bits, item.rate, samples, nil, item.extensible)
		stamp := time.Date(2020, 5, 1, 12, 0, 0, 0, time.UTC)
		if err := os.WriteFile(src, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(src, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, "Artist", "Album", "01 Opening.flac")
		result, err := ConvertFile(src, dst, true)
		if err != nil {
			t.Fatal(item, err)
		}
		if result.Channels != item.channels || result.Bits != item.bits || result.Rate != item.rate || result.Samples != 5000 {
			t.Fatal(item, result)
		}
		got, err := os.Stat(dst)
		if err != nil || !got.ModTime().Equal(stamp) {
			t.Fatal("timestamp", got.ModTime(), err)
		}
		md5sum, err := FormatMD5(dst)
		if err != nil || md5sum != result.MD5 || result.PCMSHA256 == "" {
			t.Fatal(md5sum, result, err)
		}
		again, err := ConvertFile(src, dst, true)
		if err != nil || !again.Replayed {
			t.Fatal("second run", again, err)
		}
		if after, err := os.ReadFile(src); err != nil || !bytes.Equal(after, body) {
			t.Fatal("source changed")
		}
	}
}

func TestFFmpegDecodesTheSamePCM(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	body := buildWAV(t, 2, 24, 48000, tone(2, 1200), nil, true)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "tone.flac")
	if _, err := ConvertFile(src, dst, false); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", dst, "-f", "s24le", "-ac", "2", "-ar", "48000", "-")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(out))
	}
	if len(out) != 1200*2*3 {
		t.Fatal(len(out))
	}
	for i := 0; i < 1200*2; i++ {
		got := int32(out[i*3]) | int32(out[i*3+1])<<8 | int32(out[i*3+2])<<16
		if got&0x800000 != 0 {
			got |= ^0xffffff
		}
		want := int32((i/2*17 + (i%2)*1000) % 10000)
		if got != want {
			t.Fatalf("sample %d got %d want %d", i, got, want)
		}
	}
}

func TestTagsAndCoverMoveIntoFLAC(t *testing.T) {
	info := []byte("INFO")
	info = appendChunk(info, "IART", "甲")
	info = appendChunk(info, "INAM", "歌名")
	info = appendChunk(info, "IPRD", "专辑")
	list := append([]byte("LIST"), put32(uint32(len(info)))...)
	list = append(list, info...)
	cover := append([]byte{0xff, 0xd8}, bytes.Repeat([]byte{0x11}, 40)...)
	apic := id3APICFrame(cover)
	id3 := id3Tag(apic)
	id3chunk := append([]byte("id3 "), put32(uint32(len(id3)))...)
	id3chunk = append(id3chunk, id3...)
	dir := t.TempDir()
	src := filepath.Join(dir, "tagged.wav")
	body := buildWAV(t, 1, 16, 44100, tone(1, 100), append(list, id3chunk...), false)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	tags, err := ReadDescription(src)
	if err != nil || tags.Artist != "甲" || tags.Title != "歌名" || tags.Album != "专辑" || len(tags.Cover) == 0 {
		t.Fatal(tags, err)
	}
	dst := filepath.Join(dir, "tagged.flac")
	if _, err := ConvertFile(src, dst, false); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(dst)
	if err != nil || !bytes.Contains(encoded, []byte("ARTIST=甲")) || !bytes.Contains(encoded, []byte("TITLE=歌名")) || !bytes.Contains(encoded, cover) {
		t.Fatal("tags or cover missing")
	}
}

func TestRejectsFloatCompressedAndBrokenWAV(t *testing.T) {
	dir := t.TempDir()
	floatWAV := buildWAV(t, 1, 16, 44100, tone(1, 16), nil, false)
	binary.LittleEndian.PutUint16(floatWAV[20:], 3)
	if err := os.WriteFile(filepath.Join(dir, "float.wav"), floatWAV, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ConvertFile(filepath.Join(dir, "float.wav"), filepath.Join(dir, "float.flac"), false)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("浮点")) {
		t.Fatal(err)
	}
	broken := []byte("RIFFxxxxWAVEfmt ")
	if err := os.WriteFile(filepath.Join(dir, "bad.wav"), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertFile(filepath.Join(dir, "bad.wav"), filepath.Join(dir, "bad.flac"), false); err == nil {
		t.Fatal("broken wav was converted")
	}
	if _, err := os.Stat(filepath.Join(dir, "float.flac")); !os.IsNotExist(err) {
		t.Fatal("refused conversion left a file")
	}
}

func TestInterruptedWriteLeavesTheSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	body := buildWAV(t, 2, 16, 44100, tone(2, 300), nil, false)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "tone.flac")
	partial := dst + ".partial"
	if err := os.WriteFile(partial, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertFile(src, dst, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal("partial file remained")
	}
	got, err := os.ReadFile(src)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("source changed after a previous partial write")
	}
}

func appendChunk(body []byte, id, value string) []byte {
	raw := append([]byte(value), 0)
	body = append(body, []byte(id)...)
	body = append(body, put32(uint32(len(raw)))...)
	body = append(body, raw...)
	if len(raw)%2 == 1 {
		body = append(body, 0)
	}
	return body
}

func put32(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}

func id3Tag(frame []byte) []byte {
	body := make([]byte, 10+len(frame))
	copy(body, "ID3")
	body[3] = 3
	size := len(frame)
	body[6] = byte(size >> 21)
	body[7] = byte(size >> 14)
	body[8] = byte(size >> 7)
	body[9] = byte(size)
	copy(body[10:], frame)
	return body
}

func id3APICFrame(image []byte) []byte {
	var payload bytes.Buffer
	payload.WriteByte(0)
	payload.WriteString("image/jpeg")
	payload.WriteByte(0)
	payload.WriteByte(3)
	payload.WriteByte(0)
	payload.Write(image)
	frame := make([]byte, 10+payload.Len())
	copy(frame, "APIC")
	binary.BigEndian.PutUint32(frame[4:], uint32(payload.Len()))
	copy(frame[10:], payload.Bytes())
	return frame
}
