package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sampleTorrent = "d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee"

func TestHighSiteBalanceReservesAndHonorsTheBatchBudget(t *testing.T) {
	var downloads atomic.Int32
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		values := r.URL.Query()
		switch {
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":252}}}`)
		case values.Get("action") == "browse" && values.Get("searchstr") == "Album B":
			io.WriteString(w, gazelleHit("22"))
		case values.Get("action") == "download":
			downloads.Add(1)
			query = r.URL.RawQuery
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	first := tokenBody("album-high", server.URL, []string{"dicmusic"})
	first["budget"] = 1
	first["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", first)
	job := waitJob(t, application, "process:search:album-high", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" || !strings.Contains(query, "usetoken=1") || strings.Contains(query, "torrent_pass=") {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message, query)
	}
	confirmed := postRecheck(t, application, map[string]any{
		"site": "dicmusic",
		"proof": map[string]any{
			"contract": "musicops.fixture.v1", "state": "CONSUMED", "account_id": "7",
			"resource_id": "11", "request_id": "process:search:album-high",
		},
	})
	if nested(t, confirmed, "reservation")["state"] != "CONFIRMED" {
		t.Fatal(confirmed)
	}
	second := tokenBody("album-high-2", server.URL, []string{"dicmusic"})
	second["query"] = "Album B"
	second["budget"] = 1
	second["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", second)
	again := waitJob(t, application, "process:search:album-high-2", "needs_review")
	if downloads.Load() != 1 || again.ErrorClass != "token_budget" || !strings.Contains(again.Message, "未向站点请求") {
		t.Fatal(downloads.Load(), again.ErrorClass, again.Message)
	}
}

func TestCallerBalanceCannotAuthorizeOrBlockTheSiteBalance(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		switch r.URL.Query().Get("action") {
		case "index":
			if r.URL.Query().Get("usetoken") != "" {
				t.Error("balance read used a token")
			}
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":0}}}`)
		case "download":
			downloads.Add(1)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-zero", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["balance_known"] = true
	body["balance"] = 99
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-zero", "needs_review")
	if downloads.Load() != 0 || job.ErrorClass != "token_budget" || !strings.Contains(job.Message, "未向站点请求") {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message)
	}
}

func TestLostTokenResponseStaysUnknownUntilACorrelatedRecord(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		switch r.URL.Query().Get("action") {
		case "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"personal":{"freeleechTokens":4}}}`)
		case "download":
			downloads.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-lost", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["balance"] = 99
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-lost", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" || job.ReviewKind != "token" {
		t.Fatal(downloads.Load(), job.ErrorClass, job.ReviewKind, job.Message)
	}
	bad := postRecheck(t, application, map[string]any{
		"site": "dicmusic", "request_id": "process:search:album-lost",
		"proof": map[string]any{"contract": "musicops.fixture.v1", "state": "CONSUMED", "account_id": "7", "resource_id": "11"},
	})
	if bad["downloaded"] != false || bad["site_read"] != false || nested(t, bad, "reservation")["class"] != "token_uncertain" {
		t.Fatal(bad)
	}
	wrong := postRecheck(t, application, map[string]any{
		"site": "dicmusic",
		"proof": map[string]any{
			"contract": "musicops.fixture.v1", "state": "CONSUMED", "account_id": "7", "resource_id": "22", "request_id": "process:search:album-lost",
		},
	})
	if nested(t, wrong, "reservation")["class"] != "token_uncertain" || downloads.Load() != 1 {
		t.Fatal(wrong, downloads.Load())
	}
	still := postRecheck(t, application, map[string]any{"site": "dicmusic", "request_id": "process:search:album-lost"})
	if nested(t, still, "reservation")["state"] != "REQUEST_UNKNOWN" || nested(t, still, "reservation")["downloaded"] != false {
		t.Fatal(still)
	}
	confirmed := postRecheck(t, application, map[string]any{
		"site": "dicmusic",
		"proof": map[string]any{
			"contract": "musicops.fixture.v1", "state": "CONSUMED", "account_id": "7", "resource_id": "11", "request_id": "process:search:album-lost",
		},
	})
	if nested(t, confirmed, "reservation")["state"] != "CONFIRMED" || confirmed["downloaded"] != false || confirmed["production_verified"] != false || downloads.Load() != 1 {
		t.Fatal(confirmed, downloads.Load())
	}
	opposite := postRecheck(t, application, map[string]any{
		"site": "dicmusic",
		"proof": map[string]any{
			"contract": "musicops.fixture.v1", "state": "NOT_CONSUMED", "account_id": "7", "resource_id": "11", "request_id": "process:search:album-lost",
		},
	})
	if nested(t, opposite, "reservation")["state"] != "CONFIRMED" || downloads.Load() != 1 {
		t.Fatal(opposite, downloads.Load())
	}
	again := tokenBody("album-lost-again", server.URL, []string{"dicmusic"})
	again["token_cost"] = 1
	again["balance"] = 99
	again["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", again)
	second := waitJob(t, application, "process:search:album-lost-again", "needs_review")
	if downloads.Load() != 1 || second.ErrorClass != "token_spent" || !strings.Contains(second.Message, "未向站点请求") {
		t.Fatal(downloads.Load(), second.ErrorClass, second.Message)
	}
}

func TestNotConsumedRecordAllowsOneNewTokenRequest(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		switch r.URL.Query().Get("action") {
		case "index":
			io.WriteString(w, `{"status":"success","response":{"id":8,"stats":{"tokens":2}}}`)
		case "download":
			if r.URL.Query().Get("torrent_pass") == "" || r.URL.Query().Get("authkey") != "" || r.URL.Query().Get("usetoken") != "1" {
				t.Error(r.URL.RawQuery)
			}
			downloads.Add(1)
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-release", server.URL, []string{"orpheus"})
	body["token_cost"] = 1
	body["balance"] = 0
	body["orpheus_torrent_pass"] = "sentinel-orpheus-pass"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-release", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message)
	}
	released := postRecheck(t, application, map[string]any{
		"site": "orpheus",
		"proof": map[string]any{
			"contract": "musicops.fixture.v1", "state": "NOT_CONSUMED", "account_id": "8", "resource_id": "11", "request_id": "process:search:album-release",
		},
	})
	if nested(t, released, "reservation")["state"] != "RELEASED" || downloads.Load() != 1 {
		t.Fatal(released, downloads.Load())
	}
	again := tokenBody("album-release-2", server.URL, []string{"orpheus"})
	again["token_cost"] = 1
	again["orpheus_torrent_pass"] = "sentinel-orpheus-pass"
	post(t, application, "/v1/search", again)
	second := waitJob(t, application, "process:search:album-release-2", "needs_review")
	if downloads.Load() != 2 || second.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(downloads.Load(), second.ErrorClass, second.Message)
	}
}

func TestUnreconciledDicmusicDoesNotBlockOtherSites(t *testing.T) {
	var dicBrowses atomic.Int32
	var dicDownloads atomic.Int32
	var orpheusDownloads atomic.Int32
	var openDownloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		site := r.Header.Get("X-MusicOps-Site")
		switch {
		case r.URL.Query().Get("action") == "index" && site == "dicmusic":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case r.URL.Query().Get("action") == "index" && site == "orpheus":
			io.WriteString(w, `{"status":"success","response":{"id":8,"userstats":{"tokens":3}}}`)
		case r.URL.Query().Get("action") == "download" && site == "dicmusic":
			dicDownloads.Add(1)
			io.WriteString(w, sampleTorrent)
		case r.URL.Query().Get("action") == "download" && site == "orpheus":
			if r.URL.Query().Get("authkey") != "" {
				t.Error("orpheus received a dicmusic parameter")
			}
			orpheusDownloads.Add(1)
			io.WriteString(w, sampleTorrent)
		case r.URL.Path == "/details.php":
			io.WriteString(w, `<tr id="torrent11"><td><strong class="tl_free">免费</strong></td></tr>`)
		case r.URL.Path == "/download.php":
			if r.URL.Query().Get("usetoken") != "" {
				t.Error("free download used a token")
			}
			openDownloads.Add(1)
			io.WriteString(w, sampleTorrent)
		case site == "dicmusic":
			id := "11"
			if dicBrowses.Add(1) > 1 {
				id = "22"
			}
			io.WriteString(w, gazelleHit(id))
		case site == "orpheus":
			io.WriteString(w, gazelleHit("31"))
		default:
			io.WriteString(w, `<html><a href="logout.php">logout</a><table><tr><td><table><tr><td><a href="details.php?id=11" title="Album">Album</a></td></tr></table></td><td>100 MiB</td></tr></table></html>`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	first := tokenBody("album-hold", server.URL, []string{"dicmusic"})
	first["token_cost"] = 1
	first["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", first)
	held := waitJob(t, application, "process:search:album-hold", "needs_review")
	if dicDownloads.Load() != 1 || held.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(dicDownloads.Load(), held.ErrorClass, held.Message)
	}
	second := tokenBody("album-other", server.URL, []string{"dicmusic"})
	second["token_cost"] = 1
	second["balance"] = 99
	second["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", second)
	other := waitJob(t, application, "process:search:album-other", "needs_review")
	if dicDownloads.Load() != 1 || other.ErrorClass != "account_token_hold" {
		t.Fatal(dicDownloads.Load(), other.ErrorClass, other.Message)
	}
	orpheus := tokenBody("album-orpheus", server.URL, []string{"orpheus"})
	orpheus["token_cost"] = 1
	orpheus["orpheus_torrent_pass"] = "sentinel-orpheus-pass"
	post(t, application, "/v1/search", orpheus)
	orpheusJob := waitJob(t, application, "process:search:album-orpheus", "needs_review")
	if orpheusDownloads.Load() != 1 || dicDownloads.Load() != 1 || orpheusJob.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(orpheusDownloads.Load(), dicDownloads.Load(), orpheusJob.ErrorClass)
	}
	open := tokenBody("album-open", server.URL, []string{"open.cd"})
	open["expected_title"] = "Album"
	open["free_confirmed"] = true
	post(t, application, "/v1/search", open)
	openJob := waitJob(t, application, "process:search:album-open", "needs_review")
	if openDownloads.Load() != 1 || dicDownloads.Load() != 1 || openJob.ReviewKind != "candidate" {
		t.Fatal(openDownloads.Load(), dicDownloads.Load(), openJob.ReviewKind, openJob.Message)
	}
}

func TestRateLimitRecheckDoesNotDownloadAndThenRestoresTheRead(t *testing.T) {
	var index atomic.Int32
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		switch r.URL.Query().Get("action") {
		case "index":
			if index.Add(1) == 1 {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case "download":
			downloads.Add(1)
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	clock := time.Now()
	application.now = func() time.Time { return clock }
	body := tokenBody("album-limit-token", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["balance"] = 99
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-limit-token", "needs_review")
	if downloads.Load() != 0 || index.Load() != 1 || job.ErrorClass != "rate_limited" || !strings.Contains(job.Message, "未发令牌请求") {
		t.Fatal(downloads.Load(), index.Load(), job.ErrorClass, job.Message)
	}
	early := postRecheck(t, application, map[string]any{
		"site": "dicmusic", "account_id": "7", "cookie": "session=cookie-secret-not-stored", "synthetic": true, "base_url": server.URL,
	})
	if early["site_read"] != false || index.Load() != 1 || nested(t, early, "observation")["error_class"] != "rate_limited" || downloads.Load() != 0 {
		t.Fatal(early, index.Load())
	}
	clock = clock.Add(31 * time.Second)
	restored := postRecheck(t, application, map[string]any{
		"site": "dicmusic", "cookie": "session=cookie-secret-not-stored", "synthetic": true, "base_url": server.URL,
	})
	observation := nested(t, restored, "observation")
	if restored["site_read"] != true || index.Load() != 2 || observation["known"] != true || observation["balance"] != float64(4) || observation["account_id"] != "7" || downloads.Load() != 0 || restored["production_verified"] != false {
		t.Fatal(restored, index.Load(), downloads.Load())
	}
	again := tokenBody("album-limit-restored", server.URL, []string{"dicmusic"})
	again["token_cost"] = 1
	again["balance"] = 0
	again["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", again)
	spent := waitJob(t, application, "process:search:album-limit-restored", "needs_review")
	if downloads.Load() != 1 || spent.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(downloads.Load(), spent.ErrorClass, spent.Message)
	}
}

func TestBalanceReadRetriesOnceAndDoesNotUseTheCallerBalance(t *testing.T) {
	var index atomic.Int32
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		if r.URL.Query().Get("action") != "index" {
			if r.URL.Query().Get("action") == "download" {
				downloads.Add(1)
			} else {
				io.WriteString(w, gazelleHit("11"))
			}
			return
		}
		if index.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":0}}}`)
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-retry", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["balance"] = 99
	body["balance_known"] = true
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-retry", "needs_review")
	if index.Load() != 2 || downloads.Load() != 0 || job.ErrorClass != "token_budget" {
		t.Fatal(index.Load(), downloads.Load(), job.ErrorClass, job.Message)
	}
}

func TestTokenReservationSurvivesRestartWithoutAnotherDownload(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		switch r.URL.Query().Get("action") {
		case "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case "download":
			downloads.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := os.MkdirAll(root+"/backups", 0o700); err != nil {
		t.Fatal(err)
	}
	application, err := Open(root+"/state", root+"/backups", "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	body := tokenBody("album-restart", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-restart", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message)
	}
	if err = application.Close(); err != nil {
		t.Fatal(err)
	}
	application, err = Open(root+"/state", root+"/backups", "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	kept := postRecheck(t, application, map[string]any{"site": "dicmusic", "request_id": "process:search:album-restart"})
	if nested(t, kept, "reservation")["state"] != "REQUEST_UNKNOWN" || kept["downloaded"] != false || downloads.Load() != 1 {
		t.Fatal(kept, downloads.Load())
	}
	again := tokenBody("album-restart-2", server.URL, []string{"dicmusic"})
	again["token_cost"] = 1
	again["balance"] = 99
	again["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", again)
	second := waitJob(t, application, "process:search:album-restart-2", "needs_review")
	if downloads.Load() != 1 || second.ErrorClass != "token_pending" {
		t.Fatal(downloads.Load(), second.ErrorClass, second.Message)
	}
}

func TestPageButtonChoosesTheTokenCost(t *testing.T) {
	var downloads atomic.Int32
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"11","isPersonalFreeleech":false,"isFreeleech":true}}}`)
		case values.Get("torrentid") == "11":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=11&amp;usetoken=1" title="Use 3 tokens">FL (3)</a>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case values.Get("action") == "download":
			downloads.Add(1)
			query = r.URL.RawQuery
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-button", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["free_confirmed"] = true
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-button", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" || !strings.Contains(query, "usetoken=1") || strings.Contains(query, "torrent_pass=") {
		t.Fatal(downloads.Load(), job.ErrorClass, query)
	}
	state := postRecheck(t, application, map[string]any{"site": "dicmusic", "request_id": "process:search:album-button"})
	if nested(t, state, "reservation")["cost"] != float64(3) || nested(t, state, "reservation")["downloaded"] != false {
		t.Fatal(state)
	}
}

func TestPersonalRowDoesNotSpendACallerToken(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"11","isPersonalFreeleech":false}}}`)
		case values.Get("torrentid") == "11":
			io.WriteString(w, `<tr id="torrent11"><td><strong class="tl_free" title="个人免费">个人免费</strong></td></tr>`)
		case values.Get("action") == "download":
			downloads.Add(1)
			if values.Get("usetoken") != "" {
				t.Error("personal free used a token")
			}
			io.WriteString(w, sampleTorrent)
		case values.Get("action") == "index":
			t.Error("personal free read the token balance")
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-personal", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-personal", "needs_review")
	if downloads.Load() != 1 || job.ReviewKind != "candidate" || job.ErrorClass != "" {
		t.Fatal(downloads.Load(), job.ReviewKind, job.ErrorClass, job.Message)
	}
}

