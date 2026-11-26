package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaylistImportsJSONCSVAndTextWithoutFetching(t *testing.T) {
	application := newApp(t)
	saved := post(t, application, "/v1/playlists", map[string]any{
		"name":    "JSON Mix",
		"content": `{"name":"JSON Mix","tracks":[{"title":"Song One","artist":"Artist A","album":"Album","url":"https://example.invalid/secret"}]}`,
	})
	playlist, _ := saved["playlist"].(map[string]any)
	if playlist["track_count"] != float64(1) {
		t.Fatal(saved)
	}
	stored, err := os.ReadFile(filepath.Join(application.Dir, "playlists", "json-mix.m3u"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "example.invalid") || strings.Contains(string(stored), "secret") {
		t.Fatal("imported URL was stored")
	}
	if !strings.Contains(string(stored), "#MUSICOPS-ALBUM:Album") || !strings.Contains(string(stored), "Artist A - Song One") {
		t.Fatal(string(stored))
	}

	table := post(t, application, "/v1/playlists", map[string]any{
		"name":    "Table",
		"content": "歌名,歌手,专辑\n第二首,艺人乙,专辑乙\n",
	})
	if table["ok"] != true || table["playlist"].(map[string]any)["track_count"] != float64(1) {
		t.Fatal(table)
	}
	text := post(t, application, "/v1/playlists", map[string]any{
		"name": "Lines", "text_order": "title-artist", "content": "歌名甲 - 艺人甲\n",
	})
	if text["ok"] != true {
		t.Fatal(text)
	}
	ambiguous := postCode(t, application, "/v1/playlists", map[string]any{"name": "Bad", "content": "only a title\n"})
	if ambiguous.Code != 400 || !strings.Contains(ambiguous.Body.String(), "顺序") {
		t.Fatal(ambiguous.Code, ambiguous.Body.String())
	}
}

func TestPlaylistLocateDoesNotFollowShortLinks(t *testing.T) {
	application := newApp(t)
	found := post(t, application, "/v1/playlists/locate", map[string]any{
		"text": "请看 https://music.163.com/playlist?id=998877 和 https://y.qq.com/n/ryqq/playlist/12345",
	})
	rows, _ := found["playlists"].([]any)
	if found["fetched"] != false || len(rows) != 2 {
		t.Fatal(found)
	}
	short := post(t, application, "/v1/playlists/locate", map[string]any{"text": "https://b23.tv/abcDEF"})
	shortRows, _ := short["playlists"].([]any)
	if len(shortRows) != 1 {
		t.Fatal(short)
	}
	row := shortRows[0].(map[string]any)
	if row["short_url"] != true || row["playlist_id"] != nil {
		t.Fatal(row)
	}
	if !strings.Contains(row["note"].(string), "不会被跟随") {
		t.Fatal(row)
	}
}

func TestSubscriptionRecordsExistingTracksThenQueuesOnlyNewOnes(t *testing.T) {
	application := newApp(t)
	content := "#PLAYLIST:Watch\n#MUSICOPS-ALBUM:Album\n#EXTINF:1,Artist A - Song One\n.\n#MUSICOPS-ALBUM:Album\n#EXTINF:1,Artist A - Song Two\n.\n"
	saved := post(t, application, "/v1/playlists", map[string]any{"name": "Watch", "content": content})
	if saved["ok"] != true {
		t.Fatal(saved)
	}
	subscribed := post(t, application, "/v1/subscriptions", map[string]any{"playlist_id": "watch", "interval_hours": 6})
	if subscribed["ok"] != true {
		t.Fatal(subscribed)
	}
	first := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "watch"})
	if first["queued"] != float64(0) || first["downloaded"] != false {
		t.Fatal(first)
	}
	if jobs := first["job_ids"].([]any); len(jobs) != 0 {
		t.Fatal(first)
	}
	path := filepath.Join(application.Dir, "playlists", "watch.m3u")
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := string(current) + "#MUSICOPS-ALBUM:Album\n#EXTINF:1,Artist A - Song Three\n.\n"
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	second := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "watch"})
	if second["queued"] != float64(1) || second["downloaded"] != false {
		t.Fatal(second)
	}
	ids := second["job_ids"].([]any)
	if len(ids) != 1 {
		t.Fatal(second)
	}
	message, _ := second["message"].(string)
	if !strings.Contains(message, "尚未下载") {
		t.Fatal(second)
	}
	seen := inspect(t, application, ids[0].(string))
	if seen.Job.Scene != "playlist" || seen.Job.SourceTrackCount != 1 {
		t.Fatal(seen.Job)
	}
	third := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "watch"})
	if third["queued"] != float64(0) {
		t.Fatal(third)
	}
}

