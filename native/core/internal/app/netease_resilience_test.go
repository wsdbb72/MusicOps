package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
)

func TestVerifiedNeteaseDurationToleratesListingDriftButRejectsPreview(t *testing.T) {
	tests := []struct {
		name     string
		drift    int64
		verified bool
		want     bool
	}{
		{name: "small drift from verified catalog id", drift: 4500, verified: true, want: true},
		{name: "small drift from fuzzy search is not enough", drift: 4500, verified: false, want: false},
		{name: "thirty second preview boundary", drift: 30000, verified: true, want: true},
		{name: "preview rejected", drift: 30001, verified: true, want: false},
		{name: "negative drift uses magnitude", drift: -7000, verified: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := neteaseDurationAccepted(test.drift, test.verified); got != test.want {
				t.Fatalf("neteaseDurationAccepted(%d, %v)=%v, want %v", test.drift, test.verified, got, test.want)
			}
		})
	}
}

func TestCoverTitleEquivalentHandlesPunctuationAndUnicodeWidth(t *testing.T) {
	tests := [][2]string{
		{"It's Ok", "Its OK"},
		{"ＬＯＶＥ", "love"},
		{"恋愛裁判", "恋愛裁判 (Love Trial)"},
	}
	if !coverTitleEquivalent(tests[0][0], tests[0][1]) {
		t.Fatal("ASCII punctuation changed cover title identity")
	}
	if !coverTitleEquivalent(tests[1][0], tests[1][1]) {
		t.Fatal("full-width text did not match its folded identity")
	}
	if coverTitleEquivalent(tests[2][0], tests[2][1]) {
		t.Fatal("a version suffix must not collapse to the base recording")
	}
}

func TestDownloadExternalCoverRejectsArbitraryHosts(t *testing.T) {
	if _, err := downloadExternalCover(context.Background(), "https://127.0.0.1/private.jpg"); err == nil {
		t.Fatal("arbitrary cover host was accepted")
	}
}

func TestNeteaseMediaQualityLadderFallsBackPastUnavailableLossless(t *testing.T) {
	vaultKey(t)
	var levels []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/song/enhance/player/url/v1" {
			http.NotFound(w, r)
			return
		}
		level := r.URL.Query().Get("level")
		levels = append(levels, level)
		data := []map[string]any{}
		if level == "exhigh" {
			data = append(data, map[string]any{"url": server.URL + "/track.mp3", "type": "mp3", "code": 200, "level": level})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	a := newApp(t)
	a.neteaseBases = map[string]string{"api": server.URL}
	saveNeteaseTestCookie(t, a)
	got, kind, err := a.neteaseURLFor(context.Background(), &neteaseSong{ID: 7, CatalogVerified: true})
	if err != nil || got == "" || kind != "mp3" {
		t.Fatalf("url=%q type=%q levels=%v err=%v", got, kind, levels, err)
	}
	if len(levels) != 3 || levels[0] != "lossless" || levels[1] != "hires" || levels[2] != "exhigh" {
		t.Fatalf("quality ladder unexpectedly stopped: %v", levels)
	}
}

func TestNeteaseHTTPFailureRemainsRetryable(t *testing.T) {
	vaultKey(t)
	a := newApp(t)
	saveNeteaseTestCookie(t, a)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	a.neteaseBases = map[string]string{"api": server.URL}
	_, note, class, handled := a.tryNeteaseStream(context.Background(), SearchPlan{ExpectedArtist: "Artist", PlaylistTrack: "Song"})
	if !handled || class != downloadpolicy.ClassNetwork || !strings.Contains(note, "503") {
		t.Fatalf("handled=%v class=%q note=%q", handled, class, note)
	}
}

func TestNeteaseAuthenticationFailureStaysInReview(t *testing.T) {
	vaultKey(t)
	a := newApp(t)
	saveNeteaseTestCookie(t, a)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired session", http.StatusUnauthorized)
	}))
	defer server.Close()
	a.neteaseBases = map[string]string{"api": server.URL}
	_, _, class, handled := a.tryNeteaseStream(context.Background(), SearchPlan{ExpectedArtist: "Artist", PlaylistTrack: "Song"})
	if !handled || class != downloadpolicy.ClassAuth {
		t.Fatalf("handled=%v class=%q", handled, class)
	}
}

