// Package wavflac converts integer PCM WAV to FLAC without ffmpeg.
//
// The encoder writes verbatim FLAC frames, so the samples are not
// resampled, truncated or requantized. Float, ADPCM and other compressed
// WAV files are refused. STREAMINFO carries the MD5 of the signed
// little-endian interleaved samples. LIST/INFO, ID3 and BWF text are mapped
// to Vorbis comments, and an ID3 front cover becomes a FLAC picture.
package wavflac

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Error is a refusal with a stable code. The source file is left unchanged.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func refuse(code, message string) error { return &Error{Code: code, Message: message} }

// Info describes integer PCM that can be encoded without changing samples.
type Info struct {
	Channels   int
	Bits       int
	Rate       int
	Samples    int64
	Extensible bool
	BlockAlign int
	DataOffset int64
	DataBytes  int64
}

// Tags are descriptive fields copied from the WAV. They are not a confirmed
// release identity.
type Tags struct {
	Artist    string
	Album     string
	Title     string
	Date      string
	Genre     string
	Track     string
	Comment   string
	Encoder   string
	Source    string
	Cover     []byte
	CoverMIME string
}

// Result is a finished conversion. Replayed is true when the destination
// already contained the same PCM and was left untouched.
type Result struct {
	MD5       string
	PCMSHA256 string
	Channels  int
	Bits      int
	Rate      int
	Samples   int64
	TagSource string
	Replayed  bool
}

var pcmGUID = []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}
var floatGUID = []byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}

// Inspect reads the WAV header and metadata chunks without loading samples.
func Inspect(path string) (Info, Tags, error) {
	file, err := os.Open(path)
	if err != nil {
		return Info{}, Tags{}, err
	}
	defer file.Close()
	return inspect(file)
}

func inspect(file *os.File) (Info, Tags, error) {
	var info Info
	var tags Tags
	head := make([]byte, 12)
	if _, err := io.ReadFull(file, head); err != nil || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return Info{}, Tags{}, refuse("NOT_WAV", "不是 WAV 文件，没有转换")
	}
	var sawFmt, sawData bool
	offset := int64(12)
	stat, err := file.Stat()
	if err != nil {
		return Info{}, Tags{}, err
	}
	limit := stat.Size()
	for offset+8 <= limit {
		var header [8]byte
		if _, err := file.ReadAt(header[:], offset); err != nil {
			return Info{}, Tags{}, refuse("BAD_WAV", "WAV 块不完整，没有转换")
		}
		id := string(header[0:4])
		size := int64(binary.LittleEndian.Uint32(header[4:8]))
		payload := offset + 8
		if payload+size > limit {
			return Info{}, Tags{}, refuse("BAD_WAV", "WAV 块超出文件，没有转换")
		}
		switch id {
		case "fmt ":
			if err := readFormat(file, payload, size, &info); err != nil {
				return Info{}, Tags{}, err
			}
			sawFmt = true
		case "data":
			info.DataOffset = payload
			info.DataBytes = size
			sawData = true
		case "LIST":
			readLIST(file, payload, size, &tags)
		case "id3 ", "ID3 ":
			readID3(file, payload, size, &tags)
		case "bext":
			readBext(file, payload, size, &tags)
		}
		offset = payload + size + size&1
	}
	if !sawFmt {
		return Info{}, Tags{}, refuse("BAD_WAV", "WAV 缺少 fmt 块，没有转换")
	}
	if !sawData || info.DataBytes <= 0 {
		return Info{}, Tags{}, refuse("BAD_WAV", "WAV 没有音频数据，没有转换")
	}
	if info.BlockAlign <= 0 || info.DataBytes%int64(info.BlockAlign) != 0 {
		return Info{}, Tags{}, refuse("BAD_WAV", "WAV 数据长度和声道宽度对不上，没有转换")
	}
	info.Samples = info.DataBytes / int64(info.BlockAlign)
	if info.Samples <= 0 {
		return Info{}, Tags{}, refuse("BAD_WAV", "WAV 没有音频数据，没有转换")
	}
	return info, tags, nil
}

