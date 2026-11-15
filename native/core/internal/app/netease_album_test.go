package app

import (
	"encoding/json"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNeteaseAlbumRejectsMissingTracksOrWrongEdition(t *testing.T) {
	rip := []byte(`{"code":200,"album":{"id":81,"name":"Album","size":2},"songs":[{"id":10,"name":"One","ar":[{"name":"Artist"}],"dt":3000},{"id":11,"name":"Two","artists":[{"name":"Artist"}],"duration":4000}]}`)
	song := &neteaseSong{ID: 10, Name: "One", Album: "Album", AlbumID: 81}
	tracks, err := decodeNeteaseAlbum(rip, song)
	if err != nil || len(tracks) != 2 || tracks[1].ID != 11 || tracks[1].DurationMS != 4000 {
		t.Fatal(tracks, err)
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
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/api/search/get/web", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":200,"result":{"songs":[{"id":10,"name":"One","artists":[{"name":"Artist"}],"album":{"id":80,"name":"Album","size":2},"duration":3000}]}}`)
	})
	mux.HandleFunc("/api/v1/album/80", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":200,"album":{"id":80,"name":"Album","size":2},"songs":[{"id":10,"name":"One","ar":[{"name":"Artist"}],"dt":3000},{"id":11,"name":"Two","ar":[{"name":"Artist"}],"dt":3000}]}`)
	})
	mux.HandleFunc("/api/song/enhance/player/url/v1", func(w http.ResponseWriter, r *http.Request) {
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
	plan := SearchPlan{RootJobID: "net-album-fixture", Scene: "expand", Query: "Artist Album", ExpectedTitle: "Album", ExpectedArtist: "Artist", PlaylistTrack: "One", StreamingEnabled: true,
		EnabledSites: []string{"dicmusic"}, Synthetic: true, BaseURL: server.URL, Identity: Identity{ID: "net-album-ident", Kind: "api", Source: "playlist", IdentityConfirmed: true, TracklistVerified: true, Payload: "fixture"}}
	raw, _ := json.Marshal(plan)
	_, err = a.flows.Start("start-net-album-fixture", workflow.Start{JobID: plan.RootJobID, BatchID: "pl-netease-80-fixture", Scene: "expand", Stage: "identifying", Label: "Album", Payload: raw, NowNS: a.now().UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	waitRootPublished(t, a, plan.RootJobID)
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
	if count != 2 || hits.Load() != 1 {
		t.Fatal(files, hits.Load())
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatal(err)
	}
}

func TestNeteasePlaylistPreservesRecordingAndAlbumIDs(t *testing.T) {
	raw := []any{map[string]any{"id": float64(10000000001), "name": "Song", "al": map[string]any{"id": float64(10000000002), "name": "Album"}}}
	input := neteaseArtists(raw)
	got, _, err := parseM3U(encodeM3U("Playlist", input))
	if err != nil || len(got) != 1 || got[0].NeteaseSongID != 10000000001 || got[0].NeteaseAlbumID != 10000000002 {
		t.Fatal(got, err)
	}
}
