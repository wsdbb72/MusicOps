// Tests for the 0.4.34 settings entries: every new entry is verified
// end to end over real HTTP — save, re-read, and actually take effect
// against synthetic services. Secrets are asserted never to land on disk
// in plaintext.
package app

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// vaultKey sets the Linux vault key file for the whole test. It must be
// called once per test, before any app is opened: two apps in one test
// share the vault store (their state directories share one grandparent),
// so they must also share one key file, or the second app cannot decrypt
// the first app's records.
func vaultKey(t *testing.T) {
	t.Helper()
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", filepath.Join(t.TempDir(), "vault.key"))
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err, recorder.Body.String())
	}
	return decoded
}

func TestSavedCookieUsedBySearch(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	var seenCookie atomic.Value
	seenCookie.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MusicOps-Site") == "dicmusic" {
			seenCookie.Store(r.Header.Get("Cookie"))
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
			return
		}
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()

	// Save a dicmusic cookie, then re-read: only the state is shown.
	saveRecorder := postCode(t, application, "/v1/settings/cookies", map[string]any{
		"site":        "dicmusic",
		"cookie_json": `[{"name":"session","value":"saved-value","domain":"dicmusic.com","path":"/","secure":false,"httpOnly":false}]`,
	})
	if saveRecorder.Code != http.StatusOK {
		t.Fatal(saveRecorder.Code, saveRecorder.Body.String())
	}
	if strings.Contains(saveRecorder.Body.String(), "saved-value") {
		t.Fatal("saved cookie value leaked into the settings response")
	}
	saved := decodeBody(t, saveRecorder)
	sites, _ := saved["sites"].([]any)
	if len(sites) != 4 {
		t.Fatal(saved)
	}
	var cookieState map[string]any
	for _, site := range sites {
		if site.(map[string]any)["name"] == "dicmusic" {
			cookieState = site.(map[string]any)
		}
	}
	if cookieState == nil || cookieState["saved"] != true {
		t.Fatal(saved)
	}
	if dirContains(t, application.Dir, "saved-value") {
		t.Fatal("saved cookie value was written to the native store in plaintext")
	}

	// Search with no per-call cookie: the saved cookie must be the one
	// the tracker sees.
	body := searchBody("album-saved-cookie", server.URL, []string{"dicmusic"}, false)
	body["cookie"] = ""
	post(t, application, "/v1/search", body)
	searchJob := waitJob(t, application, "search:album-saved-cookie", "completed")
	if searchJob.SearchAction != "queue_request" {
		t.Fatal(searchJob)
	}
	if got := seenCookie.Load().(string); got != "session=saved-value" {
		t.Fatalf("tracker saw Cookie %q, want the saved cookie", got)
	}
}