func TestGlobalFlagAndCallerFreeDoNotSpendOrDownload(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"11","isFreeleech":true,"isPersonalFreeleech":false}}}`)
		case values.Get("torrentid") == "11":
			io.WriteString(w, `<div>普通下载</div>`)
		case values.Get("action") == "download" || strings.HasPrefix(r.URL.Path, "/download.php"):
			downloads.Add(1)
		default:
			if r.URL.Path == "/details.php" {
				io.WriteString(w, `<div>没有免费标记</div>`)
				return
			}
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-global", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["free_confirmed"] = true
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-global", "needs_review")
	if downloads.Load() != 0 || job.ErrorClass != "evidence" {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message)
	}
}

func TestOpenCDFreeFlagWithoutARowDoesNotDownload(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/download.php") {
			downloads.Add(1)
			return
		}
		if r.URL.Path == "/details.php" {
			io.WriteString(w, `<div>没有免费标记</div>`)
			return
		}
		io.WriteString(w, `<html><a href="logout.php">logout</a><table><tr><td><table><tr><td><a href="details.php?id=11" title="Album">Album</a></td></tr></table></td><td>100 MiB</td></tr></table></html>`)
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-open-flag", server.URL, []string{"open.cd"})
	body["expected_title"] = "Album"
	body["free_confirmed"] = true
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-open-flag", "needs_review")
	if downloads.Load() != 0 || !strings.Contains(job.Message, "未发") {
		t.Fatal(downloads.Load(), job.ReviewKind, job.ErrorClass, job.Message)
	}
}

func TestPageBalanceDisagreementDoesNotSpend(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"11","isPersonalFreeleech":false}}}`)
		case values.Get("torrentid") == "11":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=11&amp;usetoken=1">FL</a><span>令牌: 9</span>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case values.Get("action") == "download":
			downloads.Add(1)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-conflict", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["balance"] = 9
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-conflict", "needs_review")
	if downloads.Load() != 0 || job.ErrorClass != "balance_conflict" || !strings.Contains(job.Message, "未发令牌请求") {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message)
	}
}

