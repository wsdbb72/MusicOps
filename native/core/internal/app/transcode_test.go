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
	"github.com/wsdbb72/MusicOps/native/core/internal/wavflac"
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
		// A failed or cancelled task will never produce a receipt; fail
		// fast with the task state instead of burning the whole deadline.
		// (A completed task always has its receipt already, because the
		// handler records it before returning the completed outcome.)
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
			if task.ID == "transcode:"+id {
				switch task.State {
				case "failed", "cancelled":
					t.Fatalf("transcode task %s ended as %s (%s), no receipt will appear", task.ID, task.State, task.Message)
				case "completed":
					t.Fatalf("transcode task %s completed without a receipt", task.ID)
				}
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
	// The receipt records the canonical (symlink-resolved) paths, so the
	// expectations must be resolved the same way.
	wantSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := filepath.EvalSymlinks(filepath.Join(application.Dir, "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	wantOutput := filepath.Join(incoming, "tone.flac")
	if receipt.Format != "flac" || receipt.Source != wantSource {
		t.Fatal(receipt)
	}
	if receipt.Output != wantOutput {
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
	if err := decoder(context.Background(), wantOutput); err != nil {
		t.Fatal("transcoded output did not fully decode", err)
	}
	sourcePCM, err := wavflac.PCMHash(source)
	if err != nil {
		t.Fatal(err)
	}
	outputPCM, err := wavflac.PCMHash(wantOutput)
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

func TestTranscodeWAVWithExtraChunksToFLAC(t *testing.T) {
	application := newApp(t)
	plain := writeTestWAV(t, filepath.Join(application.Dir, "incoming", "plain.wav"))
	// ffmpeg writes a LIST chunk between fmt and data, pushing the data
	// chunk past the first 44 bytes. A validator that only scans the
	// header prefix rejects that file; the chunk walk must accept it.
	payload := append([]byte("INFO"), make([]byte, 22)...)
	extra := make([]byte, 0, len(plain)+8+len(payload))
	extra = append(extra, plain[:36]...)
	extra = append(extra, []byte("LIST")...)
	var sizeBuf [4]byte
	binary.LittleEndian.PutUint32(sizeBuf[:], uint32(len(payload)))
	extra = append(extra, sizeBuf[:]...)
	extra = append(extra, payload...)
	extra = append(extra, plain[36:]...)
	binary.LittleEndian.PutUint32(extra[4:], uint32(len(extra)-8))
	source := filepath.Join(application.Dir, "incoming", "extra.wav")
	if err := os.WriteFile(source, extra, 0o600); err != nil {
		t.Fatal(err)
	}
	created := post(t, application, "/v1/library/transcode", map[string]any{"request_id": "tc-extra", "source": source, "format": "flac"})
	if created["task_id"] != "transcode:tc-extra" {
		t.Fatal(created)
	}
	receipt := waitTranscodeReceipt(t, application, "tc-extra")
	if receipt.Format != "flac" {
		t.Fatal(receipt)
	}
	sourcePCM, err := wavflac.PCMHash(source)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.PCMSHA256 != sourcePCM || receipt.AudioMD5 == "" {
		t.Fatal("PCM hash mismatch after transcode", receipt)
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

// TestResolveInDirSymlinkedParent guards the macOS case where the state
// directory sits under a symlinked parent (for example /var, which points
// at /private/var): paths inside the directory must still resolve, while
// escapes are still rejected.
func TestResolveInDirSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "incoming"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	source := filepath.Join(real, "incoming", "tone.wav")
	writeTestWAV(t, source)
	// resolveInDir returns the fully resolved path.
	want, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	// A relative path through the symlinked parent must resolve.
	got, err := resolveInDir(link, filepath.Join("incoming", "tone.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal(got, want)
	}
	// An absolute path through the symlinked parent must resolve the same way.
	got, err = resolveInDir(link, source)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal(got, want)
	}
	// An already-resolved absolute path must resolve the same way too:
	// the handler re-resolves the source the enqueue step stored.
	got, err = resolveInDir(link, want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal(got, want)
	}
	// Escapes are still rejected.
	if _, err := resolveInDir(link, "../outside.wav"); err == nil {
		t.Fatal("escape accepted")
	}
	if _, err := resolveInDir(link, "/etc/passwd"); err == nil {
		t.Fatal("absolute escape accepted")
	}
}
