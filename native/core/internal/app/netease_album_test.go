package app

import (
	"bytes"
	"context"
	"encoding/json"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/cookies"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestNeteaseAlbumRejectsMissingTracksOrWrongEdition(t *testing.T) {
	rip := []byte(`{"code":200,"album":{"id":81,"name":"Album","size":2,"picUrl":"https://p1.music.126.net/cover.jpg"},"songs":[{"id":10,"name":"One","ar":[{"name":"Artist"}],"dt":3000},{"id":11,"name":"Two","artists":[{"name":"Artist"}],"duration":4000}]}`)
	song := &neteaseSong{ID: 10, Name: "One", Album: "Album", AlbumID: 81}
	tracks, err := decodeNeteaseAlbum(rip, song)
	if err != nil || len(tracks) != 2 || tracks[1].ID != 11 || tracks[1].DurationMS != 4000 || tracks[0].CoverURL != "https://p1.music.126.net/cover.jpg" {
		t.Fatal(tracks, err)
	}
	legacy := &neteaseSong{ID: 10, Album: "Album", AlbumID: 81}
	if _, err = decodeNeteaseAlbum(rip, legacy); err != nil || legacy.Name != "One" || len(legacy.Artists) != 1 || legacy.Artists[0] != "Artist" {
		t.Fatal("legacy NetEase row was not recovered by song id", legacy, err)
	}
	song.Name = "Other recording"
	if _, err = decodeNeteaseAlbum(rip, song); err == nil {
		t.Fatal("album did not contain the selected recording")
	}
	song.Name = "One"
	song.AlbumID = 82
	if _, err = decodeNeteaseAlbum(rip, song); err == nil {
		t.Fatal("wrong edition accepted")
	}
	song.AlbumID = 81
	if _, err = decodeNeteaseAlbum([]byte(`{"code":200,"album":{"id":81,"name":"Album","size":2},"songs":[{"id":10,"name":"One","ar":[{"name":"Artist"}],"dt":3000}]}`), song); err == nil {
		t.Fatal("truncated tracklist accepted")
	}
}

