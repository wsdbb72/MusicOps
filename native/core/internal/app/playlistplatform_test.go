package app

import (
	"net/http"
	"net/http/httptest"
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
