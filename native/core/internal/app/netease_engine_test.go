package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestNeteaseBuiltInFailureFallsBackOnlyToNeteaseEngine(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)

	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	var sources []string
	var sourceMu sync.Mutex
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			source := r.URL.Query().Get("sources")
			sourceMu.Lock()
			sources = append(sources, source)
			sourceMu.Unlock()
			if source != "netease" {
				http.Error(w, "wrong source", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "n1", "source": "netease", "name": "Example & Test",
				"artist": "Artist", "album": "First Album", "duration": 1,
			}}})
		case "/download":
			if source := r.URL.Query().Get("source"); source != "netease" {
				http.Error(w, "wrong source", http.StatusBadRequest)
				return
			}
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/search/get/web", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"result": map[string]any{"songs": []map[string]any{{
				"id": 42, "name": "Example & Test", "duration": 1000,
				"album":   map[string]any{"id": 1, "name": "First Album"},
				"artists": []map[string]any{{"name": "Artist"}},
			}}},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": []map[string]any{{"id": 42, "url": "", "type": "flac", "code": 200}},
		})
	})
	builtIn := httptest.NewServer(mux)
	defer builtIn.Close()
	application.neteaseBases = map[string]string{"api": builtIn.URL}

	path, note, class, handled := application.tryNeteaseStream(t.Context(), SearchPlan{
		RootJobID: "netease-engine-fallback",
		Query:     "Artist Example & Test", ExpectedTitle: "Example & Test", ExpectedArtist: "Artist",
	})
	if !handled || class != downloadpolicy.ClassDownload || path == "" {
		t.Fatalf("handled=%v class=%q path=%q note=%q", handled, class, path, note)
	}
	if !strings.Contains(note, "取不到音源地址") || !strings.Contains(note, "网易云内置下载引擎：") {
		t.Fatalf("fallback provenance missing: %q", note)
	}
	sourceMu.Lock()
	gotSources := append([]string(nil), sources...)
	sourceMu.Unlock()
	if len(gotSources) != 1 || gotSources[0] != "netease" {
		t.Fatalf("engine sources=%v", gotSources)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if tags := readTags(t, path); tags.Title != "Example & Test" || tags.Artist != "Artist" {
		t.Fatalf("tags=%+v", tags)
	}
}

func TestNeteaseBuiltInSearchMissFallsBackOnlyToNeteaseEngine(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)

	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if source := r.URL.Query().Get("sources"); source != "netease" {
				t.Errorf("engine searched source %q", source)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "n-search-miss", "source": "netease", "name": "Example & Test",
				"artist": "Artist", "album": "First Album", "duration": 1,
			}}})
		case "/download":
			if source := r.URL.Query().Get("source"); source != "netease" {
				t.Errorf("engine downloaded source %q", source)
			}
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)

	builtIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []any{}}})
	}))
	defer builtIn.Close()
	application.neteaseBases = map[string]string{"api": builtIn.URL}

	path, note, class, handled := application.tryNeteaseStream(t.Context(), SearchPlan{
		RootJobID: "netease-search-engine-fallback",
		Query:     "Artist Example & Test", ExpectedTitle: "Example & Test", ExpectedArtist: "Artist",
	})
	if !handled || class != downloadpolicy.ClassDownload || path == "" {
		t.Fatalf("handled=%v class=%q path=%q note=%q", handled, class, path, note)
	}
	if !strings.Contains(note, "没有找到标题与艺人") || !strings.Contains(note, "网易云内置下载引擎：") {
		t.Fatalf("search fallback provenance missing: %q", note)
	}
}

