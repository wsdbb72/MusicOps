package ncm

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"testing"
)

// --- test-only NCM encoder (mirrors the format, used to build fixtures) ---

func aesECBEncrypt(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	// PKCS7 pad to a multiple of 16.
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	return aesECBEncryptRaw(t, padded, key)
}

func aesECBEncryptRaw(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	if len(plain)%aes.BlockSize != 0 {
		t.Fatalf("test plaintext length %d is not a multiple of 16", len(plain))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return out
}

func xorBytes(in []byte, x byte) []byte {
	out := make([]byte, len(in))
	for i, b := range in {
		out[i] = b ^ x
	}
	return out
}

func writeBlock(buf *bytes.Buffer, data []byte) {
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
	buf.Write(lenBuf[:])
	buf.Write(data)
}

// buildNCM assembles a synthetic container. metaJSON is the metadata
// object (nil for an empty meta block); contentKey is the key after the
// "neteasecloudmusic" prefix. keyPlain, when non-nil, replaces the whole
// key plaintext (used to craft an empty key / bad padding). When rawKey
// is true, keyPlain is encrypted without PKCS7 padding (it must be a
// multiple of 16 bytes); otherwise it is PKCS7-padded first.
func buildNCM(t *testing.T, audio, metaJSON, cover, contentKey, keyPlain []byte, rawKey ...bool) []byte {
	t.Helper()
	if keyPlain == nil {
		keyPlain = append([]byte(keyPrefix), contentKey...)
	}
	box, err := keybox(contentKey)
	if err != nil && len(contentKey) != 0 {
		t.Fatal(err)
	}
	// If contentKey is empty the box is unusable; use a zero box only to
	// produce bytes for the (expected to fail) fixture.
	if len(contentKey) == 0 {
		box = [256]byte{}
	}

	var buf bytes.Buffer
	buf.WriteString(magic)
	buf.Write([]byte{0, 0}) // skipped bytes
	var encKey []byte
	if len(rawKey) > 0 && rawKey[0] {
		encKey = aesECBEncryptRaw(t, keyPlain, coreKey)
	} else {
		encKey = aesECBEncrypt(t, keyPlain, coreKey)
	}
	writeBlock(&buf, xorBytes(encKey, 0x64))
	if metaJSON != nil {
		plain := append([]byte(musicPrefix), metaJSON...)
		enc := aesECBEncrypt(t, plain, metaKey)
		metaBlock := append([]byte(metaPrefix), []byte(base64.StdEncoding.EncodeToString(enc))...)
		writeBlock(&buf, xorBytes(metaBlock, 0x63))
	} else {
		writeBlock(&buf, nil)
	}
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(audio))
	buf.Write(crcBuf[:])
	buf.Write(make([]byte, 5)) // skipped bytes
	writeBlock(&buf, cover)
	buf.Write(cryptPayload(audio, box, 0))
	return buf.Bytes()
}

func metaJSON(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var (
	testWAV  = []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x44\xac\x00\x00\x88\x58\x01\x00\x02\x00\x10\x00data\x04\x00\x00\x00\x01\x02\x03\x04")
	testFLAC = append([]byte("fLaC"), []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b}...)
	testKey  = []byte("test-content-key")
	testMeta = map[string]any{
		"musicName": "Test Song",
		"artist":    []any{map[string]any{"id": float64(123), "name": "Test Artist"}},
		"album":     "Test Album",
		"albumId":   float64(456),
		"albumPic":  "https://example.invalid/pic.jpg",
		"format":    "wav",
		"duration":  float64(180000),
		"bitrate":   float64(1411200),
		"alias":     []any{"Alias One"},
		// Untyped extra field: must survive in Metadata.Raw.
		"transName": "Translated Name",
	}
)

