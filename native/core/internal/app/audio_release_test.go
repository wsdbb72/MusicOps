package app

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestPublishExecutorIsRegistered(t *testing.T) {
	application := newApp(t)
	handlers := application.handlers()
	if len(handlers) != 6 || handlers[workflow.CommandProcess] == nil || handlers[workflow.CommandSearch] == nil || handlers[workflow.CommandPublish] == nil || handlers[transcodeCommand] == nil {
		t.Fatal(len(handlers))
	}
}

func TestReleaseAllowedRejectsTerminalAndEarlyStages(t *testing.T) {
	for _, stage := range []string{"preparing", "identifying", "snapshotting", "metadata", "searching", "failed", "cancelled", "skipped"} {
		if err := releaseAllowed(&workflow.Job{Stage: stage}); err == nil {
			t.Fatal(stage, "accepted")
		}
	}
	for _, stage := range []string{"needs_review", "completed", "requesting", "downloading", "validating", "publishing", "fallback", "notifying"} {
		if err := releaseAllowed(&workflow.Job{Stage: stage}); err != nil {
			t.Fatal(stage, err)
		}
	}
	if err := releaseAllowed(&workflow.Job{Stage: "completed", Paused: true}); err == nil {
		t.Fatal("paused job accepted")
	}
	if err := releaseAllowed(&workflow.Job{Stage: "completed", CancelRequested: true}); err == nil {
		t.Fatal("cancel-requested job accepted")
	}
	if err := releaseAllowed(nil); err == nil {
		t.Fatal("nil job accepted")
	}
}

func TestVerifiedAudioIngestsLocallyAndTorrentDoesNot(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("board-audio", "http://127.0.0.1:9", []string{}, false))
	search := waitJob(t, application, "search:board-audio", "needs_review")
	if search.Stage != "needs_review" {
		t.Fatal(search.Stage)
	}
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "album.torrent"), []byte("d8:announce0:4:infod6:lengthi1ee"), 0o600); err != nil {
		t.Fatal(err)
	}
	rejected := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-audio", "artist": "Artist", "album": "Album", "title": "Title", "track": 1, "source_name": "album.torrent",
	})
	if rejected.Code != http.StatusUnprocessableEntity || strings.Contains(rejected.Body.String(), `"downloaded":true`) || !strings.Contains(rejected.Body.String(), "torrent_rejected") {
		t.Fatal(rejected.Code, rejected.Body.String())
	}
	held := getAuth(t, application, "/v1/jobs/board-audio")
	if strings.Contains(held.Body.String(), `"progress":"已完成"`) || strings.Contains(held.Body.String(), `"downloaded":true`) {
		t.Fatal(held.Body.String())
	}

	if err := os.WriteFile(filepath.Join(incoming, "song.wav"), pcmWAV(24), 0o600); err != nil {
		t.Fatal(err)
	}
	released := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-audio", "artist": "Artist", "album": "Album", "title": "Title", "track": 1, "source_name": "song.wav",
	})
	if released.Code != http.StatusOK || !strings.Contains(released.Body.String(), `"downloaded":false`) || !strings.Contains(released.Body.String(), `"library_published":true`) || !strings.Contains(released.Body.String(), `"production_verified":false`) || !strings.Contains(released.Body.String(), `"publish_task_id":"stage:board-audio:publishing"`) {
		t.Fatal(released.Code, released.Body.String())
	}
	var receipt struct {
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(released.Body.Bytes(), &receipt); err != nil || (receipt.Stage != "publishing" && receipt.Stage != "completed") {
		t.Fatal(receipt, err, released.Body.String())
	}
	source, err := os.ReadFile(filepath.Join(incoming, "song.wav"))
	published, readErr := os.ReadFile(filepath.Join(application.Dir, "library-audio", "Artist", "Album", "01 Title.wav"))
	if err != nil || readErr != nil || !bytes.Equal(source, published) {
		t.Fatal(err, readErr)
	}
	still := waitJob(t, application, "search:board-audio", "needs_review")
	if still.Stage != "needs_review" {
		t.Fatal(still.Stage)
	}
	// The ingest moves the root to publishing; the publish task verifies the
	// library record and completes the job. Wait for that cycle instead of
	// asserting on a single racy read.
	waitJob(t, application, "board-audio", "completed")
	detail := getAuth(t, application, "/v1/jobs/board-audio")
	if !strings.Contains(detail.Body.String(), `"progress":"已入库"`) || !strings.Contains(detail.Body.String(), "本地导入") || strings.Contains(detail.Body.String(), `"downloaded":true`) || strings.Contains(detail.Body.String(), `"production_verified":true`) {
		t.Fatal(detail.Body.String())
	}
	var shown struct {
		Inspection struct {
			Job struct {
				Stage string `json:"stage"`
			} `json:"job"`
			Evidence []struct {
				Kind   string `json:"kind"`
				Source string `json:"source"`
			} `json:"evidence"`
		} `json:"inspection"`
	}
	if err = json.Unmarshal(detail.Body.Bytes(), &shown); err != nil || shown.Inspection.Job.Stage != "completed" {
		t.Fatal(shown, err, detail.Body.String())
	}
	found := false
	for _, item := range shown.Inspection.Evidence {
		if item.Kind == "catalog" && item.Source == "local-library" {
			found = true
		}
	}
	if !found {
		t.Fatal("release evidence missing", detail.Body.String())
	}
}

