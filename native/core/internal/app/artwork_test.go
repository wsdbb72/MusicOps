package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lrcAPIFake is an LrcApi stand-in: /lyrics answers LRC text, /cover answers
// image bytes, and both require the header when one is configured.
func lrcAPIFake(t *testing.T, lyrics string, cover []byte, authHeader, authValue string) (*httptest.Server, *[]*url.URL) {
	t.Helper()
	seen := []*url.URL{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		copied := *r.URL
		seen = append(seen, &copied)
		if authHeader != "" && r.Header.Get(authHeader) != authValue {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/lyrics":
			if lyrics == "" {
				http.Error(w, "Lyrics not found.", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, lyrics)
		case "/cover":
			if cover == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(cover)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func saveLyricsService(t *testing.T, application *App, base, header, value string) {
	t.Helper()
	body := map[string]any{
		"base_url": base, "lyrics_path": "/lyrics", "candidates_path": "/jsonapi",
		"cover_path": "/cover", "auth_header": header, "write_mode": "sidecar",
	}
	if value != "" {
		body["authorization"] = value
	}
	if recorder := postCode(t, application, "/v1/settings/lyrics", body); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func jpegBody() []byte {
	return append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x22}, 64)...)
}

// The saved service is used without typing an address, and the saved
// authorization value goes with it: that is the whole point of configuring it
// once in the settings page.
func TestArtworkUsesTheSavedServiceAndItsCredential(t *testing.T) {
	application := newApp(t)
	cover := jpegBody()
	server, seen := lrcAPIFake(t, "[00:01.00]hello\n[00:02.00]world", cover, "Authorization", "secret-key")
	saveLyricsService(t, application, server.URL, "Authorization", "secret-key")

	lyrics, gotCover, err := application.fetchArtwork("", "", "Artist", "Album", "Song")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lyrics, "[00:01.00]hello") || !bytes.Equal(gotCover, cover) {
		t.Fatalf("lyrics=%q cover=%d", lyrics, len(gotCover))
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d", len(*seen))
	}
	for _, request := range *seen {
		if request.Query().Get("title") != "Song" || request.Query().Get("artist") != "Artist" || request.Query().Get("album") != "Album" {
			t.Fatalf("query = %s", request.RawQuery)
		}
	}
}

// A service that has nothing for this track answers 404. That must not block
// the tag write: the catalog fields are still worth writing.
func TestArtworkNotFoundDoesNotFailTheWrite(t *testing.T) {
	application := newApp(t)
	server, _ := lrcAPIFake(t, "", nil, "", "")
	saveLyricsService(t, application, server.URL, "Authorization", "")

	lyrics, cover, err := application.fetchArtwork("", "", "Artist", "Album", "Missing")
	if err != nil || lyrics != "" || cover != nil {
		t.Fatalf("%q %v %v", lyrics, cover, err)
	}
}

// The default settings carry a placeholder address; nothing is fetched until
// the user points it at a real service.
func TestArtworkSkipsThePlaceholderService(t *testing.T) {
	application := newApp(t)
	lyrics, cover, err := application.fetchArtwork("", "", "Artist", "Album", "Song")
	if err != nil || lyrics != "" || cover != nil {
		t.Fatalf("%q %v %v", lyrics, cover, err)
	}
	if _, ok := savedLyricsBase(defaultLyricsSettings()); ok {
		t.Fatal("the placeholder was treated as a configured service")
	}
}

// A credential belongs to the configured host, so a plain-HTTP address that is
// not this machine is refused before any request is made.
func TestArtworkRefusesPlainHTTPOutsideLoopback(t *testing.T) {
	application := newApp(t)
	if _, _, err := application.fetchArtwork("http://lyrics.example.com", "", "Artist", "Album", "Song"); err == nil {
		t.Fatal("a public plain-HTTP service was accepted")
	}
	if _, _, err := application.fetchArtwork("", "http://lyrics.example.com", "Artist", "Album", "Song"); err == nil {
		t.Fatal("a public plain-HTTP cover service was accepted")
	}
	// And the settings refuse it too, so the address cannot be saved first.
	if recorder := postCode(t, application, "/v1/settings/lyrics", map[string]any{"base_url": "http://lyrics.example.com"}); recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code)
	}
}

// Covers are commonly a redirect to a CDN. Without a credential that is a
// normal fetch; with one it is refused, because the value must not travel to
// the redirect target.
func TestArtworkCoverRedirectDependsOnCredential(t *testing.T) {
	image := jpegBody()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(image)
	}))
	defer cdn.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cover" {
			http.Redirect(w, r, cdn.URL+"/art.jpg", http.StatusMovedPermanently)
			return
		}
		http.NotFound(w, r)
	}))
	defer front.Close()

	anonymous := newApp(t)
	saveLyricsService(t, anonymous, front.URL, "Authorization", "")
	if _, cover, err := anonymous.fetchArtwork("", "", "Artist", "Album", "Song"); err != nil || !bytes.Equal(cover, image) {
		t.Fatalf("anonymous redirect: %d bytes, %v", len(cover), err)
	}

	credentialed := newApp(t)
	saveLyricsService(t, credentialed, front.URL, "Authorization", "secret-key")
	if _, _, err := credentialed.fetchArtwork("", "", "Artist", "Album", "Song"); err == nil {
		t.Fatal("a credentialed fetch followed a redirect to another host")
	}
}

// The end-to-end path: metadata completion with no lyrics address in the
// request still writes the LRC because the saved service supplies it.
func TestMetadataCompletionFetchesLyricsFromTheSavedService(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(library, "01 opening.flac")
	if err := os.WriteFile(path, syntheticFLAC(t, false), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	catalog := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	lyrics := "[00:01.00]hello\n[00:02.00]world"
	server, _ := lrcAPIFake(t, lyrics, nil, "Authorization", "secret-key")
	saveLyricsService(t, application, server.URL, "Authorization", "secret-key")

	written := post(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": catalog.server.URL,
	})
	if written["written"] != true || written["lyrics"] != true {
		t.Fatal(written)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(after, []byte("hello")) {
		t.Fatal("the lyrics from the saved service were not written", err)
	}
}

// The public LrcApi instance answers /jsonapi with "lrc" instead of the
// documented "lyrics". Both spellings must be accepted, or the connection test
// rejects a service that is actually working.
func TestLyricsCandidateAcceptsBothFieldNames(t *testing.T) {
	for _, body := range []string{
		`[{"id":"a","title":"童年","artist":"罗大佑","album":"之乎者也","lyrics":"[00:01.00]hello"}]`,
		`[{"id":"b","title":"童年","artist":"罗大佑","album":"之乎者也","lrc":"[00:01.00]hello"}]`,
	} {
		var items []lyricCandidate
		if err := json.Unmarshal([]byte(body), &items); err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].lyricText() == "" {
			t.Fatal(body, items)
		}
		if !items[0].identityMatches("童年", "罗大佑", "之乎者也") {
			t.Fatal("identity did not match", body)
		}
	}
}
