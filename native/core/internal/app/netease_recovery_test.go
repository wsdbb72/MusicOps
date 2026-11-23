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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

func TestNeteasePlaylistUsesSafeAlternateRecordingWhenExactURLMissing(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no exact engine result", http.StatusNotFound)
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)

	anchorCover := solidJPEG(color.RGBA{R: 18, G: 52, B: 94, A: 255})
	alternateCover := solidJPEG(color.RGBA{R: 120, G: 40, B: 30, A: 255})
	audio := syntheticFLAC(t, false)
	audio = append(audio, bytes.Repeat([]byte{0xaa}, 4096)...)

	requested := []string{}
	var netease *httptest.Server
	netease = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search/get":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"msg":"操作频繁，请稍候再试","code":405}`))
		case "/api/search/get/web":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []map[string]any{
				{"id": 100, "name": "Song", "duration": 200000, "album": map[string]any{"id": 900, "name": "Album", "picUrl": netease.URL + "/anchor.jpg"}, "artists": []map[string]any{{"name": "Artist"}, {"name": "Featured Guest"}}},
				{"id": 101, "name": "Song", "duration": 200000, "album": map[string]any{"id": 901, "name": "Archive"}, "artists": []map[string]any{{"name": "Different Artist"}}},
				{"id": 102, "name": "Song", "duration": 200500, "album": map[string]any{"id": 902, "name": "Archive", "picUrl": netease.URL + "/cover.jpg"}, "artists": []map[string]any{{"name": "Artist"}, {"name": "Featured Guest"}}},
				{"id": 103, "name": "Song", "duration": 240000, "album": map[string]any{"id": 903, "name": "Archive"}, "artists": []map[string]any{{"name": "Artist"}, {"name": "Featured Guest"}}},
				{"id": 104, "name": "Song Remix", "duration": 200000, "album": map[string]any{"id": 904, "name": "Archive"}, "artists": []map[string]any{{"name": "Artist"}}},
				{"id": 105, "name": "Song", "duration": 200000, "album": map[string]any{"id": 905, "name": "Different Release"}, "artists": []map[string]any{{"name": "Artist"}}},
				{"id": 106, "name": "Song", "duration": 200000, "album": map[string]any{"id": 906, "name": "Remix Release"}, "artists": []map[string]any{{"name": "Artist"}, {"name": "Featured Guest"}, {"name": "Remix Artist"}}},
			}}})
		case "/api/song/enhance/player/url/v1", "/api/song/enhance/player/url":
			ids := r.URL.Query().Get("ids")
			requested = append(requested, ids)
			w.Header().Set("Content-Type", "application/json")
			id, _ := strconv.ParseInt(strings.Trim(ids, "[]"), 10, 64)
			item := map[string]any{"id": id, "url": "", "type": "flac", "code": -110}
			if id == 102 {
				item = map[string]any{"id": id, "url": netease.URL + "/media/102.flac", "type": "flac", "code": 200}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []map[string]any{item}})
		case "/media/102.flac":
			w.Header().Set("Content-Type", "audio/flac")
			_, _ = w.Write(audio)
		case "/cover.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(alternateCover)
		case "/anchor.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(anchorCover)
		default:
			http.NotFound(w, r)
		}
	}))
	application.neteaseBases = map[string]string{"api": netease.URL}
	defer netease.Close()

	plan := SearchPlan{
		RootJobID: "alternate-recovery", Scene: "expand", Query: "Artist Album",
		ExpectedTitle: "Album", ExpectedArtist: "Artist / Featured Guest", PlaylistTrack: "Song",
		StreamingEnabled: true, NeteaseOnly: true, NeteaseTrackOnly: true,
		NeteaseSongID: 100, NeteaseAlbumID: 900,
	}
	path, note, class, handled := application.tryNeteaseStream(context.Background(), plan)
	if !handled || path == "" || class != downloadpolicy.ClassDownload {
		t.Fatalf("handled=%v path=%q class=%q note=%q", handled, path, class, note)
	}
	if !strings.Contains(note, "替代发行版 ID 102") || !strings.Contains(note, "Archive") {
		t.Fatalf("alternate recovery is not visible in the result: %q", note)
	}
	for _, ids := range requested {
		if strings.Contains(ids, "101") || strings.Contains(ids, "103") || strings.Contains(ids, "104") || strings.Contains(ids, "105") || strings.Contains(ids, "106") {
			t.Fatalf("an unsafe alternate was queried: %s", ids)
		}
	}
	tags := readTags(t, path)
	if tags.Title != "Song" || tags.Album != "Album" || strings.Join(tags.Artists, " / ") != "Artist / Featured Guest" {
		t.Fatalf("playlist identity was not restored: title=%q album=%q artists=%v", tags.Title, tags.Album, tags.Artists)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(flactags.FrontCover(body), anchorCover) {
		t.Fatalf("the durable playlist cover did not win: %v", err)
	}
}

