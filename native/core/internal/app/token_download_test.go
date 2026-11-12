package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
)

// tokenReviewServer is the smallest DicMusic-shaped site that produces a token
// request without a final answer: the torrent page has one token button, the
// account balance covers it, and after the fetch the page still shows no
// personal free label. That is REQUEST_UNKNOWN with the torrent bytes saved.
func tokenReviewServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"11","isPersonalFreeleech":false,"isFreeleech":false}}}`)
		case values.Get("torrentid") == "11":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=11&amp;usetoken=1" title="Use 3 tokens">FL (3)</a>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case values.Get("action") == "download":
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func provisionalTokenJob(t *testing.T, application *App, jobID string) string {
	t.Helper()
	server := tokenReviewServer(t)
	body := tokenBody(jobID, server.URL, []string{"dicmusic"})
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:"+jobID, "needs_review")
	if job.ReviewKind != "token" || job.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(job.ReviewKind, job.ErrorClass, job.Message)
	}
	return job.ID
}

// A token request with no final answer used to be a dead end: the download
// entry accepted only ReviewKind "candidate", so the saved torrent could never
// be handed to qBittorrent even though downloading it spends nothing more
// (D3 balanced, P1-7).
func TestProvisionalTokenJobCanStartDownload(t *testing.T) {
	application := newApp(t)
	id := provisionalTokenJob(t, application, "album-token-download")
	recorder := postDownload(t, application, id, downloadBody("http://127.0.0.1:1", "secret-pass"))
	if recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["stage"] != "downloading" || body["download_task_id"] != "download:"+id {
		t.Fatal(body)
	}
	task, err := application.tasks.Get("download:" + id)
	if err != nil || task == nil || task.Lane != "download" {
		t.Fatal(err, task)
	}
	raw, found, err := application.tasks.Data(task.ID, "arguments", 0)
	if err != nil || !found || !strings.Contains(string(raw), `"free_method":"token-provisional"`) {
		t.Fatal(err, found, string(raw))
	}
}

// A token that was explicitly released unspent is not a download permission:
// the user is expected to re-acquire (which may fetch the torrent again), not
// to download a torrent whose spend record says it did not happen.
func TestReleasedTokenJobCannotStartDownload(t *testing.T) {
	application := newApp(t)
	id := provisionalTokenJob(t, application, "album-token-released")
	if _, err := application.ledger.Advance(application.now(), "dicmusic", id, tokenledger.StateReleased, "test-release"); err != nil {
		t.Fatal(err)
	}
	recorder := postDownload(t, application, id, downloadBody("http://127.0.0.1:1", "secret-pass"))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "TOKEN_NOT_SETTLED") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if task, err := application.tasks.Get("download:" + id); err != nil || task != nil {
		t.Fatal("a refused download queued a task", err, task)
	}
}

// The free path keeps its old behaviour: a candidate job downloads without any
// ledger row at all.
func TestFreeCandidateStillStartsDownload(t *testing.T) {
	wav := pcmWAV(100)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav}})
	application := newApp(t)
	job := candidateJob(t, application, "album-free-download", torrent)
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav}, 1<<30)
	recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass"))
	if recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if body := decodeBody(t, recorder); body["stage"] != "downloading" {
		t.Fatal(body)
	}
}
