package app

import (
	"context"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
)

// neteaseFake serves the three calls the built-in source makes: search, URL
// resolution and the media file. kind is what the URL call reports.
func neteaseFake(t *testing.T, kind string, audio []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/api/search/get/web", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{"code": 200, "result": map[string]any{"songs": []map[string]any{
			{"id": 42, "name": "夏恋慕", "duration": 267888, "album": map[string]any{"name": "夏恋慕"},
				"artists": []map[string]any{{"name": "kobasolo"}, {"name": "春茶"}}},
			{"id": 43, "name": "别的歌", "duration": 100000, "album": map[string]any{"name": "别的"},
				"artists": []map[string]any{{"name": "别人"}}},
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("/api/song/enhance/player/url/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []map[string]any{
			{"id": 42, "url": server.URL + "/media/42.flac", "type": kind, "code": 200, "level": "lossless"},
		}})
	})
	mux.HandleFunc("/api/song/enhance/player/url", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []map[string]any{
			{"id": 42, "url": "", "type": kind, "code": 200},
		}})
	})
	mux.HandleFunc("/media/42.flac", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		_, _ = w.Write(audio)
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// saveNeteaseTestCookie stores a Cookie-Editor export for music.163.com.
func saveNeteaseTestCookie(t *testing.T, application *App) {
	t.Helper()
	export := `[{"name":"MUSIC_U","value":"test-session","domain":"music.163.com","path":"/","secure":true,"httpOnly":true,"sameSite":"unspecified","hostOnly":true}]`
	recorder := postCode(t, application, "/v1/settings/streaming/netease", map[string]any{"cookie_json": export})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if application.neteaseCookie() == "" {
		t.Fatal("the cookie was not stored")
	}
}

// The built-in source searches, resolves a lossless URL, keeps the FLAC and
// writes the identity the search confirmed.
func TestNeteaseStreamKeepsLosslessAudio(t *testing.T) {
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	// The fixture is a header-only FLAC; a real file is larger than the
	// 1024-byte sanity floor the download check applies.
	audio := append(syntheticFLAC(t, false), bytes.Repeat([]byte{0xaa}, 4096)...)
	server := neteaseFake(t, "flac", audio)
	application.neteaseBases = map[string]string{"api": server.URL}
	path, note, class, handled := application.tryNeteaseStream(context.Background(), SearchPlan{
		RootJobID: "root-1", Query: "夏恋慕 kobasolo", ExpectedTitle: "夏恋慕", ExpectedArtist: "kobasolo",
	})
	if !handled || class != downloadpolicy.ClassDownload || path == "" {
		t.Fatal(handled, class, note)
	}
	if !strings.Contains(note, "无损") {
		t.Fatal(note)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(body[:4]), "fLaC") {
		t.Fatal(err)
	}
	if realPath(t, filepath.Dir(path)) != realPath(t, filepath.Join(application.Dir, "incoming")) {
		t.Fatal(path)
	}
	tags := readTags(t, path)
	if tags.Title != "夏恋慕" || tags.Album != "夏恋慕" || len(tags.Artists) != 2 {
		t.Fatalf("%q %q %v", tags.Title, tags.Album, tags.Artists)
	}
}

// A 320k mp3 is what an account without lossless permission gets. The library
// is FLAC-only, so the file is reported, not stored.
func TestNeteaseStreamRejectsLossyAudio(t *testing.T) {
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	server := neteaseFake(t, "mp3", syntheticFLAC(t, false))
	application.neteaseBases = map[string]string{"api": server.URL}
	path, note, class, handled := application.tryNeteaseStream(context.Background(), SearchPlan{
		RootJobID: "root-2", ExpectedTitle: "夏恋慕", ExpectedArtist: "kobasolo",
	})
	if !handled || path != "" || class != downloadpolicy.ClassNoResult || !strings.Contains(note, "有损") {
		t.Fatal(handled, path, class, note)
	}
	if entries, err := os.ReadDir(filepath.Join(application.Dir, "incoming")); err == nil && len(entries) != 0 {
		t.Fatal("a lossy download was written")
	}
}

// Without a saved cookie the source is not "handled": the walk keeps the
// historical engine path instead of reporting a failure for an unconfigured
// source.
func TestNeteaseStreamWithoutCookieDefers(t *testing.T) {
	application := newApp(t)
	if _, _, _, handled := application.tryNeteaseStream(context.Background(), SearchPlan{ExpectedTitle: "x"}); handled {
		t.Fatal("an unconfigured source was handled")
	}
	view := getAuth(t, application, "/v1/settings/streaming/netease")
	if view.Code != http.StatusOK || !strings.Contains(view.Body.String(), `"saved":false`) {
		t.Fatal(view.Body.String())
	}
}

// The media URL comes from a remote document, so only NetEase's own hosts are
// fetched.
func TestNeteaseMediaHostAllowlist(t *testing.T) {
	application := newApp(t)
	for _, raw := range []string{
		"https://m701.music.126.net/a.flac", "http://music.163.com/a.flac", "https://p1.music.126.net/x",
	} {
		if !application.neteaseMediaAllowed(raw) {
			t.Fatal("rejected", raw)
		}
	}
	for _, raw := range []string{
		"https://evil.example/a.flac", "ftp://m701.music.126.net/a.flac",
		"https://music.163.com.evil.example/a.flac", "https://user:pw@music.163.com/a.flac",
	} {
		if application.neteaseMediaAllowed(raw) {
			t.Fatal("accepted", raw)
		}
	}
}

// Saving the cookie also records the playlist credential, so reading a
// playlist and falling back to a track share one paste.
func TestNeteaseCookieIsSharedWithPlaylists(t *testing.T) {
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	header, err := application.playlistCookie("netease", "music.163.com")
	if err != nil || !strings.Contains(header, "MUSIC_U=") {
		t.Fatal(header, err)
	}
	if dirContains(t, application.Dir, "test-session") {
		t.Fatal("the cookie was written to the state directory in plaintext")
	}
}
