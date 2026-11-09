// Package ncm decrypts NetEase Cloud Music (.ncm) containers into plain
// audio bytes held in memory. It mirrors src/musicops/media/ncm.py: the
// source bytes are never modified, and every failure is a real error —
// there is no fallback to undecrypted or guessed bytes.
//
// Container layout (all lengths little-endian uint32):
//
//	magic "CTENFDAM", 2 skipped bytes,
//	key length + key data (each byte XOR 0x64, AES-128-ECB with CORE_KEY,
//	PKCS7; plaintext starts with "neteasecloudmusic"),
//	meta length + meta data (each byte XOR 0x63, "163 key(Don't modify):"
//	prefix, base64, AES-128-ECB with META_KEY, "music:" prefix, JSON),
//	CRC32 (4 bytes, carried through unverified, as in the Python code),
//	5 skipped bytes,
//	cover-image length + cover-image data,
//	audio payload XORed with the RC4-style keystream built from the
//	content key (the bytes after the "neteasecloudmusic" prefix).
package ncm

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Magic and keys pinned by the NCM format (same values as ncm.py).
const magic = "CTENFDAM"

// coreKey decrypts the content key; metaKey decrypts the metadata blob.
var (
	coreKey = []byte("hzHRAmso5kInbaxW")
	metaKey = []byte{
		0x23, 0x31, 0x34, 0x6c, 0x6a, 0x6b, 0x5f, 0x21,
		0x5c, 0x5d, 0x26, 0x30, 0x55, 0x3c, 0x27, 0x28,
	}
)

const (
	keyPrefix   = "neteasecloudmusic"
	metaPrefix  = "163 key(Don't modify):"
	musicPrefix = "music:"
)

const (
	maxKeySize   = 1024 * 1024
	maxMetaSize  = 16 * 1024 * 1024
	maxCoverSize = 32 * 1024 * 1024
)

// Sentinel errors; Decrypt wraps them with context via fmt.Errorf.
var (
	ErrNotNCM         = errors.New("ncm: not an NCM container")
	ErrTruncated      = errors.New("ncm: truncated header")
	ErrTooLarge       = errors.New("ncm: header block exceeds limit")
	ErrInvalidKey     = errors.New("ncm: invalid content key")
	ErrEmptyKey       = errors.New("ncm: empty NCM content key")
	ErrInvalidMeta    = errors.New("ncm: invalid metadata")
	ErrBadPadding     = errors.New("ncm: bad AES padding")
	ErrEmptyAudio     = errors.New("ncm: empty audio payload")
	ErrUnknownFormat  = errors.New("ncm: decrypted format unknown")
	ErrFormatConflict = errors.New("ncm: metadata format conflicts with actual media")
)

// Artist is one entry of the metadata "artist" array.
type Artist struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Metadata is the parsed NCM metadata JSON. The common fields are typed;
// Raw keeps every field from the JSON object (including ones with no typed
// field above) so nothing the container carried is silently dropped.
type Metadata struct {
	MusicName string   `json:"musicName"`
	Artist    []Artist `json:"artist"`
	Album     string   `json:"album"`
	AlbumID   int64    `json:"albumId"`
	AlbumPic  string   `json:"albumPic"`
	Format    string   `json:"format"`
	Duration  int64    `json:"duration"`
	Bitrate   int64    `json:"bitrate"`
	Alias     []string `json:"alias"`

	// Raw is the full metadata object as decoded from JSON.
	Raw map[string]any `json:"-"`
}

// Result is one decrypted container. Audio is the plain media bytes and
// Format is their detected format (see Detect); it is never "ncm" or
// "unknown" on success. Cover is the embedded cover image, or nil when
// the container carries none. CRC32 is the container's stored CRC32
// value, carried through unverified, as in the Python implementation.
type Result struct {
	Audio    []byte
	Format   string
	Metadata Metadata
	Cover    []byte
	CRC32    uint32
}