func TestNeteaseAlbumCompletesOnlyMissingTracks(t *testing.T) {
	tool, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	vaultKey(t)
	a := newApp(t)
	saveNeteaseTestCookie(t, a)
	library := t.TempDir()
	post(t, a, "/v1/settings/library", map[string]any{"library_dir": library})
	folder := filepath.Join(library, "Artist", "Album")
	if err = os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(folder, "existing name.flac")
	if out, err := exec.Command(tool, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-metadata", "artist=Artist", "-metadata", "album=Album", "-metadata", "title=One", existing).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	audio, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	post(t, a, "/v1/library/reindex", map[string]any{})
	hits := atomic.Int32{}
	trackerHits := atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ajax.php", func(w http.ResponseWriter, r *http.Request) { trackerHits.Add(1); http.NotFound(w, r) })
	var server *httptest.Server
	mux.HandleFunc("/api/search/get/web", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":200,"result":{"songs":[{"id":10,"name":"One","artists":[{"name":"Artist"}],"album":{"id":80,"name":"Album","size":3},"duration":3000}]}}`)
	})
	mux.HandleFunc("/api/v1/album/80", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":200,"album":{"id":80,"name":"Album","size":3},"songs":[{"id":10,"name":"One","ar":[{"name":"Artist"}],"dt":3000},{"id":11,"name":"Two","ar":[{"name":"Artist"}],"dt":3000},{"id":12,"name":"Three","ar":[{"name":"Artist"}],"dt":3000}]}`)
	})
	mux.HandleFunc("/api/song/enhance/player/url/v1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ids") == "[12]" {
			io.WriteString(w, "{}")
			return
		}
		if r.URL.Query().Get("ids") != "[11]" {
			t.Errorf("requested unexpected song: %s", r.URL.Query().Get("ids"))
		}
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []map[string]any{{"id": 11, "url": server.URL + "/audio.flac", "type": "flac", "code": 200, "level": "lossless"}}})
	})
	mux.HandleFunc("/audio.flac", func(w http.ResponseWriter, r *http.Request) { w.Write(audio) })
	server = httptest.NewServer(mux)
	defer server.Close()
	a.neteaseBases = map[string]string{"api": server.URL}
	plan := SearchPlan{RootJobID: "net-album-fixture", Scene: "expand", Query: "Artist Album", ExpectedTitle: "Album", ExpectedArtist: "Artist", PlaylistTrack: "One", StreamingEnabled: true, NeteaseOnly: true,
		EnabledSites: []string{"dicmusic"}, Synthetic: true, BaseURL: server.URL, Identity: Identity{ID: "net-album-ident", Kind: "api", Source: "playlist", IdentityConfirmed: true, TracklistVerified: true, Payload: "fixture"}}
	raw, _ := json.Marshal(plan)
	_, err = a.flows.Start("start-net-album-fixture", workflow.Start{JobID: plan.RootJobID, BatchID: "pl-netease-80-fixture", Scene: "expand", Stage: "identifying", Label: "Album", Payload: raw, NowNS: a.now().UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	waitRootPublished(t, a, plan.RootJobID)
	root := inspect(t, a, plan.RootJobID)
	if !strings.Contains(root.Job.Message, "仍缺 1 首") || !strings.Contains(root.Job.Message, "Three") {
		t.Fatal(root.Job.Message)
	}

	files, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, f := range files {
		if strings.EqualFold(filepath.Ext(f.Name()), ".flac") {
			count++
		}
	}
	if count != 2 || hits.Load() != 1 || trackerHits.Load() != 0 {
		t.Fatal(files, hits.Load(), root.Job.Message)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatal(err)
	}
}

func TestHotSlowFallbackPublishesOneAlbumWithTrackIdentityAndCover(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	vaultKey(t)
	a := newApp(t)
	saveNeteaseTestCookie(t, a)
	library := t.TempDir()
	post(t, a, "/v1/settings/library", map[string]any{"library_dir": library})
	audio := filepath.Join(t.TempDir(), "one.flac")
	albumName := "最新热歌慢摇90"
	if out, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-metadata", "artist=Artist", "-metadata", "album="+albumName, "-metadata", "title=One", audio).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	media, err := os.ReadFile(audio)
	if err != nil {
		t.Fatal(err)
	}
	cover := solidJPEG(color.RGBA{R: 20, G: 80, B: 160, A: 255})
	mux := http.NewServeMux()
	mux.HandleFunc("/ajax.php", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/api/search/get/web", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []map[string]any{{"id": 10, "name": "One", "artists": []map[string]any{{"name": "Artist"}}, "album": map[string]any{"id": 80, "name": albumName, "size": 2}, "duration": 3000}}}})
	})
	var server *httptest.Server
	mux.HandleFunc("/api/v1/album/80", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "album": map[string]any{"id": 80, "name": albumName, "size": 2, "picUrl": server.URL + "/cover.jpg"}, "songs": []map[string]any{{"id": 10, "name": "One", "ar": []map[string]any{{"name": "Artist"}}, "dt": 3000}}})
	})
	mux.HandleFunc("/api/song/enhance/player/url/v1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []map[string]any{{"id": 10, "url": server.URL + "/one.flac", "type": "flac", "code": 200, "level": "lossless"}}})
	})
	mux.HandleFunc("/one.flac", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(media) })
	mux.HandleFunc("/cover.jpg", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(cover) })
	server = httptest.NewServer(mux)
	defer server.Close()
	a.neteaseBases = map[string]string{"api": server.URL}
	plan := SearchPlan{
		RootJobID: "net-album-direct-fallback", Scene: "expand", Query: "Artist One", ExpectedTitle: albumName, ExpectedArtist: "Artist", AlbumArtist: hotSlowCompilationArtist,
		PlaylistTrack: "One", NeteaseSongID: 10, NeteaseAlbumID: 80, NeteaseTrackOnly: true,
		StreamingEnabled: true, NeteaseOnly: true, EnabledSites: []string{"dicmusic"}, Synthetic: true, BaseURL: server.URL,
		Identity: Identity{ID: "net-direct-ident", Kind: "api", Source: "playlist", IdentityConfirmed: true, TracklistVerified: true, Payload: "fixture"},
	}
	raw, _ := json.Marshal(plan)
	if _, err := a.flows.Start("start-net-direct-fallback", workflow.Start{JobID: plan.RootJobID, BatchID: "pl-netease-direct-fixture", Scene: "expand", Stage: "identifying", Label: "Album", Payload: raw, NowNS: a.now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	waitRootPublished(t, a, plan.RootJobID)
	files, err := os.ReadDir(filepath.Join(library, hotSlowCompilationArtist, hotSlowCompilationAlbum))
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, file := range files {
		if strings.EqualFold(filepath.Ext(file.Name()), ".flac") && strings.Contains(file.Name(), "One") {
			found = filepath.Join(library, hotSlowCompilationArtist, hotSlowCompilationAlbum, file.Name())
		}
	}
	if found == "" {
		t.Fatal("direct NetEase fallback did not publish the original track", files)
	}
	body, err := os.ReadFile(found)
	tags, tagErr := flactags.ReadBytes(body)
	if err != nil || tagErr != nil || tags.Album != hotSlowCompilationAlbum || tags.AlbumArtist != hotSlowCompilationArtist || tags.Artist != "Artist" {
		t.Fatalf("body=%d tags=%+v err=%v tagErr=%v", len(body), tags, err, tagErr)
	}
	if got := flactags.FrontCover(body); !bytes.Equal(got, cover) {
		t.Fatal("the selected song's per-track cover was not preserved")
	}
}
func TestNeteaseCoverBytesUsesSelectedAlbumArtwork(t *testing.T) {
	a := newApp(t)
	cover := solidJPEG(color.RGBA{R: 12, G: 34, B: 56, A: 255})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/album/80":
			_ = json.NewEncoder(w).Encode(map[string]any{"album": map[string]any{"id": 80, "picUrl": "http://" + r.Host + "/cover.jpg"}})
		case "/cover.jpg":
			_, _ = w.Write(cover)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a.neteaseBases = map[string]string{"api": server.URL}
	got, err := a.neteaseCoverBytes(context.Background(), &neteaseSong{AlbumID: 80})
	if err != nil || !bytes.Equal(got, cover) {
		t.Fatalf("cover bytes=%d err=%v", len(got), err)
	}
}

func TestHotSlowPrefersOriginalSongCover(t *testing.T) {
	a := newApp(t)
	cover := solidJPEG(color.RGBA{R: 70, G: 20, B: 90, A: 255})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search/get/web":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []map[string]any{
				{"id": 2, "name": "Song", "artists": []map[string]any{{"name": "Artist"}}, "album": map[string]any{"id": 90, "name": "最新热歌慢摇90"}, "duration": 180000},
				{"id": 1, "name": "Song", "artists": []map[string]any{{"name": "Composer"}, {"name": "Artist"}}, "album": map[string]any{"id": 80, "name": "Original Release"}, "duration": 180500},
			}}})
		case "/api/v1/album/80":
			_ = json.NewEncoder(w).Encode(map[string]any{"album": map[string]any{"id": 80, "name": "Original Release", "picUrl": "http://" + r.Host + "/cover.jpg"}})
		case "/cover.jpg":
			_, _ = w.Write(cover)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a.neteaseBases = map[string]string{"api": server.URL}
	got, err := a.neteasePreferredCoverBytes(context.Background(), &neteaseSong{
		ID: 2, Name: "Song", Artists: []string{"Artist"}, Album: "最新热歌慢摇90", AlbumID: 90, DurationMS: 180000,
	})
	if err != nil || !bytes.Equal(got, cover) {
		t.Fatalf("cover bytes=%d err=%v", len(got), err)
	}
}