func TestExclusiveNeteasePlaylistWithoutCookieUsesNeteaseEngine(t *testing.T) {
	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	var sources []string
	var sourceMu sync.Mutex
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			source := r.URL.Query().Get("sources")
			sourceMu.Lock()
			sources = append(sources, source)
			sourceMu.Unlock()
			if source != "netease" {
				http.Error(w, "wrong source", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "n2", "source": "netease", "name": "Example & Test",
				"artist": "Artist", "album": "First Album", "duration": 1,
			}}})
		case "/download":
			if source := r.URL.Query().Get("source"); source != "netease" {
				http.Error(w, "wrong source", http.StatusBadRequest)
				return
			}
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()

	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true})
	body := searchBody("album-netease-no-cookie", engine.URL, nil, true)
	body["batch_id"] = "pl-netease-590536461-test"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-netease-no-cookie", "needs_review")
	if !strings.Contains(job.Message, "网易云内置下载引擎") && !strings.Contains(job.Message, "netease 音频已通过校验并保存") {
		t.Fatalf("Netease engine fallback missing from job: %s", job.Message)
	}
	sourceMu.Lock()
	gotSources := append([]string(nil), sources...)
	sourceMu.Unlock()
	if len(gotSources) == 0 {
		t.Fatal("Netease engine was not queried")
	}
	for _, source := range gotSources {
		if source != "netease" {
			t.Fatalf("exclusive playlist queried another provider: %v", gotSources)
		}
	}
	matches, err := filepath.Glob(filepath.Join(application.Dir, "incoming", "stream-*-netease.flac"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("fallback files=%v err=%v", matches, err)
	}
}

func TestStreamEngineKeepsTemporaryFailureClasses(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{status: http.StatusTooManyRequests, want: downloadpolicy.ClassRateLimited},
		{status: http.StatusBadGateway, want: downloadpolicy.ClassNetwork},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			application := newApp(t)
			t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
			_, _, class := application.tryConfiguredStream(t.Context(), "netease", SearchPlan{ExpectedTitle: "Track", ExpectedArtist: "Artist"})
			if class != test.want {
				t.Fatalf("class=%q want=%q", class, test.want)
			}
		})
	}
}

func TestStreamEngineKeepsDownloadNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "n5", "source": "netease", "name": "Track", "artist": "Artist", "album": "Album", "duration": 1,
			}}})
		case "/download":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	_, _, class := application.tryConfiguredStream(t.Context(), "netease", SearchPlan{ExpectedTitle: "Track", ExpectedArtist: "Artist"})
	if class != downloadpolicy.ClassNetwork {
		t.Fatalf("download class=%q", class)
	}
}
func TestStreamSearchUsesDurableNeteaseIDBeforeAlbumHeuristics(t *testing.T) {
	tests := []struct {
		name       string
		wantID     int64
		wantSongID string
	}{
		{name: "exact id wins over album labels", wantID: 42, wantSongID: "42"},
		{name: "known id never falls back to another candidate", wantID: 43, wantSongID: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/search" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{
					{"id": 42, "source": "netease", "name": "Example & Test", "artist": "Artist", "album": "Expanded Reissue", "duration": 1},
					{"id": "99", "source": "netease", "name": "Example & Test", "artist": "Artist", "album": "First Album", "duration": 1},
				}})
			}))
			defer server.Close()
			application := newApp(t)
			song, status, err := application.streamSearch(t.Context(), server.URL, "netease", SearchPlan{
				ExpectedTitle: "First Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test", NeteaseSongID: test.wantID,
			}, "")
			if err != nil || status != "" {
				t.Fatalf("status=%q err=%v", status, err)
			}
			if test.wantSongID == "" {
				if song != nil {
					t.Fatalf("known id accepted another card: %+v", song)
				}
				return
			}
			if song == nil || song.ID != test.wantSongID {
				t.Fatalf("song=%+v", song)
			}
		})
	}
}