func TestSubscriptionRetriesSeenTrackThatIsStillMissing(t *testing.T) {
	application := newApp(t)
	content := "#PLAYLIST:Retry\n#MUSICOPS-ALBUM:Album\n#EXTINF:1,Artist A - Missing Song\n.\n"
	if saved := post(t, application, "/v1/playlists", map[string]any{"name": "Retry", "content": content}); saved["ok"] != true {
		t.Fatal(saved)
	}
	if subscribed := post(t, application, "/v1/subscriptions", map[string]any{"playlist_id": "retry", "interval_hours": 24}); subscribed["ok"] != true {
		t.Fatal(subscribed)
	}
	first := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "retry"})
	if first["queued"] != float64(0) {
		t.Fatal(first)
	}
	rows, err := application.loadSubscriptions()
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	rows[0].RetryMissing = map[string]string{}
	if err := application.storeSubscriptions(rows); err != nil {
		t.Fatal(err)
	}
	second := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "retry"})
	if second["queued"] != float64(1) || !strings.Contains(second["message"].(string), "仍未入库") {
		t.Fatal(second)
	}
	third := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "retry"})
	if third["queued"] != float64(0) {
		t.Fatal(third)
	}
}

func TestSubscriptionQueuesMissingTargetAlbumWhenSongExistsElsewhere(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	if response := post(t, application, "/v1/settings/library", map[string]any{"library_dir": library}); response["ok"] != true {
		t.Fatal(response)
	}
	indexPlaylistTestFile(t, application, library,
		"Compilation/Artist A - Song One.flac",
		"Artist A", "Compilation", "Song One")
	application.invalidatePlaylistLibraryCache()
	content := "#PLAYLIST:Album target\n#MUSICOPS-ALBUM:Single Album\n#EXTINF:200,Artist A - Song One\n.\n"
	if saved := post(t, application, "/v1/playlists", map[string]any{"name": "Album target", "content": content}); saved["ok"] != true {
		t.Fatal(saved)
	}
	if subscribed := post(t, application, "/v1/subscriptions", map[string]any{"playlist_id": "album-target", "interval_hours": 6, "include_existing": true}); subscribed["ok"] != true {
		t.Fatal(subscribed)
	}
	checked := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "album-target"})
	if checked["queued"] != float64(1) {
		t.Fatalf("the target album was suppressed by a recording in another release: %+v", checked)
	}
	ids := checked["job_ids"].([]any)
	if len(ids) != 1 {
		t.Fatal(checked)
	}
	job, err := application.flows.Job(ids[0].(string))
	if err != nil || job == nil || job.SourceTrackCount != 1 || !strings.Contains(job.Label, "Single Album") {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestSubscriptionNeteaseJobCarriesConfirmedSourceIdentity(t *testing.T) {
	application := newApp(t)
	jobID := "sub-netease-590536461-test"
	tracks := []playlistTrack{{
		Artist: "Artist", Title: "Song", Album: "Album", Duration: "180",
		NeteaseSongID: 42, NeteaseAlbumID: 84,
	}}
	if err := application.queueSubscriptionAlbum(jobID, "netease-590536461", "Album", tracks); err != nil {
		t.Fatal(err)
	}
	job, err := application.flows.Job(jobID)
	if err != nil || job == nil || job.ActiveTaskID == "" {
		t.Fatal(job, err)
	}
	task, err := application.tasks.Get(job.ActiveTaskID)
	if err != nil || task == nil {
		t.Fatal(task, err)
	}
	raw, found, err := application.tasks.Data(task.ID, "arguments", 0)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	var plan SearchPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.Identity.IdentityConfirmed || !plan.Identity.TracklistVerified || plan.Identity.Source != "netease" ||
		!plan.NeteaseOnly || !plan.NeteaseTrackOnly || plan.PlaylistTrack != "Song" || plan.NeteaseSongID != 42 || plan.NeteaseAlbumID != 84 {
		t.Fatal(plan)
	}
	if !isNeteasePlaylistBatch(jobID) {
		t.Fatal("subscription NetEase job was not recognized as a NetEase playlist batch")
	}
}

func TestNeteaseSubscriptionRecoveryReconstructsLegacyIdentity(t *testing.T) {
	application := newApp(t)
	jobID := "sub-netease-590536461-legacy"
	incoming := application.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	tracks := []playlistTrack{
		{Artist: "Artist", Title: "One", Album: "Album", Duration: "180", NeteaseSongID: 1, NeteaseAlbumID: 9},
		{Artist: "Artist", Title: "Two", Album: "Album", Duration: "181", NeteaseSongID: 2, NeteaseAlbumID: 9},
	}
	if err := os.WriteFile(filepath.Join(incoming, playlistFileName(jobID)), encodeM3U("Album", tracks), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := SearchPlan{RootJobID: jobID, Scene: "playlist", Query: "Album", ExpectedTitle: "Album"}
	if !application.recoverNeteasePlaylistIDs(context.Background(), jobID, &plan) {
		t.Fatal("legacy subscription identity was not recovered")
	}
	if !plan.Identity.IdentityConfirmed || !plan.Identity.TracklistVerified || plan.Identity.Source != "netease" ||
		!plan.NeteaseOnly || plan.NeteaseTrackOnly || plan.NeteaseAlbumID != 9 {
		t.Fatal(plan)
	}
	if id := neteasePlaylistIDFromBatch(jobID); id != "590536461" {
		t.Fatal(id)
	}
}