func readFormat(file *os.File, offset, size int64, info *Info) error {
	if size < 16 {
		return refuse("BAD_WAV", "WAV 格式块太短，没有转换")
	}
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return refuse("BAD_WAV", "WAV 格式块不完整，没有转换")
	}
	format := binary.LittleEndian.Uint16(buf[0:2])
	info.Channels = int(binary.LittleEndian.Uint16(buf[2:4]))
	info.Rate = int(binary.LittleEndian.Uint32(buf[4:8]))
	info.BlockAlign = int(binary.LittleEndian.Uint16(buf[12:14]))
	info.Bits = int(binary.LittleEndian.Uint16(buf[14:16]))
	switch format {
	case 1:
	case 0xFFFE:
		info.Extensible = true
		if size < 40 {
			return refuse("BAD_WAV", "可扩展 WAV 格式块不完整，没有转换")
		}
		guid := buf[24:40]
		if bytes.Equal(guid, floatGUID) {
			return refuse("FLOAT_WAV", "浮点 WAV 不支持，没有降级转换")
		}
		if !bytes.Equal(guid, pcmGUID) {
			return refuse("COMPRESSED_WAV", "压缩或非整数 PCM WAV 不支持，没有转换")
		}
		valid := int(binary.LittleEndian.Uint16(buf[18:20]))
		if valid != 0 && valid != info.Bits {
			return refuse("BAD_WAV", "有效位宽与样本宽度不一致，没有降级")
		}
	case 3:
		return refuse("FLOAT_WAV", "浮点 WAV 不支持，没有降级转换")
	default:
		return refuse("COMPRESSED_WAV", "压缩或非整数 PCM WAV 不支持，没有转换")
	}
	if info.Channels < 1 || info.Channels > 8 {
		return refuse("CHANNELS", "FLAC 最多 8 声道，没有降级转换")
	}
	switch info.Bits {
	case 8, 16, 24, 32:
	default:
		return refuse("BAD_WAV", "只支持 8、16、24、32 位整数 PCM，没有降级")
	}
	if info.Rate < 1 || info.Rate > 655350 {
		return refuse("BAD_WAV", "采样率无法写入 FLAC，没有转换")
	}
	if info.BlockAlign != info.Channels*info.Bits/8 {
		return refuse("BAD_WAV", "WAV 块对齐和位宽不一致，没有转换")
	}
	return nil
}

func readLIST(file *os.File, offset, size int64, tags *Tags) {
	if size < 4 {
		return
	}
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return
	}
	if string(buf[:4]) != "INFO" {
		return
	}
	rest := buf[4:]
	for len(rest) >= 8 {
		id := string(rest[:4])
		n := int(binary.LittleEndian.Uint32(rest[4:8]))
		rest = rest[8:]
		if n < 0 || n > len(rest) {
			return
		}
		value := trimCString(string(rest[:n]))
		next := n + n&1
		if next > len(rest) {
			return
		}
		rest = rest[next:]
		if value == "" || id == "\x00\x00\x00\x00" {
			continue
		}
		setTag(tags, infoKey(id), value, "info")
	}
}

func infoKey(id string) string {
	switch id {
	case "IART":
		return "artist"
	case "INAM":
		return "title"
	case "IPRD", "IALB":
		return "album"
	case "ICRD", "ICRY":
		return "date"
	case "IGNR":
		return "genre"
	case "ICMT", "ISBJ":
		return "comment"
	case "ITRK", "IPRT":
		return "track"
	case "ISFT":
		return "encoder"
	default:
		return ""
	}
}

func readBext(file *os.File, offset, size int64, tags *Tags) {
	if size < 346 {
		return
	}
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return
	}
	setTag(tags, "comment", trimCString(string(buf[0:256])), "bext")
	setTag(tags, "encoder", trimCString(string(buf[256:288])), "bext")
	setTag(tags, "date", trimCString(string(buf[320:330])), "bext")
}

func setTag(tags *Tags, key, value, source string) {
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	switch key {
	case "artist":
		if tags.Artist == "" {
			tags.Artist = value
		}
	case "album":
		if tags.Album == "" {
			tags.Album = value
		}
	case "title":
		if tags.Title == "" {
			tags.Title = value
		}
	case "date":
		if tags.Date == "" {
			tags.Date = value
		}
	case "genre":
		if tags.Genre == "" {
			tags.Genre = value
		}
	case "track":
		if tags.Track == "" {
			tags.Track = value
		}
	case "comment":
		if tags.Comment == "" {
			tags.Comment = value
		}
	case "encoder":
		if tags.Encoder == "" {
			tags.Encoder = value
		}
	}
	if tags.Source == "" {
		tags.Source = source
	} else if !strings.Contains(tags.Source, source) {
		tags.Source += "+" + source
	}
}

func readID3(file *os.File, offset, size int64, tags *Tags) {
	if size < 10 {
		return
	}
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, offset); err != nil || string(buf[:3]) != "ID3" {
		return
	}
	version := buf[3]
	if version != 3 && version != 4 {
		return
	}
	body := buf[10:]
	total := syncsafe(buf[6:10])
	if total > len(body) {
		total = len(body)
	}
	body = body[:total]
	if buf[5]&0x40 != 0 && len(body) >= 4 {
		ext := syncsafe(body[:4])
		if version == 4 {
			ext = syncsafe(body[:4])
		}
		if ext <= len(body) {
			body = body[ext:]
		}
	}
	for len(body) >= 10 {
		id := string(body[:4])
		if id == "\x00\x00\x00\x00" {
			return
		}
		var frame int
		if version == 4 {
			frame = syncsafe(body[4:8])
		} else {
			frame = int(binary.BigEndian.Uint32(body[4:8]))
		}
		body = body[10:]
		if frame < 0 || frame > len(body) {
			return
		}
		data := body[:frame]
		body = body[frame:]
		switch id {
		case "TIT2":
			setTag(tags, "title", id3Text(data), "id3")
		case "TPE1":
			setTag(tags, "artist", id3Text(data), "id3")
		case "TALB":
			setTag(tags, "album", id3Text(data), "id3")
		case "TYER", "TDRC":
			setTag(tags, "date", id3Text(data), "id3")
		case "TRCK":
			setTag(tags, "track", id3Text(data), "id3")
		case "TCON":
			setTag(tags, "genre", id3Text(data), "id3")
		case "COMM":
			setTag(tags, "comment", id3Comm(data), "id3")
		case "APIC":
			if tags.Cover == nil {
				if mime, picture := id3APIC(data); len(picture) > 0 {
					tags.Cover = picture
					tags.CoverMIME = mime
					if tags.Source == "" {
						tags.Source = "id3"
					}
				}
			}
		}
	}
}

