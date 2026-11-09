// Package flactags reads and writes Vorbis comment tags in FLAC files.
//
// It parses the FLAC metadata block chain, preserves every non-comment
// block byte-for-byte, and never touches the audio frames that follow the
// metadata. Tags are written by replacing the existing VORBIS_COMMENT block
// (or inserting one directly after STREAMINFO when none exists) and the
// result is always written through a temporary file plus rename, so a
// failed write never truncates or corrupts the source file.
//
// Only the standard library is used.
package flactags

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FLAC metadata block types (SPEC: https://xiph.org/flac/format.html).
const (
	blockStreamInfo    = 0
	blockPadding       = 1
	blockApplication   = 2
	blockSeekTable     = 3
	blockVorbisComment = 4
	blockCueSheet      = 5
	blockPicture       = 6
	blockReservedMin   = 7
	blockReservedMax   = 126
	blockInvalid       = 127
)

const (
	streamInfoSize = 34
	flacMagic      = "fLaC"
	// defaultVendor is written when a file has no VORBIS_COMMENT block yet
	// (and therefore no vendor string to preserve).
	defaultVendor = "MusicOps"
)

// StreamInfo is the decoded STREAMINFO block (always the first block).
type StreamInfo struct {
	MinBlockSize  uint16
	MaxBlockSize  uint16
	MinFrameSize  uint32
	MaxFrameSize  uint32
	SampleRate    uint32 // Hz; 0 means not recorded in the stream
	Channels      int
	BitsPerSample int
	TotalSamples  uint64 // 0 means unknown
}

// Tags is the decoded tag set of a FLAC file.
//
// The named fields are conveniences over Raw. Read populates both: every
// Vorbis comment key lands in Raw (canonical upper-case key -> values in
// file order) and the well-known keys also populate the named fields
// (first value wins). On Write, Raw — when non-nil — is the source of
// truth and the named fields overlay it: a non-empty named field replaces
// that key in Raw, Artists (when non-empty) replaces ARTIST, and a
// positive TrackNumber replaces TRACKNUMBER. An empty named field never
// deletes a key present in Raw; to delete a key, omit it from Raw and
// leave the corresponding field empty. When Raw is nil, the named fields
// alone define the written comment block.
type Tags struct {
	Title       string
	Artist      string   // first ARTIST value; use Artists for multi-value
	Artists     []string // all ARTIST values; overrides Artist on Write when non-empty
	Album       string
	AlbumArtist string
	TrackNumber int // 0 means absent
	Date        string
	Genre       string
	Lyrics      string

	StreamInfo StreamInfo

	// Raw holds every Vorbis comment key, canonical upper-case key ->
	// values in file order. It is never nil after a successful Read.
	Raw map[string][]string
}

// block is one metadata block: its type and its payload bytes (excluding
// the 4-byte block header). Payloads are kept verbatim.
type block struct {
	typ  byte
	data []byte
}

// parsedFile is a fully parsed FLAC file: metadata blocks plus the
// untouched audio bytes that follow them.
type parsedFile struct {
	blocks     []block
	commentIdx int // index into blocks of the VORBIS_COMMENT block, or -1
	vendor     string
	entries    map[string][]string // decoded comment entries, nil when no comment block
	streamInfo StreamInfo
	audio      []byte // every byte after the metadata chain, verbatim
}

// ReadFile reads the tags of the FLAC file at path.
func ReadFile(path string) (*Tags, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("flactags: read %s: %w", path, err)
	}
	return ReadBytes(data)
}

// ReadBytes decodes the tags of an in-memory FLAC file.
func ReadBytes(data []byte) (*Tags, error) {
	pf, err := parseFLAC(data)
	if err != nil {
		return nil, err
	}
	return pf.tags(), nil
}

// Write rewrites the tags of the FLAC file at path in place. The file is
// written through a temporary file in the same directory plus rename, so
// the original is untouched if writing fails.
func Write(path string, tags Tags) error {
	return WriteTo(path, path, tags)
}

// WriteTo reads the FLAC file at src, rewrites its tags, and writes the
// result to dst. When dst differs from src and dst already exists, WriteTo
// refuses rather than overwriting a different file. When dst == src the
// file is updated in place via a temporary file plus rename.
func WriteTo(src, dst string, tags Tags) error {
	if dst != src {
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("flactags: destination %s already exists", dst)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("flactags: stat destination %s: %w", dst, err)
		}
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("flactags: read %s: %w", src, err)
	}
	out, err := WriteBytes(data, tags)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, out, src)
}

