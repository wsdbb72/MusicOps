package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestRetryRemainingUsesTheRecordedBackoff(t *testing.T) {
	now := time.Unix(0, 1_000)
	if got := RetryRemaining(1_000, 600, now); got != 600*time.Second {
		t.Fatal(got)
	}
	if got := RetryRemaining(1_000, 600, now.Add(601*time.Second)); got != 0 {
		t.Fatal(got)
	}
}

func TestSearchHitQueuesAcquisitionWithoutADownloadExecutor(t *testing.T) {
	hits := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-MusicOps-Site") {
		case "dicmusic":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
		case "open.cd":
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
		default:
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
		}
	}))
	defer hits.Close()
	application := newApp(t)
	body := searchBody("album-exact", hits.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	response := post(t, application, "/v1/search", body)
	if response["downloaded"] != false || response["python"] != false || response["production_verified"] != false {
		t.Fatal(response)
	}
	searchJob := waitJob(t, application, "search:album-exact", "completed")
	if searchJob.SearchAction != "queue_request" {
		t.Fatal(searchJob)
	}
	child := waitJob(t, application, "process:search:album-exact", "needs_review")
	if !strings.Contains(child.Message, "尚未下载") || strings.Contains(child.Message, "已入库") {
		t.Fatal(child)
	}
	if dirContains(t, application.Dir, "cookie-secret-not-stored") {
		t.Fatal("cookie was written to the native store")
	}
}

func TestRateLimitWaitsBeforeAnotherRead(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()
	application := newApp(t)
	clock := time.Now()
	application.searcher.Now = func() time.Time { return clock }
	var waited time.Duration
	application.Wait = func(_ context.Context, delay time.Duration) error {
		waited = delay
		clock = clock.Add(delay)
		return nil
	}
	post(t, application, "/v1/search", searchBody("album-limit", server.URL, []string{"dicmusic"}, false))
	job := waitJob(t, application, "search:album-limit", "skipped")
	if job.SearchRetryCount != 1 || waited < 590*time.Second || calls.Load() != 2 {
		t.Fatal(job.SearchRetryCount, waited, calls.Load(), job.Stage, job.SearchAction)
	}
}

func TestAuthFailureStaysInReview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("album-auth", server.URL, []string{"dicmusic"}, false))
	job := waitJob(t, application, "search:album-auth", "needs_review")
	if job.SearchAction != "review_search" || job.ReviewKind != "search" {
		t.Fatal(job)
	}
	inspection := inspect(t, application, "search:album-auth")
	if len(inspection.Children) != 0 {
		t.Fatal(inspection.Children)
	}
}

func TestAlbumSceneStartsWithoutPlaylistCounts(t *testing.T) {
	application := newApp(t)
	body := searchBody("album-upgrade", "http://127.0.0.1:9", nil, false)
	body["scene"] = "upgrade"
	body["source_track_ids"] = []string{}
	body["cookie"] = ""
	accepted := post(t, application, "/v1/search", body)
	if accepted["downloaded"] != false || accepted["python"] != false {
		t.Fatal(accepted)
	}
	job := waitJob(t, application, "search:album-upgrade", "needs_review")
	if job.SearchAction != "review_search" {
		t.Fatal(job)
	}
}

func TestFreeTorrentIsStoredWithoutPublishingTheAlbum(t *testing.T) {
	torrent := []byte("d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/download.php") {
			downloads.Add(1)
			if r.URL.Query().Get("usetoken") != "" {
				t.Fatal("free download used a token")
			}
			w.Write(torrent)
			return
		}
		io.WriteString(w, `<html><a href="logout.php">logout</a><table><tr><td><table><tr><td><a href="details.php?id=11" title="Album">Album</a></td></tr></table></td><td>100 MiB</td></tr></table></html>`)
	}))
	defer server.Close()
	application := newApp(t)
	body := searchBody("album-free", server.URL, []string{"open.cd"}, false)
	body["expected_title"] = "Album"
	body["free_confirmed"] = true
	post(t, application, "/v1/search", body)
	searchJob := waitJob(t, application, "search:album-free", "completed")
	if searchJob.SearchAction != "queue_request" {
		t.Fatal(searchJob)
	}
	job := waitJob(t, application, "process:search:album-free", "needs_review")
	if downloads.Load() != 1 || job.ReviewKind != "candidate" || !strings.Contains(job.Message, "尚未入库") || strings.Contains(job.Message, "已入库") {
		t.Fatal(downloads.Load(), job)
	}
	matches, err := filepath.Glob(filepath.Join(application.Dir, "torrents", "*.torrent"))
	if err != nil || len(matches) != 1 {
		t.Fatal(matches, err)
	}
}