// Decrypt parses and decrypts a whole NCM container held in memory.
// data is not modified. It returns an error for a wrong magic, a
// truncated or oversized header block, a bad content key or metadata
// blob, bad AES padding, an empty audio payload, an unrecognised
// decrypted format, or a metadata format that conflicts with the
// decrypted audio.
func Decrypt(data []byte) (Result, error) {
	var out Result
	r := bytes.NewReader(data)

	take := func(n int) ([]byte, error) {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("%w: need %d bytes: %v", ErrTruncated, n, err)
		}
		return buf, nil
	}
	sized := func(limit int) ([]byte, error) {
		sizeBuf, err := take(4)
		if err != nil {
			return nil, err
		}
		size := binary.LittleEndian.Uint32(sizeBuf)
		if size > uint32(limit) {
			return nil, fmt.Errorf("%w: block of %d bytes exceeds limit %d", ErrTooLarge, size, limit)
		}
		return take(int(size))
	}

	head, err := take(8)
	if err != nil {
		return out, err
	}
	if string(head) != magic {
		return out, ErrNotNCM
	}
	if _, err := take(2); err != nil {
		return out, err
	}

	encKey, err := sized(maxKeySize)
	if err != nil {
		return out, err
	}
	for i := range encKey {
		encKey[i] ^= 0x64
	}
	key, err := aesECBDecrypt(encKey, coreKey)
	if err != nil {
		return out, err
	}
	if !bytes.HasPrefix(key, []byte(keyPrefix)) {
		return out, ErrInvalidKey
	}
	contentKey := key[len(keyPrefix):]
	if len(contentKey) == 0 {
		return out, ErrEmptyKey
	}
	box, err := keybox(contentKey)
	if err != nil {
		return out, err
	}

	metaRaw, err := sized(maxMetaSize)
	if err != nil {
		return out, err
	}
	if len(metaRaw) > 0 {
		meta, err := decryptMetadata(metaRaw)
		if err != nil {
			return out, err
		}
		out.Metadata = meta
	}

	crcBuf, err := take(4)
	if err != nil {
		return out, err
	}
	out.CRC32 = binary.LittleEndian.Uint32(crcBuf)
	if _, err := take(5); err != nil {
		return out, err
	}

	cover, err := sized(maxCoverSize)
	if err != nil {
		return out, err
	}
	if len(cover) > 0 {
		out.Cover = cover
	}

	// Everything left is the encrypted audio payload.
	encAudio, err := io.ReadAll(r)
	if err != nil {
		return out, fmt.Errorf("ncm: reading audio payload: %w", err)
	}
	if len(encAudio) == 0 {
		return out, ErrEmptyAudio
	}
	audio := cryptPayload(encAudio, box, 0)
	out.Audio = audio

	format := Detect(audio)
	if format == "unknown" || format == "ncm" {
		return out, ErrUnknownFormat
	}
	out.Format = format
	if declared := lowerASCII(out.Metadata.Format); declared != "" && declared != format {
		return out, fmt.Errorf("%w: metadata says %q, decrypted audio is %q", ErrFormatConflict, declared, format)
	}
	return out, nil
}

// DecryptReader reads a whole NCM container from r and decrypts it.
// It is Decrypt over bytes read from r; r is consumed to EOF.
func DecryptReader(r io.Reader) (Result, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Result{}, fmt.Errorf("ncm: reading container: %w", err)
	}
	return Decrypt(data)
}

// decryptMetadata reverses the meta-block encoding: XOR 0x63, check the
// "163 key(Don't modify):" prefix, base64-decode, AES-128-ECB decrypt
// with metaKey, check the "music:" prefix, and parse the JSON object.
func decryptMetadata(raw []byte) (Metadata, error) {
	var meta Metadata
	decoded := make([]byte, len(raw))
	for i, b := range raw {
		decoded[i] = b ^ 0x63
	}
	if !bytes.HasPrefix(decoded, []byte(metaPrefix)) {
		return meta, ErrInvalidMeta
	}
	b64 := decoded[len(metaPrefix):]
	enc, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil {
		return meta, fmt.Errorf("%w: base64: %v", ErrInvalidMeta, err)
	}
	plain, err := aesECBDecrypt(enc, metaKey)
	if err != nil {
		return meta, err
	}
	if !bytes.HasPrefix(plain, []byte(musicPrefix)) {
		return meta, ErrInvalidMeta
	}
	payload := plain[len(musicPrefix):]
	if err := json.Unmarshal(payload, &meta); err != nil {
		return meta, fmt.Errorf("%w: %v", ErrInvalidMeta, err)
	}
	var rawMap map[string]any
	if err := json.Unmarshal(payload, &rawMap); err != nil {
		return meta, fmt.Errorf("%w: %v", ErrInvalidMeta, err)
	}
	meta.Raw = rawMap
	return meta, nil
}

// aesECBDecrypt decrypts data with AES-128 in ECB mode and removes PKCS7
// padding. The Go stdlib has no ECB mode, so blocks are decrypted one at
// a time with cipher.Block. data must be non-empty and a multiple of 16.
func aesECBDecrypt(data, key []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: invalid AES block length %d", ErrBadPadding, len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("ncm: AES key: %w", err)
	}
	plain := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(plain[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return pkcs7Unpad(plain)
}

// pkcs7Unpad validates and strips PKCS7 padding (block size 16).
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, ErrBadPadding
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return nil, ErrBadPadding
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, ErrBadPadding
		}
	}
	return data[:len(data)-pad], nil
}

// keybox builds the RC4-style key scheduling box from the content key.
func keybox(key []byte) ([256]byte, error) {
	var box [256]byte
	if len(key) == 0 {
		return box, ErrEmptyKey
	}
	for i := range box {
		box[i] = byte(i)
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (int(box[i]) + j + int(key[i%len(key)])) & 0xff
		box[i], box[j] = box[j], box[i]
	}
	return box, nil
}

// cryptPayload XORs data with the keystream derived from box, starting
// at stream offset. The transform is its own inverse, so the same
// function encrypts and decrypts.
func cryptPayload(data []byte, box [256]byte, offset int) []byte {
	out := make([]byte, len(data))
	for i, v := range data {
		j := (offset + i + 1) & 0xff
		out[i] = v ^ box[(int(box[j])+int(box[(int(box[j])+j)&0xff]))&0xff]
	}
	return out
}

// lowerASCII lowercases ASCII letters only; metadata format strings are
// ASCII ("flac", "mp3", ...), so no Unicode folding is needed or wanted.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
