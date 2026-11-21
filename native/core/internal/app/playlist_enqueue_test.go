package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/wavflac"
)

func TestTrackKeyOfPath(t *testing.T) {
	artist, album, title := trackKeyOfPath(filepath.Join("music", "周杰伦", "范特西", "03 简单爱.flac"))
	if artist != "周杰伦" || album != "范特西" || title != "简单爱" {
		t.Fatalf("%q %q %q", artist, album, title)
	}
	_, _, title = trackKeyOfPath(filepath.Join("music", "A", "B", "01 - Song Name.flac"))
	if title != "song name" {
		t.Fatalf("%q", title)
	}
	artist, album, title = trackPathKeyOfPath("Artist: One/Album: Two/01 Song._.flac")
	if artist != "artist one" || album != "album two" || title != "song" {
		t.Fatalf("forward-slash path key = %q %q %q", artist, album, title)
	}
}

func TestPlaylistPresenceRequiresMatchingUnchangedAudio(t *testing.T) {
	application := newApp(t)
	incoming, library := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(incoming, "track.wav"), pcmWAV(100), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "playlist-presence", RootID: "playlist-album", Artist: "Artist A", Album: "Album X",
		Title: "Song One", Track: 1, SourceName: "track.wav", Incoming: incoming, Library: library,
	})
	if err != nil {
		t.Fatal(err)
	}
	index := application.playlistLibrary()
	if !index.hasTrack("Artist A", "Album X", "Song One") || index.hasTrack("Artist A", "Album Y", "Song One") || index.hasTrack("Artist A", "", "Song One") {
		t.Fatal("presence must match the album")
	}
	if _, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "playlist-single-presence", RootID: "playlist-single", Artist: "Artist A", Album: "Song One",
		Title: "Song One", Track: 1, SourceName: "track.wav", Incoming: incoming, Library: library,
	}); err != nil {
		t.Fatal(err)
	}
	application.invalidatePlaylistLibraryCache()
	report, err := application.enqueuePlaylistTracks(context.Background(), "pl-netease-single", []playlistTrack{{Artist: "Artist A", Title: "Song One", NeteaseSongID: 12}}, []int{0}, playlistEnqueueOptions{AlbumCompletion: true})
	if err != nil || len(report.Skipped) != 1 || len(report.Created) != 0 {
		t.Fatalf("a published single without source album metadata was requeued: %+v, %v", report, err)
	}
	tagged := filepath.Join(incoming, "hot.flac")
	if _, err := wavflac.ConvertFile(filepath.Join(incoming, "track.wav"), tagged, true); err != nil {
		t.Fatal(err)
	}
	if err := flactags.Write(tagged, flactags.Tags{Artists: []string{"Original Artist", "Guest / MC"}, Album: hotSlowCompilationAlbum, Title: "Hot Song"}); err != nil {
		t.Fatal(err)
	}
	if _, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "playlist-hot-presence", RootID: "playlist-hot-album", Artist: "Original Artist", FolderArtist: hotSlowCompilationArtist,
		Album: hotSlowCompilationAlbum, Title: "Hot Song", Track: 1, SourceName: "hot.flac", Incoming: filepath.Dir(tagged), Library: library,
	}); err != nil {
		t.Fatal(err)
	}
	application.invalidatePlaylistLibraryCache()
	hotIndex := application.playlistLibrary()
	if !hotIndex.hasTrack("Guest / MC / Original Artist", "最新热歌慢摇114", "Hot Song") ||
		hotIndex.hasTrack("Original Artist", "最新热歌慢摇114", "Hot Song") ||
		hotIndex.hasTrack("Other Artist", "最新热歌慢摇114", "Hot Song") ||
		hotIndex.hasTrack("Original Artist", "最新热歌慢摇114", "Other Song") {
		t.Fatal("unified compilation presence did not match the original row artist and album")
	}
	path := filepath.Join(library, "Artist A", "Album X", "01 Song One.wav")
	if err := os.WriteFile(path, pcmWAV(200), 0o600); err != nil {
		t.Fatal(err)
	}
	if application.playlistLibrary().hasTrack("Artist A", "Album X", "Song One") {
		t.Fatal("a changed file was reported present")
	}
}

