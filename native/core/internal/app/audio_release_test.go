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

func TestPublishExecutorStaysUnregistered(t *testing.T) {
	application := newApp(t)
	handlers := application.handlers()
	if len(handlers) != 4 || handlers[workflow.CommandProcess] == nil || handlers[workflow.CommandSearch] == nil {
		t.Fatal(len(handlers))
	}
	if _, registered := handlers["publish_album"]; registered {
		t.Fatal("publish executor registered")
	}
}

func TestVerifiedAudioCompletesAndTorrentDoesNot(t *testing.T) {
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
	if released.Code != http.StatusOK || !strings.Contains(released.Body.String(), `"downloaded":true`) || !strings.Contains(released.Body.String(), `"library_published":true`) || !strings.Contains(released.Body.String(), `"production_verified":false`) {
		t.Fatal(released.Code, released.Body.String())
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
	detail := getAuth(t, application, "/v1/jobs/board-audio")
	if !strings.Contains(detail.Body.String(), `"progress":"已完成"`) || !strings.Contains(detail.Body.String(), "放入库") || !strings.Contains(detail.Body.String(), `"downloaded":true`) || strings.Contains(detail.Body.String(), `"production_verified":true`) {
		t.Fatal(detail.Body.String())
	}
	var shown struct {
		Inspection struct {
			Job struct {
				Stage string `json:"stage"`
			} `json:"job"`
		} `json:"inspection"`
	}
	if err = json.Unmarshal(detail.Body.Bytes(), &shown); err != nil || shown.Inspection.Job.Stage != "completed" {
		t.Fatal(shown, err, detail.Body.String())
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
		if item.Progress != "已完成" || item.Headline != "音乐已校验并放入库中。" || !item.Downloaded || !item.LibraryPublished {
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