func TestLiveRecheckIsNotSent(t *testing.T) {
	application := newApp(t)
	raw := map[string]any{"site": "dicmusic", "base_url": "https://dicmusic.com", "synthetic": false, "cookie": "session=cookie-secret-not-stored"}
	recorder := postRaw(t, application, "/v1/tokens/recheck", raw)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "LIVE_SITE_NOT_CALLED") || strings.Contains(recorder.Body.String(), "cookie-secret") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestSummedPageBalanceMatchesTheAccountAndStillSpends(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+values.Get("id")+`","isPersonalFreeleech":false,"isFreeleech":false}}}`)
		case values.Get("torrentid") != "":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=`+values.Get("torrentid")+`&amp;usetoken=1">FL</a><span>令牌: 0+1</span>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":1}}}`)
		case values.Get("action") == "download":
			downloads.Add(1)
			if values.Get("usetoken") != "1" || values.Get("authkey") == "" {
				t.Error(r.URL.RawQuery)
			}
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-sum", server.URL, []string{"dicmusic"})
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-sum", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" || job.ReviewKind != "token" {
		t.Fatal(downloads.Load(), job.ErrorClass, job.ReviewKind, job.Message)
	}
}

func TestProfileOrPageBalanceIsUsedWhenTheIndexOmitsTokens(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+values.Get("id")+`","isPersonalFreeleech":false}}}`)
		case values.Get("torrentid") != "":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=`+values.Get("torrentid")+`&amp;usetoken=1">FL</a>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"username":"listener"}}`)
		case values.Get("action") == "user":
			if values.Get("id") != "7" {
				t.Error(values.Get("id"))
			}
			io.WriteString(w, `{"status":"success","response":{"id":7,"personal":{"freeleechTokens":3}}}`)
		case values.Get("action") == "download":
			downloads.Add(1)
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-profile", server.URL, []string{"dicmusic"})
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-profile", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message)
	}
}