func TestPreflightWithoutAnyCookieIsRefused(t *testing.T) {
	// Its own test: apps in one test share the vault store, so a second
	// app here would see the first test's saved cookie.
	vaultKey(t)
	application := newApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer server.Close()
	recorder := postCode(t, application, "/v1/sites/preflight", map[string]any{"site": "dicmusic", "synthetic": true, "base_url": server.URL})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "no saved cookie") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestAISettingsWireReviewModel(t *testing.T) {
	var seenAuth atomic.Value
	seenAuth.Store("")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"连接成功"}]}]}`)
	}))
	defer server.Close()
	vaultKey(t)
	application := newApp(t)
	application.aiHTTPClient = server.Client()

	if application.reviewModel != nil {
		t.Fatal("review model must start nil")
	}
	post(t, application, "/v1/settings/ai", map[string]any{
		"enabled": true, "base_url": server.URL, "model": "test-model", "api_key": "test-key-123",
	})
	got := getAuth(t, application, "/v1/settings/ai")
	settings := decodeBody(t, got)["ai"].(map[string]any)
	if settings["key_saved"] != true || settings["model"] != "test-model" || settings["enabled"] != true {
		t.Fatal(settings)
	}
	if strings.Contains(got.Body.String(), "test-key-123") {
		t.Fatal("API key leaked into the settings response")
	}
	if dirContains(t, application.Dir, "test-key-123") {
		t.Fatal("API key was written to the native store in plaintext")
	}
	if application.reviewModel == nil {
		t.Fatal("saving an enabled AI model must wire the production review model")
	}
	tested := post(t, application, "/v1/settings/ai/test", map[string]any{})
	if tested["ok"] != true {
		t.Fatal(tested)
	}
	if auth := seenAuth.Load().(string); auth != "Bearer test-key-123" {
		t.Fatalf("AI server saw Authorization %q", auth)
	}

	// Enabling without a model is refused (fresh app, nothing saved).
	fresh := newApp(t)
	recorder := postCode(t, fresh, "/v1/settings/ai", map[string]any{"enabled": true, "base_url": server.URL, "model": ""})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	// Disabling unwires the model again.
	post(t, application, "/v1/settings/ai", map[string]any{"enabled": false})
	if application.reviewModel != nil {
		t.Fatal("disabling AI must unwire the review model")
	}
}

func TestQBittorrentSettingsAndTest(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	fake := newFakeQBT(t, map[string][]byte{}, 0)
	post(t, application, "/v1/settings/qbittorrent", map[string]any{
		"base_url": fake.url(), "username": "user", "password": "pass",
	})
	got := getAuth(t, application, "/v1/settings/qbittorrent")
	settings := decodeBody(t, got)["qbittorrent"].(map[string]any)
	if settings["login_saved"] != true || settings["base_url"] != fake.url() {
		t.Fatal(settings)
	}
	if strings.Contains(got.Body.String(), `"password":"pass"`) || dirContains(t, application.Dir, "pass") {
		t.Fatal("qBittorrent password leaked")
	}
	tested := post(t, application, "/v1/settings/qbittorrent/test", map[string]any{})
	info, _ := tested["qbittorrent"].(map[string]any)
	if tested["ok"] != true || info["version"] != "5.0.0" || info["webapi_version"] != "2.11.0" {
		t.Fatal(tested)
	}
	if _, ok := info["torrent_count"]; !ok {
		t.Fatal("test result must report the torrent count", tested)
	}

	// A server that refuses login reports AUTH_FAILED.
	rejecting := newFakeQBT(t, map[string][]byte{}, 0)
	rejecting.rejectLogin = true
	other := newApp(t)
	post(t, other, "/v1/settings/qbittorrent", map[string]any{
		"base_url": rejecting.url(), "username": "user", "password": "wrong",
	})
	recorder := postCode(t, other, "/v1/settings/qbittorrent/test", map[string]any{})
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "AUTH_FAILED") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestStreamingOrderEditableAndFollowed(t *testing.T) {
	application := newApp(t)
	reversed := []string{"playlist", "netease", "qqmusic", "deezer", "qobuz", "applemusic", "tidal"}
	post(t, application, "/v1/settings/download", map[string]any{"streaming_order": reversed})
	got := getAuth(t, application, "/v1/settings/download")
	settings := decodeBody(t, got)
	order, _ := settings["streaming_order"].([]any)
	if len(order) != len(reversed) {
		t.Fatal(settings)
	}
	for i, name := range reversed {
		if order[i] != name {
			t.Fatal(order)
		}
	}
	// Steps only include enabled sources (qobuz and deezer are off by
	// default), but they must follow the saved order.
	steps, _ := settings["streaming_sources"].([]any)
	wantSteps := []string{"playlist", "netease", "qqmusic", "applemusic", "tidal"}
	if len(steps) != len(wantSteps) {
		t.Fatal(steps)
	}
	for i, name := range wantSteps {
		if steps[i].(map[string]any)["name"] != name {
			t.Fatal("fallback steps do not follow the saved order", steps)
		}
	}

	// An incomplete order is refused.
	recorder := postCode(t, application, "/v1/settings/download", map[string]any{"streaming_order": []string{"tidal", "applemusic"}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}

	// The fallback walk itself follows the saved order: the playlist
	// note comes before the tidal note.
	server := emptyResultsServer(t)
	defer server.Close()
	post(t, application, "/v1/search", searchBody("album-order", server.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-order", "needs_review")
	playlistAt := strings.Index(job.Message, "playlist")
	tidalAt := strings.Index(job.Message, "tidal")
	if playlistAt < 0 || tidalAt < 0 || playlistAt > tidalAt {
		t.Fatal(job.Message)
	}
}

func TestSolverSettingsAndTest(t *testing.T) {
	var seenBody atomic.Value
	seenBody.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seenBody.Store(string(raw))
		if r.URL.Path != "/v1" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"status":"ok","sessions":[],"version":"1.2.3"}`)
	}))
	defer server.Close()
	application := newApp(t)
	post(t, application, "/v1/settings/solver", map[string]any{"base_url": server.URL, "timeout": 30})
	got := getAuth(t, application, "/v1/settings/solver")
	if decodeBody(t, got)["solver"].(map[string]any)["base_url"] != server.URL {
		t.Fatal(got.Body.String())
	}
	tested := post(t, application, "/v1/settings/solver/test", map[string]any{})
	if tested["ok"] != true || tested["sessions"] != float64(0) || tested["version"] != "1.2.3" {
		t.Fatal(tested)
	}
	if seen := seenBody.Load().(string); !strings.Contains(seen, `"sessions.list"`) {
		t.Fatalf("solver saw %q", seen)
	}
	recorder := postCode(t, application, "/v1/settings/solver", map[string]any{"base_url": "http://example.com:8191"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestLyricsSettingsAndTest(t *testing.T) {
	var seenAuth atomic.Value
	seenAuth.Store("")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/jsonapi" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("title") != "童年" || r.URL.Query().Get("artist") != "罗大佑" {
			t.Errorf("lyrics test query = %v", r.URL.Query())
		}
		io.WriteString(w, `[{"id":"1","title":"童年","artist":"罗大佑","album":"之乎者也","lyrics":"[00:01.00]池塘边的榕树上\n[00:05.00]知了在声声叫着夏天\n"}]`)
	}))
	defer server.Close()
	vaultKey(t)
	application := newApp(t)
	previous := noRedirectClient
	noRedirectClient = server.Client()
	t.Cleanup(func() { noRedirectClient = previous })

	post(t, application, "/v1/settings/lyrics", map[string]any{
		"base_url": server.URL, "authorization": "raw-auth-value",
	})
	got := getAuth(t, application, "/v1/settings/lyrics")
	if decodeBody(t, got)["lyrics"].(map[string]any)["auth_saved"] != true {
		t.Fatal(got.Body.String())
	}
	tested := post(t, application, "/v1/settings/lyrics/test", map[string]any{})
	if tested["ok"] != true || tested["valid"] != float64(1) {
		t.Fatal(tested)
	}
	if auth := seenAuth.Load().(string); auth != "raw-auth-value" {
		t.Fatalf("lyrics server saw Authorization %q, want the raw value without a Bearer prefix", auth)
	}
}

