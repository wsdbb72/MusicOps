// Media endpoints: NCM decryption, FLAC tag read/write and LRC
// sidecar/embedded lyrics. These are synchronous handlers, not queue
// tasks: decryption is CPU work on a file already inside the state
// directory (Python decrypt_ncm likewise works on a local file, not a
// download), and tag/lyrics edits are single-file rewrites.
//
// OpenCC is deliberately absent here: the Python side converts
// traditional to simplified Chinese through the opencc package, whose
// dictionaries ship inside that package, not in this repository (a repo
// grep finds no .ocd dictionary file and no Go OpenCC implementation).
// A conversion endpoint without the dictionary data would be a fake, so
// none is provided; see the Phase 3 slice 7 notes in MIGRATION_PLAN.md.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
	"github.com/wsdbb72/MusicOps/native/core/internal/ncm"
)

// ---------------------------------------------------------------------------
// NCM decryption
//
// POST /v1/library/ncm/decrypt {"source": "incoming/foo.ncm"}
//
// The source must already be inside the state directory (resolveInDir
// rejects escapes, including through symlinks) and must end in .ncm. The
// decrypted audio is written to incoming/<stem>.<format> — the same
// incoming/ directory /v1/audio/release reads from, so a decrypted file
// can proceed to the library through the normal verified release path.
// It is a new work copy, never the source: the source file is hashed
// before and after and must be unchanged, the output name always carries
// the decrypted format (never .ncm), and an existing output with
// different bytes is a 409, not an overwrite. When a system decoder is
// available the decrypted output is fully decoded before it is renamed
// into place, matching the transcode proof; without a decoder the result
// is honestly reported as structural-only and still requires validation
// by /v1/audio/release (Python decrypt_ncm returns REQUIRES_VALIDATION
// for the same reason). The embedded cover is reported (presence +
// SHA-256) but not written out: there is no cover-file naming contract,
// and inventing one would strand files the library never reads.
func (a *App) decryptNCM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Source) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	source, err := a.resolveLibraryPath(body.Source)
	if err != nil || !strings.EqualFold(filepath.Ext(source), ".ncm") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > audiorelease.MaxAudioBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	before, err := hashFile(source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	data, err := os.ReadFile(source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	result, err := ncm.Decrypt(data)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "NCM_DECRYPT_FAILED", "error": err.Error()})
		return
	}
	// The source must be untouched by the decrypt.
	if after, err := hashFile(source); err != nil || after != before {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_SOURCE_CHANGED"})
		return
	}
	stem := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	output := filepath.Join(filepath.Dir(source), stem+"."+result.Format)
	if output == source {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "NCM_CONFLICT"})
		return
	}
	outputHash := sha256.Sum256(result.Audio)
	outputDigest := hex.EncodeToString(outputHash[:])
	if existing, err := os.ReadFile(output); err == nil {
		sum := sha256.Sum256(existing)
		if hex.EncodeToString(sum[:]) == outputDigest {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replayed": true, "output": output, "format": result.Format, "source_sha256": before, "output_sha256": outputDigest, "metadata": ncmMetadataView(result.Metadata), "has_cover": len(result.Cover) > 0, "cover_sha256": coverDigest(result.Cover), "decode_state": "replayed", "validation_state": "pending"})
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "NCM_CONFLICT", "error": "decrypted output already exists with different content"})
		return
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_UNAVAILABLE"})
		return
	}
	// Write through a temp file in the same directory plus rename, so a
	// failed decrypt or decode never leaves a partial output behind.
	temp, err := os.CreateTemp(filepath.Dir(output), ".ncm-decrypt-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_UNAVAILABLE"})
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(result.Audio); err != nil {
		temp.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_UNAVAILABLE"})
		return
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_UNAVAILABLE"})
		return
	}
	if err := temp.Close(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_UNAVAILABLE"})
		return
	}
	decodeState := "structural"
	if decoder := audiorelease.SystemDecoder(); decoder != nil {
		if err := decoder(r.Context(), tempName); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "AUDIO_REJECTED", "error": "decrypted audio did not fully decode"})
			return
		}
		decodeState = "decoded"
	}
	if err := os.Rename(tempName, output); err != nil {
		if errors.Is(err, os.ErrExist) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "NCM_CONFLICT"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_UNAVAILABLE"})
		return
	}
	if after, err := hashFile(source); err != nil || after != before {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "NCM_SOURCE_CHANGED"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "output": output, "format": result.Format, "source_sha256": before, "output_sha256": outputDigest,
		"metadata": ncmMetadataView(result.Metadata), "has_cover": len(result.Cover) > 0, "cover_sha256": coverDigest(result.Cover),
		"decode_state": decodeState, "validation_state": "pending",
	})
}