func TestNeteaseRateLimitStopsTheQualityLadder(t *testing.T) {
	vaultKey(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	a := newApp(t)
	saveNeteaseTestCookie(t, a)
	a.neteaseBases = map[string]string{"api": server.URL}
	_, _, err := a.neteaseURLFor(context.Background(), &neteaseSong{ID: 7, CatalogVerified: true})
	if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassRateLimited || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}
func TestNeteaseTransientFailureAfterEmptyQualityStillRetries(t *testing.T) {
	vaultKey(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	a := newApp(t)
	saveNeteaseTestCookie(t, a)
	a.neteaseBases = map[string]string{"api": server.URL}
	_, _, err := a.neteaseURLFor(context.Background(), &neteaseSong{ID: 7, CatalogVerified: true})
	if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassNetwork || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}
func TestNeteaseUnexpectedHTMLIsRetryable(t *testing.T) {
	if got := classifyNeteaseFailure(errNeteaseUnexpectedHTML); got != downloadpolicy.ClassNetwork {
		t.Fatalf("HTML/anti-bot response class = %q", got)
	}
}

func TestNeteaseCancelAndMalformedJSONStayRetryable(t *testing.T) {
	if got := classifyNeteaseFailure(context.Canceled); got != downloadpolicy.ClassNetwork {
		t.Fatalf("cancellation class = %q; cancellation must not become a terminal no-result", got)
	}
	for _, body := range []string{"{\"data\":["} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			a := newApp(t)
			_, err := a.neteaseGet(context.Background(), server.URL+"/api", "")
			if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassNetwork {
				t.Fatalf("malformed response err=%v class=%q", err, classifyNeteaseFailure(err))
			}
		})
	}
}

func TestNeteaseGetRejectsNonObjectJSONAsRetryable(t *testing.T) {
	for _, body := range []string{"null", "[]", `"ok"`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			a := newApp(t)
			_, err := a.neteaseGet(context.Background(), server.URL+"/api", "")
			if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassNetwork {
				t.Fatalf("non-object JSON err=%v class=%q", err, classifyNeteaseFailure(err))
			}
		})
	}
}

func TestNeteaseSearchMissingShapeIsRetryableButEmptySongsIsNoResult(t *testing.T) {
	cases := []struct {
		name string
		body string
		none bool
	}{
		{name: "empty object", body: `{}`},
		{name: "code without result", body: `{"code":200}`},
		{name: "null result", body: `{"result":null}`},
		{name: "missing songs", body: `{"result":{}}`},
		{name: "null songs", body: `{"result":{"songs":null}}`},
		{name: "explicit empty songs", body: `{"result":{"songs":[]}}`, none: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			a := newApp(t)
			a.neteaseBases = map[string]string{"api": server.URL}
			song, err := a.neteaseSearchSong(context.Background(), SearchPlan{ExpectedTitle: "Song", ExpectedArtist: "Artist"})
			if test.none {
				if err != nil || song != nil {
					t.Fatalf("explicit empty songs should be a clean no-result: song=%+v err=%v", song, err)
				}
				return
			}
			if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassNetwork {
				t.Fatalf("missing contract err=%v class=%q", err, classifyNeteaseFailure(err))
			}
		})
	}
}

func TestNeteaseAudioAndCoverMissingShapeAreRetryable(t *testing.T) {
	audioBodies := []string{`{}`, `{"code":200}`, `{"data":null}`}
	for _, body := range audioBodies {
		t.Run("audio "+body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			a := newApp(t)
			a.neteaseBases = map[string]string{"api": server.URL}
			_, _, err := a.neteaseURLFor(context.Background(), &neteaseSong{ID: 7})
			if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassNetwork {
				t.Fatalf("audio contract err=%v class=%q", err, classifyNeteaseFailure(err))
			}
		})
	}
	coverBodies := []string{`{}`, `{"album":null}`, `{"album":{}}`}
	for _, body := range coverBodies {
		t.Run("cover "+body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			a := newApp(t)
			a.neteaseBases = map[string]string{"api": server.URL}
			_, err := a.neteaseCoverURLFor(context.Background(), &neteaseSong{AlbumID: 42})
			if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassNetwork {
				t.Fatalf("cover contract err=%v class=%q", err, classifyNeteaseFailure(err))
			}
		})
	}
}

func TestNeteaseURLContractMismatchIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":"not-an-array"}`)
	}))
	defer server.Close()
	a := newApp(t)
	a.neteaseBases = map[string]string{"api": server.URL}
	_, _, err := a.neteaseURLFor(context.Background(), &neteaseSong{ID: 7, CatalogVerified: true})
	if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassNetwork {
		t.Fatalf("URL contract err=%v class=%q", err, classifyNeteaseFailure(err))
	}
}
