package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestPublicPlaylistFetchAndSubscriptionStayMetadataOnly(t *testing.T) {
	var songs atomic.Value
	songs.Store(`{"code":0,"cdlist":[{"disstid":123,"dissname":"公开歌单","songnum":1,"songlist":[{"name":"第一首","singer":[{"name":"甲"}],"albumname":"甲专","interval":200}]}]}`)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/qzone/fcg-bin/fcg_ucc_getcdinfo_byids_cp.fcg" || r.URL.Query().Get("disstid") != "123" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Cookie") != "" {
			t.Errorf("public fetch sent a cookie: %s", r.Header.Get("Cookie"))
		}
		_, _ = w.Write([]byte(songs.Load().(string)))
	}))
	defer server.Close()
	application := newApp(t)
	fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "qqmusic", "playlist_id": "123", "base_url": server.URL, "save": true, "subscribe": true,
	})
	if fetched["downloaded"] != false || fetched["imported"] != false || fetched["track_count"] != float64(1) || fetched["playlist_id"] != "qqmusic-123" {
		t.Fatal(fetched)
	}
	application.playlistBases = map[string]string{"qqmusic": server.URL}
	first := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "qqmusic-123"})
	if first["queued"] != float64(0) || first["downloaded"] != false {
		t.Fatal(first)
	}
	songs.Store(`{"code":0,"cdlist":[{"disstid":123,"dissname":"公开歌单","songnum":2,"songlist":[{"name":"第一首","singer":[{"name":"甲"}],"albumname":"甲专","interval":200},{"name":"第二首","singer":[{"name":"乙"}],"albumname":"乙专","interval":180}]}]}`)
	second := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "qqmusic-123"})
	if second["downloaded"] != false || second["queued"] == float64(0) {
		t.Fatal(second)
	}
	message, _ := second["message"].(string)
	if message == "" || !contains(message, "尚未下载") {
		t.Fatal(second)
	}
	if calls.Load() < 3 {
		t.Fatal(calls.Load())
	}
}

func TestPlaylistFetchRefusesRemoteBaseAndMissingCookie(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	application := newApp(t)
	remote := postCode(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "qqmusic", "playlist_id": "123", "base_url": "https://c.y.qq.com",
	})
	if remote.Code != http.StatusBadGateway || called {
		t.Fatal(remote.Code, remote.Body.String(), called)
	}
	if err := application.casSaveJSON([]byte("app/settings/playlist-sources"), []byte(`{"kuwo":{"credential_ref":"vault://playlist/kuwo"}}`)); err != nil {
		t.Fatal(err)
	}
	missing := postCode(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "kuwo", "playlist_id": "9", "base_url": server.URL,
	})
	if missing.Code != http.StatusBadGateway || called {
		t.Fatal(missing.Code, missing.Body.String(), called)
	}
}

func TestKuwoPlaylistReadsThePublicPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/newh5app/wapi/api/www/playlist/playListInfo" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"id":"9","name":"酷我公开","total":1,"musicList":[{"name":"夜曲","artist":"周杰伦","album":"十一月","duration":230}]}}`))
	}))
	defer server.Close()
	application := newApp(t)
	fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "kuwo", "playlist_id": "9", "base_url": server.URL,
	})
	if fetched["downloaded"] != false || fetched["track_count"] != float64(1) || fetched["name"] != "酷我公开" {
		t.Fatal(fetched)
	}
}

func TestPlaylistFetchInfersPlatformFromAQQShareLink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/qzone/fcg-bin/fcg_ucc_getcdinfo_byids_cp.fcg" || r.URL.Query().Get("disstid") != "123" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"cdlist":[{"disstid":123,"dissname":"链接歌单","songnum":1,"songlist":[{"name":"第一首","singer":[{"name":"甲"}],"albumname":"甲专","interval":200}]}]}`))
	}))
	defer server.Close()
	application := newApp(t)
	found, locateErr := parsePlaylistLocator("https://y.qq.com/n/ryqq/playlist/123", "auto")
	if locateErr != nil || len(found) != 1 || found[0].Platform != "qqmusic" || found[0].PlaylistID != "123" {
		t.Fatalf("locator=%+v err=%v", found, locateErr)
	}
	fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
		"playlist_id": "https://y.qq.com/n/ryqq/playlist/123", "base_url": server.URL, "save": true,
	})
	if fetched["platform"] != "qqmusic" || fetched["playlist_id"] != "qqmusic-123" || fetched["track_count"] != float64(1) {
		t.Fatal(fetched)
	}
	mismatch := postCode(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "netease", "playlist_id": "https://y.qq.com/n/ryqq/playlist/123", "base_url": server.URL, "save": true,
	})
	if mismatch.Code != http.StatusBadRequest {
		t.Fatal(mismatch.Code, mismatch.Body.String())
	}
}

