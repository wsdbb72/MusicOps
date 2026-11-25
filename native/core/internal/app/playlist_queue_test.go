package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
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

func TestPlaylistAlbumGroupingUsesSourceAlbumIDAndCompilationKey(t *testing.T) {
	first := playlistTrack{Artist: "Artist One", Album: "Shared Album", NeteaseAlbumID: 99}
	second := playlistTrack{Artist: "Artist Two", Album: "Shared Album", NeteaseAlbumID: 99}
	if playlistAlbumGroupKey(first, first.Album) != playlistAlbumGroupKey(second, second.Album) {
		t.Fatal("tracks from one NetEase album were split by their per-track artists")
	}
	hotOne := playlistTrack{Artist: "Artist One"}
	hotTwo := playlistTrack{Artist: "Artist Two"}
	album := hotSlowCompilationAlbum + " 24"
	if playlistAlbumGroupKey(hotOne, album) != playlistAlbumGroupKey(hotTwo, album) {
		t.Fatal("compilation tracks were split into different album jobs")
	}
	other := playlistTrack{Artist: "Artist One", Album: "Shared Album"}
	if playlistAlbumGroupKey(first, first.Album) == playlistAlbumGroupKey(other, other.Album) {
		t.Fatal("album ID grouping collided with an unrelated album")
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
func TestPlaylistReenqueueStageClassification(t *testing.T) {
	for _, stage := range []string{"preparing", "identifying", "snapshotting", "metadata", "searching", "requesting", "downloading", "fallback", "validating", "publishing", "notifying"} {
		if !playlistJobReusable(stage) {
			t.Fatalf("active stage %q was not reusable", stage)
		}
	}
	for _, stage := range []string{"completed", "failed", "cancelled", "skipped", "needs_review"} {
		if playlistJobReusable(stage) {
			t.Fatalf("terminal stage %q was reusable", stage)
		}
	}
}
func TestPlaylistReenqueueReusesActiveAndReplacesTerminalJob(t *testing.T) {
	application := newApp(t)
	application.mu.Lock()
	application.paused = true
	application.mu.Unlock()
	row := playlistTrack{Artist: "Artist", Album: "Album", Title: "Song", NeteaseSongID: 1, NeteaseAlbumID: 2}
	first, err := application.startPlaylistAlbumJob(context.Background(), nil, "pl-netease-1", []string{"dicmusic"}, 0, row, true, false)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := application.startPlaylistAlbumJob(context.Background(), nil, "pl-netease-1", []string{"dicmusic"}, 0, row, true, false)
	if err != nil || reused != first {
		t.Fatalf("active job was not reused: first=%s reused=%s err=%v", first, reused, err)
	}
	if _, err := application.flows.Control("cancel-reenqueue-test", first, "cancel", application.now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	replacement, err := application.startPlaylistAlbumJob(context.Background(), nil, "pl-netease-1", []string{"dicmusic"}, 0, row, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == first {
		t.Fatal("terminal job was returned instead of a fresh revision")
	}
	job, err := application.flows.Job(replacement)
	if err != nil || job == nil || job.Stage != "identifying" {
		t.Fatal(job, err)
	}
}

func TestPlaylistReenqueueCancelsLiveDescendantOfCompletedRoot(t *testing.T) {
	application := newApp(t)
	application.mu.Lock()
	application.paused = true
	application.mu.Unlock()
	row := playlistTrack{Artist: "Artist", Album: "Album", Title: "Song", NeteaseSongID: 1, NeteaseAlbumID: 2}
	first, err := application.startPlaylistAlbumJob(context.Background(), nil, "pl-netease-descendant", []string{"dicmusic"}, 0, row, true, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	token := claimAndBeginWorkflowTask(t, application, first, "identify", now+100)
	if _, err := application.flows.Complete("handoff-"+first, token, workflow.Complete{
		JobID: first, Edge: workflow.EdgeQueueSearch, TaskState: "completed", NowNS: now + 200,
		Evidence: []workflow.EvidenceInput{{ID: "identity-" + first, Kind: "api", Source: "musicbrainz", SubjectID: "release-live-descendant", TracklistVerified: true, IdentityConfirmed: true, Payload: []byte("verified tracklist")}},
	}); err != nil {
		t.Fatal(err)
	}
	searchID := "search:" + first
	token = claimAndBeginWorkflowTask(t, application, searchID, "identify", now+300)
	if _, err := application.flows.Interrupt("interrupt-"+searchID, token, now+400, "search interrupted"); err != nil {
		t.Fatal(err)
	}

	replacement, err := application.startPlaylistAlbumJob(context.Background(), nil, "pl-netease-descendant", []string{"dicmusic"}, 0, row, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == first {
		t.Fatal("completed root with a live descendant was reused")
	}
	oldSearch, err := application.flows.Job(searchID)
	if err != nil || oldSearch == nil || oldSearch.Stage != "cancelled" || !oldSearch.CancelRequested {
		t.Fatalf("old descendant was not cancelled: job=%+v err=%v", oldSearch, err)
	}
}
