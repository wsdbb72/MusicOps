package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

func startSearchJob(t *testing.T, application *App, jobID string) {
	t.Helper()
	post(t, application, "/v1/search", searchBody(jobID, "http://127.0.0.1:9", []string{}, false))
	waitJob(t, application, "search:"+jobID, "needs_review")
}

func TestPublishControlledAudio(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-ctl")
	wav := pcmWAV(24)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	}))
	defer server.Close()
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-ctl", "source_url": server.URL, "artist": "Artist", "album": "Album", "title": "Title", "track": 1,
	})
	if posted.Code != http.StatusOK || !strings.Contains(posted.Body.String(), `"publish_task_id":"stage:pub-ctl:publishing"`) {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-ctl", "completed")
	published, err := os.ReadFile(filepath.Join(application.Dir, "library-audio", "Artist", "Album", "01 Title.wav"))
	if err != nil || !bytes.Equal(wav, published) {
		t.Fatal(err)
	}
	detail := getAuth(t, application, "/v1/jobs/pub-ctl")
	if !strings.Contains(detail.Body.String(), "controlled-audio") {
		t.Fatal(detail.Body.String())
	}
}

func TestPublishRejectsBadShapes(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-bad")
	for _, body := range []map[string]any{
		{"job_id": "pub-bad", "source_url": "http://example.com/x.wav", "artist": "Artist", "album": "Album", "title": "Title", "track": 1},
		{"job_id": "pub-bad", "source_url": "http://127.0.0.1:9/x.wav", "artist": "Artist", "album": "Album", "title": "Title", "track": 1, "receipts": []map[string]any{{"request_id": "r1", "sha256": "00"}}},
		{"job_id": "pub-bad"},
		{"job_id": "pub-bad", "source_url": "http://127.0.0.1:9/x.wav", "artist": "Artist", "album": "Album", "title": "Title", "track": 0},
	} {
		posted := postCode(t, application, "/v1/audio/publish", body)
		if posted.Code != http.StatusBadRequest {
			t.Fatal(posted.Code, posted.Body.String())
		}
	}
}

func ingestTrack(t *testing.T, application *App, root string, track int, frames int) audiorelease.Result {
	t.Helper()
	incoming, library := filepath.Join(application.Dir, "incoming"), filepath.Join(application.Dir, "library-audio")
	if err := os.MkdirAll(incoming, 0o700); err != nil || os.MkdirAll(library, 0o700) != nil {
		t.Fatal(err)
	}
	name := "song" + strconv.Itoa(track) + ".wav"
	if err := os.WriteFile(filepath.Join(incoming, name), pcmWAV(frames), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("album-audio/" + root + "/track/" + strconv.Itoa(track)))
	result, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "audio-" + hex.EncodeToString(sum[:16]), RootID: root, Artist: "Artist", Album: "Album",
		Title: "Title " + strconv.Itoa(track), Track: track, SourceName: name, Origin: "local_import",
		Incoming: incoming, Library: library, Decoder: audiorelease.SystemDecoder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPublishMultiReceipt(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-multi")
	first := ingestTrack(t, application, "pub-multi", 1, 24)
	second := ingestTrack(t, application, "pub-multi", 2, 48)
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-multi",
		"receipts": []map[string]any{
			{"track_id": "1", "request_id": first.RequestID, "sha256": first.SHA256, "format": first.Format, "origin": first.Origin, "source_name": first.SourceName, "decode_state": first.DecodeState},
			{"track_id": "2", "request_id": second.RequestID, "sha256": second.SHA256, "format": second.Format, "origin": second.Origin, "source_name": second.SourceName, "decode_state": second.DecodeState},
		},
	})
	if posted.Code != http.StatusOK || !strings.Contains(posted.Body.String(), `"publish_task_id":"stage:pub-multi:publishing"`) {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-multi", "completed")
	for _, name := range []string{"01 Title 1.wav", "02 Title 2.wav"} {
		if _, err := os.Stat(filepath.Join(application.Dir, "library-audio", "Artist", "Album", name)); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestPublishRejectsTamperedReceipt(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-tamper")
	first := ingestTrack(t, application, "pub-tamper", 1, 24)
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-tamper",
		"receipts": []map[string]any{
			{"track_id": "1", "request_id": first.RequestID, "sha256": strings.Repeat("0", 64), "format": first.Format, "origin": first.Origin, "source_name": first.SourceName, "decode_state": first.DecodeState},
		},
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitTaskState(t, application, "stage:pub-tamper:publishing", "failed")
	// A publish verification failure fails the job (stage "failed", retryable
	// from review) instead of leaving it in publishing with a stuck task.
	held := inspect(t, application, "pub-tamper")
	if held.Job.Stage != "failed" {
		t.Fatal("tampered publish did not fail the job", held.Job.Stage)
	}
}