func TestNeteasePlaylistRecoversIDsFromOriginalM3UOnRetry(t *testing.T) {
	a := newApp(t)
	batchID := "pl-netease-recover-fixture"
	rows := []playlistTrack{
		{Title: "One", Artist: "Artist / Guest", Album: "Album", Duration: "180000", NeteaseSongID: 10000000001, NeteaseAlbumID: 10000000002},
	}
	dir := filepath.Join(a.Dir, "incoming")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, playlistFileName(batchID)), encodeM3U("Recover", rows), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := SearchPlan{
		RootJobID: "trk-net-recover", ExpectedTitle: "Album", ExpectedArtist: "Artist / Guest",
		Identity: Identity{Payload: `{"title":"One"}`},
	}
	if !a.recoverNeteasePlaylistIDs(context.Background(), batchID, &plan) {
		t.Fatal("the original playlist row was not recovered")
	}
	if plan.PlaylistTrack != "One" || plan.NeteaseSongID != 10000000001 || plan.NeteaseAlbumID != 10000000002 {
		t.Fatalf("recovered plan=%+v", plan)
	}
	plan.ExpectedArtist = "Different Artist"
	plan.NeteaseSongID, plan.NeteaseAlbumID = 0, 0
	if a.recoverNeteasePlaylistIDs(context.Background(), batchID, &plan) {
		t.Fatal("a different artist must not reuse the selected song id")
	}
}

