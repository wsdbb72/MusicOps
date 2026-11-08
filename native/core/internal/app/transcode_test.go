package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// writeTestWAV builds a one-second 44.1kHz mono 16-bit PCM WAV with a sine
// tone. It is real audio the decoder can fully decode, not a header stub.
func writeTestWAV(t *testing.T, path string) []byte {
	t.Helper()
	const rate = 44100
	samples := make([]int16, rate)
	for i := range samples {
		samples[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(i)/rate))
	}
	data := make([]byte, 44+len(samples)*2)
	copy(data[0:], "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVE")
	copy(data[12:], "fmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], rate)
	binary.LittleEndian.PutUint32(data[28:], rate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(samples)*2))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(sample))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func transcodeReceipts(t *testing.T, application *App) []transcodeReceipt {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/library/transcode", nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Receipts []transcodeReceipt `json:"receipts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Receipts
}

func waitTranscodeReceipt(t *testing.T, application *App, id string) transcodeReceipt {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, receipt := range transcodeReceipts(t, application) {
			if receipt.RequestID == id {
				return receipt
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("transcode receipt never appeared for", id)
	return transcodeReceipt{}
}

func waitTaskState(t *testing.T, application *App, id, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		request := httptest.NewRequest(http.MethodGet, "/v1/logs", nil)
		request.SetBasicAuth("musicops", "test-password-16b")
		recorder := httptest.NewRecorder()
		application.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatal(recorder.Code, recorder.Body.String())
		}
		var decoded struct {
			Tasks []taskrepo.Task `json:"tasks"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		for _, task := range decoded.Tasks {
			if task.ID == id {
				if task.State == want {
					return
				}
				if task.State == "completed" || task.State == "failed" || task.State == "cancelled" {
					t.Fatalf("task %s ended as %s, want %s", id, task.State, want)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("task never reached", want, id)
}

func TestTranscodeWAVToFLAC(t *testing.T) {
	application := newApp(t)
	source := filepath.Join(application.Dir, "incoming", "tone.wav")
	writeTestWAV(t, source)
	sourceHash, err := hashFile(source)
	if err != nil {
		t.Fatal(err)
	}
	created := post(t, application, "/v1/library/transcode", map[string]any{"request_id": "tc-1", "source": source, "format": "flac"})
	if created["task_id"] != "transcode:tc-1" {
		t.Fatal(created)
	}
	receipt := waitTranscodeReceipt(t, application, "tc-1")
	if receipt.Format != "flac" || receipt.Source != source {
		t.Fatal(receipt)
	}
	output := filepath.Join(application.Dir, "incoming", "tone.flac")
	if receipt.Output != output {
		t.Fatal(receipt)
	}
	// The source is untouched and the output fully decodes to identical PCM.
	if after, err := hashFile(source); err != nil || after != sourceHash {
		t.Fatal("source changed during transcode", after, err)
	}
	decoder := audiorelease.SystemDecoder()
	if decoder == nil {
		t.Fatal("system decoder unavailable")
	}
	if err := decoder(context.Background(), output); err != nil {
		t.Fatal("transcoded output did not fully decode", err)
	}
	sourcePCM, err := pcmHash(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	outputPCM, err := pcmHash(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	if sourcePCM != outputPCM {
		t.Fatal("PCM hash mismatch after transcode")
	}
	if receipt.PCMSHA256 != sourcePCM || receipt.SHA256 == "" {
		t.Fatal(receipt)
	}
	// A duplicate submission of the same request id is a conflict, not a
	// second conversion.
	recorder := postCode(t, application, "/v1/library/transcode", map[string]any{"request_id": "tc-1", "source": source, "format": "flac"})
	if recorder.Code != http.StatusConflict {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestTranscodeRejectsNonPCM(t *testing.T) {
	application := newApp(t)
	// A text file is not a WAV at all; the task must fail, not produce a receipt.
	source := filepath.Join(application.Dir, "incoming", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/library/transcode", map[string]any{"request_id": "tc-2", "source": source, "format": "flac"})
	waitTaskState(t, application, "transcode:tc-2", "failed")
	if receipts := transcodeReceipts(t, application); len(receipts) != 0 {
		t.Fatal(receipts)
	}
}

func TestTranscodeRejectsBadRequests(t *testing.T) {
	application := newApp(t)
	source := filepath.Join(application.Dir, "incoming", "tone.wav")
	writeTestWAV(t, source)
	for _, body := range []map[string]any{
		{"request_id": "tc-3", "source": source, "format": "mp3"},
		{"request_id": "tc-3", "source": "/etc/passwd", "format": "flac"},
		{"request_id": "tc-3", "source": "../outside.wav", "format": "flac"},
		{"request_id": "", "source": source, "format": "flac"},
	} {
		recorder := postCode(t, application, "/v1/library/transcode", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatal(body, recorder.Code, recorder.Body.String())
		}
	}
}
