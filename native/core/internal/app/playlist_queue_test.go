package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "歌单入库" must create a job: saving the name list alone left the page
// looking like nothing happened.
func TestPlaylistFetchEnqueuesAJob(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/playlist/detail", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"playlist": map[string]any{
			"id": 42, "name": "小歌单", "trackCount": 2,
			"tracks": []map[string]any{
				{"id": 1, "name": "第一首", "ar": []map[string]any{{"name": "甲"}}, "al": map[string]any{"name": "专"}, "dt": 200000},
				{"id": 2, "name": "第二首", "ar": []map[string]any{{"name": "乙"}}, "al": map[string]any{"name": "专"}, "dt": 200000},
			},
			"trackIds": []map[string]any{{"id": 1, "v": 0}, {"id": 2, "v": 0}},
		}})
	})
	mux.HandleFunc("/api/v3/song/detail", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "songs": []map[string]any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	application := newApp(t)
	application.playlistBases = map[string]string{"netease": server.URL}

	body := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "netease", "playlist_id": "42", "enqueue": true,
	})
	jobID, _ := body["job_id"].(string)
	if jobID == "" {
		t.Fatal(body)
	}
	if body["note"] == "" || !strings.Contains(body["note"].(string), "已创建歌单入库任务") {
		t.Fatal(body["note"])
	}
	m3u := filepath.Join(application.Dir, "incoming", "playlist-"+jobID+".m3u")
	if _, err := os.Stat(m3u); err != nil {
		t.Fatal(err)
	}
	jobs := getAuth(t, application, "/v1/jobs")
	if jobs.Code != http.StatusOK || !strings.Contains(jobs.Body.String(), jobID) {
		t.Fatal(jobs.Body.String())
	}
	if !strings.Contains(jobs.Body.String(), `"scene":"playlist"`) {
		t.Fatal(jobs.Body.String())
	}
}

// A share link is accepted in the same field as a bare id.
func TestPlaylistFetchAcceptsAShareLink(t *testing.T) {
	application := newApp(t)
	recorder := postCode(t, application, "/v1/playlists/fetch", map[string]any{
		"playlist_id": "https://music.163.com/playlist?id=590536461&uct2=abc=", "enqueue": false,
	})
	// No network double here: the point is that the id was extracted and the
	// request reached the platform step instead of failing as a bad id.
	if recorder.Code == http.StatusBadRequest && strings.Contains(recorder.Body.String(), "歌单 ID") {
		t.Fatal(recorder.Body.String())
	}
}
