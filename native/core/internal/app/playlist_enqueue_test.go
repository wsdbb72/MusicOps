package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

	presentAlbum := index.hasTrack("Artist A", "Album X", "Song One")
	presentWrongAlbum := index.hasTrack("Artist A", "Album Y", "Song One")
	presentNoAlbum := index.hasTrack("Artist A", "", "Song One")
	if !presentAlbum || presentWrongAlbum || presentNoAlbum {
		t.Fatalf("presence album=%v wrongAlbum=%v noAlbum=%v", presentAlbum, presentWrongAlbum, presentNoAlbum)
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
	if err := flactags.Write(tagged, flactags.Tags{Artists: []string{"Original Artist;Guest / MC"}, Album: hotSlowCompilationAlbum, Title: "Hot Song"}); err != nil {
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

func indexPlaylistTestFile(t *testing.T, application *App, library, relative, artist, album, title string) string {
	t.Helper()
	path := filepath.Join(library, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := flactags.WriteBytes(syntheticFLAC(t, false), flactags.Tags{
		Artists: splitPlaylistArtists(artist), Album: album, Title: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(relative))
	requestID := "playlist-layout-" + hex.EncodeToString(sum[:8])
	if added, err := audiorelease.IndexExisting(application.engine, audiorelease.Existing{
		RequestID: requestID, RootID: requestID + "-root", Track: 1,
		Path: path, Library: library, Format: "flac", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatalf("index %s: added=%v err=%v", path, added, err)
	}
	return path
}

func TestPlaylistPresenceUsesEmbeddedIdentityForFlatArtistAlbumFolder(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	indexPlaylistTestFile(t, application, library,
		"郑智化 - 岁月风华之恋曲典藏/郑智化 - 水手.flac",
		"郑智化", "岁月风华之恋曲典藏", "水手")
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	if !index.hasTrack("郑智化", "岁月风华之恋曲典藏", "水手") {
		t.Fatal("an embedded artist/title match was reported missing because of the folder layout")
	}
	if index.hasTrack("郑智化", "另一张专辑", "水手") {
		t.Fatal("a different embedded album was accepted")
	}
	application.invalidatePlaylistLibraryCache()
	report, err := application.enqueuePlaylistTracks(context.Background(), "pl-layout-duplicate", []playlistTrack{{
		Artist: "郑智化", Album: "岁月风华之恋曲典藏", Title: "水手", NeteaseSongID: 375,
	}}, []int{0}, playlistEnqueueOptions{AlbumCompletion: true})
	if err != nil || len(report.Created) != 0 || len(report.Skipped) != 1 {
		t.Fatalf("an existing embedded track was re-queued: report=%+v err=%v", report, err)
	}
}

func TestPlaylistPresenceRejectsWrongCreditsAndDamagedCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, fileArtist, tagArtist, tagAlbum string
	}{
		{name: "wrong artist", fileArtist: "郑智化", tagArtist: "Other Artist", tagAlbum: "岁月风华之恋曲典藏"},
		{name: "wrong album", fileArtist: "郑智化", tagArtist: "郑智化", tagAlbum: "另一张专辑"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application := newApp(t)
			library := t.TempDir()
			indexPlaylistTestFile(t, application, library,
				tc.fileArtist+" - 岁月风华之恋曲典藏/"+tc.fileArtist+" - 水手.flac",
				tc.tagArtist, tc.tagAlbum, "水手")
			application.invalidatePlaylistLibraryCache()
			if application.playlistLibrary().hasTrack("郑智化", "岁月风华之恋曲典藏", "水手") {
				t.Fatal("filename-only evidence was accepted despite conflicting embedded identity")
			}
		})
	}

	application := newApp(t)
	library := t.TempDir()
	path := filepath.Join(library, "郑智化 - 岁月风华之恋曲典藏", "郑智化 - 水手.flac")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a readable FLAC"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := audiorelease.IndexExisting(application.engine, audiorelease.Existing{
		RequestID: "playlist-layout-damaged", RootID: "playlist-layout-damaged-root", Track: 1,
		Path: path, Library: library, Format: "flac", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatal(added, err)
	}
	application.invalidatePlaylistLibraryCache()
	if application.playlistLibrary().hasTrack("郑智化", "岁月风华之恋曲典藏", "水手") {
		t.Fatal("a damaged file whose embedded identity could not be read was accepted")
	}
}

func TestPlaylistPresenceAcceptsVariousArtistsWithEmbeddedTrackCredit(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	indexPlaylistTestFile(t, application, library,
		"群星/合辑/群星 - Shared Song.flac",
		"Guest Singer", "Shared Album", "Shared Song")
	application.invalidatePlaylistLibraryCache()
	if !application.playlistLibrary().hasTrack("Various Artists", "Shared Album", "Shared Song") {
		t.Fatal("Various Artists did not accept embedded track-credit evidence")
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
func TestPlaylistPresenceUsesFullTagsWhenFolderNameIsTruncated(t *testing.T) {
	application := newApp(t)
	incoming, library := t.TempDir(), t.TempDir()
	wav := filepath.Join(incoming, "track.wav")
	if err := os.WriteFile(wav, pcmWAV(140), 0o600); err != nil {
		t.Fatal(err)
	}
	tagged := filepath.Join(incoming, "track.flac")
	if _, err := wavflac.ConvertFile(wav, tagged, true); err != nil {
		t.Fatal(err)
	}
	fullAlbum := strings.TrimSpace(strings.Repeat("Catalogue Album With A Long Descriptive Name ", 6))
	folderAlbum := "Catalogue Album File Safe Truncated"
	if err := flactags.Write(tagged, flactags.Tags{
		Artists: []string{"Artist;Guest"}, Album: fullAlbum, Title: "Long Album Song",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "truncated-folder-presence", RootID: "truncated-folder-album", Artist: "Artist",
		Album: folderAlbum, Title: "Long Album Song", Track: 1, SourceName: "track.flac", Incoming: incoming, Library: library,
	}); err != nil {
		t.Fatal(err)
	}
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	if !index.hasTrack("Artist / Guest", fullAlbum, "Long Album Song") {
		t.Fatal("full embedded album and artist tags were ignored because the folder name differed")
	}
	if index.hasTrack("Artist / Guest", fullAlbum+" Other", "Long Album Song") || index.hasTrack("Other Artist", fullAlbum, "Long Album Song") {
		t.Fatal("full-tag fallback collapsed a different album or artist")
	}
}

func TestPlaylistPresenceMatchesFeaturedCreditButKeepsVersionsDistinct(t *testing.T) {
	a := newApp(t)
	indexPlaylistTestFile(t, a, t.TempDir(), "CLIFF EDGE _ 中村舞子/Best Of Love/01 Endless Tears.flac",
		"CLIFF EDGE / 中村舞子", "Best Of Love", "Endless Tears")
	index := a.playlistLibrary()
	if !index.hasTrack("CLIFF EDGE / 中村舞子", "Best of LOVE", "Endless Tears feat. 中村舞子") {
		t.Fatal("a feature listed in the track credits should match")
	}
	for _, title := range []string{"Endless Tears feat. Other", "Endless Tears feat. 中村舞子 (Remix)"} {
		if index.hasTrack("CLIFF EDGE / 中村舞子", "Best of LOVE", title) {
			t.Fatalf("different recording matched: %s", title)
		}
	}
}

func TestNeteasePlaylistPresenceUsesManagedFolderAndMeasuredDuration(t *testing.T) {
	a := newApp(t)
	library := t.TempDir()
	post(t, a, "/v1/settings/library", map[string]any{"library_dir": library})
	album := strings.Repeat("長いアルバム", 9)
	title := strings.Repeat("長い曲名", 12)
	folder := filepath.Join(library, "Artist", publishComponent(album))
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, publishComponent(title)+".flac")
	if err := os.WriteFile(path, oneSecondFLAC(t, "Artist", title), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := flactags.Write(path, flactags.Tags{Title: title, Artist: "Artist", Album: "Localized Album"}); err != nil {
		t.Fatal(err)
	}
	if err := a.indexReleasedPath(path); err != nil {
		t.Fatal(err)
	}
	index := a.playlistLibrary()
	row := playlistTrack{Artist: "Artist", Title: title, Album: album, Duration: "1", NeteaseSongID: 123}
	if !index.hasPlaylistRow(row) {
		t.Fatal("unchanged recording in the managed album folder was missed")
	}
	row.Duration = "30"
	if index.hasPlaylistRow(row) {
		t.Fatal("accepted a different duration")
	}
	row.Duration, row.Title = "1", title+" (Remix)"
	if index.hasPlaylistRow(row) {
		t.Fatal("accepted a different title")
	}
}

func TestNeteasePlaylistPresenceAccountsForWholeSecondDuration(t *testing.T) {
	a := newApp(t)
	library := t.TempDir()
	post(t, a, "/v1/settings/library", map[string]any{"library_dir": library})
	folder := filepath.Join(library, "ClariS", "Song")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "ClariS - Song.flac")
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=4.1",
		"-metadata", "artist=ClariS", "-metadata", "album=Song (Anime Edition)", "-metadata", "title=Song", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate FLAC: %v: %s", err, out)
	}
	if err := a.indexReleasedPath(path); err != nil {
		t.Fatal(err)
	}
	index := a.playlistLibrary()
	row := playlistTrack{Artist: "ClariS", Title: "Song", Album: "Song", Duration: "1", NeteaseSongID: 123}
	if !index.hasPlaylistRow(row) {
		t.Fatal("a whole-second playlist duration lost its precision margin")
	}
	row.Duration = "1.050"
	if index.hasPlaylistRow(row) {
		t.Fatal("a precise duration should retain the original tolerance")
	}
}

func TestPlaylistPresenceMatchesSanitizedTitleOnlyWithMatchingTags(t *testing.T) {
	a := newApp(t)
	library := t.TempDir()
	indexPlaylistTestFile(t, a, library, "Artist/Album/Artist - STARTDASH!!.flac",
		"Artist", "Album", "START:DASH!!")
	index := a.playlistLibrary()
	if !index.hasTrack("Artist", "Album", "START:DASH!!") {
		t.Fatal("file with punctuation removed from its name should match its tags")
	}
	if index.hasTrack("Artist", "Album", "STARTDASH!!") {
		// The historical path match does not include the artist-prefixed name.
		t.Fatal("compact candidate lookup must not replace the original tag check")
	}
}

func TestPlaylistPresenceParallelMatchesLibrary(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	indexPlaylistTestFile(t, application, library, "Artist One/Album One/01 Song One.flac", "Artist One", "Album One", "Song One")
	indexPlaylistTestFile(t, application, library, "Artist Two/Album Two/01 Song Two.flac", "Artist Two", "Album Two", "Song Two")
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	rows := make([]playlistTrack, 40)
	for i := range rows {
		rows[i] = playlistTrack{Artist: "Missing Artist", Album: "Missing Album", Title: "Missing " + strconv.Itoa(i)}
	}
	rows[3] = playlistTrack{Artist: "Artist One", Album: "Album One", Title: "Song One"}
	rows[27] = playlistTrack{Artist: "Artist Two", Album: "Album Two", Title: "Song Two"}
	present := playlistPresenceForIndex(index, rows)
	if len(present) != len(rows) || !present[3] || !present[27] {
		t.Fatalf("parallel presence did not retain known rows: %v", present)
	}
	for i, ok := range present {
		if ok && i != 3 && i != 27 {
			t.Fatalf("parallel presence reported missing row %d as present", i)
		}
	}
}

func TestPlaylistPresenceMatchesSameRecordingAcrossAlbums(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	application := newApp(t)
	library := t.TempDir()
	path := filepath.Join(library, "Artist", "Other Album", "01 Song.mp3")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-metadata", "artist=Artist", "-metadata", "album=Other Album", "-metadata", "title=Song", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate mp3: %v %s", err, output)
	}
	if added, err := audiorelease.IndexExisting(application.engine, audiorelease.Existing{
		RequestID: "playlist-cross-album", RootID: "playlist-cross-album-root", Track: 1,
		Path: path, Library: library, Format: "mp3", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatalf("index cross-album recording: added=%v err=%v", added, err)
	}
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	row := playlistTrack{Artist: "Artist", Album: "Single", Title: "Song", Duration: "30", NeteaseSongID: 42}
	if !index.hasPlaylistRow(row) {
		t.Fatal("same title, artist and duration in another album was reported missing")
	}
	row.Duration = "45"
	if index.hasPlaylistRow(row) {
		t.Fatal("a duration mismatch was accepted as the same recording")
	}
}