// WriteBytes returns a copy of the FLAC file in data with its tags replaced
// by tags. Every non-comment metadata block and every audio byte is
// reproduced verbatim; only the VORBIS_COMMENT block changes (it is
// replaced in place, or inserted directly after STREAMINFO when absent).
func WriteBytes(data []byte, tags Tags) ([]byte, error) {
	pf, err := parseFLAC(data)
	if err != nil {
		return nil, err
	}
	entries, err := buildEntries(pf.entries, tags)
	if err != nil {
		return nil, err
	}
	vendor := pf.vendor
	if vendor == "" {
		vendor = defaultVendor
	}
	payload, err := EncodeVorbisComment(vendor, entries)
	if err != nil {
		return nil, err
	}

	comment := block{typ: blockVorbisComment, data: payload}
	blocks := make([]block, 0, len(pf.blocks)+1)
	for i, b := range pf.blocks {
		if i == pf.commentIdx {
			continue // drop the old comment block; the new one is inserted below
		}
		blocks = append(blocks, b)
		if i == 0 {
			// STREAMINFO is always block 0; the comment block goes right after it.
			blocks = append(blocks, comment)
		}
	}

	var out bytes.Buffer
	out.WriteString(flacMagic)
	for i, b := range blocks {
		last := i == len(blocks)-1
		if err := writeBlockHeader(&out, b.typ, len(b.data), last); err != nil {
			return nil, err
		}
		out.Write(b.data)
	}
	out.Write(pf.audio)
	return out.Bytes(), nil
}

// parseFLAC parses the FLAC magic, the metadata block chain and the audio
// tail. It validates the structure strictly: bad magic, a truncated block,
// a first block that is not STREAMINFO, a STREAMINFO of the wrong size, a
// reserved/invalid block type, a chain that never terminates, or an
// invalid Vorbis comment block are all errors.
func parseFLAC(data []byte) (*parsedFile, error) {
	if len(data) < len(flacMagic) || string(data[:len(flacMagic)]) != flacMagic {
		return nil, errors.New("flactags: not a FLAC file (missing fLaC magic)")
	}
	pf := &parsedFile{commentIdx: -1}
	rest := data[len(flacMagic):]
	terminated := false
	for len(rest) > 0 {
		if len(rest) < 4 {
			return nil, errors.New("flactags: truncated metadata block header")
		}
		header := rest[0]
		last := header&0x80 != 0
		typ := header & 0x7f
		length := int(rest[1])<<16 | int(rest[2])<<8 | int(rest[3])
		rest = rest[4:]
		if typ == blockInvalid || (typ >= blockReservedMin && typ <= blockReservedMax) {
			return nil, fmt.Errorf("flactags: invalid or reserved metadata block type %d", typ)
		}
		if len(rest) < length {
			return nil, fmt.Errorf("flactags: truncated metadata block type %d (need %d bytes, have %d)", typ, length, len(rest))
		}
		payload := make([]byte, length)
		copy(payload, rest[:length])
		rest = rest[length:]

		if len(pf.blocks) == 0 && typ != blockStreamInfo {
			return nil, fmt.Errorf("flactags: first metadata block is type %d, want STREAMINFO (0)", typ)
		}
		if typ == blockStreamInfo {
			if length != streamInfoSize {
				return nil, fmt.Errorf("flactags: STREAMINFO block is %d bytes, want %d", length, streamInfoSize)
			}
			si, err := decodeStreamInfo(payload)
			if err != nil {
				return nil, err
			}
			pf.streamInfo = si
		}
		if typ == blockVorbisComment {
			if pf.commentIdx >= 0 {
				return nil, errors.New("flactags: duplicate VORBIS_COMMENT block")
			}
			pf.commentIdx = len(pf.blocks)
			vendor, entries, err := DecodeVorbisComment(payload)
			if err != nil {
				return nil, fmt.Errorf("flactags: invalid VORBIS_COMMENT block: %w", err)
			}
			pf.vendor = vendor
			pf.entries = entries
		}
		pf.blocks = append(pf.blocks, block{typ: typ, data: payload})
		if last {
			terminated = true
			break
		}
	}
	if len(pf.blocks) == 0 {
		return nil, errors.New("flactags: no metadata blocks")
	}
	if !terminated {
		return nil, errors.New("flactags: metadata block chain never terminated")
	}
	pf.audio = make([]byte, len(rest))
	copy(pf.audio, rest)
	return pf, nil
}