func syncsafe(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

func id3Text(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	switch data[0] {
	case 0:
		return trimCString(string(data[1:]))
	case 3:
		return trimCString(string(data[1:]))
	case 1:
		return trimCString(utf16Decode(data[1:], true))
	case 2:
		return trimCString(utf16Decode(data[1:], false))
	default:
		return ""
	}
}

func id3Comm(data []byte) string {
	if len(data) < 5 {
		return ""
	}
	rest := data[4:]
	switch data[0] {
	case 0, 3:
		if i := bytes.IndexByte(rest, 0); i >= 0 {
			rest = rest[i+1:]
		}
		return trimCString(string(rest))
	default:
		return id3Text(data)
	}
}

func id3APIC(data []byte) (string, []byte) {
	if len(data) < 4 {
		return "", nil
	}
	rest := data[1:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", nil
	}
	mime := string(rest[:end])
	rest = rest[end+1:]
	if len(rest) < 2 {
		return "", nil
	}
	rest = rest[1:]
	if data[0] == 1 || data[0] == 2 {
		if len(rest) >= 2 && (rest[0] == 0xff || rest[0] == 0xfe) {
			rest = rest[2:]
		}
		for i := 0; i+1 < len(rest); i += 2 {
			if rest[i] == 0 && rest[i+1] == 0 {
				rest = rest[i+2:]
				break
			}
		}
	} else if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = rest[i+1:]
	}
	if len(rest) < 32 {
		return "", nil
	}
	if mime == "" {
		if rest[0] == 0xff && rest[1] == 0xd8 {
			mime = "image/jpeg"
		} else if len(rest) >= 8 && string(rest[:8]) == "\x89PNG\r\n\x1a\n" {
			mime = "image/png"
		}
	}
	return mime, append([]byte(nil), rest...)
}

func utf16Decode(data []byte, bom bool) string {
	if bom && len(data) >= 2 && data[0] == 0xfe && data[1] == 0xff {
		return utf16BE(data[2:])
	}
	if bom && len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe {
		return utf16LE(data[2:])
	}
	if bom {
		return utf16LE(data)
	}
	return utf16BE(data)
}

