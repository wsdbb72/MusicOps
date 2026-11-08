package torrentfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchStoresMetainfoWithoutALibraryPublication(t *testing.T) {
	body := []byte("d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()
		if r.Header.Get("Cookie") == "" {
			t.Fatal("missing cookie")
		}
		w.Write(body)
	}))
	defer server.Close()
	dir := t.TempDir()
	receipt, err := Fetch(context.Background(), Request{Site: "open.cd", Resource: "11", Cookie: "session=a", Synthetic: true, BaseURL: server.URL, Directory: dir})
	if err != nil || receipt.Downloaded || receipt.LibraryPublished || receipt.ProductionVerified || receipt.Size != len(body) {
		t.Fatal(receipt, err)
	}
	if seen != "/download.php?id=11" {
		t.Fatal(seen)
	}
	stored, err := os.ReadFile(receipt.Path)
	if err != nil || string(stored) != string(body) || !strings.HasPrefix(filepath.Base(receipt.Path), receipt.MetaSHA256) {
		t.Fatal(err)
	}
	hash, err := infoHashV1(body)
	if err != nil || hash != receipt.InfoHash || len(hash) != 40 {
		t.Fatal(hash, err)
	}
}

func TestTokenDownloadUsesTheCanonicalParameter(t *testing.T) {
	body := []byte("d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()
		w.Write(body)
	}))
	defer server.Close()
	receipt, err := Fetch(context.Background(), Request{Site: "dicmusic", Resource: "11", Cookie: "session=a", AuthKey: "sentinel-dic", Synthetic: true, BaseURL: server.URL, UseToken: true, Directory: t.TempDir()})
	if err != nil || !receipt.UseToken {
		t.Fatal(receipt, err)
	}
	parsed, err := url.ParseRequestURI(seen)
	if err != nil || parsed.Path != "/torrents.php" || parsed.Query().Get("action") != "download" || parsed.Query().Get("id") != "11" || parsed.Query().Get("usetoken") != "1" || parsed.Query().Get("authkey") != "sentinel-dic" || parsed.Query().Get("torrent_pass") != "" {
		t.Fatal("dicmusic query mismatch")
	}
}

func TestOrpheusTokenQueryDoesNotUseDicMusicParameters(t *testing.T) {
	body := []byte("d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()
		w.Write(body)
	}))
	defer server.Close()
	_, err := Fetch(context.Background(), Request{Site: "orpheus", Resource: "11", Cookie: "session=a", TorrentPass: "sentinel-ops", Synthetic: true, BaseURL: server.URL, UseToken: true, Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.ParseRequestURI(seen)
	if err != nil || parsed.Query().Get("torrent_pass") != "sentinel-ops" || parsed.Query().Get("authkey") != "" || parsed.Query().Get("usetoken") != "1" {
		t.Fatal("orpheus query mismatch")
	}
	hits := 0
	rejected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer rejected.Close()
	if _, err = Fetch(context.Background(), Request{Site: "dicmusic", Resource: "11", Cookie: "session=a", TorrentPass: "sentinel-ops", Synthetic: true, BaseURL: rejected.URL, UseToken: true, Directory: t.TempDir()}); err == nil || hits != 0 {
		t.Fatal(err, hits)
	}
}

func TestHTMLAndRedirectDoNotCreateAFile(t *testing.T) {
	htmlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ioWrite(w, "<html>login</html>")
	}))
	defer htmlServer.Close()
	if _, err := Fetch(context.Background(), Request{Site: "open.cd", Resource: "11", Cookie: "session=a", Synthetic: true, BaseURL: htmlServer.URL, Directory: t.TempDir()}); err == nil {
		t.Fatal("html accepted")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/other", http.StatusFound)
	}))
	defer redirect.Close()
	dir := t.TempDir()
	if _, err := Fetch(context.Background(), Request{Site: "open.cd", Resource: "11", Cookie: "session=a", Synthetic: true, BaseURL: redirect.URL, Directory: dir}); err == nil {
		t.Fatal("redirect accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestMissingCookieDoesNotNeedAServer(t *testing.T) {
	if _, err := Fetch(context.Background(), Request{Site: "open.cd", Resource: "11", Directory: t.TempDir()}); err == nil {
		t.Fatal("empty cookie accepted")
	}
}

func ioWrite(w http.ResponseWriter, body string) { _, _ = w.Write([]byte(body)) }
