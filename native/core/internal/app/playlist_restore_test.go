package app

import (
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