func TestLyricsTestWithoutAuthIsNotConfigured(t *testing.T) {
	// Its own test: apps in one test share the vault store, so a second
	// app here would see the first test's saved authorization.
	vaultKey(t)
	application := newApp(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("lyrics test must not send a request without authorization")
	}))
	defer server.Close()
	previous := noRedirectClient
	noRedirectClient = server.Client()
	t.Cleanup(func() { noRedirectClient = previous })
	post(t, application, "/v1/settings/lyrics", map[string]any{"base_url": server.URL})
	recorder := postCode(t, application, "/v1/settings/lyrics/test", map[string]any{})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "NOT_CONFIGURED") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestDiscogsTokenAndTest(t *testing.T) {
	var seenAuth atomic.Value
	seenAuth.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/database/search" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"results":[]}`)
	}))
	defer server.Close()
	vaultKey(t)
	application := newApp(t)
	post(t, application, "/v1/settings/discogs", map[string]any{"token": "discogs-token-123"})
	got := getAuth(t, application, "/v1/settings/discogs")
	if decodeBody(t, got)["discogs"].(map[string]any)["token_saved"] != true {
		t.Fatal(got.Body.String())
	}
	if dirContains(t, application.Dir, "discogs-token-123") {
		t.Fatal("Discogs token was written to the native store in plaintext")
	}
	tested := post(t, application, "/v1/settings/discogs/test", map[string]any{"base_url": server.URL})
	if tested["ok"] != true {
		t.Fatal(tested)
	}
	if auth := seenAuth.Load().(string); auth != "Discogs token=discogs-token-123" {
		t.Fatalf("Discogs saw Authorization %q", auth)
	}
	recorder := postCode(t, application, "/v1/settings/discogs", map[string]any{"token": "short"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestLibraryDirCustomPublish(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "mylib")
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	got := getAuth(t, application, "/v1/settings/library")
	settings := decodeBody(t, got)["library"].(map[string]any)
	if settings["effective_library_dir"] != library {
		t.Fatal(settings)
	}
	if info, err := os.Stat(library); err != nil || !info.IsDir() {
		t.Fatal("saving a library directory must create it", err)
	}

	// A relative path and the filesystem root are refused.
	recorder := postCode(t, application, "/v1/settings/library", map[string]any{"library_dir": "relative/path"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	recorder = postCode(t, application, "/v1/settings/library", map[string]any{"library_dir": "/"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}

	// A new publish lands in the custom directory and the record
	// remembers its root.
	startSearchJob(t, application, "pub-lib")
	wav := pcmWAV(24)
	audio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	}))
	defer audio.Close()
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-lib", "source_url": audio.URL, "artist": "Artist", "album": "Album", "title": "Title", "track": 1,
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-lib", "completed")
	published, err := os.ReadFile(filepath.Join(library, "Artist", "Album", "01 Title.wav"))
	if err != nil || !bytes.Equal(wav, published) {
		t.Fatal(err)
	}
	entries := getAuth(t, application, "/v1/library")
	body := entries.Body.String()
	if !strings.Contains(body, `"file":"Artist/Album/01 Title.wav"`) || !strings.Contains(body, `"library_dir":"`+library+`"`) {
		t.Fatal(body)
	}
}

func TestLibraryDirSymlinkResolves(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// A symlink is followed, not rejected: on macOS even the system
	// temp dir sits under the /var -> /private/var symlink.
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": link})
	got := getAuth(t, application, "/v1/settings/library")
	if decodeBody(t, got)["library"].(map[string]any)["effective_library_dir"] != link {
		t.Fatal(got.Body.String())
	}
	// A link to the backup directory still resolves to the backup
	// directory and is refused.
	recorder := postCode(t, application, "/v1/settings/library", map[string]any{"library_dir": application.Backup})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	startSearchJob(t, application, "pub-link")
	wav := pcmWAV(24)
	audio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	}))
	defer audio.Close()
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-link", "source_url": audio.URL, "artist": "Artist", "album": "Album", "title": "Title", "track": 1,
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-link", "completed")
	published, err := os.ReadFile(filepath.Join(real, "Artist", "Album", "01 Title.wav"))
	if err != nil || !bytes.Equal(wav, published) {
		t.Fatal(err)
	}
}