func utf16LE(data []byte) string {
	if len(data)%2 == 1 {
		data = data[:len(data)-1]
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return string(utf16.Decode(units))
}

func utf16BE(data []byte) string {
	if len(data)%2 == 1 {
		data = data[:len(data)-1]
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.BigEndian.Uint16(data[i*2:])
	}
	return string(utf16.Decode(units))
}

func trimCString(value string) string {
	return strings.TrimSpace(strings.TrimRight(value, "\x00"))
}

// ConvertFile writes a verified FLAC beside the source. The source is not
// modified. An existing destination is reused only when its PCM matches.
func ConvertFile(src, dst string, fillFromPath bool) (Result, error) {
	info, tags, err := Inspect(src)
	if err != nil {
		return Result{}, err
	}
	if fillFromPath {
		mergePathHints(src, &tags)
	}
	if _, err := os.Stat(dst); err == nil {
		if same, cmpErr := samePCM(src, dst); cmpErr == nil && same {
			sum, hashErr := filePCMHash(src)
			if hashErr != nil {
				return Result{}, hashErr
			}
			return Result{MD5: sum.md5, PCMSHA256: sum.sha, Channels: info.Channels, Bits: info.Bits, Rate: info.Rate, Samples: info.Samples, TagSource: tags.Source, Replayed: true}, nil
		}
		return Result{}, refuse("CONFLICT", "目标 FLAC 已存在且内容不同，没有覆盖")
	}
	temp := dst + ".partial"
	_ = os.Remove(temp)
	defer os.Remove(temp)
	sum, err := writeFLAC(src, temp, info, tags)
	if err != nil {
		return Result{}, err
	}
	if err := verifyFLAC(src, temp, info); err != nil {
		return Result{}, err
	}
	if err := os.Rename(temp, dst); err != nil {
		return Result{}, err
	}
	if st, err := os.Stat(src); err == nil {
		_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	return Result{MD5: sum.md5, PCMSHA256: sum.sha, Channels: info.Channels, Bits: info.Bits, Rate: info.Rate, Samples: info.Samples, TagSource: tags.Source}, nil
}

type digests struct{ md5, sha string }

func filePCMHash(path string) (digests, error) {
	info, _, err := Inspect(path)
	if err != nil {
		return digests{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return digests{}, err
	}
	defer file.Close()
	md := md5.New()
	sh := sha256.New()
	if err := streamPCM(file, info, func(block []byte) error {
		_, _ = md.Write(block)
		_, _ = sh.Write(block)
		return nil
	}); err != nil {
		return digests{}, err
	}
	return digests{md5: hex.EncodeToString(md.Sum(nil)), sha: hex.EncodeToString(sh.Sum(nil))}, nil
}

// PCMHash is the SHA-256 of the signed little-endian samples. A FLAC path
// is decoded first, so a WAV and the FLAC made from it hash the same.
func PCMHash(path string) (string, error) {
	if strings.EqualFold(filepath.Ext(path), ".flac") {
		return flacPCMHash(path)
	}
	sum, err := filePCMHash(path)
	if err != nil {
		return "", err
	}
	return sum.sha, nil
}

func flacPCMHash(path string) (string, error) {
	decoded, err := decodePCM(path)
	if err != nil {
		return "", err
	}
	sh := sha256.New()
	width := decoded.bits / 8
	buf := make([]byte, width)
	for _, sample := range decoded.samples {
		writeCanonical(buf, sample, decoded.bits)
		_, _ = sh.Write(buf)
	}
	return hex.EncodeToString(sh.Sum(nil)), nil
}

func writeCanonical(dst []byte, sample int32, bits int) {
	switch bits {
	case 8:
		dst[0] = byte(int8(sample))
	case 16:
		binary.LittleEndian.PutUint16(dst, uint16(int16(sample)))
	case 24:
		u := uint32(sample)
		dst[0] = byte(u)
		dst[1] = byte(u >> 8)
		dst[2] = byte(u >> 16)
	default:
		binary.LittleEndian.PutUint32(dst, uint32(sample))
	}
}

func samePCM(wavPath, flacPath string) (bool, error) {
	info, _, err := Inspect(wavPath)
	if err != nil {
		return false, err
	}
	return verifyFLAC(wavPath, flacPath, info) == nil, nil
}

func mergePathHints(path string, tags *Tags) {
	hint := pathHint(path)
	filled := false
	if tags.Title == "" && hint.Title != "" {
		tags.Title = hint.Title
		filled = true
	}
	if tags.Artist == "" && hint.Artist != "" {
		tags.Artist = hint.Artist
		filled = true
	}
	if tags.Album == "" && hint.Album != "" {
		tags.Album = hint.Album
		filled = true
	}
	if tags.Track == "" && hint.Track != "" {
		tags.Track = hint.Track
		filled = true
	}
	if filled && tags.Source == "" {
		tags.Source = "path"
	} else if filled && !strings.Contains(tags.Source, "path") {
		tags.Source += "+path"
	}
}

func pathHint(path string) Tags {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	title := base
	track := ""
	if i := strings.IndexAny(base, " -."); i > 0 && i <= 3 {
		if n, err := strconv.Atoi(base[:i]); err == nil && n > 0 && n < 1000 {
			track = strconv.Itoa(n)
			title = strings.TrimLeft(base[i+1:], " .-")
		}
	}
	parent := filepath.Base(filepath.Dir(path))
	grand := filepath.Base(filepath.Dir(filepath.Dir(path)))
	var album, artist string
	if folderOK(parent) && folderOK(grand) {
		album, artist = parent, grand
	}
	if !folderOK(title) {
		title = ""
	}
	return Tags{Title: title, Album: album, Artist: artist, Track: track}
}

func folderOK(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "incoming", "recycle", "library-audio", "state", "native", "musicops", "wav":
		return false
	default:
		return true
	}
}

func writeFLAC(src, dst string, info Info, tags Tags) (digests, error) {
	in, err := os.Open(src)
	if err != nil {
		return digests{}, err
	}
	defer in.Close()
	if _, err := in.Seek(info.DataOffset, io.SeekStart); err != nil {
		return digests{}, err
	}
	frames, err := os.CreateTemp(filepath.Dir(dst), ".wavflac-frames-*")
	if err != nil {
		return digests{}, err
	}
	frameName := frames.Name()
	defer os.Remove(frameName)
	md := md5.New()
	sh := sha256.New()
	var sizes []int
	const block = 4096
	left := info.Samples
	frameNo := 0
	minBlock, maxBlock := int(info.Samples), 0
	if minBlock > block {
		minBlock = block
	}
	for left > 0 {
		n := int64(block)
		if n > left {
			n = left
		}
		raw, err := readPCMBlock(in, info, n, md, sh)
		if err != nil {
			frames.Close()
			return digests{}, err
		}
		encoded, err := encodeFrame(info, frameNo, raw)
		if err != nil {
			frames.Close()
			return digests{}, err
		}
		if _, err := frames.Write(encoded); err != nil {
			frames.Close()
			return digests{}, err
		}
		sizes = append(sizes, len(encoded))
		if int(n) < minBlock {
			minBlock = int(n)
		}
		if int(n) > maxBlock {
			maxBlock = int(n)
		}
		left -= n
		frameNo++
	}
	if err := frames.Close(); err != nil {
		return digests{}, err
	}
	minFrame, maxFrame := sizes[0], sizes[0]
	for _, size := range sizes {
		if size < minFrame {
			minFrame = size
		}
		if size > maxFrame {
			maxFrame = size
		}
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return digests{}, err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(dst)
		}
	}()
	if err := writeMetadata(out, info, tags, minBlock, maxBlock, minFrame, maxFrame, md.Sum(nil)); err != nil {
		return digests{}, err
	}
	frameFile, err := os.Open(frameName)
	if err != nil {
		return digests{}, err
	}
	defer frameFile.Close()
	if _, err := io.Copy(out, frameFile); err != nil {
		return digests{}, err
	}
	if err := out.Close(); err != nil {
		return digests{}, err
	}
	if len(tags.Cover) > 0 {
		body, err := os.ReadFile(dst)
		if err != nil {
			return digests{}, err
		}
		updated, err := embedCover(body, tags.Cover, tags.CoverMIME)
		if err != nil {
			return digests{}, err
		}
		if err := os.WriteFile(dst, updated, 0o600); err != nil {
			return digests{}, err
		}
	}
	ok = true
	return digests{md5: hex.EncodeToString(md.Sum(nil)), sha: hex.EncodeToString(sh.Sum(nil))}, nil
}

func readPCMBlock(file *os.File, info Info, count int64, md, sh hash.Hash) ([]int32, error) {
	bytesPer := info.Bits / 8
	raw := make([]byte, int(count)*info.BlockAlign)
	if _, err := io.ReadFull(file, raw); err != nil {
		return nil, refuse("BAD_WAV", "WAV 音频数据不完整，没有转换")
	}
	out := make([]int32, int(count)*info.Channels)
	canon := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i += bytesPer {
		sample, encoded := decodeSample(raw[i:i+bytesPer], info.Bits)
		out[i/bytesPer] = sample
		canon = append(canon, encoded...)
	}
	_, _ = md.Write(canon)
	_, _ = sh.Write(canon)
	return out, nil
}

func decodeSample(raw []byte, bits int) (int32, []byte) {
	switch bits {
	case 8:
		signed := int32(int8(raw[0] - 128))
		return signed, []byte{byte(int8(signed))}
	case 16:
		return int32(int16(binary.LittleEndian.Uint16(raw))), append([]byte(nil), raw...)
	case 24:
		v := int32(raw[0]) | int32(raw[1])<<8 | int32(raw[2])<<16
		if v&0x800000 != 0 {
			v |= ^0xffffff
		}
		return v, append([]byte(nil), raw...)
	default:
		return int32(binary.LittleEndian.Uint32(raw)), append([]byte(nil), raw...)
	}
}

func streamPCM(file *os.File, info Info, fn func([]byte) error) error {
	if _, err := file.Seek(info.DataOffset, io.SeekStart); err != nil {
		return err
	}
	left := info.DataBytes
	buf := make([]byte, info.BlockAlign*1024)
	for left > 0 {
		n := int64(len(buf))
		if n > left {
			n = left
		}
		if _, err := io.ReadFull(file, buf[:n]); err != nil {
			return err
		}
		canon := make([]byte, 0, n)
		bps := info.Bits / 8
		for i := int64(0); i < n; i += int64(bps) {
			_, encoded := decodeSample(buf[i:i+int64(bps)], info.Bits)
			canon = append(canon, encoded...)
		}
		if err := fn(canon); err != nil {
			return err
		}
		left -= n
	}
	return nil
}

func writeMetadata(w io.Writer, info Info, tags Tags, minBlock, maxBlock, minFrame, maxFrame int, md5sum []byte) error {
	comment := vorbisComment(tags)
	if _, err := w.Write([]byte("fLaC")); err != nil {
		return err
	}
	stream := streamInfo(info, minBlock, maxBlock, minFrame, maxFrame, md5sum)
	last := len(comment) == 0
	if err := writeBlock(w, 0, stream, last); err != nil {
		return err
	}
	if len(comment) > 0 {
		if err := writeBlock(w, 4, comment, true); err != nil {
			return err
		}
	}
	return nil
}

func streamInfo(info Info, minBlock, maxBlock, minFrame, maxFrame int, sum []byte) []byte {
	buf := make([]byte, 34)
	binary.BigEndian.PutUint16(buf[0:2], uint16(minBlock))
	binary.BigEndian.PutUint16(buf[2:4], uint16(maxBlock))
	putUint24(buf[4:7], uint32(minFrame))
	putUint24(buf[7:10], uint32(maxFrame))
	packed := uint64(info.Rate) << 44
	packed |= uint64(info.Channels-1) << 41
	packed |= uint64(info.Bits-1) << 36
	packed |= uint64(info.Samples) & ((1 << 36) - 1)
	binary.BigEndian.PutUint64(buf[10:18], packed)
	copy(buf[18:34], sum)
	return buf
}

func putUint24(dst []byte, v uint32) {
	dst[0] = byte(v >> 16)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v)
}