func TestNeteasePlaylistRefreshesLegacyM3UOnce(t *testing.T) {
	vaultKey(t)
	a := newApp(t)
	saveNeteaseTestCookie(t, a)
	stale, err := cookies.ParseEditor(`[{"name":"MUSIC_U","value":"stale-session","domain":"music.163.com","path":"/","secure":true,"httpOnly":true,"sameSite":"unspecified","hostOnly":true}]`, neteaseHost)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := cookies.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.vault.Set("vault://playlist/netease-stale", encoded, neteaseHost); err != nil {
		t.Fatal(err)
	}
	if err := a.casSaveJSON([]byte("app/settings/playlist-sources"), []byte(`{"netease":{"credential_ref":"vault://playlist/netease-stale"}}`)); err != nil {
		t.Fatal(err)
	}
	batchID := "pl-netease-590536461-refresh"
	dir := filepath.Join(a.Dir, "incoming")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	local := []playlistTrack{{Title: "One", Artist: "Artist", Album: "Album", Duration: "180000"}}
	if err := os.WriteFile(filepath.Join(dir, playlistFileName(batchID)), encodeM3U("Recover", local), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != a.neteaseCookie() {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/api/v3/playlist/detail" || r.URL.Query().Get("id") != "590536461" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":200,"playlist":{"id":"590536461","name":"Recover","trackCount":1,"tracks":[{"id":"10000000001","name":"One","ar":[{"name":"Artist"}],"al":{"id":"10000000002","name":"Album"},"dt":180000}]}}`)
	}))
	defer server.Close()
	a.playlistBases = map[string]string{"netease": server.URL}
	plan := SearchPlan{RootJobID: "trk-refresh", ExpectedTitle: "Album", ExpectedArtist: "Artist", Identity: Identity{Payload: `{"title":"One"}`}}
	if !a.recoverNeteasePlaylistIDs(context.Background(), batchID, &plan) || plan.NeteaseSongID != 10000000001 || plan.NeteaseAlbumID != 10000000002 {
		t.Fatalf("plan=%+v", plan)
	}
	rows, _, err := a.playlistTrackRows(batchID)
	if err != nil || len(rows) != 1 || rows[0].NeteaseSongID != 10000000001 || rows[0].NeteaseAlbumID != 10000000002 {
		t.Fatalf("merged rows=%+v err=%v", rows, err)
	}
}

func TestNeteasePlaylistMergesIDsIntoLegacyM3U(t *testing.T) {
	local := []playlistTrack{{Title: "One", Artist: "Artist", Album: "Album"}}
	remote := []playlistTrack{{Title: "One", Artist: "Artist", Album: "Album", NeteaseSongID: 10000000001, NeteaseAlbumID: 10000000002}}
	if updated := mergeNeteasePlaylistIDs(local, remote); updated != 1 || local[0].NeteaseSongID != 10000000001 || local[0].NeteaseAlbumID != 10000000002 {
		t.Fatalf("updated=%d local=%+v", updated, local)
	}
}

func TestNeteasePlaylistPreservesRecordingAndAlbumIDs(t *testing.T) {
	raw := []any{map[string]any{"id": float64(10000000001), "name": "Song", "al": map[string]any{"id": float64(10000000002), "name": "Album"}}}
	input := neteaseArtists(raw)
	got, _, err := parseM3U(encodeM3U("Playlist", input))
	if err != nil || len(got) != 1 || got[0].NeteaseSongID != 10000000001 || got[0].NeteaseAlbumID != 10000000002 {
		t.Fatal(got, err)
	}
	evidence := playlistRowEvidence("new-job", "pl-netease-fixture", got[0])
	var payload struct {
		SongID  int64 `json:"netease_song_id"`
		AlbumID int64 `json:"netease_album_id"`
	}
	if json.Unmarshal(evidence.Payload, &payload) != nil || payload.SongID != 10000000001 || payload.AlbumID != 10000000002 {
		t.Fatalf("evidence payload=%s", evidence.Payload)
	}
}

func TestIdentifyRecoversLegacyNeteaseIdentityWithoutManualReview(t *testing.T) {
	a := newApp(t)
	batchID := "pl-netease-identity-fixture"
	rows := []playlistTrack{{Title: "One", Artist: "Artist", Album: "Album", Duration: "180000", NeteaseSongID: 10000000001, NeteaseAlbumID: 10000000002}}
	dir := filepath.Join(a.Dir, "incoming")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, playlistFileName(batchID)), encodeM3U("Identity", rows), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := SearchPlan{
		RootJobID: "trk-netease-identity", Scene: "expand", Query: "Artist Album",
		ExpectedTitle: "Album", ExpectedArtist: "Artist", PlaylistTrack: "One",
		StreamingEnabled: true, EnabledSites: []string{}, NeteaseOnly: true,
		Identity: Identity{ID: "legacy-identity", Kind: "catalog", Source: "playlist", Payload: `{"title":"One"}`},
	}
	raw, _ := json.Marshal(plan)
	if _, err := a.flows.Start("start-"+plan.RootJobID, workflow.Start{
		JobID: plan.RootJobID, BatchID: batchID, Scene: "expand", Stage: "identifying",
		Label: "Artist - Album", Payload: raw, NowNS: a.now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	var job *workflow.Job
	for time.Now().Before(deadline) {
		job, _ = a.flows.Job(plan.RootJobID)
		if job != nil && job.Stage != "identifying" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job == nil || job.Stage == "identifying" {
		t.Fatalf("identity job did not advance: %+v", job)
	}
	evidence, err := a.flows.EvidenceFor(plan.RootJobID, nil, 20)
	confirmed := false
	if err == nil {
		for _, item := range evidence.Evidence {
			if item.Source == "netease" && item.IdentityConfirmed {
				confirmed = true
				break
			}
		}
	}
	if !confirmed {
		t.Fatalf("recovered NetEase identity evidence was not accepted: %+v err=%v", evidence, err)
	}
	if job.Stage == "needs_review" && job.ErrorClass == "IDENTITY_UNCONFIRMED" {
		t.Fatalf("recovered identity returned to manual review: %+v", job)
	}
}