func TestNeteaseAlternateArtistsRequirePrimaryAndRejectUnknownCredits(t *testing.T) {
	tests := []struct {
		name string
		want []string
		got  []string
		ok   bool
	}{
		{name: "complete credits", want: []string{"Artist", "Guest"}, got: []string{"Guest", "Artist"}, ok: true},
		{name: "primary only", want: []string{"Artist", "Guest"}, got: []string{"Artist"}, ok: true},
		{name: "unknown extra artist", want: []string{"Artist", "Guest"}, got: []string{"Artist", "Someone Else"}},
		{name: "uncredited remix artist superset", want: []string{"Artist", "Guest"}, got: []string{"Artist", "Guest", "Remix Artist"}},
		{name: "substring tribute artist", want: []string{"Artist"}, got: []string{"Artist Tribute"}},
		{name: "featured artist only", want: []string{"Artist", "Guest"}, got: []string{"Guest"}},
		{name: "empty credits", want: []string{"Artist"}, got: nil},
		{name: "punctuation in stage name", want: []string{"Bill Withers", "Grover Washington Jr."}, got: []string{"Grover Washington, Jr.", "Bill Withers"}, ok: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := neteaseAlternateArtistsMatch(test.want, test.got); got != test.ok {
				t.Fatalf("neteaseAlternateArtistsMatch(%v, %v)=%v want %v", test.want, test.got, got, test.ok)
			}
		})
	}
}

func TestNeteaseAlternateTitleKeepsContextButRejectsVersionDrift(t *testing.T) {
	tests := []struct {
		name string
		want string
		got  string
		ok   bool
	}{
		{name: "exact", want: "Song", got: "Song", ok: true},
		{name: "contextual film credit", want: "星光下的梦想", got: "星光下的梦想(《喜羊羊与灰太狼》动画电影插曲)", ok: true},
		{name: "unexpected remix", want: "Monsters", got: "Monsters (Remix)"},
		{name: "instrumental is a different recording", want: "Shed a Light (Like Instrumental Mix)", got: "Shed a Light"},
		{name: "live version is different", want: "Song", got: "Song (Live)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := neteaseAlternateTitleMatches(test.want, test.got); got != test.ok {
				t.Fatalf("neteaseAlternateTitleMatches(%q, %q)=%v want %v", test.want, test.got, got, test.ok)
			}
		})
	}
}

func TestNeteaseAlternateSongsUsesTitleOnlySearchAndStageNamePunctuation(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	application.Wait = func(_ context.Context, _ time.Duration) error { return nil }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := r.URL.Query().Get("s")
		songs := []map[string]any{}
		if query == "Just the Two of Us" {
			songs = append(songs, map[string]any{
				"id": 26850810, "name": "Just The Two Of Us (with Grover Washington Jr.)", "duration": 237100,
				"album":   map[string]any{"id": 42, "name": "Ultimate Collection"},
				"artists": []map[string]any{{"name": "Bill Withers"}},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": songs}})
	}))
	defer server.Close()
	application.neteaseBases = map[string]string{"api": server.URL}
	anchor := &neteaseSong{
		ID: 1090226, Name: "Just the Two of Us",
		Artists: []string{"Bill Withers", "Grover Washington Jr."}, Album: "Greatest Hits: Lean On Me",
		DurationMS: 237609, AlbumID: 9, CatalogVerified: true,
	}
	candidates, err := application.neteaseAlternateSongs(context.Background(), SearchPlan{
		ExpectedArtist: "Bill Withers / Grover Washington Jr.", ExpectedTitle: "Greatest Hits: Lean On Me", PlaylistTrack: "Just the Two of Us",
	}, anchor)
	if err != nil || len(candidates) != 1 || candidates[0].ID != 26850810 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
}

func TestNeteaseAlternateSubsetRequiresSameAlbumAndNearIdenticalDuration(t *testing.T) {
	if !neteaseAlternateSubsetEvidenceAllowed(true, 1_999, true) {
		t.Fatal("a same-album subset inside the strict duration window must be allowed")
	}
	if neteaseAlternateSubsetEvidenceAllowed(false, 0, true) {
		t.Fatal("a subset from another release must be rejected")
	}
	if neteaseAlternateSubsetEvidenceAllowed(true, 2_001, true) {
		t.Fatal("a subset outside the strict duration window must be rejected")
	}
	if neteaseAlternateSubsetEvidenceAllowed(true, 0, false) {
		t.Fatal("a subset without comparable durations must be rejected")
	}
}