func TestLibraryPathKeyKeepsPunctuationOnlyIdentity(t *testing.T) {
	if libraryPathKey("?") == "" || libraryPathKey("?") != libraryPathKey("_") || libraryPathKey("...") != libraryPathKey("_") {
		t.Fatal("punctuation-only names collapsed to empty identity")
	}
	if libraryPathKey("Album: X") != libraryPathKey("Album_ X") || libraryPathKey("Album: X") == libraryPathKey("Album Y") {
		t.Fatal("filesystem-safe punctuation folding lost album identity")
	}
}

func TestPlaylistPresenceFoldsFilesystemSanitizedPunctuation(t *testing.T) {
	application := newApp(t)
	incoming, library := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(incoming, "track.wav"), pcmWAV(120), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "punctuation-presence", RootID: "punctuation-album", Artist: "Artist_ One", Album: "Album_ Part_", Title: "Song", Track: 1,
		SourceName: "track.wav", Incoming: incoming, Library: library,
	}); err != nil {
		t.Fatal(err)
	}
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	if !index.hasTrack("Artist: One", "Album: Part?", "Song.") {
		t.Fatal("filesystem-sanitized source identity was reported missing")
	}
	if index.hasTrack("Artist One", "Different Album", "Song") || index.hasTrack("Different Artist", "Album Part", "Song") {
		t.Fatal("punctuation folding collapsed a different artist or album")
	}
}

func TestEnqueueSkipsExactDuplicateRows(t *testing.T) {
	application := newApp(t)
	row := playlistTrack{Artist: "Artist", Album: "Album", Title: "Song", NeteaseSongID: 7, NeteaseAlbumID: 9}
	report, err := application.enqueuePlaylistTracks(context.Background(), "pl-duplicate", []playlistTrack{row, row}, []int{0, 1}, playlistEnqueueOptions{AlbumCompletion: false})
	if err != nil || len(report.Created) != 1 || len(report.Skipped) != 1 || report.Tracks != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestPlaylistSelection(t *testing.T) {
	if got := playlistSelection(3, nil); len(got) != 3 {
		t.Fatal(got)
	}
	got := playlistSelection(5, []int{3, 1, 3, 9, -1})
	if len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatal(got)
	}
}