// tags builds the public Tags view of a parsed file.
func (pf *parsedFile) tags() *Tags {
	t := &Tags{StreamInfo: pf.streamInfo, Raw: map[string][]string{}}
	for k, v := range pf.entries {
		cp := make([]string, len(v))
		copy(cp, v)
		t.Raw[k] = cp
	}
	first := func(key string) string {
		if v, ok := t.Raw[key]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	t.Title = first("TITLE")
	t.Artist = first("ARTIST")
	t.Artists = append([]string(nil), t.Raw["ARTIST"]...)
	t.Album = first("ALBUM")
	t.AlbumArtist = first("ALBUMARTIST")
	t.Date = first("DATE")
	t.Genre = first("GENRE")
	t.Lyrics = first("LYRICS")
	if s := first("TRACKNUMBER"); s != "" {
		t.TrackNumber = parseTrackNumber(s)
	}
	return t
}

// parseTrackNumber parses "3" and "3/12" style track numbers, returning
// the leading integer, or 0 when there is no leading integer.
func parseTrackNumber(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// buildEntries merges a Tags value over the entries decoded from the
// existing comment block (which may be nil) into the entry map to write.
// When tags.Raw is non-nil it replaces the existing entries wholesale;
// the named fields then overlay it.
func buildEntries(existing map[string][]string, tags Tags) (map[string][]string, error) {
	entries := map[string][]string{}
	if tags.Raw != nil {
		for k, v := range tags.Raw {
			cp := make([]string, len(v))
			copy(cp, v)
			entries[canonicalKey(k)] = cp
		}
	} else {
		for k, v := range existing {
			cp := make([]string, len(v))
			copy(cp, v)
			entries[canonicalKey(k)] = cp
		}
	}
	set := func(key, value string) {
		if value != "" {
			entries[key] = []string{value}
		}
	}
	set("TITLE", tags.Title)
	if len(tags.Artists) > 0 {
		entries["ARTIST"] = append([]string(nil), tags.Artists...)
	} else {
		set("ARTIST", tags.Artist)
	}
	set("ALBUM", tags.Album)
	set("ALBUMARTIST", tags.AlbumArtist)
	if tags.TrackNumber < 0 {
		return nil, fmt.Errorf("flactags: negative track number %d", tags.TrackNumber)
	}
	if tags.TrackNumber > 0 {
		entries["TRACKNUMBER"] = []string{strconv.Itoa(tags.TrackNumber)}
	}
	set("DATE", tags.Date)
	set("GENRE", tags.Genre)
	set("LYRICS", tags.Lyrics)
	// Validate every key/value the caller supplied via Raw.
	for k, vs := range entries {
		if err := validateCommentKey(k); err != nil {
			return nil, err
		}
		for _, v := range vs {
			if !utf8.ValidString(v) {
				return nil, fmt.Errorf("flactags: value for %s is not valid UTF-8", k)
			}
		}
	}
	return entries, nil
}

// canonicalKey upper-cases a Vorbis comment key (keys are case-insensitive;
// canonical casing is upper-case).
func canonicalKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// EncodeVorbisComment encodes a Vorbis comment block payload exactly as
// specified: uint32 little-endian vendor length, vendor bytes, uint32
// little-endian entry count, then for each entry a uint32 little-endian
// length followed by "KEY=value" bytes. Keys are emitted in sorted order
// so encoding is deterministic; values keep their given order. Keys must
// be valid (ASCII printable except '='), vendor and values must be valid
// UTF-8.
func EncodeVorbisComment(vendor string, entries map[string][]string) ([]byte, error) {
	if !utf8.ValidString(vendor) {
		return nil, errors.New("flactags: vendor string is not valid UTF-8")
	}
	keys := make([]string, 0, len(entries))
	total := 0
	for k, vs := range entries {
		if err := validateCommentKey(k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
		for _, v := range vs {
			if !utf8.ValidString(v) {
				return nil, fmt.Errorf("flactags: value for %s is not valid UTF-8", k)
			}
			total++
		}
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	writeU32LE(&buf, uint32(len(vendor)))
	buf.WriteString(vendor)
	writeU32LE(&buf, uint32(total))
	for _, k := range keys {
		for _, v := range entries[k] {
			entry := k + "=" + v
			writeU32LE(&buf, uint32(len(entry)))
			buf.WriteString(entry)
		}
	}
	return buf.Bytes(), nil
}

// DecodeVorbisComment decodes a Vorbis comment block payload, returning
// the vendor string and the entries keyed by canonical upper-case key,
// values in file order. It rejects truncated input, a declared length
// that overruns the payload, entries without '=', invalid keys, and
// invalid UTF-8 anywhere.
func DecodeVorbisComment(data []byte) (string, map[string][]string, error) {
	r := bytes.NewReader(data)
	readU32LE := func() (uint32, error) {
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, errors.New("flactags: truncated Vorbis comment")
		}
		return binary.LittleEndian.Uint32(b[:]), nil
	}
	readBytes := func(n uint32) ([]byte, error) {
		if uint64(n) > uint64(r.Len()) {
			return nil, errors.New("flactags: Vorbis comment length overruns block")
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, errors.New("flactags: truncated Vorbis comment")
		}
		return b, nil
	}
	vendorLen, err := readU32LE()
	if err != nil {
		return "", nil, err
	}
	vendorBytes, err := readBytes(vendorLen)
	if err != nil {
		return "", nil, err
	}
	if !utf8.Valid(vendorBytes) {
		return "", nil, errors.New("flactags: vendor string is not valid UTF-8")
	}
	count, err := readU32LE()
	if err != nil {
		return "", nil, err
	}
	// Sanity bound: each entry needs at least 4 length bytes plus "K=V".
	if uint64(count) > uint64(len(data))/4+1 {
		return "", nil, errors.New("flactags: Vorbis comment entry count is implausible")
	}
	entries := map[string][]string{}
	for i := uint32(0); i < count; i++ {
		entryLen, err := readU32LE()
		if err != nil {
			return "", nil, err
		}
		entry, err := readBytes(entryLen)
		if err != nil {
			return "", nil, err
		}
		if !utf8.Valid(entry) {
			return "", nil, fmt.Errorf("flactags: comment entry %d is not valid UTF-8", i)
		}
		eq := bytes.IndexByte(entry, '=')
		if eq <= 0 {
			return "", nil, fmt.Errorf("flactags: comment entry %d has no KEY=value separator", i)
		}
		key := canonicalKey(string(entry[:eq]))
		if err := validateCommentKey(key); err != nil {
			return "", nil, fmt.Errorf("flactags: comment entry %d: %w", i, err)
		}
		entries[key] = append(entries[key], string(entry[eq+1:]))
	}
	if r.Len() != 0 {
		return "", nil, errors.New("flactags: trailing bytes after Vorbis comment entries")
	}
	return string(vendorBytes), entries, nil
}

// validateCommentKey enforces the Vorbis comment key rules: non-empty,
// ASCII 0x20-0x7E excluding '='.
func validateCommentKey(key string) error {
	if key == "" {
		return errors.New("flactags: empty Vorbis comment key")
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c < 0x20 || c > 0x7e || c == '=' {
			return fmt.Errorf("flactags: invalid character %#U in Vorbis comment key %q", c, key)
		}
	}
	return nil
}

// decodeStreamInfo decodes the 34-byte STREAMINFO payload.
func decodeStreamInfo(b []byte) (StreamInfo, error) {
	if len(b) != streamInfoSize {
		return StreamInfo{}, fmt.Errorf("flactags: STREAMINFO block is %d bytes, want %d", len(b), streamInfoSize)
	}
	var si StreamInfo
	si.MinBlockSize = binary.BigEndian.Uint16(b[0:2])
	si.MaxBlockSize = binary.BigEndian.Uint16(b[2:4])
	si.MinFrameSize = uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6])
	si.MaxFrameSize = uint32(b[7])<<16 | uint32(b[8])<<8 | uint32(b[9])
	// Next 8 bytes: 20 bits sample rate, 3 bits channels-1, 5 bits bps-1,
	// 36 bits total samples.
	v := binary.BigEndian.Uint64(b[10:18])
	si.SampleRate = uint32(v >> 44)
	si.Channels = int((v>>41)&0x7) + 1
	si.BitsPerSample = int((v>>36)&0x1f) + 1
	si.TotalSamples = v & ((1 << 36) - 1) // low 36 bits
	return si, nil
}

func writeBlockHeader(w io.Writer, typ byte, length int, last bool) error {
	if length < 0 || length > 0xffffff {
		return fmt.Errorf("flactags: metadata block length %d out of range", length)
	}
	header := typ & 0x7f
	if last {
		header |= 0x80
	}
	if _, err := w.Write([]byte{header, byte(length >> 16), byte(length >> 8), byte(length)}); err != nil {
		return fmt.Errorf("flactags: write block header: %w", err)
	}
	return nil
}

func writeU32LE(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

// writeFileAtomic writes data to dst via a temporary file in the same
// directory plus rename. When src is non-empty the destination inherits
// src's permission bits; otherwise 0644 is used.
func writeFileAtomic(dst string, data []byte, src string) error {
	dir := filepath.Dir(dst)
	perm := os.FileMode(0644)
	if src != "" {
		if fi, err := os.Stat(src); err == nil {
			perm = fi.Mode().Perm()
		}
	}
	tmp, err := os.CreateTemp(dir, ".flactags-*")
	if err != nil {
		return fmt.Errorf("flactags: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("flactags: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("flactags: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flactags: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flactags: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("flactags: rename temp file to %s: %w", dst, err)
	}
	return nil
}