func TestHandoffStaysUnfinishedUntilAudioIsReleased(t *testing.T) {
	jobs := []jobView{
		{ID: "board-1", RootID: "board-1", Stage: "completed", Message: "身份证据已接收，等待四站只读检索；尚未下载", UpdatedNS: 1},
		{ID: "search:board-1", RootID: "board-1", ParentID: "board-1", Stage: "needs_review", Message: "检索需要核对；尚未下载", UpdatedNS: 2},
	}
	held := markReleased(presentJobs(jobs), nil)
	for _, item := range held {
		if item.Progress == "已完成" || item.Downloaded || item.LibraryPublished {
			t.Fatal(item)
		}
	}
	done := markReleased(presentJobs(jobs), map[string]bool{"board-1": true})
	for _, item := range done {
		if item.Progress != "已入库" || item.Headline != "音乐已校验并放入库中（本地导入，未经站点下载）。" || item.Downloaded || !item.LibraryPublished {
			t.Fatal(item)
		}
	}
}

func postCode(t *testing.T, application *App, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.SetBasicAuth("musicops", "test-password-16b")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	return recorder
}

func pcmWAV(frames int) []byte {
	data := bytes.Repeat([]byte{0, 0}, frames)
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(4+(8+16)+(8+len(data))))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)
	return buf.Bytes()
}

func TestSecondTrackAndOriginThroughHTTP(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("board-audio", "http://127.0.0.1:9", []string{}, false))
	search := waitJob(t, application, "search:board-audio", "needs_review")
	if search.Stage != "needs_review" {
		t.Fatal(search.Stage)
	}
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "one.wav"), pcmWAV(24), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "two.wav"), pcmWAV(48), 0o600); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		OK         bool   `json:"ok"`
		SHA256     string `json:"sha256"`
		Origin     string `json:"origin"`
		SourceName string `json:"source_name"`
		Downloaded bool   `json:"downloaded"`
	}
	first := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-audio", "artist": "Artist", "album": "Album", "title": "One", "track": 1, "source_name": "one.wav", "origin": "manual",
	})
	if first.Code != http.StatusOK {
		t.Fatal(first.Code, first.Body.String())
	}
	if err := json.Unmarshal(first.Body.Bytes(), &receipt); err != nil || !receipt.OK || receipt.Origin != "manual" || receipt.SourceName != "one.wav" || receipt.Downloaded || receipt.SHA256 == "" {
		t.Fatal(receipt, err, first.Body.String())
	}
	second := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-audio", "artist": "Artist", "album": "Album", "title": "Two", "track": 2, "source_name": "two.wav",
	})
	if second.Code != http.StatusOK {
		t.Fatal(second.Code, second.Body.String())
	}
	var secondReceipt struct {
		SHA256 string `json:"sha256"`
		Origin string `json:"origin"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondReceipt); err != nil || secondReceipt.SHA256 == "" || secondReceipt.SHA256 == receipt.SHA256 || secondReceipt.Origin != "local_import" {
		t.Fatal(secondReceipt, err, second.Body.String())
	}
	if _, err := os.Stat(filepath.Join(application.Dir, "library-audio", "Artist", "Album", "01 One.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(application.Dir, "library-audio", "Artist", "Album", "02 Two.wav")); err != nil {
		t.Fatal(err)
	}
	bad := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-audio", "artist": "Artist", "album": "Album", "title": "Three", "track": 3, "source_name": "two.wav", "origin": "torrent",
	})
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "INVALID_REQUEST") {
		t.Fatal(bad.Code, bad.Body.String())
	}
}
