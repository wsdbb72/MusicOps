package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func uploadFile(t *testing.T, application *App, filename string, body []byte, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if origin != "" {
		_ = writer.WriteField("origin", origin)
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/incoming", &buf)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestValidIncomingName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../escape.wav", "a/b.wav", `a\b.wav`, "/abs.wav"} {
		if validIncomingName(bad) {
			t.Fatal(bad, "accepted")
		}
	}
	for _, good := range []string{"song.wav", "song.flac", "a b.wav", "01 Title.wav"} {
		if !validIncomingName(good) {
			t.Fatal(good, "rejected")
		}
	}
}

func TestUploadIncomingDeliversWAV(t *testing.T) {
	application := newApp(t)
	body := pcmWAV(24)
	recorder := uploadFile(t, application, "song.wav", body, "manual")
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var receipt struct {
		OK         bool   `json:"ok"`
		SourceName string `json:"source_name"`
		Size       int64  `json:"size"`
		SHA256     string `json:"sha256"`
		Origin     string `json:"origin"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil || !receipt.OK || receipt.SourceName != "song.wav" || receipt.Size != int64(len(body)) || receipt.Origin != "manual" {
		t.Fatal(receipt, err)
	}
	sum := sha256.Sum256(body)
	if receipt.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal(receipt)
	}
	stored, err := os.ReadFile(filepath.Join(application.Dir, "incoming", "song.wav"))
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatal(err)
	}
	// A second delivery of the same name is refused, never overwritten.
	again := uploadFile(t, application, "song.wav", body, "")
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), "INCOMING_EXISTS") {
		t.Fatal(again.Code, again.Body.String())
	}
}

func TestUploadIncomingRejectsNonAudioAndBadNames(t *testing.T) {
	application := newApp(t)
	torrent := uploadFile(t, application, "album.torrent", []byte("d8:announce0:4:infod6:lengthi1ee"), "")
	if torrent.Code != http.StatusUnprocessableEntity || !strings.Contains(torrent.Body.String(), "AUDIO_REJECTED") {
		t.Fatal(torrent.Code, torrent.Body.String())
	}
	empty := uploadFile(t, application, "empty.wav", []byte{}, "")
	if empty.Code != http.StatusUnprocessableEntity || !strings.Contains(empty.Body.String(), "AUDIO_REJECTED") {
		t.Fatal(empty.Code, empty.Body.String())
	}
	// No file part at all.
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/incoming", &buf)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestUploadIncomingRejectsOversized(t *testing.T) {
	application := newApp(t)
	big := bytes.Repeat([]byte{0}, (32<<20)+1)
	recorder := uploadFile(t, application, "big.wav", big, "")
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "AUDIO_TOO_LARGE") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(application.Dir, "incoming", "big.wav")); err == nil {
		t.Fatal("oversized file written")
	}
}
