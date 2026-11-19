package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