func vorbisComment(tags Tags) []byte {
	pairs := [][2]string{}
	add := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			pairs = append(pairs, [2]string{key, value})
		}
	}
	add("TITLE", tags.Title)
	add("ARTIST", tags.Artist)
	add("ALBUM", tags.Album)
	add("DATE", tags.Date)
	add("GENRE", tags.Genre)
	add("TRACKNUMBER", tags.Track)
	add("COMMENT", tags.Comment)
	add("ENCODER", tags.Encoder)
	if tags.Source != "" {
		add("MUSICOPS_TAG_SOURCE", tags.Source)
	}
	vendor := "MusicOps wavflac"
	var buf bytes.Buffer
	writeLE := func(b []byte) {
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(b)))
		buf.Write(n[:])
		buf.Write(b)
	}
	writeLE([]byte(vendor))
	var count [4]byte
	binary.LittleEndian.PutUint32(count[:], uint32(len(pairs)))
	buf.Write(count[:])
	for _, pair := range pairs {
		writeLE([]byte(pair[0] + "=" + pair[1]))
	}
	return buf.Bytes()
}

func writeBlock(w io.Writer, typ byte, payload []byte, last bool) error {
	header := typ & 0x7f
	if last {
		header |= 0x80
	}
	if _, err := w.Write([]byte{header, byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func encodeFrame(info Info, number int, samples []int32) ([]byte, error) {
	perChannel := len(samples) / info.Channels
	if perChannel == 0 || len(samples)%info.Channels != 0 {
		return nil, refuse("BAD_WAV", "WAV 样本数和声道对不上，没有转换")
	}
	var header bytes.Buffer
	header.WriteByte(0xff)
	header.WriteByte(0xf8)
	blockCode := blockSizeCode(perChannel)
	header.WriteByte(byte(blockCode << 4))
	header.WriteByte(byte((info.Channels-1)<<4 | sampleSizeCode(info.Bits)<<1))
	header.Write(utf8Number(number))
	if blockCode == 7 {
		var extra [2]byte
		binary.BigEndian.PutUint16(extra[:], uint16(perChannel-1))
		header.Write(extra[:])
	}
	crc := crc8(header.Bytes())
	header.WriteByte(crc)
	bits := newBitWriter()
	for ch := 0; ch < info.Channels; ch++ {
		bits.write(1, 7) // 0 + verbatim type 000001
		bits.write(0, 1) // no wasted bits
		for i := 0; i < perChannel; i++ {
			bits.write(uint64(uint32(samples[i*info.Channels+ch])), info.Bits)
		}
	}
	bits.align()
	body := append(header.Bytes(), bits.bytes()...)
	sum := crc16(body)
	var tail [2]byte
	binary.BigEndian.PutUint16(tail[:], sum)
	return append(body, tail[:]...), nil
}

func utf8Number(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	if n < 0x800 {
		return []byte{0xc0 | byte(n>>6), 0x80 | byte(n&0x3f)}
	}
	if n < 0x10000 {
		return []byte{0xe0 | byte(n>>12), 0x80 | byte((n>>6)&0x3f), 0x80 | byte(n&0x3f)}
	}
	return []byte{0xf0 | byte(n>>18), 0x80 | byte((n>>12)&0x3f), 0x80 | byte((n>>6)&0x3f), 0x80 | byte(n&0x3f)}
}

func readUTF8Number(frame []byte, pos int) (int, int, error) {
	if pos >= len(frame) {
		return 0, pos, io.ErrUnexpectedEOF
	}
	b := frame[pos]
	width := 1
	switch {
	case b < 0x80:
		return int(b), pos + 1, nil
	case b&0xe0 == 0xc0:
		width = 2
	case b&0xf0 == 0xe0:
		width = 3
	case b&0xf8 == 0xf0:
		width = 4
	default:
		return 0, pos, io.ErrUnexpectedEOF
	}
	if pos+width > len(frame) {
		return 0, pos, io.ErrUnexpectedEOF
	}
	n := int(b & (0xff >> (width + 1)))
	for i := 1; i < width; i++ {
		if frame[pos+i]&0xc0 != 0x80 {
			return 0, pos, io.ErrUnexpectedEOF
		}
		n = (n << 6) | int(frame[pos+i]&0x3f)
	}
	return n, pos + width, nil
}

func blockSizeCode(n int) int {
	switch n {
	case 192:
		return 1
	case 576:
		return 2
	case 1152:
		return 3
	case 2304:
		return 4
	case 4608:
		return 5
	case 256:
		return 8
	case 512:
		return 9
	case 1024:
		return 10
	case 2048:
		return 11
	case 4096:
		return 12
	case 8192:
		return 13
	case 16384:
		return 14
	case 32768:
		return 15
	default:
		return 7
	}
}

func sampleSizeCode(bits int) int {
	switch bits {
	case 8:
		return 1
	case 16:
		return 4
	case 24:
		return 6
	case 32:
		return 7
	default:
		return 0
	}
}

type bitWriter struct {
	buf []byte
	acc uint64
	n   int
}

func newBitWriter() *bitWriter { return &bitWriter{} }

func (b *bitWriter) write(v uint64, bits int) {
	for bits > 0 {
		take := bits
		if take > 32 {
			take = 32
		}
		shift := bits - take
		part := (v >> shift) & ((uint64(1) << take) - 1)
		b.acc = (b.acc << take) | part
		b.n += take
		bits -= take
		for b.n >= 8 {
			b.n -= 8
			b.buf = append(b.buf, byte(b.acc>>uint(b.n)))
			if b.n == 0 {
				b.acc = 0
			} else {
				b.acc &= (uint64(1) << uint(b.n)) - 1
			}
		}
	}
}

func (b *bitWriter) align() {
	if b.n > 0 {
		b.write(0, 8-b.n)
	}
}

func (b *bitWriter) bytes() []byte { return b.buf }

func crc8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func crc16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func verifyFLAC(wavPath, flacPath string, info Info) error {
	decoded, err := decodePCM(flacPath)
	if err != nil {
		return err
	}
	if decoded.channels != info.Channels || decoded.bits != info.Bits || decoded.rate != info.Rate {
		return refuse("VERIFY", "转码后的声道、位宽或采样率和源文件不一致")
	}
	file, err := os.Open(wavPath)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(info.DataOffset, io.SeekStart); err != nil {
		return err
	}
	pos := 0
	left := info.Samples
	for left > 0 {
		n := int64(4096)
		if n > left {
			n = left
		}
		raw, err := readPCMBlock(file, info, n, noopHash{}, noopHash{})
		if err != nil {
			return err
		}
		if pos+len(raw) > len(decoded.samples) {
			return refuse("VERIFY", "转码前后 PCM 样本数量不一致")
		}
		for i, sample := range raw {
			if decoded.samples[pos+i] != sample {
				return refuse("VERIFY", "转码前后 PCM 样本不一致，没有替换原文件")
			}
		}
		pos += len(raw)
		left -= n
	}
	if pos != len(decoded.samples) {
		return refuse("VERIFY", "转码前后 PCM 样本数量不一致")
	}
	body, err := os.ReadFile(flacPath)
	if err != nil {
		return err
	}
	sum := md5.New()
	file.Seek(info.DataOffset, io.SeekStart)
	if err := streamPCM(file, info, func(block []byte) error {
		_, _ = sum.Write(block)
		return nil
	}); err != nil {
		return err
	}
	want := sum.Sum(nil)
	got := streamMD5(body)
	if got == nil || !bytes.Equal(got, want) {
		return refuse("VERIFY", "FLAC 里的 MD5 和 PCM 不一致，没有替换原文件")
	}
	return nil
}

type noopHash struct{}

func (noopHash) Write(p []byte) (int, error) { return len(p), nil }
func (noopHash) Sum(b []byte) []byte         { return b }
func (noopHash) Reset()                      {}
func (noopHash) Size() int                   { return 0 }
func (noopHash) BlockSize() int              { return 0 }

type decodedAudio struct {
	channels int
	bits     int
	rate     int
	samples  []int32
}

func decodePCM(path string) (decodedAudio, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return decodedAudio{}, err
	}
	if len(body) < 4 || string(body[:4]) != "fLaC" {
		return decodedAudio{}, refuse("VERIFY", "转码输出不是 FLAC")
	}
	pos := 4
	var audio decodedAudio
	var md5sum []byte
	for pos+4 <= len(body) {
		header := body[pos]
		last := header&0x80 != 0
		typ := header & 0x7f
		n := int(body[pos+1])<<16 | int(body[pos+2])<<8 | int(body[pos+3])
		pos += 4
		if pos+n > len(body) {
			return decodedAudio{}, refuse("VERIFY", "FLAC 元数据不完整")
		}
		block := body[pos : pos+n]
		pos += n
		if typ == 0 && len(block) == 34 {
			v := binary.BigEndian.Uint64(block[10:18])
			audio.rate = int(v >> 44)
			audio.channels = int((v>>41)&0x7) + 1
			audio.bits = int((v>>36)&0x1f) + 1
			md5sum = append([]byte(nil), block[18:34]...)
		}
		if last {
			break
		}
	}
	if audio.channels == 0 || audio.bits == 0 {
		return decodedAudio{}, refuse("VERIFY", "FLAC 缺少流信息")
	}
	frame := 0
	for pos < len(body) {
		next, samples, err := decodeFrame(body[pos:], audio, frame)
		if err != nil {
			return decodedAudio{}, err
		}
		audio.samples = append(audio.samples, samples...)
		pos += next
		frame++
	}
	_ = md5sum
	return audio, nil
}

func streamMD5(body []byte) []byte {
	if len(body) < 42 || string(body[:4]) != "fLaC" {
		return nil
	}
	n := int(body[5])<<16 | int(body[6])<<8 | int(body[7])
	if body[4]&0x7f != 0 || 8+n > len(body) || n < 34 {
		return nil
	}
	return body[8+18 : 8+34]
}

func decodeFrame(frame []byte, info decodedAudio, number int) (int, []int32, error) {
	if len(frame) < 6 || frame[0] != 0xff || frame[1] != 0xf8 {
		return 0, nil, refuse("VERIFY", "FLAC 帧头无法读取")
	}
	blockCode := int(frame[2] >> 4)
	channels := int(frame[3]>>4) + 1
	bitsCode := int(frame[3]>>1) & 7
	if channels != info.channels || sampleSizeCode(info.bits) != bitsCode {
		return 0, nil, refuse("VERIFY", "FLAC 帧和流信息不一致")
	}
	gotNumber, pos, err := readUTF8Number(frame, 4)
	if err != nil || gotNumber != number {
		return 0, nil, refuse("VERIFY", "FLAC 帧序号不连续")
	}
	blockSize := blockSizeFromCode(blockCode)
	if blockCode == 7 {
		if pos+2 > len(frame) {
			return 0, nil, refuse("VERIFY", "FLAC 帧头无法读取")
		}
		blockSize = int(binary.BigEndian.Uint16(frame[pos:pos+2])) + 1
		pos += 2
	}
	if crc8(frame[:pos]) != frame[pos] {
		return 0, nil, refuse("VERIFY", "FLAC 帧头校验失败")
	}
	pos++
	reader := newBitReader(frame[pos:])
	out := make([]int32, blockSize*channels)
	for ch := 0; ch < channels; ch++ {
		kind, err := reader.read(7)
		if err != nil || kind != 1 {
			return 0, nil, refuse("VERIFY", "只接受未压缩的 FLAC 子帧")
		}
		wasted, err := reader.read(1)
		if err != nil || wasted != 0 {
			return 0, nil, refuse("VERIFY", "FLAC 子帧无法读取")
		}
		for i := 0; i < blockSize; i++ {
			raw, err := reader.read(info.bits)
			if err != nil {
				return 0, nil, refuse("VERIFY", "FLAC 样本无法读取")
			}
			out[i*channels+ch] = signExtend(raw, info.bits)
		}
	}
	reader.align()
	used := pos + reader.bytesUsed()
	if used+2 > len(frame) {
		return 0, nil, refuse("VERIFY", "FLAC 帧校验缺失")
	}
	if crc16(frame[:used]) != binary.BigEndian.Uint16(frame[used:used+2]) {
		return 0, nil, refuse("VERIFY", "FLAC 帧校验失败")
	}
	return used + 2, out, nil
}

func blockSizeFromCode(code int) int {
	switch code {
	case 1:
		return 192
	case 2, 3, 4, 5:
		return 576 << (code - 2)
	case 8, 9, 10, 11, 12, 13, 14, 15:
		return 256 << (code - 8)
	default:
		return 0
	}
}

func signExtend(v uint64, bits int) int32 {
	shift := 32 - bits
	return int32(uint32(v)<<uint(shift)) >> uint(shift)
}

type bitReader struct {
	data []byte
	acc  uint64
	n    int
	used int
}

func newBitReader(data []byte) *bitReader { return &bitReader{data: data} }

func (b *bitReader) read(bits int) (uint64, error) {
	for b.n < bits {
		if b.used >= len(b.data) {
			return 0, io.ErrUnexpectedEOF
		}
		b.acc = (b.acc << 8) | uint64(b.data[b.used])
		b.used++
		b.n += 8
	}
	b.n -= bits
	return (b.acc >> uint(b.n)) & ((uint64(1) << uint(bits)) - 1), nil
}

func (b *bitReader) align() {
	if b.n%8 != 0 {
		drop := b.n % 8
		b.n -= drop
		b.acc &= (uint64(1) << uint(b.n)) - 1
	}
	b.n = 0
	b.acc = 0
}

func (b *bitReader) bytesUsed() int { return b.used }

func embedCover(data, image []byte, mime string) ([]byte, error) {
	if mime == "" {
		if len(image) >= 2 && image[0] == 0xff && image[1] == 0xd8 {
			mime = "image/jpeg"
		} else if len(image) >= 8 && string(image[:8]) == "\x89PNG\r\n\x1a\n" {
			mime = "image/png"
		} else {
			return nil, refuse("BAD_WAV", "WAV 内嵌封面不是 JPEG 或 PNG，没有转换")
		}
	}
	payload := picturePayload(mime, image)
	if len(data) < 4 || string(data[:4]) != "fLaC" {
		return nil, refuse("VERIFY", "转码输出不是 FLAC")
	}
	blocks := []struct {
		typ  byte
		data []byte
	}{}
	pos := 4
	for pos+4 <= len(data) {
		header := data[pos]
		last := header&0x80 != 0
		typ := header & 0x7f
		n := int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4
		if pos+n > len(data) {
			return nil, refuse("VERIFY", "FLAC 元数据不完整")
		}
		blocks = append(blocks, struct {
			typ  byte
			data []byte
		}{typ, append([]byte(nil), data[pos:pos+n]...)})
		pos += n
		if last {
			break
		}
	}
	audio := append([]byte(nil), data[pos:]...)
	blocks = append(blocks, struct {
		typ  byte
		data []byte
	}{6, payload})
	var out bytes.Buffer
	out.WriteString("fLaC")
	for i, block := range blocks {
		if err := writeBlock(&out, block.typ, block.data, i == len(blocks)-1); err != nil {
			return nil, err
		}
	}
	out.Write(audio)
	return out.Bytes(), nil
}

func picturePayload(mime string, image []byte) []byte {
	desc := "MusicOps front cover"
	var buf bytes.Buffer
	put := func(v uint32) {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], v)
		buf.Write(b[:])
	}
	put(3)
	put(uint32(len(mime)))
	buf.WriteString(mime)
	put(uint32(len(desc)))
	buf.WriteString(desc)
	put(0)
	put(0)
	put(24)
	put(0)
	put(uint32(len(image)))
	buf.Write(image)
	return buf.Bytes()
}

// ReadDescription returns tags for a WAV without converting it.
func ReadDescription(path string) (Tags, error) {
	_, tags, err := Inspect(path)
	return tags, err
}

// FormatMD5 reports the STREAMINFO MD5 of a FLAC file.
func FormatMD5(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := streamMD5(body)
	if sum == nil {
		return "", fmt.Errorf("没有读到 FLAC MD5")
	}
	return hex.EncodeToString(sum), nil
}