// Rows from one album become one job: the pipeline works per album, and a
// partially present album is completed rather than patched track by track.
func TestEnqueueGroupsRowsByAlbum(t *testing.T) {
	application := newApp(t)
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No catalogue answer: identity stays unconfirmed and the job parks at
		// the metadata checkpoint, which is the manual fallback.
		http.NotFound(w, r)
	}))
	defer catalog.Close()
	rows := []playlistTrack{
		{Artist: "Artist A", Title: "Song One", Album: "Album X"},
		{Artist: "Artist A", Title: "Song Two", Album: "Album X"},
		{Artist: "Artist B", Title: "Song Nine", Album: ""},
	}
	report, err := application.enqueuePlaylistTracks(context.Background(), "pl-test-group", rows, []int{0, 1, 2},
		playlistEnqueueOptions{AlbumCompletion: true, CatalogBaseURL: catalog.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 2 || report.Albums != 1 || report.Tracks != 3 {
		t.Fatalf("%+v", report)
	}
	first, err := application.flows.Job(report.Created[0])
	if err != nil {
		t.Fatal(err)
	}
	if first.Scene != "expand" || first.Stage != "identifying" || first.Label != "Artist A - Album X" {
		t.Fatalf("%+v", first)
	}
	raw, found, dataErr := application.tasks.Data(first.ActiveTaskID, "arguments", 0)
	plan, decodeErr := decodePlan(raw)
	if dataErr != nil || !found || decodeErr != nil || plan.Query != "Artist A Song One" {
		t.Fatalf("query plan=%+v found=%v dataErr=%v decodeErr=%v", plan, found, dataErr, decodeErr)
	}
	second, err := application.flows.Job(report.Created[1])
	if err != nil {
		t.Fatal(err)
	}
	if second.Label != "Artist B - Song Nine" {
		t.Fatalf("%+v", second)
	}
}

func TestHotSlowCompilationAlbumsFoldToSingleTitleAndArtist(t *testing.T) {
	for _, input := range []string{"最新热歌慢摇19", "最新热歌慢摇 101", "最新热歌慢摇１１４", "最新热歌慢摇"} {
		plan := SearchPlan{ExpectedTitle: input}
		normalizePlaylistAlbumPlan(&plan)
		if plan.ExpectedTitle != hotSlowCompilationAlbum || plan.AlbumArtist != hotSlowCompilationArtist {
			t.Fatalf("input %q normalized to %+v", input, plan)
		}
	}
	if got := normalizePlaylistAlbumTitle("最新热歌慢摇精选"); got != "最新热歌慢摇精选" {
		t.Fatalf("unrelated suffix changed: %q", got)
	}
}

func TestEnqueueNormalizesHotSlowCompilationAlbumPlan(t *testing.T) {
	application := newApp(t)
	rows := []playlistTrack{{Artist: "Artist", Title: "Song", Album: "最新热歌慢摇114"}}
	report, err := application.enqueuePlaylistTracks(context.Background(), "pl-hot-slow", rows, []int{0}, playlistEnqueueOptions{AlbumCompletion: true})
	if err != nil || len(report.Created) != 1 {
		t.Fatal(report, err)
	}
	job, err := application.flows.Job(report.Created[0])
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	raw, found, err := application.tasks.Data(job.ActiveTaskID, "arguments", 0)
	plan, decodeErr := decodePlan(raw)
	if err != nil || !found || decodeErr != nil || plan.ExpectedTitle != hotSlowCompilationAlbum || plan.AlbumArtist != hotSlowCompilationArtist {
		t.Fatalf("plan=%+v found=%v err=%v decode=%v", plan, found, err, decodeErr)
	}
}

func TestHotSlowCompilationNeverExpandsAWholeSourceVolume(t *testing.T) {
	application := newApp(t)
	rows := []playlistTrack{
		{Artist: "Artist One", Title: "Song One", Album: "最新热歌慢摇90", NeteaseSongID: 11, NeteaseAlbumID: 90},
		{Artist: "Artist Two", Title: "Song Two", Album: "最新热歌慢摇91", NeteaseSongID: 22, NeteaseAlbumID: 91},
	}
	report, err := application.enqueuePlaylistTracks(context.Background(), "pl-hot-slow-separate", rows, []int{0, 1}, playlistEnqueueOptions{AlbumCompletion: true})
	if err != nil || len(report.Created) != 2 || report.Albums != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	for _, id := range report.Created {
		job, err := application.flows.Job(id)
		if err != nil || job == nil {
			t.Fatalf("job=%+v err=%v", job, err)
		}
		raw, found, err := application.tasks.Data(job.ActiveTaskID, "arguments", 0)
		plan, decodeErr := decodePlan(raw)
		if err != nil || !found || decodeErr != nil || !plan.NeteaseTrackOnly || plan.ExpectedTitle != hotSlowCompilationAlbum || plan.AlbumArtist != hotSlowCompilationArtist {
			t.Fatalf("plan=%+v found=%v err=%v decode=%v", plan, found, err, decodeErr)
		}
	}
}

func TestDecodePlanBoundsLongEnsembleArtistQueries(t *testing.T) {
	artist := "Lionel Richie / Stevie Wonder / Paul Simon / Kenny Rogers / James Ingram / Tina Turner"
	for i := 0; i < 12; i++ {
		artist += " / Guest Artist " + string(rune('A'+i))
	}
	plan := SearchPlan{
		RootJobID: "trk-long-query", Query: artist + " We Are The World", ExpectedTitle: "We Are The World", ExpectedArtist: artist,
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(decoded.Query)) > 300 || decoded.Query != "Lionel Richie We Are The World" {
		t.Fatalf("query=%q runes=%d", decoded.Query, len([]rune(decoded.Query)))
	}
}