func TestPlaylistStreamPositionKeepsLibraryTrackNumber(t *testing.T) {
	plan := SearchPlan{LibraryTracks: []localWorkflowTrack{{Path: filepath.Join("Artist", "Album", "08 Song.mp3")}}}
	if got := playlistStreamPosition(plan); got != 8 {
		t.Fatalf("position=%d", got)
	}
	if got := playlistStreamPosition(SearchPlan{}); got != 1 {
		t.Fatalf("default position=%d", got)
	}
	authorized := filepath.Join("Artist", "Album", "08 Song.flac")
	if got := playlistStreamReplacementPath(SearchPlan{Scene: "upgrade", LibraryTracks: []localWorkflowTrack{{Path: authorized}}}); got != authorized {
		t.Fatalf("replacement path=%q want=%q", got, authorized)
	}
	if got := playlistStreamReplacementPath(SearchPlan{Scene: "expand", LibraryTracks: []localWorkflowTrack{{Path: authorized}}}); got != "" {
		t.Fatalf("expand unexpectedly authorized replacement: %q", got)
	}
}

func TestNeteaseDefinitiveNoResultSkipsWithoutReviewLoop(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": t.TempDir()})
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true})
	t.Setenv("MUSICOPS_SERVICES_DIR", t.TempDir())
	var engineSearches atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		engineSearches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"songs": []any{}})
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)
	var builtInSearches atomic.Int32
	builtIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search/get/web" {
			builtInSearches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []any{}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer builtIn.Close()
	application.neteaseBases = map[string]string{"api": builtIn.URL}

	plan := SearchPlan{
		RootJobID: "netease-definitive-no-result", Scene: "expand", Query: "Artist Example & Test",
		ExpectedTitle: "First Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test",
		StreamingEnabled: true, NeteaseOnly: true, EnabledSites: []string{"dicmusic"}, BaseURL: builtIn.URL, Synthetic: true,
		Identity: Identity{ID: "netease-no-result-evidence", Kind: "api", Source: "playlist", IdentityConfirmed: true, TracklistVerified: true, Payload: "fixture"},
	}
	raw, _ := json.Marshal(plan)
	if _, err := application.flows.Start("start-"+plan.RootJobID, workflow.Start{
		JobID: plan.RootJobID, BatchID: "pl-netease-590536461-test", Scene: "expand", Stage: "identifying",
		Label: "First Album", Payload: raw, NowNS: application.now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	var process *workflow.Job
	for time.Now().Before(deadline) {
		process, _ = application.flows.Job("process:search:" + plan.RootJobID)
		if process != nil && (process.Stage == "skipped" || process.Stage == "failed" || process.Stage == "needs_review") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if process == nil || process.Stage != "skipped" {
		var activeTask *taskrepo.Task
		if process != nil && process.ActiveTaskID != "" {
			activeTask, _ = application.tasks.Get(process.ActiveTaskID)
		}
		t.Fatalf("process=%+v task=%+v health=%+v builtIn=%d engine=%d", process, activeTask, application.runner.Health(), builtInSearches.Load(), engineSearches.Load())
	}
	if builtInSearches.Load() != 1 || engineSearches.Load() != 1 {
		t.Fatalf("search calls builtIn=%d engine=%d", builtInSearches.Load(), engineSearches.Load())
	}
}
func TestUpgradeReplacesLossyFLACAtOriginalTrackNumber(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(folder, "08 Song.flac")
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-metadata", "artist=Artist", "-metadata", "album=Album", "-metadata", "title=Song", oldPath)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("generate old FLAC: %v %s", runErr, output)
	}
	oldBytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	incoming := application.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(incoming, "lossless-upgrade.flac")
	command = exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=880:duration=1",
		"-metadata", "artist=Artist", "-metadata", "album=Album", "-metadata", "title=Song", staged)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("generate lossless FLAC: %v %s", runErr, output)
	}
	body, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	body, err = flactags.WriteBytes(body, flactags.Tags{Raw: map[string][]string{"LYRICS": {"fixture lyric"}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err = flactags.EmbedFrontCover(body, solidJPEG(color.RGBA{R: 30, G: 50, B: 70, A: 255}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, body, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := SearchPlan{
		Scene: "upgrade", ExpectedArtist: "Artist", ExpectedTitle: "Album", PlaylistTrack: "Song",
		LibraryTracks: []localWorkflowTrack{{Path: oldPath, Artist: "Artist", Album: "Album", Title: "Song", Codec: "flac-lossy", Duration: 1}},
	}
	if position := playlistStreamPosition(plan); position != 8 {
		t.Fatalf("position=%d", position)
	}
	if _, err := application.ingestPlaylistStream(t.Context(), workflow.Job{RootID: "lib-upgrade-flac"}, plan, staged, playlistStreamPosition(plan)); err != nil {
		t.Fatal(err)
	}
	newBytes, err := os.ReadFile(oldPath)
	if err != nil || bytes.Equal(newBytes, oldBytes) {
		t.Fatalf("original FLAC was not replaced: bytes=%d err=%v", len(newBytes), err)
	}
	if tags := readTags(t, oldPath); tags.Title != "Song" || tags.Album != "Album" || tags.Artist != "Artist" {
		t.Fatalf("replaced tags=%+v", tags)
	}
}
func TestStreamSearchUsesPlaylistTrackForAlbumPlan(t *testing.T) {
	tests := []struct {
		name        string
		expected    string
		engineAlbum string
	}{
		{name: "album", expected: "First Album", engineAlbum: "First Album"},
		{name: "normalized compilation", expected: hotSlowCompilationAlbum, engineAlbum: hotSlowCompilationAlbum + " 27"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/search" || r.URL.Query().Get("sources") != "netease" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
					"id": "n3", "source": "netease", "name": "Example & Test",
					"artist": "Artist", "album": test.engineAlbum, "duration": 1,
				}}})
			}))
			defer server.Close()
			application := newApp(t)
			song, status, err := application.streamSearch(t.Context(), server.URL, "netease", SearchPlan{
				Query: "Artist Example & Test", ExpectedTitle: test.expected, ExpectedArtist: "Artist", PlaylistTrack: "Example & Test",
			}, "")
			if err != nil || status != "" || song == nil || song.Name != "Example & Test" {
				t.Fatalf("song=%+v status=%q err=%v", song, status, err)
			}
		})
	}
}