func TestIncompletePlaylistNeverOverwritesTheSavedListOrQueues(t *testing.T) {
	server := incompleteQQPlaylistServer(t)
	defer server.Close()

	application := newApp(t)
	slug := "qqmusic-123"
	complete := []playlistTrack{
		{Artist: "甲", Album: "完整专辑", Title: "第一首", Duration: "200"},
		{Artist: "乙", Album: "完整专辑", Title: "第二首", Duration: "180"},
	}
	if err := application.writeSavedPlaylist(slug, "完整歌单", complete); err != nil {
		t.Fatal(err)
	}
	path := application.savedPlaylistsDir() + string(os.PathSeparator) + slug + ".m3u"
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "qqmusic", "playlist_id": "123", "base_url": server.URL,
		"save": true, "subscribe": true, "enqueue": true,
	})
	if fetched["truncated"] != true || fetched["track_count"] != float64(1) || fetched["job_id"] != "" {
		t.Fatal(fetched)
	}
	note, _ := fetched["note"].(string)
	if !contains(note, "可能不完整") {
		t.Fatal(fetched)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("incomplete fetch overwrote saved playlist:\n%s", after)
	}
	subscriptions, err := application.loadSubscriptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 0 {
		t.Fatalf("incomplete fetch created a subscription: %+v", subscriptions)
	}
}

func TestIncompleteSubscriptionRefreshPreservesTheCanonicalFileAndSeen(t *testing.T) {
	server := incompleteQQPlaylistServer(t)
	defer server.Close()

	application := newApp(t)
	application.playlistBases = map[string]string{"qqmusic": server.URL}
	slug := "qqmusic-123"
	complete := []playlistTrack{
		{Artist: "甲", Album: "完整专辑", Title: "第一首", Duration: "200"},
		{Artist: "乙", Album: "完整专辑", Title: "第二首", Duration: "180"},
	}
	if err := application.writeSavedPlaylist(slug, "完整歌单", complete); err != nil {
		t.Fatal(err)
	}
	path := application.savedPlaylistsDir() + string(os.PathSeparator) + slug + ".m3u"
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lastChecked := "2026-01-01T00:00:00Z"
	if err := application.storeSubscriptions([]playlistSubscription{{
		PlaylistID: slug, Name: "完整歌单", IntervalHours: 6, Platform: "qqmusic", RemoteID: "123",
		Seen: []string{"existing-key"}, LastChecked: lastChecked,
	}}); err != nil {
		t.Fatal(err)
	}
	checked := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": slug})
	if checked["preserved"] != true || checked["truncated"] != true || checked["queued"] != float64(0) || checked["remote_track_count"] != float64(1) {
		t.Fatal(checked)
	}
	message, _ := checked["message"].(string)
	if !contains(message, "已保留本地完整名单") {
		t.Fatal(checked)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("truncated refresh overwrote saved playlist:\n%s", after)
	}
	rows, err := application.loadSubscriptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Seen) != 1 || rows[0].Seen[0] != "existing-key" || rows[0].LastChecked != lastChecked {
		t.Fatalf("subscription state changed after truncated refresh: %+v", rows)
	}
}

func incompleteQQPlaylistServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/qzone/fcg-bin/fcg_ucc_getcdinfo_byids_cp.fcg" || r.URL.Query().Get("disstid") != "123" {
			http.NotFound(w, r)
			return
		}
		songList := `[{"name":"第一首","singer":[{"name":"甲"}],"albumname":"完整专辑","interval":200}]`
		if r.URL.Query().Get("song_begin") != "0" {
			songList = `[]`
		}
		_, _ = w.Write([]byte(`{"code":0,"cdlist":[{"disstid":123,"dissname":"完整歌单","songnum":2,"songlist":` + songList + `}]}`))
	}))
}