func TestNeteaseSearchPrefersDurablePlaylistIDOverDuplicateLabels(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []map[string]any{
			{"id": 200, "name": "Song", "duration": 200000, "album": map[string]any{"id": 900, "name": "Reissue Album"}, "artists": []map[string]any{{"name": "Artist"}}},
			{"id": 201, "name": "Song", "duration": 200500, "album": map[string]any{"id": 900, "name": "Reissue Album"}, "artists": []map[string]any{{"name": "Artist"}}},
		}}})
	}))
	defer server.Close()
	application.neteaseBases = map[string]string{"api": server.URL}
	song, err := application.neteaseSearchSong(context.Background(), SearchPlan{
		ExpectedTitle: "Playlist Album", ExpectedArtist: "Artist", PlaylistTrack: "Song", NeteaseSongID: 201,
	})
	if err != nil || song == nil || song.ID != 201 {
		t.Fatalf("durable id did not win: song=%+v err=%v", song, err)
	}
}

func TestNeteaseRecoveredTagsKeepFullLongMetadata(t *testing.T) {
	a := newApp(t)
	path := filepath.Join(t.TempDir(), "recovered.flac")
	if err := os.WriteFile(path, syntheticFLAC(t, false), 0o600); err != nil {
		t.Fatal(err)
	}
	longTitle := `The Four Seasons, Violin Concerto in F Minor, Op. 8 No. 4, RV 297 "Winter": I. Allegro non molto`
	longAlbum := "Vivaldi: The Four Seasons (Complete Recording and Deluxe Remastered Anniversary Edition Notes)"
	longArtist := "Anne-Sophie Mutter / Herbert von Karajan / Peter Alward / Wiener Philharmoniker / Special Guest Orchestra"
	if len(longTitle) <= 80 || len(longAlbum) <= 80 || len(longArtist) <= 80 {
		t.Fatal("fixtures must exceed the old filename-length truncation")
	}
	if err := a.rewriteNeteaseRecoveredPlaylistTags(path, SearchPlan{ExpectedTitle: longAlbum, ExpectedArtist: longArtist, AlbumArtist: longArtist, PlaylistTrack: longTitle}); err != nil {
		t.Fatal(err)
	}
	tags, err := flactags.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != longTitle || tags.Album != longAlbum || tags.AlbumArtist != longArtist {
		t.Fatalf("long metadata was truncated: title=%q album=%q album_artist=%q", tags.Title, tags.Album, tags.AlbumArtist)
	}
	if strings.Join(tags.Artists, " / ") != longArtist {
		t.Fatalf("credited artists=%q want=%q", strings.Join(tags.Artists, " / "), longArtist)
	}
}

func TestNeteaseSecondaryStreamRestoresOriginalReleaseIdentity(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveNeteaseTestCookie(t, application)

	cover := solidJPEG(color.RGBA{R: 24, G: 72, B: 128, A: 255})
	audio := oneSecondFLAC(t, "Artist", "Song")
	netease := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/song/enhance/player/url"):
			_, _ = w.Write([]byte(`{"code":200,"data":[{"id":42,"url":"","type":"flac","code":-110}]}`))
		case strings.HasPrefix(r.URL.Path, "/song/media/outer/url"):
			http.NotFound(w, r)
		case r.URL.Path == "/cover.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(cover)
		default:
			http.NotFound(w, r)
		}
	}))
	defer netease.Close()
	application.neteaseBases = map[string]string{"api": netease.URL}

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if r.URL.Query().Get("sources") != "kugou" {
				_, _ = io.WriteString(w, `<html></html>`)
				return
			}
			_, _ = io.WriteString(w, `<html><li class="song-card" data-id="k1" data-source="kugou" data-name="Song" data-artist="Artist" data-album="Other Release" data-duration="1"></li></html>`)
		case "/download":
			if r.URL.Query().Get("source") == "netease" {
				http.NotFound(w, r)
				return
			}
			if r.URL.Query().Get("source") != "kugou" || r.URL.Query().Get("id") != "k1" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", engine.URL)

	plan := SearchPlan{
		RootJobID: "secondary-net", Scene: "expand", ExpectedTitle: "Album", ExpectedArtist: "Artist",
		PlaylistTrack: "Song", StreamingEnabled: true, NeteaseOnly: true, NeteaseTrackOnly: true,
		NeteaseSongID: 42, NeteaseAlbumID: 7,
	}
	anchor := &neteaseSong{ID: 42, Name: "Song", Artists: []string{"Artist"}, Album: "Album", AlbumID: 7, DurationMS: 1000, CoverURL: netease.URL + "/cover.jpg", CatalogVerified: true}
	path, note, class, handled := application.stageNeteaseSongWithEngine(t.Context(), plan, anchor)
	if !handled || path == "" || class != downloadpolicy.ClassDownload || !strings.Contains(note, "kugou") {
		t.Fatalf("handled=%v path=%q class=%q note=%q", handled, path, class, note)
	}
	tags := readTags(t, path)
	if tags.Title != "Song" || tags.Album != "Album" || strings.Join(tags.Artists, " / ") != "Artist" {
		t.Fatalf("original NetEase identity was not restored: title=%q album=%q artists=%v", tags.Title, tags.Album, tags.Artists)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(flactags.FrontCover(body), cover) {
		t.Fatalf("original NetEase cover was not restored: %v", err)
	}
}