func TestPageTotalBindsToTheAccountWhenNeitherDocumentHasTokens(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+values.Get("id")+`","isPersonalFreeleech":false}}}`)
		case values.Get("torrentid") != "":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=`+values.Get("torrentid")+`&amp;usetoken=1">FL</a><span>令牌: 0+1</span>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":8,"username":"listener"}}`)
		case values.Get("action") == "user":
			io.WriteString(w, `{"status":"success","response":{"id":8,"personal":{}}}`)
		case values.Get("action") == "download":
			downloads.Add(1)
			if values.Get("usetoken") != "1" {
				t.Error("page balance did not authorize the token request")
			}
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-page", server.URL, []string{"orpheus"})
	body["orpheus_torrent_pass"] = "sentinel-orpheus-pass"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-page", "needs_review")
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" || strings.Contains(job.Message, "未发令牌请求") {
		t.Fatal(downloads.Load(), job.ErrorClass, job.Message)
	}
}

func TestPersonalFreeAfterTheTokenRequestConfirmsAndReleasesTheAccount(t *testing.T) {
	var tokenUses atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			personal := values.Get("id") == "11" && tokenUses.Load() > 0
			flag := "false"
			if personal {
				flag = "true"
			}
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+values.Get("id")+`","isPersonalFreeleech":`+flag+`,"isFreeleech":false}}}`)
		case values.Get("torrentid") != "":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=`+values.Get("torrentid")+`&amp;usetoken=1">FL</a>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case values.Get("action") == "download":
			if values.Get("usetoken") == "1" {
				tokenUses.Add(1)
			} else {
				t.Error("confirmation fetched the torrent again with a token")
			}
			io.WriteString(w, sampleTorrent)
		case values.Get("searchstr") == "Album B":
			io.WriteString(w, gazelleHit("22"))
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-settled", server.URL, []string{"dicmusic"})
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-settled", "needs_review")
	if tokenUses.Load() != 1 || job.ReviewKind != "candidate" || job.ErrorClass != "" || !strings.Contains(job.Message, "尚未入库") {
		t.Fatal(tokenUses.Load(), job.ReviewKind, job.ErrorClass, job.Message)
	}
	state := postRecheck(t, application, map[string]any{"site": "dicmusic", "request_id": "process:search:album-settled"})
	if nested(t, state, "reservation")["state"] != "CONFIRMED" || state["downloaded"] != false {
		t.Fatal(state)
	}
	again := tokenBody("album-settled-2", server.URL, []string{"dicmusic"})
	again["query"] = "Album B"
	again["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", again)
	second := waitJob(t, application, "process:search:album-settled-2", "needs_review")
	if tokenUses.Load() != 2 || second.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(tokenUses.Load(), second.ErrorClass, second.Message)
	}
}