func TestNeteaseEngineAcceptsLossySourceForPlaylistFallback(t *testing.T) {
	mp3 := filepath.Join(t.TempDir(), "netease-engine.mp3")
	command := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libmp3lame", mp3)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate mp3: %v %s", err, output)
	}
	audio, err := os.ReadFile(mp3)
	if err != nil {
		t.Fatal(err)
	}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "lossy", "source": "netease", "name": "Example & Test",
				"artist": "Artist", "album": "First Album", "duration": 1,
			}}})
		case "/download":
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)
	path, note, class := application.tryConfiguredStream(t.Context(), "netease", SearchPlan{
		RootJobID: "netease-lossy-engine", Query: "Artist Example & Test",
		ExpectedTitle: "First Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test",
	})
	if path == "" || class != downloadpolicy.ClassDownload || !strings.Contains(note, "有损") {
		t.Fatalf("path=%q class=%q note=%q", path, class, note)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(body[:4]), "fLaC") {
		t.Fatalf("lossy source was not converted to FLAC: %v", err)
	}
	tags := readTags(t, path)
	if !isLossyFLACProvenance(strings.Join(tags.Raw["COMMENT"], "\n")) {
		t.Fatalf("lossy provenance missing: %+v", tags.Raw)
	}
}

func TestNeteaseEngineTranscodeFailureRemainsRetryable(t *testing.T) {
	corruptMP3 := append([]byte("ID3"), bytes.Repeat([]byte{0x55}, 4096)...)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "bad-lossy", "source": "netease", "name": "Example & Test",
				"artist": "Artist", "album": "First Album", "duration": 1,
			}}})
		case "/download":
			_, _ = w.Write(corruptMP3)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)
	path, note, class := application.tryConfiguredStream(t.Context(), "netease", SearchPlan{
		RootJobID: "netease-bad-lossy", Query: "Artist Example & Test",
		ExpectedTitle: "First Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test",
	})
	if path != "" || class != downloadpolicy.ClassRateLimited || !strings.Contains(note, "转 FLAC 失败：") {
		t.Fatalf("path=%q class=%q note=%q", path, class, note)
	}
}