func TestDecryptWAVRoundTrip(t *testing.T) {
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), nil, testKey, nil)
	before := append([]byte(nil), data...)

	res, err := Decrypt(data)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(data, before) {
		t.Fatal("Decrypt modified its input")
	}
	if !bytes.Equal(res.Audio, testWAV) {
		t.Fatalf("audio mismatch:\n got %x\nwant %x", res.Audio, testWAV)
	}
	if res.Format != "wav" {
		t.Fatalf("Format = %q, want wav", res.Format)
	}
	m := res.Metadata
	if m.MusicName != "Test Song" || m.Album != "Test Album" || m.AlbumID != 456 ||
		m.AlbumPic != "https://example.invalid/pic.jpg" || m.Format != "wav" ||
		m.Duration != 180000 || m.Bitrate != 1411200 {
		t.Fatalf("metadata mismatch: %+v", m)
	}
	if len(m.Artist) != 1 || m.Artist[0].ID != 123 || m.Artist[0].Name != "Test Artist" {
		t.Fatalf("artist mismatch: %+v", m.Artist)
	}
	if len(m.Alias) != 1 || m.Alias[0] != "Alias One" {
		t.Fatalf("alias mismatch: %v", m.Alias)
	}
	if m.Raw["transName"] != "Translated Name" {
		t.Fatalf("Raw lost untyped field: %v", m.Raw)
	}
	if res.CRC32 != crc32.ChecksumIEEE(testWAV) {
		t.Fatalf("CRC32 = %x, want checksum of audio", res.CRC32)
	}
	if res.Cover != nil {
		t.Fatalf("Cover = %v, want nil", res.Cover)
	}
}

func TestDecryptFLACRoundTrip(t *testing.T) {
	meta := map[string]any{"musicName": "FLAC Song", "format": "flac"}
	data := buildNCM(t, testFLAC, metaJSON(t, meta), nil, testKey, nil)
	res, err := Decrypt(data)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(res.Audio, testFLAC) {
		t.Fatal("FLAC audio mismatch")
	}
	if res.Format != "flac" {
		t.Fatalf("Format = %q, want flac", res.Format)
	}
	if res.Metadata.MusicName != "FLAC Song" {
		t.Fatalf("MusicName = %q", res.Metadata.MusicName)
	}
}

func TestDecryptCoverPreserved(t *testing.T) {
	cover := []byte{0xff, 0xd8, 0xff, 0xe0, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x02}
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), cover, testKey, nil)
	res, err := Decrypt(data)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(res.Cover, cover) {
		t.Fatalf("Cover mismatch: got %x want %x", res.Cover, cover)
	}
}

func TestDecryptReader(t *testing.T) {
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), nil, testKey, nil)
	res, err := DecryptReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecryptReader: %v", err)
	}
	if !bytes.Equal(res.Audio, testWAV) || res.Format != "wav" {
		t.Fatalf("DecryptReader result mismatch: format=%q audio=%x", res.Format, res.Audio)
	}
}

func TestDecryptNoMetadata(t *testing.T) {
	data := buildNCM(t, testWAV, nil, nil, testKey, nil)
	res, err := Decrypt(data)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if res.Format != "wav" || !bytes.Equal(res.Audio, testWAV) {
		t.Fatalf("no-metadata result mismatch: %+v", res.Format)
	}
	if res.Metadata.MusicName != "" {
		t.Fatalf("Metadata should be empty, got %+v", res.Metadata)
	}
}

func TestDecryptWrongMagic(t *testing.T) {
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), nil, testKey, nil)
	data[0] = 'X'
	if _, err := Decrypt(data); !errors.Is(err, ErrNotNCM) {
		t.Fatalf("Decrypt = %v, want ErrNotNCM", err)
	}
	if _, err := Decrypt([]byte("hello world, not ncm")); !errors.Is(err, ErrNotNCM) {
		t.Fatalf("non-NCM: err = %v, want ErrNotNCM", err)
	}
	// Fewer than 8 bytes: truncated before the magic can be checked.
	if _, err := Decrypt([]byte("hello")); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short input: err = %v, want ErrTruncated", err)
	}
	if _, err := Decrypt(nil); !errors.Is(err, ErrTruncated) {
		t.Fatalf("nil input: err = %v, want ErrTruncated", err)
	}
}

func TestDecryptTruncatedHeader(t *testing.T) {
	full := buildNCM(t, testWAV, metaJSON(t, testMeta), []byte{1, 2, 3}, testKey, nil)
	// Cut at several points: inside magic, inside key block, inside meta
	// block, inside CRC/skipped bytes, inside cover block.
	cuts := []int{4, 10, 20, len(full) - len(testWAV) - 20, len(full) - len(testWAV) - 3, len(full) - len(testWAV) - 1}
	for _, cut := range cuts {
		if cut <= 0 || cut >= len(full) {
			t.Fatalf("bad test cut %d for len %d", cut, len(full))
		}
		if _, err := Decrypt(full[:cut]); err == nil {
			t.Fatalf("cut at %d: expected error, got success", cut)
		}
	}
}

func TestDecryptOversizedKeyBlock(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(magic)
	buf.Write([]byte{0, 0})
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], maxKeySize+1)
	buf.Write(lenBuf[:])
	if _, err := Decrypt(buf.Bytes()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized key: err = %v, want ErrTooLarge", err)
	}
}