func ncmMetadataView(meta ncm.Metadata) map[string]any {
	artists := make([]string, 0, len(meta.Artist))
	for _, artist := range meta.Artist {
		artists = append(artists, artist.Name)
	}
	return map[string]any{
		"title": meta.MusicName, "artists": artists, "album": meta.Album,
		"album_id": meta.AlbumID, "format": meta.Format, "duration_ms": meta.Duration,
		"bitrate": meta.Bitrate, "alias": meta.Alias,
	}
}

func coverDigest(cover []byte) string {
	if len(cover) == 0 {
		return ""
	}
	sum := sha256.Sum256(cover)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// FLAC tags
//
// GET  /v1/library/tags?path=incoming/song.flac
// POST /v1/library/tags {"path": "...", "tags": {...}}
//
// Only FLAC is supported, matching what flactags can actually read and
// write; any other format is refused with UNSUPPORTED_FORMAT instead of
// pretending mutagen-style multi-format editing exists. The field
// whitelist mirrors Python tags.py SUPPORTED (title, artist, album,
// albumartist, tracknumber, discnumber, date, genre, composer, lyrics);
// an unknown field is a 400. A write copies the original to a backup
// temp file first, rewrites through flactags (temp file + rename, other
// metadata blocks — including picture/cover blocks — and audio bytes
// preserved verbatim), then reads the file back and compares every
// requested field; on a read-back mismatch the backup is restored and
// the write reports TAG_READBACK_MISMATCH, matching Python
// apply_tag_changes. A hardlinked file is refused, as in Python
// (HARDLINK_EDIT_FORBIDDEN).

// tagFieldWhitelist maps the JSON field name to the canonical Vorbis
// comment key.
var tagFieldWhitelist = map[string]string{
	"title": "TITLE", "artist": "ARTIST", "album": "ALBUM", "albumartist": "ALBUMARTIST",
	"tracknumber": "TRACKNUMBER", "discnumber": "DISCNUMBER", "date": "DATE",
	"genre": "GENRE", "composer": "COMPOSER", "lyrics": "LYRICS",
}

func (a *App) libraryTags(w http.ResponseWriter, r *http.Request) {
	path, err := a.resolveLibraryPath(r.URL.Query().Get("path"))
	if err != nil || !strings.EqualFold(filepath.Ext(path), ".flac") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	tags, err := flactags.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "TAG_READ_FAILED", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "tags": tagView(tags), "stream_info": streamInfoView(tags.StreamInfo)})
}

func tagView(tags *flactags.Tags) map[string]any {
	return map[string]any{
		"title": tags.Title, "artist": tags.Artists, "album": tags.Album,
		"albumartist": tags.AlbumArtist, "tracknumber": tags.TrackNumber,
		"date": tags.Date, "genre": tags.Genre, "lyrics": tags.Lyrics, "raw": tags.Raw,
	}
}

func streamInfoView(info flactags.StreamInfo) map[string]any {
	return map[string]any{
		"sample_rate": info.SampleRate, "channels": info.Channels,
		"bits_per_sample": info.BitsPerSample, "total_samples": info.TotalSamples,
	}
}

