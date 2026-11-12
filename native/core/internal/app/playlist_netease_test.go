package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
)

// The detail document carries every track id but only a handful of full track
// objects. The rest has to come from /api/v3/song/detail, and a playlist of a
// few thousand tracks must not stop at an arbitrary batch count.
func TestNeteasePlaylistReadsEveryTrackID(t *testing.T) {
	const total = 250
	var detailCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/playlist/detail", func(w http.ResponseWriter, r *http.Request) {
		ids := make([]map[string]any, 0, total)
		for i := 1; i <= total; i++ {
			ids = append(ids, map[string]any{"id": i, "v": 0})
		}
		payload := map[string]any{"playlist": map[string]any{
			"id": total, "name": "大歌单", "trackCount": total,
			"tracks": []map[string]any{
				{"id": 1, "name": "第一首", "ar": []map[string]any{{"name": "甲"}}, "al": map[string]any{"name": "专"}, "dt": 200000},
				{"id": 2, "name": "第二首", "ar": []map[string]any{{"name": "乙"}}, "al": map[string]any{"name": "专"}, "dt": 200000},
			},
			"trackIds": ids,
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("/api/v3/song/detail", func(w http.ResponseWriter, r *http.Request) {
		detailCalls.Add(1)
		var query []struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal([]byte(r.URL.Query().Get("c")), &query); err != nil {
			http.Error(w, "bad c", http.StatusBadRequest)
			return
		}
		songs := make([]map[string]any, 0, len(query))
		for _, item := range query {
			songs = append(songs, map[string]any{
				"id": item.ID, "name": fmt.Sprintf("曲目 %d", item.ID),
				"ar": []map[string]any{{"name": "甲"}}, "al": map[string]any{"name": "专"}, "dt": 180000,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "songs": songs})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	application := newApp(t)
	remote, err := application.fetchRemotePlaylist(context.Background(), "netease", strconv.Itoa(total), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(remote.Tracks) != total || remote.Truncated {
		t.Fatalf("tracks=%d truncated=%v", len(remote.Tracks), remote.Truncated)
	}
	if detailCalls.Load() != 3 {
		t.Fatalf("song/detail calls = %d, want 3 (100+100+48)", detailCalls.Load())
	}
	if remote.Tracks[0].Title != "第一首" || remote.Tracks[total-1].Title != fmt.Sprintf("曲目 %d", total) {
		t.Fatalf("first=%q last=%q", remote.Tracks[0].Title, remote.Tracks[total-1].Title)
	}
}

// The share link the user pastes carries extra query parameters (uct2 and
// friends). The id is still the only thing that matters, and the canonical URL
// drops the rest.
func TestPlaylistLocatorAcceptsThePastedNeteaseLink(t *testing.T) {
	const link = "https://music.163.com/playlist?id=590536461&uct2=U2FsdGVkX18GcLRlKGSRtZUe07TLXO4UmKFdF/jAbr8="
	found, err := parsePlaylistLocator(link, "auto")
	if err != nil || len(found) != 1 {
		t.Fatalf("%+v %v", found, err)
	}
	if found[0].Platform != "netease" || found[0].PlaylistID != "590536461" {
		t.Fatal(found[0])
	}
	if found[0].URL != "https://music.163.com/playlist?id=590536461" || found[0].Short {
		t.Fatal(found[0])
	}
}

// Reading the real document over the network is what the endpoint has to
// accept; the request shape is asserted against a local double instead so the
// test suite stays offline.
func TestNeteaseRequestShape(t *testing.T) {
	spec := remotePlatforms["netease"]
	request, err := playlistPageRequest(context.Background(), spec, "https://music.163.com", "590536461", 0)
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.Path != "/api/v3/playlist/detail" {
		t.Fatal(request.URL.Path)
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || values.Get("id") != "590536461" || values.Get("n") != "10000" {
		t.Fatal(request.URL.RawQuery)
	}
}