func TestDecryptBadPadding(t *testing.T) {
	// Key plaintext whose PKCS7 padding is invalid: last byte says 7,
	// but the six bytes before it are not 7.
	plain := make([]byte, 32)
	copy(plain, keyPrefix)
	for i := len(keyPrefix); i < 31; i++ {
		plain[i] = 'A'
	}
	plain[31] = 0x07
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), nil, testKey, plain, true)
	// buildNCM used contentKey for the keybox, but decryption fails
	// before the keybox is used; the point is the padding error.
	if _, err := Decrypt(data); !errors.Is(err, ErrBadPadding) {
		t.Fatalf("bad padding: err = %v, want ErrBadPadding", err)
	}
}

func TestDecryptEmptyKey(t *testing.T) {
	// Key plaintext is exactly the prefix: no content key remains.
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), nil, nil, []byte(keyPrefix))
	if _, err := Decrypt(data); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("empty key: err = %v, want ErrEmptyKey", err)
	}
}

func TestDecryptBadKeyPrefix(t *testing.T) {
	plain := append([]byte("wrongprefix123456"), []byte("0123456789abcdef")...)
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), nil, testKey, plain)
	if _, err := Decrypt(data); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("bad key prefix: err = %v, want ErrInvalidKey", err)
	}
}

func TestDecryptEmptyAudio(t *testing.T) {
	data := buildNCM(t, nil, metaJSON(t, testMeta), nil, testKey, nil)
	if _, err := Decrypt(data); !errors.Is(err, ErrEmptyAudio) {
		t.Fatalf("empty audio: err = %v, want ErrEmptyAudio", err)
	}
}

func TestDecryptUnknownDecryptedFormat(t *testing.T) {
	audio := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	data := buildNCM(t, audio, nil, nil, testKey, nil)
	if _, err := Decrypt(data); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("unknown format: err = %v, want ErrUnknownFormat", err)
	}
}

func TestDecryptFormatConflict(t *testing.T) {
	meta := map[string]any{"musicName": "X", "format": "mp3"}
	data := buildNCM(t, testWAV, metaJSON(t, meta), nil, testKey, nil)
	if _, err := Decrypt(data); !errors.Is(err, ErrFormatConflict) {
		t.Fatalf("format conflict: err = %v, want ErrFormatConflict", err)
	}
}

func TestDecryptCorruptMetadataPrefix(t *testing.T) {
	data := buildNCM(t, testWAV, metaJSON(t, testMeta), nil, testKey, nil)
	// Corrupt a byte inside the meta block (after the key block). Find
	// the meta length field: magic(8) + skip(2) + keylen(4) + key.
	keyLen := int(binary.LittleEndian.Uint32(data[10:14]))
	metaStart := 14 + keyLen + 4
	data[metaStart] ^= 0xff
	if _, err := Decrypt(data); err == nil {
		t.Fatal("corrupt metadata: expected error, got success")
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"ncm", []byte("CTENFDAMxxxx"), "ncm"},
		{"flac", []byte("fLaC\x00\x00\x00\x22"), "flac"},
		{"mp3 id3", []byte("ID3\x04\x00\x00"), "mp3"},
		{"mp3 sync", []byte{0xff, 0xfb, 0x90, 0x00}, "mp3"},
		{"mp3 sync f3", []byte{0xff, 0xf3, 0x90, 0x00}, "mp3"},
		{"wav", []byte("RIFF\x10\x00\x00\x00WAVEfmt "), "wav"},
		{"wav short", []byte("RIFF\x00"), "unknown"},
		{"aac", []byte{0xff, 0xf1, 0x50, 0x80}, "aac"},
		{"aac f9", []byte{0xff, 0xf9, 0x50, 0x80}, "aac"},
		{"ogg", []byte("OggS\x00\x02"), "ogg"},
		{"ape", []byte("MAC \x96\x00"), "ape"},
		{"dsf", []byte("DSD \x1c\x00\x00\x00"), "dsf"},
		{"m4a", []byte("\x00\x00\x00\x18ftypM4A "), "m4a"},
		{"empty", nil, "unknown"},
		{"random", []byte{0x01, 0x02, 0x03}, "unknown"},
		{"riff not wave", []byte("RIFF\x10\x00\x00\x00AVI "), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(tc.data); got != tc.want {
				t.Fatalf("Detect(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}