func TestPlaylistMatchKeyFoldsBoundedOrthographicVariants(t *testing.T) {
	tests := []struct {
		left, right string
	}{
		{"人間失格～メインテーマ～", "人间失格~メインテーマ~"},
		{"i BELIEVE～星に願いを", "i BELIEVE ~星に愿いを"},
		{"5年後･･･", "5年后…"},
	}
	for _, test := range tests {
		if got, want := playlistMatchKey(test.left), playlistMatchKey(test.right); got != want {
			t.Fatalf("playlistMatchKey(%q)=%q, %q)=%q", test.left, got, test.right, want)
		}
	}
	if !playlistArtistFallbackMatches("KEMU VOXX / GUMI", "KEMU VOXX,GUMI") {
		t.Fatal("comma-separated multi-artist path did not match slash-separated playlist credits")
	}
	if playlistArtistFallbackMatches("Artist", "Artist Tribute") {
		t.Fatal("single-artist fallback accepted a different path credit")
	}
}

func TestPlaylistPresenceMatchesCJKVariantTags(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	application := newApp(t)
	library := t.TempDir()
	path := filepath.Join(library, "中島ノブユキ", "人間失格", "中島ノブユキ - 人间失格~メインテーマ~.mp3")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-metadata", "artist=中島ノブユキ", "-metadata", "album=人间失格", "-metadata", "title=人间失格~メインテーマ~", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate mp3: %v %s", err, output)
	}
	if added, err := audiorelease.IndexExisting(application.engine, audiorelease.Existing{
		RequestID: "playlist-cjk-variant", RootID: "playlist-cjk-variant-root", Track: 1,
		Path: path, Library: library, Format: "mp3", SourceName: "reindex", MetadataOnly: true,
	}); err != nil || !added {
		t.Fatalf("index CJK variant: added=%v err=%v", added, err)
	}
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	row := playlistTrack{Artist: "中島ノブユキ", Album: "人間失格", Title: "人間失格～メインテーマ～", Duration: "30", NeteaseSongID: 42}
	if !index.hasPlaylistRow(row) {
		t.Fatal("equivalent CJK and punctuation variants were reported missing")
	}
	if !index.hasRecordingIdentity("中島ノブユキ", "人間失格～メインテーマ～") {
		t.Fatal("stale failure recovery did not find the exact credited recording")
	}
	if index.hasRecordingIdentity("別の歌手", "人間失格～メインテーマ～") {
		t.Fatal("stale failure recovery ignored the credited artist")
	}
	row.Title = "人間失格～ライブテーマ～"
	if index.hasPlaylistRow(row) {
		t.Fatal("different CJK title was accepted")
	}
}