func (a *App) saveLibraryTags(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string                     `json:"path"`
		Tags map[string]json.RawMessage `json:"tags"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Path) == "" || len(body.Tags) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	path, err := a.resolveLibraryPath(body.Path)
	if err != nil || !strings.EqualFold(filepath.Ext(path), ".flac") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	updates, err := parseTagUpdates(body.Tags)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	if err := writeFLACTags(path, updates); err != nil {
		writeTagError(w, err)
		return
	}
	tags, err := flactags.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "TAG_READ_FAILED"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "tags": tagView(tags), "stream_info": streamInfoView(tags.StreamInfo)})
}

// parseTagUpdates validates the whitelist and decodes each value. A value
// may be a string, a number (tracknumber) or an array of strings; an
// empty string/array deletes the key, matching Python's "empty values
// remove the tag" rule.
func parseTagUpdates(raw map[string]json.RawMessage) (map[string][]string, error) {
	updates := map[string][]string{}
	for field, value := range raw {
		key, ok := tagFieldWhitelist[field]
		if !ok {
			return nil, fmt.Errorf("unsupported field %q", field)
		}
		var single string
		var multi []string
		var number json.Number
		switch {
		case json.Unmarshal(value, &multi) == nil:
			for _, item := range multi {
				if !utf8.ValidString(item) {
					return nil, fmt.Errorf("field %q is not valid UTF-8", field)
				}
			}
		case json.Unmarshal(value, &single) == nil:
			if !utf8.ValidString(single) {
				return nil, fmt.Errorf("field %q is not valid UTF-8", field)
			}
			multi = []string{single}
		case json.Unmarshal(value, &number) == nil:
			multi = []string{number.String()}
		default:
			return nil, fmt.Errorf("field %q must be a string, number or array of strings", field)
		}
		if field == "tracknumber" {
			track, err := parseTrackNumberValue(multi)
			if err != nil {
				return nil, err
			}
			if track == 0 && len(multi) > 0 && strings.TrimSpace(multi[0]) != "" {
				return nil, fmt.Errorf("field %q is not a track number", field)
			}
			if track > 0 {
				multi = []string{strconv.Itoa(track)}
			} else {
				multi = nil
			}
		}
		updates[key] = multi
	}
	return updates, nil
}

// parseTrackNumberValue reads the leading integer of "3" or "3/12".
func parseTrackNumberValue(values []string) (int, error) {
	if len(values) == 0 {
		return 0, nil
	}
	text := strings.TrimSpace(values[0])
	if i := strings.IndexByte(text, '/'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	if text == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid track number %q", values[0])
	}
	return n, nil
}

// tagWriteError marks failures that must restore the backup.
type tagWriteError struct {
	code    string
	status  int
	restore bool
	err     error
}

func (e *tagWriteError) Error() string { return e.err.Error() }

// writeFLACTags applies updates to one FLAC file with a backup and a
// read-back check. updates maps canonical Vorbis keys to values; a nil
// or empty value deletes the key.
func writeFLACTags(path string, updates map[string][]string) error {
	if err := checkNotHardlinked(path); err != nil {
		return &tagWriteError{code: "HARDLINK_EDIT_FORBIDDEN", status: http.StatusConflict, err: err}
	}
	current, err := flactags.ReadFile(path)
	if err != nil {
		return &tagWriteError{code: "TAG_READ_FAILED", status: http.StatusUnprocessableEntity, err: err}
	}
	merged := map[string][]string{}
	for key, values := range current.Raw {
		merged[key] = append([]string(nil), values...)
	}
	for key, values := range updates {
		if len(values) == 0 {
			delete(merged, key)
		} else {
			merged[key] = append([]string(nil), values...)
		}
	}
	// Keep the backup beside the file (same filesystem) so a restore is a
	// rename away; it is removed on success.
	backup, err := backupFile(path)
	if err != nil {
		return &tagWriteError{code: "TAG_UNAVAILABLE", status: http.StatusInternalServerError, err: err}
	}
	defer os.Remove(backup)
	if err := flactags.Write(path, flactags.Tags{Raw: merged}); err != nil {
		return &tagWriteError{code: "TAG_WRITE_FAILED", status: http.StatusUnprocessableEntity, restore: true, err: err}
	}
	after, err := flactags.ReadFile(path)
	if err != nil {
		restoreBackup(path, backup)
		return &tagWriteError{code: "TAG_READBACK_MISMATCH", status: http.StatusInternalServerError, restore: true, err: err}
	}
	for key, want := range updates {
		got := after.Raw[key]
		if len(want) == 0 {
			if len(got) != 0 {
				restoreBackup(path, backup)
				return &tagWriteError{code: "TAG_READBACK_MISMATCH", status: http.StatusInternalServerError, restore: true, err: fmt.Errorf("field %s was not deleted", key)}
			}
			continue
		}
		if len(got) != len(want) {
			restoreBackup(path, backup)
			return &tagWriteError{code: "TAG_READBACK_MISMATCH", status: http.StatusInternalServerError, restore: true, err: fmt.Errorf("field %s read back differently", key)}
		}
		for i := range want {
			if got[i] != want[i] {
				restoreBackup(path, backup)
				return &tagWriteError{code: "TAG_READBACK_MISMATCH", status: http.StatusInternalServerError, restore: true, err: fmt.Errorf("field %s read back differently", key)}
			}
		}
	}
	return nil
}

func writeTagError(w http.ResponseWriter, err error) {
	var tagErr *tagWriteError
	if errors.As(err, &tagErr) {
		writeJSON(w, tagErr.status, map[string]any{"ok": false, "code": tagErr.code, "error": tagErr.err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "TAG_UNAVAILABLE"})
}

// backupFile copies path to a temp file in the same directory and returns
// the temp path. The caller removes it on success.
func backupFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".musicops-backup-*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// restoreBackup atomically replaces path with the backup copy.
func restoreBackup(path, backup string) {
	data, err := os.ReadFile(backup)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".musicops-restore-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}

// ---------------------------------------------------------------------------
// LRC lyrics
//
// GET  /v1/library/lyrics?path=incoming/song.flac
// POST /v1/library/lyrics {"path": "...", "text": "...", "sidecar": true, "embed": false}
//
// The text must parse (lrc.Parse); a document with no valid timed line
// is a 400. On write the text is normalized the way Python player_lrc
// normalizes it: the [offset:] value is folded into the timestamps
// (players disagree on its sign or ignore it) and the output is
// canonical [mm:ss.cc] lines, so sidecar and embedded copies always hold
// the same timeline. The sidecar is "<audio>.lrc" next to the audio file
// (Plex/Navidrome read it); embedding writes the same normalized text
// into the FLAC LYRICS Vorbis comment (Roon/Navidrome read it) through
// the same backup + read-back path as tag writes. Embedding is FLAC-only
// — flactags is the only tag writer this process has — and a non-FLAC
// embed request is refused honestly instead of pretending to embed.
// The sidecar write overwrites an existing sidecar: this endpoint is the
// user-directed edit path, unlike lrc.WriteSidecar, which refuses to
// overwrite a different sidecar and exists for merge-time safety.

// lyricView is the parsed form of one lyric source.
func lyricView(text string) (map[string]any, error) {
	parsed, err := lrc.Parse(text)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"text": parsed.Format(), "lines": parsed.Lines, "meta": parsed.Meta,
		"offset_ms": parsed.OffsetMS, "malformed": parsed.Malformed,
	}, nil
}

func (a *App) libraryLyrics(w http.ResponseWriter, r *http.Request) {
	path, err := a.resolveLibraryPath(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	var sidecar any
	if parsed, err := lrc.ReadSidecar(path); err == nil {
		view, err := lyricView(parsed.Format())
		if err == nil {
			sidecar = view
		}
	}
	var embedded any
	embeddedNote := ""
	if strings.EqualFold(filepath.Ext(path), ".flac") {
		if tags, err := flactags.ReadFile(path); err == nil && strings.TrimSpace(tags.Lyrics) != "" {
			view, err := lyricView(tags.Lyrics)
			if err == nil {
				embedded = view
			} else {
				embedded = map[string]any{"text": tags.Lyrics, "synchronized": false}
			}
		}
	} else {
		embeddedNote = "embedded lyrics are only supported for FLAC"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "sidecar": sidecar, "embedded": embedded, "embedded_note": embeddedNote})
}

func (a *App) saveLibraryLyrics(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Text    string `json:"text"`
		Sidecar bool   `json:"sidecar"`
		Embed   bool   `json:"embed"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Path) == "" || (!body.Sidecar && !body.Embed) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	path, err := a.resolveLibraryPath(body.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	parsed, err := lrc.Parse(body.Text)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_LRC", "error": err.Error()})
		return
	}
	// Normalize like Python player_lrc: fold the offset into the
	// timestamps and drop it, so players see one timeline.
	normalized := &lrc.Lyrics{Lines: parsed.ShiftedLines(), Meta: parsed.Meta}
	text := normalized.Format()
	if _, err := lrc.Parse(text); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LYRICS_UNAVAILABLE"})
		return
	}
	sidecarWritten, embedded := false, false
	if body.Sidecar {
		if err := writeSidecarOverwrite(path, text); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LYRICS_UNAVAILABLE", "error": err.Error()})
			return
		}
		sidecarWritten = true
	}
	if body.Embed {
		if !strings.EqualFold(filepath.Ext(path), ".flac") {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "UNSUPPORTED_FORMAT", "error": "embedded lyrics are only supported for FLAC"})
			return
		}
		if err := writeFLACTags(path, map[string][]string{"LYRICS": {text}}); err != nil {
			writeTagError(w, err)
			return
		}
		embedded = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "text": text, "sidecar_written": sidecarWritten, "embedded": embedded, "lines": len(normalized.Lines)})
}

