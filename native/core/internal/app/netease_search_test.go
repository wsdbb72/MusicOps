package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
)

func TestNeteaseSearchFallsBackWhenLegacyEndpointRateLimits(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	var legacyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/search/get":
			_, _ = w.Write([]byte(`{"msg":"操作频繁，请稍候再试","code":405}`))
		case "/api/search/get/web":
			legacyCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []map[string]any{
				{"id": 5, "name": "Song", "duration": 200000, "album": map[string]any{"id": 9, "name": "Reissue Album"}, "artists": []map[string]any{{"name": "Artist"}}},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	application.neteaseBases = map[string]string{"api": server.URL}

	song, err := application.neteaseSearchSong(context.Background(), SearchPlan{
		ExpectedTitle: "Album", ExpectedArtist: "Artist", PlaylistTrack: "Song", NeteaseSongID: 5,
	})
	if err != nil || song == nil || song.ID != 5 || legacyCalls.Load() != 1 {
		t.Fatalf("song=%+v err=%v legacyCalls=%d", song, err, legacyCalls.Load())
	}
}

func TestNeteaseSearchExplicitEmptyArrayDoesNotTryFallback(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	var legacyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/search/get" {
			_, _ = w.Write([]byte(`{"code":200,"result":{"songs":[]}}`))
			return
		}
		legacyCalls.Add(1)
		http.Error(w, "fallback must not run", http.StatusInternalServerError)
	}))
	defer server.Close()
	application.neteaseBases = map[string]string{"api": server.URL}

	song, err := application.neteaseSearchSong(context.Background(), SearchPlan{ExpectedTitle: "Song", ExpectedArtist: "Artist"})
	if err != nil || song != nil || legacyCalls.Load() != 0 {
		t.Fatalf("song=%+v err=%v legacyCalls=%d", song, err, legacyCalls.Load())
	}
}

func TestNeteaseBothSearchEndpointsRateLimitedRemainRetryable(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"msg":"操作频繁，请稍候再试","code":405}`))
	}))
	defer server.Close()
	application.neteaseBases = map[string]string{"api": server.URL}

	_, err := application.neteaseSearchSong(context.Background(), SearchPlan{ExpectedTitle: "Song", ExpectedArtist: "Artist"})
	if err == nil || classifyNeteaseFailure(err) != downloadpolicy.ClassRateLimited {
		t.Fatalf("err=%v class=%s", err, classifyNeteaseFailure(err))
	}
}

func TestNeteaseAlternateRateLimitRemainsRetryable(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no engine result", http.StatusNotFound)
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/song/enhance/player/url/v1", "/api/song/enhance/player/url":
			_, _ = w.Write([]byte(`{"code":200,"data":[{"id":42,"url":"","type":"flac","code":-110}]}`))
		case "/api/search/get/web", "/api/search/get":
			_, _ = w.Write([]byte(`{"msg":"操作频繁，请稍候再试","code":405}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	application.neteaseBases = map[string]string{"api": server.URL}

	path, note, class, handled := application.stageNeteaseSongWithEngine(context.Background(), SearchPlan{
		ExpectedTitle: "Album", ExpectedArtist: "Artist", PlaylistTrack: "Song", NeteaseSongID: 42,
	}, &neteaseSong{ID: 42, Name: "Song", Artists: []string{"Artist"}, Album: "Album", DurationMS: 200000, CatalogVerified: true})
	if !handled || path != "" || class != downloadpolicy.ClassRateLimited || !containsAll(note, "替代录音检索失败", "405") {
		t.Fatalf("handled=%v path=%q class=%q note=%q", handled, path, class, note)
	}
}
