package ncm

import "bytes"

// Detect identifies a media format from leading magic bytes, using the
// same rules and precedence as detect_media in ncm.py. It returns one of
// "ncm", "flac", "mp3", "wav", "aac", "ogg", "ape", "dsf", "m4a" or
// "unknown". Short inputs are safe: every check that needs more bytes
// than are present simply does not match.
func Detect(data []byte) string {
	if bytes.HasPrefix(data, []byte("CTENFDAM")) {
		return "ncm"
	}
	if bytes.HasPrefix(data, []byte("fLaC")) {
		return "flac"
	}
	if len(data) >= 2 && data[0] == 0xff && data[1]&0xf6 == 0xf0 {
		return "aac"
	}
	if bytes.HasPrefix(data, []byte("ID3")) ||
		(len(data) >= 2 && data[0] == 0xff && data[1]&0xe0 == 0xe0 && data[1]&0x06 != 0) {
		return "mp3"
	}
	if len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")) {
		return "wav"
	}
	if bytes.HasPrefix(data, []byte("OggS")) {
		return "ogg"
	}
	if bytes.HasPrefix(data, []byte("MAC ")) {
		return "ape"
	}
	if bytes.HasPrefix(data, []byte("DSD ")) {
		return "dsf"
	}
	if len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) {
		return "m4a"
	}
	return "unknown"
}