// writeSidecarOverwrite writes normalized LRC text to "<audio>.lrc"
// through a temp file plus rename, replacing an existing sidecar. This
// is the user-directed edit path; lrc.WriteSidecar's refusal to
// overwrite exists for merge-time safety and is not used here.
func writeSidecarOverwrite(audioPath, text string) error {
	sidecar := lrc.SidecarPath(audioPath)
	dir := filepath.Dir(sidecar)
	tmp, err := os.CreateTemp(dir, ".lrc-*")
	if err != nil {
		return fmt.Errorf("lrc: create temp sidecar in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.WriteString(tmp, text); err != nil {
		tmp.Close()
		return fmt.Errorf("lrc: write temp sidecar: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("lrc: sync temp sidecar: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("lrc: close temp sidecar: %w", err)
	}
	if err := os.Rename(tmpName, sidecar); err != nil {
		return fmt.Errorf("lrc: rename temp sidecar to %s: %w", sidecar, err)
	}
	return nil
}

// streamInfoFor reads the stream parameters of one library audio file:
// FLAC STREAMINFO via flactags, WAV fmt chunk parsed directly. Anything
// else reports an error instead of guessed values.
func streamInfoFor(path, format string) (sampleRate uint32, channels int, bits int, err error) {
	switch strings.ToLower(format) {
	case "flac":
		tags, err := flactags.ReadFile(path)
		if err != nil {
			return 0, 0, 0, err
		}
		return tags.StreamInfo.SampleRate, tags.StreamInfo.Channels, tags.StreamInfo.BitsPerSample, nil
	case "wav":
		return wavStreamInfo(path)
	default:
		return 0, 0, 0, fmt.Errorf("no stream info for format %q", format)
	}
}

// wavStreamInfo parses the fmt chunk of a PCM WAV header.
func wavStreamInfo(path string) (uint32, int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer file.Close()
	header := make([]byte, 44)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, 0, 0, errors.New("file too short for a WAV header")
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, 0, 0, errors.New("not a WAV file")
	}
	rate := uint32(header[24]) | uint32(header[25])<<8 | uint32(header[26])<<16 | uint32(header[27])<<24
	channels := int(uint16(header[22]) | uint16(header[23])<<8)
	bits := int(uint16(header[34]) | uint16(header[35])<<8)
	return rate, channels, bits, nil
}

// pcmFingerprint decodes one audio file to raw s16le PCM at 44.1 kHz
// stereo (the same normalization transcode uses to prove a conversion)
// and returns its SHA-256. It needs ffmpeg; the caller reports the
// no-decoder case honestly instead of substituting a file hash.
func pcmFingerprint(ctx context.Context, path string) (string, error) {
	return pcmHash(ctx, path)
}
