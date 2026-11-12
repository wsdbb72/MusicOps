package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

// uploadIncoming receives one audio file into incoming/. It only delivers the
// bytes: the name, extension and size are checked here, but the audio itself
// is verified later by /v1/audio/release, which is the only step that can
// put a file into the library. Nothing is written anywhere else, and an
// existing file is never overwritten.
func (a *App) uploadIncoming(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, audiorelease.MaxAudioBytes+(1<<20))
	if err := r.ParseMultipartForm(audiorelease.MaxAudioBytes); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "code": "AUDIO_TOO_LARGE"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	origin := "local_import"
	if values := r.MultipartForm.Value["origin"]; len(values) > 0 && values[0] != "" {
		origin = values[0]
	}
	if origin != "manual" && origin != "local_import" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	defer file.Close()
	name := header.Filename
	if !validIncomingName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".wav", ".flac":
	default:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "AUDIO_REJECTED", "class": "audio_rejected"})
		return
	}
	if header.Size <= 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "AUDIO_REJECTED", "class": "audio_rejected"})
		return
	}
	if header.Size > audiorelease.MaxAudioBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "code": "AUDIO_TOO_LARGE"})
		return
	}
	incoming := a.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	// The name is a validated basename, so the destination is always inside
	// incoming/. O_EXCL keeps an existing delivery from being overwritten.
	out, err := os.OpenFile(filepath.Join(incoming, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "INCOMING_EXISTS"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, sum), file)
	closeErr := out.Close()
	if err != nil || closeErr != nil {
		os.Remove(filepath.Join(incoming, name))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	if written == 0 {
		os.Remove(filepath.Join(incoming, name))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "AUDIO_REJECTED", "class": "audio_rejected"})
		return
	}
	if written > audiorelease.MaxAudioBytes {
		os.Remove(filepath.Join(incoming, name))
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "code": "AUDIO_TOO_LARGE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "source_name": name, "size": written, "sha256": hex.EncodeToString(sum.Sum(nil)[:]), "origin": origin,
	})
}

// validIncomingName accepts only a plain basename: no directories, no empty
// names, no separators. The standard multipart parser already strips
// directory components, so this is defence in depth for hand-built requests.
func validIncomingName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}
