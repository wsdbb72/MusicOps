package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLibraryListsReleasedAudio(t *testing.T) {
	application := newApp(t)
	empty := getAuth(t, application, "/v1/library")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"entries":[]`) || !strings.Contains(empty.Body.String(), `"count":0`) {
		t.Fatal(empty.Code, empty.Body.String())
	}
	post(t, application, "/v1/search", searchBody("board-lib", "http://127.0.0.1:9", []string{}, false))
	waitJob(t, application, "search:board-lib", "needs_review")
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "song.wav"), pcmWAV(24), 0o600); err != nil {
		t.Fatal(err)
	}
	released := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-lib", "artist": "Artist", "album": "Album", "title": "Title", "track": 1, "source_name": "song.wav",
	})
	if released.Code != http.StatusOK {
		t.Fatal(released.Code, released.Body.String())
	}
	waitJob(t, application, "board-lib", "completed")
	listed := getAuth(t, application, "/v1/library")
	var decoded struct {
		OK      bool `json:"ok"`
		Count   int  `json:"count"`
		Entries []struct {
			File        string `json:"file"`
			Format      string `json:"format"`
			DecodeState string `json:"decode_state"`
			Origin      string `json:"origin"`
			FilePresent bool   `json:"file_present"`
			RootID      string `json:"root_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &decoded); err != nil || !decoded.OK || decoded.Count != 1 || len(decoded.Entries) != 1 {
		t.Fatal(decoded, err, listed.Body.String())
	}
	entry := decoded.Entries[0]
	// DecodeState depends on the host: "decoded" when ffmpeg/ffprobe are
	// installed, "structural" otherwise. Both are honest receipts.
	if entry.File != "Artist/Album/01 Title.flac" || entry.Format != "flac" || (entry.DecodeState != "structural" && entry.DecodeState != "decoded") || entry.Origin != "local_import" || !entry.FilePresent || entry.RootID != "board-lib" {
		t.Fatal(entry)
	}
}

func TestLogsListsRecentTasks(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("board-log", "http://127.0.0.1:9", []string{}, false))
	waitJob(t, application, "search:board-log", "needs_review")
	listed := getAuth(t, application, "/v1/logs")
	var decoded struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
		Tasks []struct {
			ID      string `json:"task_id"`
			Command string `json:"command"`
			State   string `json:"state"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &decoded); err != nil || !decoded.OK || decoded.Count == 0 {
		t.Fatal(decoded, err, listed.Body.String())
	}
	seen := map[string]bool{}
	for _, task := range decoded.Tasks {
		seen[task.Command] = true
	}
	if !seen["identify_release"] || !seen["search_release"] {
		t.Fatal(decoded.Tasks)
	}
}

func TestSiteDefaultsApplyToSearch(t *testing.T) {
	application := newApp(t)
	// No settings yet: every site disabled.
	initial := getAuth(t, application, "/v1/sites")
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), `"count":0`) {
		t.Fatal(initial.Code, initial.Body.String())
	}
	var initialDecoded struct {
		Sites []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(initial.Body.Bytes(), &initialDecoded); err != nil || len(initialDecoded.Sites) != 4 {
		t.Fatal(initialDecoded, err)
	}
	for _, site := range initialDecoded.Sites {
		if site.Enabled {
			t.Fatal("site enabled by default", site)
		}
	}
	// An unknown site is rejected.
	bad := postCode(t, application, "/v1/sites", map[string]any{"enabled_sites": []string{"example.com"}})
	if bad.Code != http.StatusBadRequest {
		t.Fatal(bad.Code, bad.Body.String())
	}
	// Save dicmusic only.
	saved := postCode(t, application, "/v1/sites", map[string]any{"enabled_sites": []string{"dicmusic"}})
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"count":1`) {
		t.Fatal(saved.Code, saved.Body.String())
	}
	// A search that names no sites queries only the saved default.
	var mu sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("X-MusicOps-Site")] = true
		mu.Unlock()
		io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`)
	}))
	defer server.Close()
	body := searchBody("board-sites", server.URL, nil, false)
	delete(body, "enabled_sites")
	post(t, application, "/v1/search", body)
	waitJob(t, application, "search:board-sites", "completed")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || !seen["dicmusic"] {
		t.Fatal(seen)
	}
}