func TestTokenHTTPRejectionDoesNotBanTheSiteOrRetry(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+values.Get("id")+`","isPersonalFreeleech":false,"isFreeleech":true}}}`)
		case values.Get("torrentid") != "":
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=`+values.Get("torrentid")+`&amp;usetoken=1">FL</a>`)
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case values.Get("action") == "download":
			downloads.Add(1)
			io.WriteString(w, `<html><body>站点拒绝这次下载</body></html>`)
		case values.Get("searchstr") == "Album B":
			io.WriteString(w, gazelleHit("22"))
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-rejected", server.URL, []string{"dicmusic"})
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-rejected", "needs_review")
	allowed, class, _ := application.ledger.ReadAllowed("dicmusic", time.Now())
	if downloads.Load() != 1 || job.ErrorClass != "REQUEST_UNKNOWN" || job.ReviewKind != "token" || !allowed || class != "" {
		t.Fatal(downloads.Load(), job.ErrorClass, job.ReviewKind, allowed, class, job.Message)
	}
	again := tokenBody("album-rejected-2", server.URL, []string{"dicmusic"})
	again["query"] = "Album B"
	again["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", again)
	second := waitJob(t, application, "process:search:album-rejected-2", "needs_review")
	if downloads.Load() != 1 || second.ErrorClass != "account_token_hold" || !strings.Contains(second.Message, "未向站点请求") {
		t.Fatal(downloads.Load(), second.ErrorClass, second.Message)
	}
}

