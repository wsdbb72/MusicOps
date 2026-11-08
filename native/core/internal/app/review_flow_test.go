package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
)

func TestNormalizedAndSameReleaseCanQueueWithoutAModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-MusicOps-Site") {
		case "dicmusic":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Ｅｘａｍｐｌｅ　＆　Ｔｅｓｔ","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
		case "redacted":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":12,"groupName":"Example & Test","artist":"Artist","torrents":[{"torrentId":13,"format":"FLAC","encoding":"Lossless","seeders":2}]}]}}`)
		case "open.cd":
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
		default:
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	var calls atomic.Int32
	application.reviewModel = reviewCaller{calls: &calls, body: []byte(`{"decision":"pass","candidate_id":"dicmusic:11","reason":"unused"}`)}
	body := searchBody("album-fold", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-fold", "completed")
	if job.SearchAction != "queue_request" || calls.Load() != 0 {
		t.Fatal(job.SearchAction, calls.Load())
	}
}

func TestEditionArtistAndIncompleteEvidenceStayInReview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") == "dicmusic" {
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example & Test Live","artist":"Tribute Band","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":4,"remasterTitle":"Live"}]}]}}`)
			return
		}
		if r.Header.Get("X-MusicOps-Site") == "open.cd" {
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()
	application := newApp(t)
	calls := atomic.Int32{}
	application.reviewModel = reviewCaller{calls: &calls, err: context.DeadlineExceeded}
	body := searchBody("album-live", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	body["expected_artist"] = "Artist"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-live", "needs_review")
	if job.SearchAction == "queue_request" || !strings.Contains(job.Message, "暂时没有完成自动核对") || calls.Load() == 0 {
		t.Fatal(job.SearchAction, job.Message, calls.Load())
	}
}

func TestSecondSearchPageCanFindTheRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") != "dicmusic" {
			if r.Header.Get("X-MusicOps-Site") == "open.cd" {
				io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
				return
			}
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			io.WriteString(w, `{"status":"success","response":{"currentPage":2,"pages":2,"results":[{"groupId":10,"groupName":"Example & Test","artist":"Artist","aliases":["Example and Test"],"torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":2,"results":[{"groupId":9,"groupName":"Something Else","artist":"Artist","torrents":[{"torrentId":8,"format":"FLAC","encoding":"Lossless","seeders":1}]}]}}`)
	}))
	defer server.Close()
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("album-page", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false))
	job := waitJob(t, application, "search:album-page", "completed")
	if job.SearchAction != "queue_request" {
		t.Fatal(job)
	}
}

func TestModelPassCannotOverrideMissingEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") == "dicmusic" {
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"蓝 Live","artist":"Artist","aliases":["Blue"],"torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
			return
		}
		if r.Header.Get("X-MusicOps-Site") == "open.cd" {
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()
	application := newApp(t)
	calls := atomic.Int32{}
	application.reviewModel = reviewCaller{calls: &calls, body: []byte(`{"decision":"pass","candidate_id":"dicmusic:11","reason":"sounds the same"}`)}
	body := searchBody("album-hallucination", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	body["expected_title"] = "Blue"
	body["expected_artist"] = "Artist"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-hallucination", "needs_review")
	if job.SearchAction == "queue_request" || calls.Load() == 0 || strings.Contains(job.Message, "已入库") {
		t.Fatal(job.SearchAction, job.Message, calls.Load())
	}
}

type reviewCaller struct {
	calls *atomic.Int32
	body  []byte
	err   error
}

func (c reviewCaller) Complete(context.Context, []byte) ([]byte, error) {
	if c.calls != nil {
		c.calls.Add(1)
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.body, nil
}

var _ reviewai.Completer = reviewCaller{}
