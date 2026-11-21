package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseM3U(t *testing.T) {
	rows, name, err := parseM3U([]byte("#PLAYLIST:My Mix\n#EXTINF:123,Artist A - Song One\n/media/a.mp3\n#EXTINF:200,Artist B - Song Two\n/media/b.flac\n"))
	if err != nil || name != "My Mix" || len(rows) != 2 || rows[0].Artist != "Artist A" || rows[0].Title != "Song One" || rows[0].Duration != "123" || rows[1].Artist != "Artist B" || rows[1].Title != "Song Two" {
		t.Fatal(rows, name, err)
	}
	// A bare URI without EXTINF is rejected: no guessing from the path.
	if _, _, err := parseM3U([]byte("/media/a.mp3\n")); err == nil {
		t.Fatal("bare URI accepted")
	}
	// A dangling EXTINF is rejected.
	if _, _, err := parseM3U([]byte("#EXTINF:123,Artist A - Song One\n")); err == nil {
		t.Fatal("dangling EXTINF accepted")
	}
	// A malformed EXTINF is rejected.
	if _, _, err := parseM3U([]byte("#EXTINF:abc,Artist A - Song One\n/media/a.mp3\n")); err == nil {
		t.Fatal("malformed EXTINF accepted")
	}
	// A title without an artist separator is kept as a title.
	rows, _, err = parseM3U([]byte("#EXTINF:99,Just A Title\n/media/a.mp3\n"))
	if err != nil || len(rows) != 1 || rows[0].Artist != "" || rows[0].Title != "Just A Title" {
		t.Fatal(rows, err)
	}
}

func emptyResultsServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
}

func TestStreamingFallbackWithoutPlaylist(t *testing.T) {
	server := emptyResultsServer(t)
	defer server.Close()
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("album-stream", server.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-stream", "skipped")
	if job.ReviewKind != "" || !strings.Contains(job.Message, "tidal 未接入") || !strings.Contains(job.Message, "playlist 未找到歌单文件") || !strings.Contains(job.Message, "已自动结束") || !strings.Contains(job.Message, "尚未下载") {
		t.Fatal(job)
	}
	detail := inspect(t, application, "process:search:album-stream")
	found := false
	for _, item := range detail.Evidence {
		if item.Source == "streaming" && item.Kind == "api" {
			found = true
		}
	}
	if !found {
		t.Fatal("streaming walk evidence missing", detail.Evidence)
	}
}

func TestStreamingFallbackImportsPlaylist(t *testing.T) {
	server := emptyResultsServer(t)
	defer server.Close()
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	playlist := "#PLAYLIST:Test Mix\n#EXTINF:123,Artist A - Song One\n/media/a.mp3\n#EXTINF:200,Artist B - Song Two\n/media/b.flac\n"
	if err := os.WriteFile(filepath.Join(incoming, "playlist-album-pl.m3u"), []byte(playlist), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/search", searchBody("album-pl", server.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-pl", "needs_review")
	if job.ReviewKind != "candidate" || !strings.Contains(job.Message, "已读取 2 首曲目") || !strings.Contains(job.Message, "Test Mix") {
		t.Fatal(job)
	}
	detail := inspect(t, application, "process:search:album-pl")
	found := false
	for _, item := range detail.Evidence {
		if item.Source == "playlist" && item.Kind == "catalog" {
			found = true
		}
	}
	if !found {
		t.Fatal("playlist evidence missing", detail.Evidence)
	}
}

func TestStreamingFallbackRejectsBadPlaylist(t *testing.T) {
	server := emptyResultsServer(t)
	defer server.Close()
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "playlist-album-bad.m3u"), []byte("/media/a.mp3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/search", searchBody("album-bad", server.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-bad", "skipped")
	if job.ReviewKind != "" || !strings.Contains(job.Message, "无法解析") || !strings.Contains(job.Message, "已自动结束") {
		t.Fatal(job)
	}
}

func TestStreamingSourceStatus(t *testing.T) {
	application := newApp(t)
	if err := application.runner.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	data := getJSON(t, application, "/v1/settings/download")
	if data["streaming_fallback_wired"] != true {
		t.Fatal(data)
	}
	status, _ := data["streaming_source_status"].([]any)
	if len(status) == 0 {
		t.Fatal(data)
	}
	byName := map[string]map[string]any{}
	for _, item := range status {
		entry, _ := item.(map[string]any)
		byName[entry["name"].(string)] = entry
	}
	if byName["tidal"]["configured"] != false || byName["playlist"]["configured"] != true {
		t.Fatal(byName)
	}
}

func TestStreamingSourceStatusRecognizesBundledEngine(t *testing.T) {
	services := t.TempDir()
	suffix := ""
	if filepath.Separator == '\\' {
		suffix = ".exe"
	}
	binary := filepath.Join(services, string(engineStreaming), string(engineStreaming)+suffix)
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_SERVICES_DIR", services)
	application := newApp(t)
	status := application.streamingSourceStatus()
	for _, entry := range status {
		if entry.Name == "tidal" || entry.Name == "applemusic" || entry.Name == "qqmusic" {
			if !entry.Configured {
				t.Fatalf("bundled go-music-dl was not reported for %s: %+v", entry.Name, entry)
			}
		}
	}
}
