package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

type jooxRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn jooxRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestJOOXTrackSecretMatchesPublicEndpointContract(t *testing.T) {
	const id = "0wRcDZ1uD+J_kpU8IXQI8Q=="
	if got, want := jooxTrackSecret(id), "e7d648c47c9235e0166f01a62a0e541b"; got != want {
		t.Fatalf("secret=%s want=%s", got, want)
	}
}

func TestJOOXDirectFallbackUsesPlaybackURLWhenEngineReturns404(t *testing.T) {
	const id = "test+track=="
	audio := oneSecondFLAC(t, "Artist", "Song")
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)

	application := newApp(t)
	application.jooxAPIBaseURL = jooxTrackAPIBase
	audioCalls := 0
	application.jooxHTTPClient = &http.Client{Transport: jooxRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Hostname() {
		case "cache.api.joox.com":
			if request.URL.Query().Get("secret") != jooxTrackSecret(id) || !strings.HasPrefix(request.URL.RawQuery, "country=hk&lang=zh_TW&lyric=1&fs=1&im=0&uid=142420656&usk=") || !strings.HasSuffix(request.URL.Path, "/"+id) {
				return nil, fmt.Errorf("unexpected metadata request %s", request.URL.String())
			}
			body := fmt.Sprintf(`{"id":%q,"play_duration":1,"play_url_list":["https://ak-hk.stream.music.joox.com/test.flac?x=1&amp;y=2"]}`, id)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		case "ak-hk.stream.music.joox.com":
			audioCalls++
			if request.URL.RawQuery != "x=1&y=2" {
				return nil, fmt.Errorf("HTML entities were not decoded: %s", request.URL.RawQuery)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(audio)), Request: request}, nil
		default:
			return nil, fmt.Errorf("unexpected JOOX host %s", request.URL.Hostname())
		}
	})}
	plan := SearchPlan{RootJobID: "joox-direct", ExpectedTitle: "Album", ExpectedArtist: "Artist", PlaylistTrack: "Song", ExpectedDuration: 1}
	song := &streamSong{ID: id, Source: "joox", Name: "Song", Artist: "Artist", Album: "Album", Duration: 1}
	path, note, class := application.tryConfiguredStreamSong(t.Context(), "joox", plan, song)
	if path == "" || class != "download" || audioCalls != 1 || !strings.Contains(note, "joox") {
		t.Fatalf("path=%q note=%q class=%q audioCalls=%d", path, note, class, audioCalls)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	tags, err := flactags.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "Song" || tags.Artist != "Artist" || tags.Album != "Album" {
		t.Fatalf("tags=%+v", tags)
	}
}

func TestJOOXRedirectPolicyRejectsNonJOOXHosts(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://evil.example/audio.mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jooxSafeRedirect(request, nil); err == nil {
		t.Fatal("cross-host JOOX redirect was accepted")
	}
	request.URL.Host = "ak-hk.stream.music.joox.com"
	if err := jooxSafeRedirect(request, nil); err != nil {
		t.Fatalf("approved JOOX CDN redirect was rejected: %v", err)
	}
}