func tokenBody(jobID, base string, sites []string) map[string]any {
	body := searchBody(jobID, base, sites, false)
	body["identity"] = map[string]any{
		"evidence_id": "ev-" + jobID, "kind": "catalog", "source": "musicbrainz", "subject_id": "release-" + jobID,
		"tracklist_verified": true, "identity_confirmed": true, "payload": "catalogue-row",
	}
	return body
}

func noteTokenEvidence(w http.ResponseWriter, r *http.Request) bool {
	query := r.URL.Query()
	switch {
	case query.Get("action") == "torrent" && query.Get("id") != "":
		io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+query.Get("id")+`","isPersonalFreeleech":false,"isFreeleech":false}}}`)
		return true
	case query.Get("action") == "" && query.Get("torrentid") != "" && query.Get("id") != "":
		io.WriteString(w, `<a href="torrents.php?action=download&amp;id=`+query.Get("torrentid")+`&amp;usetoken=1">FL</a>`)
		return true
	default:
		return false
	}
}

func gazelleHit(id string) string {
	return `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":` + id + `,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`
}

func postRecheck(t *testing.T, application *App, body map[string]any) map[string]any {
	t.Helper()
	recorder := postRaw(t, application, "/v1/tokens/recheck", body)
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "cookie-secret") || strings.Contains(recorder.Body.String(), "sentinel-") {
		t.Fatal(recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func postRaw(t *testing.T, application *App, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	request.SetBasicAuth("musicops", "test-password-16b")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	return recorder
}

func nested(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := body[key].(map[string]any)
	if !ok {
		t.Fatal(key, body)
	}
	return value
}