func TestMusicServersNotifiedOnPublish(t *testing.T) {
	var plexSections, plexRefresh, navidromePing, navidromeScan atomic.Int32
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "plex-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/library/sections":
			plexSections.Add(1)
			io.WriteString(w, `<MediaContainer><Directory key="3" type="artist" title="Music"/></MediaContainer>`)
		case "/library/sections/3/refresh":
			plexRefresh.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()
	navidrome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/ping":
			navidromePing.Add(1)
			io.WriteString(w, `{"subsonic-response":{"status":"ok","version":"1.16.1"}}`)
		case "/rest/startScan":
			navidromeScan.Add(1)
			io.WriteString(w, `{"subsonic-response":{"status":"ok","version":"1.16.1"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer navidrome.Close()

	vaultKey(t)
	application := newApp(t)
	post(t, application, "/v1/settings/servers/plex", map[string]any{
		"enabled": true, "url": plex.URL, "section_id": "3", "token": "plex-token",
	})
	post(t, application, "/v1/settings/servers/navidrome", map[string]any{
		"enabled": true, "url": navidrome.URL, "username": "user", "password": "nav-pass",
	})
	got := getAuth(t, application, "/v1/settings/servers")
	servers := decodeBody(t, got)["servers"].(map[string]any)
	if servers["plex"].(map[string]any)["token_saved"] != true || servers["navidrome"].(map[string]any)["password_saved"] != true {
		t.Fatal(servers)
	}
	if dirContains(t, application.Dir, "plex-token") || dirContains(t, application.Dir, "nav-pass") {
		t.Fatal("music server secrets were written to the native store in plaintext")
	}

	// The connection tests hit the fake servers without scanning.
	plexTested := post(t, application, "/v1/settings/servers/plex/test", map[string]any{})
	if plexTested["ok"] != true || plexTested["count"] != float64(1) {
		t.Fatal(plexTested)
	}
	sections, _ := plexTested["sections"].([]any)
	if len(sections) != 1 || sections[0].(map[string]any)["id"] != "3" {
		t.Fatal(plexTested)
	}
	if plexRefresh.Load() != 0 {
		t.Fatal("the Plex test must not trigger a scan")
	}
	navidromeTested := post(t, application, "/v1/settings/servers/navidrome/test", map[string]any{})
	if navidromeTested["ok"] != true {
		t.Fatal(navidromeTested)
	}
	if navidromeScan.Load() != 0 {
		t.Fatal("the Navidrome test must not trigger a scan")
	}

	// A publish notifies both servers, Navidrome first.
	startSearchJob(t, application, "pub-notify")
	wav := pcmWAV(24)
	audio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	}))
	defer audio.Close()
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-notify", "source_url": audio.URL, "artist": "Artist", "album": "Album", "title": "Title", "track": 1,
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-notify", "completed")
	if navidromeScan.Load() != 1 {
		t.Fatalf("Navidrome scan requests = %d, want 1", navidromeScan.Load())
	}
	if plexRefresh.Load() != 1 {
		t.Fatalf("Plex refresh requests = %d, want 1", plexRefresh.Load())
	}
}

func TestPlaylistSaveAttachAndFallback(t *testing.T) {
	application := newApp(t)
	content := "#PLAYLIST:Test Mix\n#EXTINF:123,Artist A - Song One\n/media/a.mp3\n#EXTINF:200,Artist B - Song Two\n/media/b.flac\n"
	saved := post(t, application, "/v1/playlists", map[string]any{"content": content, "name": "Test Mix"})
	playlist, _ := saved["playlist"].(map[string]any)
	if playlist["id"] != "test-mix" || playlist["track_count"] != float64(2) {
		t.Fatal(saved)
	}
	listed := getAuth(t, application, "/v1/playlists")
	if !strings.Contains(listed.Body.String(), `"id":"test-mix"`) {
		t.Fatal(listed.Body.String())
	}

	// Attach the saved playlist to a job.
	attached := post(t, application, "/v1/jobs/pl-job/playlist", map[string]any{"playlist_id": "test-mix"})
	attachedPlaylist, _ := attached["playlist"].(map[string]any)
	if attachedPlaylist["exists"] != true || attachedPlaylist["track_count"] != float64(2) {
		t.Fatal(attached)
	}
	got := getAuth(t, application, "/v1/jobs/pl-job/playlist")
	if !strings.Contains(got.Body.String(), `"track_count":2`) {
		t.Fatal(got.Body.String())
	}

	// A multipart upload saves under its file name.
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	part, err := form.CreateFormFile("file", "Upload Mix.m3u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/playlists", &buf)
	request.SetBasicAuth("musicops", "test-password-16b")
	request.Header.Set("Content-Type", form.FormDataContentType())
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var uploaded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded["playlist"].(map[string]any)["id"] != "upload-mix" {
		t.Fatal(uploaded)
	}

	// The fallback reads the attached playlist: search with the saved
	// playlist lands in review with the imported tracks.
	server := emptyResultsServer(t)
	defer server.Close()
	body := searchBody("album-pl-attach", server.URL, []string{"dicmusic", "orpheus"}, true)
	body["playlist_id"] = "test-mix"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-pl-attach", "needs_review")
	if job.ReviewKind != "candidate" || !strings.Contains(job.Message, "已导入 2 首曲目") || !strings.Contains(job.Message, "Test Mix") {
		t.Fatal(job)
	}
}