func TestUnknownBalanceDoesNotRequestATokenDownload(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "download" {
			downloads.Add(1)
		}
		io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
	}))
	defer server.Close()
	application := newApp(t)
	body := searchBody("album-token", server.URL, []string{"dicmusic"}, false)
	body["token_cost"] = 1
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-token", "needs_review")
	if downloads.Load() != 0 || job.ReviewKind != "token" || !strings.Contains(job.Message, "尚未下载") {
		t.Fatal(downloads.Load(), job)
	}
}

func TestTokenFetchStaysUnreconciled(t *testing.T) {
	torrent := []byte("d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "index" {
			if r.URL.Query().Get("usetoken") != "" {
				t.Fatal("balance read used a token")
			}
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
			return
		}
		if r.URL.Query().Get("action") == "download" {
			query = r.URL.RawQuery
			w.Write(torrent)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
	}))
	defer server.Close()
	application := newApp(t)
	body := searchBody("album-spend", server.URL, []string{"dicmusic"}, false)
	body["token_cost"] = 1
	body["balance_known"] = true
	body["balance"] = 99
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-spend", "needs_review")
	parsed, err := url.ParseQuery(query)
	if err != nil || parsed.Get("action") != "download" || parsed.Get("id") != "11" || parsed.Get("usetoken") != "1" || parsed.Get("authkey") != "sentinel-dic-auth" || parsed.Get("torrent_pass") != "" || job.ErrorClass != "REQUEST_UNKNOWN" || job.ReviewKind != "token" || !strings.Contains(job.Message, "尚未下载") {
		t.Fatal(job.ErrorClass, job.ReviewKind, job.Message)
	}
	if dirContains(t, application.Dir, "sentinel-dic-auth") {
		t.Fatal("download authenticator was written")
	}
	matches, err := filepath.Glob(filepath.Join(application.Dir, "torrents", "*.torrent"))
	if err != nil || len(matches) != 1 {
		t.Fatal(matches, err)
	}
}

func newApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	application, err := Open(filepath.Join(dir, "state"), filepath.Join(dir, "backups"), "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(application.Backup, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	return application
}

func searchBody(jobID, base string, sites []string, streaming bool) map[string]any {
	return map[string]any{
		"job_id": jobID, "batch_id": "batch-1", "scene": "playlist", "query": "Artist Album",
		"expected_title": "Example & Test", "source_track_ids": []string{"track-1"},
		"enabled_sites": sites, "streaming_enabled": streaming, "synthetic": true, "base_url": base,
		"cookie":   "session=cookie-secret-not-stored",
		"identity": map[string]any{"evidence_id": "ev-catalog", "kind": "catalog", "source": "musicbrainz", "subject_id": "release-1", "tracklist_verified": true, "identity_confirmed": true, "payload": "catalogue-row"},
	}
}

func post(t *testing.T, application *App, path string, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.SetBasicAuth("musicops", "test-password-16b")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted && recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func waitJob(t *testing.T, application *App, id, stage string) workflow.Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last workflow.Inspection
	for time.Now().Before(deadline) {
		last = inspect(t, application, id)
		if last.Job.Stage == stage {
			return last.Job
		}
		if last.Job.Stage == "failed" || last.Job.Stage == "needs_review" && stage != "needs_review" {
			t.Fatal(last.Job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(id, last.Job)
	return workflow.Job{}
}

func inspect(t *testing.T, application *App, id string) workflow.Inspection {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+id, nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusNotFound {
		return workflow.Inspection{}
	}
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Inspection workflow.Inspection `json:"inspection"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Inspection
}

func dirContains(t *testing.T, dir, needle string) bool {
	t.Helper()
	found := false
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil && bytes.Contains(data, []byte(needle)) {
			found = true
		}
		return nil
	})
	return found
}
