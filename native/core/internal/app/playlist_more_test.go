package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMorePlaylistProvidersStayMetadataOnly(t *testing.T) {
	spotifyID := "0123456789012345678901"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/playlist/detail", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"playlist":{"id":1001,"name":"网易","trackCount":1,"tracks":[{"name":"歌","ar":[{"name":"甲"}],"al":{"name":"专"},"dt":200000}]}}`)
	})
	mux.HandleFunc("/api/v3/special/song", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":1,"data":{"specialname":"酷狗","total":1,"info":[{"songname":"歌","singername":"甲","album_name":"专","duration":180}]}}`)
	})
	mux.HandleFunc("/luna/playlist/detail", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, `{"status_code":0,"playlist":{"id":"soda1","title":"汽水","count_tracks":1},"media_resources":[{"entity":{"track_wrapper":{"track":{"id":"t","name":"歌","artists":[{"name":"甲"}],"album":{"name":"专"},"duration":200000}}}}]}`)
	})
	mux.HandleFunc("/MIGUM3.0/resource/playlist/song/v2.0", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"000000","data":{"name":"咪咕","totalCount":1,"songList":[{"contentId":"1","name":"歌","singers":[{"name":"甲"}],"album":"专","duration":200}]}}`)
	})
	mux.HandleFunc("/v1/playlists/"+spotifyID, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer spotifytokenvalue" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+spotifyID+`","name":"Spotify","tracks":{"total":1,"items":[{"track":{"id":"t","name":"歌","artists":[{"name":"甲"}],"album":{"name":"专"},"duration_ms":200000}}]}}`)
	})
	mux.HandleFunc("/v1/catalog/cn/playlists/pl.abc", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"pl.abc","attributes":{"name":"Apple"},"relationships":{"tracks":{"data":[{"attributes":{"name":"歌","artistName":"甲","albumName":"专","durationInMillis":200000}}]}}}]}`)
	})
	mux.HandleFunc("/youtube/v3/playlistItems", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"snippet":{"title":"歌","videoOwnerChannelTitle":"甲"}}]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	vaultKey(t)
	application := newApp(t)
	if _, err := application.vault.Set("vault://playlist/spotify", "spotifytokenvalue", "api.spotify.com"); err != nil {
		t.Fatal(err)
	}
	if err := application.casSaveJSON([]byte("app/settings/playlist-sources"), []byte(`{"spotify":{"credential_ref":"vault://playlist/spotify"}}`)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		platform string
		id       string
		name     string
	}{
		{"netease", "1001", "网易"},
		{"kugou", "2002", "酷狗"},
		{"qishui", "soda1", "汽水"},
		{"migu", "3003", "咪咕"},
		{"spotify", spotifyID, "Spotify"},
		{"applemusic", "pl.abc", "Apple"},
		{"youtube", "PLabcdefghij", "YouTube 歌单"},
	}
	for _, item := range cases {
		fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
			"platform": item.platform, "playlist_id": item.id, "base_url": server.URL,
		})
		if fetched["downloaded"] != false || fetched["imported"] != false || fetched["track_count"] != float64(1) || fetched["name"] != item.name {
			t.Fatal(item.platform, fetched)
		}
	}
	blocked := postCode(t, application, "/v1/playlists/fetch", map[string]any{"platform": "youtube", "playlist_id": "PLabcdefghij"})
	if blocked.Code != http.StatusBadGateway || !strings.Contains(blocked.Body.String(), "API Key") {
		t.Fatal(blocked.Code, blocked.Body.String())
	}
}

func TestNeteaseSubscriptionRefreshStaysMetadataOnly(t *testing.T) {
	var songs atomic.Value
	songs.Store(`{"playlist":{"id":1001,"name":"网易","trackCount":1,"tracks":[{"name":"第一首","ar":[{"name":"甲"}],"al":{"name":"专"},"dt":200000}]}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, songs.Load().(string))
	}))
	defer server.Close()
	application := newApp(t)
	fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "netease", "playlist_id": "1001", "base_url": server.URL, "save": true, "subscribe": true,
	})
	if fetched["downloaded"] != false || fetched["playlist_id"] != "netease-1001" {
		t.Fatal(fetched)
	}
	application.playlistBases = map[string]string{"netease": server.URL}
	first := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "netease-1001"})
	if first["queued"] != float64(0) || first["downloaded"] != false {
		t.Fatal(first)
	}
	songs.Store(`{"playlist":{"id":1001,"name":"网易","trackCount":2,"tracks":[{"name":"第一首","ar":[{"name":"甲"}],"al":{"name":"专"},"dt":200000},{"name":"第二首","ar":[{"name":"乙"}],"al":{"name":"专"},"dt":180000}]}}`)
	second := post(t, application, "/v1/subscriptions/check", map[string]any{"playlist_id": "netease-1001"})
	message, _ := second["message"].(string)
	if second["downloaded"] != false || second["queued"] == float64(0) || !contains(message, "尚未下载") {
		t.Fatal(second)
	}
}