func TestEngineTranscodeHonorsCancellation(t *testing.T) {
	application := newApp(t)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if _, bundled := application.bundledTools(); !bundled {
			t.Skip("ffmpeg unavailable")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := application.transcodeEngineAudio(ctx, []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), "netease", SearchPlan{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled transcode error=%v", err)
	}
}

func TestStreamSearchRejectsCardsFromAnotherProvider(t *testing.T) {
	var downloadCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if source := r.URL.Query().Get("sources"); source != "netease" {
				t.Errorf("search source=%q", source)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "t1", "source": "tidal", "name": "Example & Test",
				"artist": "Artist", "album": "First Album", "duration": 1,
			}}})
		case "/download":
			downloadCalls++
			_, _ = w.Write(oneSecondFLAC(t, "Artist", "Example & Test"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	application := newApp(t)
	song, status, err := application.streamSearch(t.Context(), server.URL, "netease", SearchPlan{
		ExpectedTitle: "Example & Test", ExpectedArtist: "Artist",
	}, "")
	if err != nil || status != "" || song != nil {
		t.Fatalf("song=%+v status=%q err=%v", song, status, err)
	}
	if downloadCalls != 0 {
		t.Fatalf("cross-provider card triggered %d downloads", downloadCalls)
	}
}

func TestVerifiedPlaylistTrackPublishesWhenAlbumDiffers(t *testing.T) {
	plan := SearchPlan{ExpectedTitle: "First Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test"}
	if !shouldPublishPlaylistStream(plan, "incoming/track.flac") {
		t.Fatal("a verified track from an album plan must continue to ingest")
	}
	if shouldPublishPlaylistStream(SearchPlan{ExpectedTitle: "First Album"}, "incoming/track.flac") {
		t.Fatal("an album-only plan without a selected track was published as a single track")
	}
	if shouldPublishPlaylistStream(plan, "") {
		t.Fatal("a playlist plan without validated audio was published")
	}
}

func TestNeteaseAlbumEmptySearchFallsBackToEngineAndPublishes(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	t.Setenv("MUSICOPS_SERVICES_DIR", t.TempDir())

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	audioPath := filepath.Join(t.TempDir(), "engine-source.flac")
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-metadata", "artist=Artist", "-metadata", "album=First Album", "-metadata", "title=Example & Test", audioPath)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("generate FLAC: %v %s", runErr, output)
	}
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	audio, err = flactags.WriteBytes(audio, flactags.Tags{Raw: map[string][]string{"LYRICS": {"fixture lyric"}}})
	if err != nil {
		t.Fatal(err)
	}
	audio, err = flactags.EmbedFrontCover(audio, solidJPEG(color.RGBA{R: 20, G: 40, B: 60, A: 255}))
	if err != nil {
		t.Fatal(err)
	}

	var engineSearches, engineDownloads atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if source := r.URL.Query().Get("sources"); source != "netease" {
				t.Errorf("engine source=%q", source)
			}
			engineSearches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"songs": []map[string]any{{
				"id": "n4", "source": "netease", "name": "Example & Test",
				"artist": "Artist", "album": "First Album", "duration": 1,
			}}})
		case "/download":
			engineDownloads.Add(1)
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)

	var builtInSearches atomic.Int32
	builtIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search/get/web" {
			builtInSearches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []any{}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer builtIn.Close()
	application.neteaseBases = map[string]string{"api": builtIn.URL}

	plan := SearchPlan{
		RootJobID: "netease-empty-search-engine", Scene: "expand", Query: "Artist Example & Test",
		ExpectedTitle: "First Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test",
		StreamingEnabled: true, NeteaseOnly: true, EnabledSites: []string{},
		Identity: Identity{ID: "netease-empty-search-evidence", Kind: "api", Source: "playlist", IdentityConfirmed: true, TracklistVerified: true, Payload: "fixture"},
	}
	raw, _ := json.Marshal(plan)
	if _, err := application.flows.Start("start-"+plan.RootJobID, workflow.Start{
		JobID: plan.RootJobID, BatchID: "pl-netease-590536461-test", Scene: "expand", Stage: "identifying",
		Label: "First Album", Payload: raw, NowNS: application.now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	var rootJob workflow.Job
	var searchJob, processJob *workflow.Job
	rootPublished := false
	for time.Now().Before(deadline) {
		rootJob = inspect(t, application, plan.RootJobID).Job
		searchJob, _ = application.flows.Job("search:" + plan.RootJobID)
		processJob, _ = application.flows.Job("process:search:" + plan.RootJobID)
		if task, err := application.tasks.Get("stage:" + plan.RootJobID + ":publishing"); err == nil && task != nil && task.State == "completed" && rootJob.Stage == "completed" {
			rootPublished = true
			break
		}
		for _, current := range []*workflow.Job{searchJob, processJob} {
			if current != nil && current.ID != "" && (current.Stage == "failed" || current.Stage == "skipped" || current.Stage == "needs_review") {
				t.Fatalf("search stopped before publish root=%+v search=%+v process=%+v builtInSearch=%d engineSearch=%d engineDownload=%d", rootJob, searchJob, processJob, builtInSearches.Load(), engineSearches.Load(), engineDownloads.Load())
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !rootPublished {
		t.Fatalf("publish timeout root=%+v search=%+v process=%+v builtInSearch=%d engineSearch=%d engineDownload=%d", rootJob, searchJob, processJob, builtInSearches.Load(), engineSearches.Load(), engineDownloads.Load())
	}
	if builtInSearches.Load() != 1 {
		t.Fatalf("built-in search calls=%d engineSearch=%d engineDownload=%d root=%+v process=%+v", builtInSearches.Load(), engineSearches.Load(), engineDownloads.Load(), rootJob, processJob)
	}
	if engineSearches.Load() != 1 || engineDownloads.Load() != 1 {
		t.Fatalf("engine search/download=%d/%d", engineSearches.Load(), engineDownloads.Load())
	}
	var published string
	_ = filepath.WalkDir(library, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".flac") {
			published = path
		}
		return nil
	})
	if published == "" {
		t.Fatal("engine fallback did not publish a library file")
	}
	if tags := readTags(t, published); tags.Title != "Example & Test" || tags.Album != "First Album" || tags.Artist != "Artist" {
		t.Fatalf("published tags=%+v", tags)
	}
}

func TestNeteaseAlbumSearchFailureStaysRetryableWhenEngineAlsoFails(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary engine failure", http.StatusBadGateway)
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)

	builtIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary NetEase failure", http.StatusServiceUnavailable)
	}))
	defer builtIn.Close()
	application.neteaseBases = map[string]string{"api": builtIn.URL}

	plan := SearchPlan{
		RootJobID: "netease-retryable-engine-failure", Scene: "expand", Query: "Artist Example & Test",
		ExpectedTitle: "First Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test",
		StreamingEnabled: true, NeteaseOnly: true, EnabledSites: []string{},
		Identity: Identity{ID: "netease-retryable-evidence", Kind: "api", Source: "playlist", IdentityConfirmed: true, TracklistVerified: true, Payload: "fixture"},
	}
	raw, _ := json.Marshal(plan)
	if _, err := application.flows.Start("start-"+plan.RootJobID, workflow.Start{
		JobID: plan.RootJobID, BatchID: "pl-netease-590536461-test", Scene: "expand", Stage: "identifying",
		Label: "First Album", Payload: raw, NowNS: application.now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	var job workflow.Job
	for time.Now().Before(deadline) {
		current, _ := application.flows.Job("process:search:" + plan.RootJobID)
		if current != nil {
			job = *current
			if job.Stage == "needs_review" || job.Stage == "failed" || job.Stage == "skipped" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Stage != "needs_review" || job.ErrorClass != "STREAM_RETRY" || !strings.Contains(job.Message, "自动核对重试") {
		t.Fatalf("temporary failures lost their retry classification: %+v", job)
	}
}
