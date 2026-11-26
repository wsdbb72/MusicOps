package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestLibraryAutomationSettingsCanBeConfigured(t *testing.T) {
	application := newApp(t)
	body := getJSON(t, application, "/v1/library/automation")
	if body["ok"] != true {
		t.Fatal(body)
	}
	settings, _ := body["settings"].(map[string]any)
	if settings["enabled"] != true || settings["scrape"] != true || settings["upgrade"] != true {
		t.Fatal(settings)
	}
	saved := post(t, application, "/v1/library/automation", map[string]any{
		"enabled": false, "scrape": false, "dedupe": true, "upgrade": false, "expand": true, "interval_hours": 12,
	})
	result, _ := saved["settings"].(map[string]any)
	if result["enabled"] != false || result["scrape"] != false || result["upgrade"] != false || result["interval_hours"] != float64(12) {
		t.Fatal(result)
	}
}

func TestLibraryAutomationEnqueuesLossyUpgradeOnce(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	track := filepath.Join(folder, "01 Song.mp3")
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-metadata", "artist=Artist", "-metadata", "album=Album", "-metadata", "title=Song", track)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	settings := libraryAutomationSettings{Enabled: true, Upgrade: true, IntervalHours: 6}
	if err := application.runLibraryAutomationPass(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	_, report := application.libraryAutomation.snapshot()
	if report.UpgradeJobs != 1 {
		t.Fatalf("report=%+v", report)
	}
	application.libraryAutomation.update(func(report *libraryAutomationReport) { *report = libraryAutomationReport{} })
	if err := application.runLibraryAutomationPass(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	_, again := application.libraryAutomation.snapshot()
	if again.UpgradeJobs != 0 {
		t.Fatalf("duplicate automation job: %+v", again)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStop()
	if err := application.runner.Stop(stopCtx); err != nil {
		t.Fatalf("stop automation executor: %v", err)
	}
}

func TestLibraryAutomationQueuesEveryLossyTrack(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"One", "Two"} {
		track := filepath.Join(folder, title+".mp3")
		command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-metadata", "artist=Artist", "-metadata", "album=Album", "-metadata", "title="+title, track)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v %s", title, err, output)
		}
	}
	if err := application.runLibraryAutomationPass(context.Background(), libraryAutomationSettings{Enabled: true, Upgrade: true, IntervalHours: 6}); err != nil {
		t.Fatal(err)
	}
	_, report := application.libraryAutomation.snapshot()
	if report.UpgradeJobs != 2 {
		t.Fatalf("two lossy tracks produced %d upgrade jobs: %+v", report.UpgradeJobs, report)
	}
	recent, err := application.tasks.Recent(nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	seenSnapshot := 0
	for _, task := range recent.Tasks {
		if task.Command == workflow.CommandSnapshot {
			seenSnapshot++
			if task.Priority != automationWorkflowPriority {
				t.Fatalf("automation task priority=%d, want %d", task.Priority, automationWorkflowPriority)
			}
		}
	}
	if seenSnapshot != 2 {
		t.Fatalf("snapshot tasks=%d, want 2", seenSnapshot)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStop()
	if err := application.runner.Stop(stopCtx); err != nil {
		t.Fatalf("stop automation executor: %v", err)
	}
}

func TestLibraryAutomationQueuesCorruptFLACReplacement(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	folder := filepath.Join(library, "Artist - Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "03 Song.flac")
	if err := os.WriteFile(path, make([]byte, 256), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := application.runLibraryAutomationPass(context.Background(), libraryAutomationSettings{Enabled: true, Scrape: true, Upgrade: true, IntervalHours: 6}); err != nil {
		t.Fatal(err)
	}
	_, report := application.libraryAutomation.snapshot()
	if report.UpgradeJobs != 1 {
		t.Fatalf("corrupt FLAC produced %d upgrade jobs: %+v", report.UpgradeJobs, report)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStop()
	if err := application.runner.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryAutomationFetchesArtworkWithoutCatalogIdentity(t *testing.T) {
	application := newApp(t)
	service, _ := lrcAPIFake(t, "[00:01.00]direct artwork", jpegBody(), "Authorization", "")
	saveLyricsService(t, application, service.URL, "Authorization", "")
	_, full := writeScrapeFLAC(t, application, "direct-artwork.flac", 1, flactags.Tags{
		Title: "Song", Artists: []string{"Artist"}, AlbumArtist: "Artist", Album: "Album",
		TrackNumber: 1, Date: "2026", Genre: "Pop",
	})
	before, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	application.runAutomatedScrape(context.Background(), application.Dir, []string{full}, false)
	_, report := application.libraryAutomation.snapshot()
	if report.ArtworkProcessed != 1 || report.ArtworkWritten != 1 || report.ArtworkFailed != 0 || report.ArtworkSkipped != 0 {
		t.Fatalf("unexpected artwork report: %+v", report)
	}
	after, err := os.ReadFile(full)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("automation artwork changed the audio stream", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(full, filepath.Ext(full)) + ".lrc"); err != nil {
		t.Fatal("lyrics sidecar missing", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(full, filepath.Ext(full)) + ".jpg"); err != nil {
		t.Fatal("cover sidecar missing", err)
	}
}

func TestAlbumExpansionRequiresConfirmedMultiTrackRelease(t *testing.T) {
	application := newApp(t)
	catalog := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	track := localWorkflowTrack{Codec: "mp3", Artist: "Some Artist", Album: "First Album", Title: "Opening", Duration: 240.25}
	expand, err := application.albumNeedsExpansion(context.Background(), catalogClientForTest(catalog.server.URL), []localWorkflowTrack{track})
	if err != nil || !expand {
		t.Fatalf("multi-track release was not expandable: expand=%v err=%v", expand, err)
	}

	single := newApp(t)
	singleDetail := `{"id":"cccccccc-dddd-eeee-ffff-111111111111","title":"Single","date":"2020-01-01","artist-credit":[{"artist":{"name":"Some Artist"},"name":"Some Artist"}],"release-group":{"id":"dddddddd-eeee-ffff-1111-222222222222"},"media":[{"position":1,"track-count":1,"tracks":[{"id":"t1","title":"Opening","position":1,"length":240250,"artist-credit":[{"artist":{"name":"Some Artist"},"name":"Some Artist"}]}]}]}`
	catalogSingle := newFakeMusicBrainz(t, catalogCompletionSearch, singleDetail, http.StatusOK)
	expand, err = single.albumNeedsExpansion(context.Background(), catalogClientForTest(catalogSingle.server.URL), []localWorkflowTrack{track})
	if err != nil || expand {
		t.Fatalf("single release was marked for expansion: expand=%v err=%v", expand, err)
	}
}

func catalogClientForTest(baseURL string) *musicbrainz.Client {
	client := musicbrainz.NewClient()
	client.BaseURL = baseURL
	return client
}

func TestLibraryAutomationRunEndpointReturnsAcceptedOrRunning(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": t.TempDir()})
	recorder := postCode(t, application, "/v1/library/automation/run", map[string]any{})
	if recorder.Code != http.StatusAccepted && recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestGenericTagWriterPreservesMP3AudioStream(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), "track.mp3")
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=330:duration=1",
		"-metadata", "artist=Artist", "-metadata", "album=Album", "-metadata", "title=Song", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	before, err := genericAudioHash(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeGenericTags(context.Background(), path, map[string][]string{
		"ALBUMARTIST": {"Various Artists"}, "DATE": {"2026"}, "GENRE": {"Pop"},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := genericAudioHash(context.Background(), path)
	if err != nil || after != before {
		t.Fatalf("audio stream changed: before=%s after=%s err=%v", before, after, err)
	}
	info, err := probeGenericAudio(context.Background(), path)
	if err != nil || firstNonEmpty(info.Tags["album_artist"], info.Tags["albumartist"]) != "Various Artists" || info.Date != "2026" || info.Genre != "Pop" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestLibraryAutomationReusesUnchangedScanCache(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	path := filepath.Join(library, "Artist", "Album", "cached.mp3")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-real-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cache := libraryAutomationScanDocument{Version: libraryAutomationScanVersion, Root: library, Files: map[string]cachedLibraryScan{
		"Artist/Album/cached.mp3": {
			Size: info.Size(), ModNS: info.ModTime().UnixNano(), NeedsCatalog: false, NeedsArtwork: false,
			ArtworkCheckedNS: time.Now().UnixNano(), ArtworkValid: true,
		},
	}}
	if err := application.saveLibraryAutomationScanCache(cache); err != nil {
		t.Fatal(err)
	}
	application.libraryAutomation.update(func(report *libraryAutomationReport) { *report = libraryAutomationReport{} })
	application.runAutomatedScrape(context.Background(), library, []string{path}, false)
	_, report := application.libraryAutomation.snapshot()
	if report.ScanChecked != 1 || report.ScanReused != 1 || report.ScanFailed != 0 || len(report.Errors) != 0 {
		t.Fatalf("unchanged cached file was probed or failed: %+v", report)
	}

	if err := os.WriteFile(path, []byte("changed-and-still-not-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	application.libraryAutomation.update(func(report *libraryAutomationReport) { *report = libraryAutomationReport{} })
	application.runAutomatedScrape(context.Background(), library, []string{path}, false)
	_, changed := application.libraryAutomation.snapshot()
	if changed.ScanChecked != 1 || changed.ScanFailed != 1 || changed.ScanReused != 0 {
		t.Fatalf("changed file was not re-probed: %+v", changed)
	}
}

func TestLibraryWholeScrapeAndStopEndpoints(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": t.TempDir()})
	start := postCode(t, application, "/v1/library/scrape/whole", map[string]any{})
	if start.Code != http.StatusAccepted && start.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
	}
	stop := postCode(t, application, "/v1/library/automation/stop", map[string]any{})
	if stop.Code != http.StatusAccepted {
		t.Fatalf("stop status=%d body=%s", stop.Code, stop.Body.String())
	}
}

func TestLibraryWholeUpgradeAndStopEndpoints(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": t.TempDir()})
	start := postCode(t, application, "/v1/library/upgrade/whole", map[string]any{})
	if start.Code != http.StatusAccepted && start.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
	}
	stop := postCode(t, application, "/v1/library/automation/stop", map[string]any{})
	if stop.Code != http.StatusAccepted {
		t.Fatalf("stop status=%d body=%s", stop.Code, stop.Body.String())
	}
}

func TestLibraryIndexReportsIncrementalProgress(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 251; i++ {
		path := filepath.Join(folder, fmt.Sprintf("%03d Track.flac", i))
		if err := os.WriteFile(path, []byte("metadata-only"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	updates := 0
	lastCurrent := ""
	_, _, _, err := application.indexLibraryTreeWithProgress(context.Background(), library, func(checked int, current string) {
		updates++
		lastCurrent = current
	})
	if err != nil {
		t.Fatal(err)
	}
	if updates < 2 || lastCurrent != "索引完成" {
		t.Fatalf("updates=%d current=%q", updates, lastCurrent)
	}
}

func TestLibraryIndexRechecksChangedVisitedSnapshot(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "01 Track.flac")
	body, err := flactags.WriteBytes(syntheticFLAC(t, false), flactags.Tags{Title: "Track", Artist: "Artist", Album: "Album", TrackNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if added, _, _, err := application.indexLibraryTree(context.Background(), library); err != nil || added != 1 {
		t.Fatalf("first index: added=%d err=%v", added, err)
	}
	if added, _, _, err := application.indexLibraryTree(context.Background(), library); err != nil || added != 0 {
		t.Fatalf("unchanged index: added=%d err=%v", added, err)
	}
	if tracks := application.verifiedPlaylistAlbumTracks(library, "Artist", "Album", "Album"); len(tracks) != 1 {
		t.Fatalf("unchanged snapshot disappeared: %v", tracks)
	}
	if err := os.WriteFile(path, append(body, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	if tracks := application.verifiedPlaylistAlbumTracks(library, "Artist", "Album", "Album"); len(tracks) != 0 {
		t.Fatalf("changed file retained the old snapshot: %v", tracks)
	}
	if _, _, _, err := application.indexLibraryTree(context.Background(), library); err != nil {
		t.Fatal(err)
	}
	if tracks := application.verifiedPlaylistAlbumTracks(library, "Artist", "Album", "Album"); len(tracks) != 1 {
		t.Fatalf("changed snapshot was not refreshed: %v", tracks)
	}
}

func TestCatalogBackoffUsesRateLimitOrServerDefaults(t *testing.T) {
	rateDelay, ok := catalogBackoffDelay(&musicbrainz.RateLimitError{RetryAfter: 3 * time.Minute, StatusCode: http.StatusTooManyRequests})
	if !ok || rateDelay != 3*time.Minute {
		t.Fatalf("rate delay=%s ok=%v", rateDelay, ok)
	}
	serverDelay, ok := catalogBackoffDelay(&musicbrainz.ServerError{StatusCode: http.StatusServiceUnavailable})
	if !ok || serverDelay != 15*time.Minute {
		t.Fatalf("server delay=%s ok=%v", serverDelay, ok)
	}
}

func TestUnconfirmedTrackMatchStillRequiresAnExactTitle(t *testing.T) {
	view := &catalogReleaseView{Tracks: []catalogReleaseTrack{{Title: "Other Song", Duration: 10}}}
	if _, found := matchCatalogTrackForResult(view, "Wanted Song", 10, false); found {
		t.Fatal("an unconfirmed row matched by duration alone")
	}
	if _, found := matchCatalogTrackForResult(view, "Wanted Song", 10, true); !found {
		t.Fatal("confirmed duration-only identity was not retained")
	}
	view.Tracks = append(view.Tracks, catalogReleaseTrack{Title: "Wanted Song", Duration: 42})
	if track, found := matchCatalogTrackForResult(view, "Wanted Song", 40, false); !found || track.Title != "Wanted Song" {
		t.Fatalf("exact unconfirmed title not matched: %+v found=%v", track, found)
	}
}

func TestAutomationArtworkNoMatchIsSkippedNotFailed(t *testing.T) {
	application := newApp(t)
	_, full := writeScrapeFLAC(t, application, "no-match.flac", 1, flactags.Tags{
		Title: "No Match", Artists: []string{"Artist"}, Album: "Album", TrackNumber: 1,
	})
	service, _ := lrcAPIFake(t, "", nil, "Authorization", "")
	saveLyricsService(t, application, service.URL, "Authorization", "")
	wrote, err := application.writeAutomationArtwork(context.Background(), full)
	if err != nil || wrote {
		t.Fatalf("wrote=%v err=%v; a no-match must be a clean skip", wrote, err)
	}
}

func TestLibraryAutomationMergesBestQualityDuplicate(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	wave := filepath.Join(incoming, "second.wav")
	writeToneWAV(t, wave, 440)
	writeFLACFromWAV(t, wave, filepath.Join(incoming, "first.flac"))
	releaseLibraryFile(t, application, "first.flac", "auto-dup-1", "Artist", "Album", "Song", 1)
	releaseLibraryFile(t, application, "second.wav", "auto-dup-2", "Artist", "Album", "Song", 1)
	settings := libraryAutomationSettings{Enabled: true, Dedupe: true, IntervalHours: 6}
	if err := application.runLibraryAutomationPass(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	_, report := application.libraryAutomation.snapshot()
	if report.DuplicateGroups < 1 || report.Deduplicated < 1 || len(report.Errors) != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func TestLibraryAutomationPersistsAndRestoresDuplicateFingerprintCache(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	wave := filepath.Join(incoming, "cache-second.wav")
	writeToneWAV(t, wave, 440)
	writeFLACFromWAV(t, wave, filepath.Join(incoming, "cache-first.flac"))
	releaseLibraryFile(t, application, "cache-first.flac", "cache-dup-1", "Artist", "Album", "Song", 1)
	releaseLibraryFile(t, application, "cache-second.wav", "cache-dup-2", "Artist", "Album", "Song", 1)

	settings := libraryAutomationSettings{Enabled: true, Dedupe: true, IntervalHours: 6}
	if err := application.runLibraryAutomationPass(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	root := application.libraryDir()
	cache := application.loadLibraryDuplicateCache(root)
	if len(cache.Files) < 2 {
		t.Fatalf("expected fingerprints for both duplicates: %+v", cache.Files)
	}
	restored := 0
	for key, entry := range cache.Files {
		if entry.Duplicate == nil || entry.Duplicate.FileSize <= 0 || !entry.DuplicateOnly {
			t.Fatalf("duplicate fingerprint not persisted: %s %+v", key, entry)
		}
		path := filepath.Join(root, filepath.FromSlash(key))
		if _, err := os.Stat(path); err != nil {
			continue
		}
		result := scanAutomationFile(context.Background(), libraryAutomationScanInput{path: path, key: key, cached: entry, hasCache: true})
		if !result.ok || result.reused || result.entry.DuplicateOnly || result.entry.Duplicate == nil {
			t.Fatalf("metadata scan lost duplicate fingerprint: %+v", result)
		}
		restored++
	}
	if restored != 1 {
		t.Fatalf("expected one surviving file, restored=%d", restored)
	}
}

func TestAutomationRepairsInvalidExistingCover(t *testing.T) {
	application := newApp(t)
	_, full := writeScrapeFLAC(t, application, "invalid-cover.flac", 2, flactags.Tags{
		Title: "Song", Artists: []string{"Artist"}, AlbumArtist: "Artist", Album: "Album", TrackNumber: 1,
		Date: "2026", Genre: "Pop",
	})
	if err := writeLyricsSidecar(full, "[00:01.00]lyrics"); err != nil {
		t.Fatal(err)
	}
	cover := strings.TrimSuffix(full, filepath.Ext(full)) + ".jpg"
	valid := jpegBody()
	if err := os.WriteFile(cover, valid[:len(valid)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if !maintenanceMissingSidecars(full) {
		t.Fatal("truncated sidecar was treated as valid artwork")
	}
	if !maintenanceMissingArtwork(full, &flactags.Tags{HasFrontCover: true}) {
		t.Fatal("invalid sidecar was hidden by an embedded cover marker")
	}
	if err := os.WriteFile(cover, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if maintenanceMissingSidecars(full) {
		t.Fatal("fully decodable sidecar was still marked missing")
	}
	if maintenanceMissingArtwork(full, &flactags.Tags{HasFrontCover: true}) {
		t.Fatal("valid sidecar was still marked missing")
	}
}

func TestGroupWorkflowTracksSeparatesMixedAlbums(t *testing.T) {
	tracks := []localWorkflowTrack{
		{Path: "comp/01.flac", Artist: "Artist A", AlbumArtist: "Various", Album: "Album A"},
		{Path: "comp/02.flac", Artist: "Artist B", AlbumArtist: "Various", Album: "Album B"},
		{Path: "comp/03.flac", Artist: "Artist C", AlbumArtist: "Different Album Artist", Album: "Album A"},
	}
	groups := groupWorkflowTracksByAlbum(tracks)
	if len(groups) != 2 || len(groups[0]) != 2 || len(groups[1]) != 1 {
		t.Fatalf("groups=%+v", groups)
	}
	if groups[0][0].Artist == groups[0][1].Artist {
		t.Fatal("per-track artists were collapsed")
	}
}

func TestCachedArtworkInvalidatedWhenSidecarChanges(t *testing.T) {
	root := t.TempDir()
	track := filepath.Join(root, "track.flac")
	if err := os.WriteFile(track, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	cover := strings.TrimSuffix(track, filepath.Ext(track)) + ".jpg"
	valid := jpegBody()
	if err := os.WriteFile(cover, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	entry := cachedLibraryScan{NeedsArtwork: false}
	recordArtworkValidation(&entry, track, now)
	if !cachedArtworkFresh(track, entry, now.Add(time.Minute)) {
		t.Fatal("unchanged valid artwork was not cached")
	}
	if err := os.WriteFile(cover, valid[:len(valid)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if cachedArtworkFresh(track, entry, now.Add(time.Minute)) {
		t.Fatal("changed/truncated artwork reused the old validation")
	}
}

func TestCachedArtworkRequestsPeriodicRefresh(t *testing.T) {
	now := time.Now()
	entry := cachedLibraryScan{ArtworkCheckedNS: now.Add(-libraryArtworkRefreshTTL - time.Minute).UnixNano(), ArtworkValid: true}
	if !artworkRefreshDue(entry, now) {
		t.Fatal("valid artwork was never scheduled for identity refresh")
	}
}

func TestPeriodicArtworkRefreshOverwritesValidSidecar(t *testing.T) {
	application := newApp(t)
	server, _ := lrcAPIFake(t, "[00:01.00]refreshed lyrics", append(jpegBody(), []byte("\\nrefreshed")...), "", "")
	saveLyricsService(t, application, server.URL, "", "")
	_, full := writeScrapeFLAC(t, application, "refresh-cover.flac", 2, flactags.Tags{
		Title: "Song", Artists: []string{"Artist"}, AlbumArtist: "Artist", Album: "Album", TrackNumber: 1,
		Date: "2026", Genre: "Pop",
	})
	cover := strings.TrimSuffix(full, filepath.Ext(full)) + ".jpg"
	if err := os.WriteFile(cover, jpegBody(), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cover)
	wrote, err := application.writeAutomationArtworkMode(context.Background(), full, true)
	if err != nil || !wrote {
		t.Fatalf("wrote=%v err=%v", wrote, err)
	}
	after, _ := os.ReadFile(cover)
	if bytes.Equal(before, after) {
		t.Fatal("valid cover was not replaced during periodic identity refresh")
	}
}

func TestAutomationMigratesDuplicateCacheBeforeMetadataScan(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	legacy := libraryAutomationScanDocument{
		Version: 3, Root: library,
		Files: map[string]cachedLibraryScan{
			"Artist/Album/01.flac": {Duplicate: &cachedDuplicateFingerprint{FileSize: 42, FileModNS: 7, FileSHA256: "ordered"}},
		},
	}
	if err := application.saveLibraryAutomationScanCache(legacy); err != nil {
		t.Fatal(err)
	}
	if err := application.runLibraryAutomationPass(context.Background(), libraryAutomationSettings{Enabled: true, IntervalHours: 6}); err != nil {
		t.Fatal(err)
	}
	cache := application.loadLibraryDuplicateCache(library)
	if cache.Files["Artist/Album/01.flac"].Duplicate == nil {
		t.Fatalf("dedupe cache was lost before metadata scan: %+v", cache)
	}
}

func TestDuplicateCacheMigratesLegacyScanDocument(t *testing.T) {
	application := newApp(t)
	root := t.TempDir()
	legacy := libraryAutomationScanDocument{
		Version: 3, Root: root,
		Files: map[string]cachedLibraryScan{
			"Artist/Album/01.flac": {Duplicate: &cachedDuplicateFingerprint{FileSize: 42, FileModNS: 7, FileSHA256: "abc"}},
		},
	}
	if err := application.saveLibraryAutomationScanCache(legacy); err != nil {
		t.Fatal(err)
	}
	cache := application.loadLibraryDuplicateCache(root)
	entry, ok := cache.Files["Artist/Album/01.flac"]
	if !ok || entry.Duplicate == nil || entry.Duplicate.FileSHA256 != "abc" {
		t.Fatalf("legacy duplicate fingerprint was not migrated: %+v", cache)
	}
}

func TestAlbumAuditSignatureChangesWhenAlbumFilesChange(t *testing.T) {
	base := []localWorkflowTrack{
		{Path: "album/01.flac", Artist: "Artist", AlbumArtist: "Album Artist", Album: "Album", Title: "One"},
		{Path: "album/02.flac", Artist: "Artist", AlbumArtist: "Album Artist", Album: "Album", Title: "Two"},
	}
	first := albumWorkflowSignature(base)
	extended := append(append([]localWorkflowTrack(nil), base...), localWorkflowTrack{Path: "album/03.flac", Artist: "Artist", AlbumArtist: "Album Artist", Album: "Album", Title: "Three"})
	if albumWorkflowSignature(extended) == first {
		t.Fatal("adding an album track did not invalidate the catalogue audit")
	}
}

func TestLibraryAutomationUsesPathIdentityForArtworkFallback(t *testing.T) {
	application := newApp(t)
	service, _ := lrcAPIFake(t, "[00:01.00]path artwork", jpegBody(), "", "")
	saveLyricsService(t, application, service.URL, "", "")
	folder := filepath.Join(application.Dir, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(folder, "01 Song.flac")
	if err := os.WriteFile(full, flacWithDuration(t, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	application.runAutomatedScrape(context.Background(), application.Dir, []string{full}, false)
	_, report := application.libraryAutomation.snapshot()
	if report.ArtworkWritten != 1 || report.ArtworkFailed != 0 {
		t.Fatalf("path identity did not recover artwork: %+v", report)
	}
	if _, err := os.Stat(strings.TrimSuffix(full, filepath.Ext(full)) + ".lrc"); err != nil {
		t.Fatal("lyrics sidecar missing")
	}
}

func TestLibraryAutomationStopsArtworkBatchAfterServiceFailure(t *testing.T) {
	application := newApp(t)
	var hits atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(service.Close)
	saveLyricsService(t, application, service.URL, "", "")
	paths := make([]string, 0, 30)
	for i := 1; i <= 30; i++ {
		_, full := writeScrapeFLAC(t, application, fmt.Sprintf("%02d.flac", i), 2, flactags.Tags{
			Title: "Song", Artists: []string{"Artist"}, AlbumArtist: "Artist", Album: "Album", TrackNumber: i,
			Date: "2026", Genre: "Pop",
		})
		paths = append(paths, full)
	}
	application.runAutomatedScrape(context.Background(), application.Dir, paths, false)
	_, report := application.libraryAutomation.snapshot()
	if !report.Backlog || report.RetryAfterSec < int((10*time.Minute)/time.Second) {
		t.Fatalf("service failure did not schedule retry: %+v", report)
	}
	if report.ArtworkProcessed >= len(paths) {
		t.Fatalf("artwork batch consumed the full backlog: %+v", report)
	}
	if hits.Load() > int64(libraryAutomationArtworkWorkers*2) {
		t.Fatalf("service failure caused %d requests; circuit breaker did not stop the batch", hits.Load())
	}
}

func TestLocalAlbumNeedsCatalogCheckHandlesSmallAndNumberedGaps(t *testing.T) {
	small := []localWorkflowTrack{{Path: "01 A.flac"}, {Path: "02 B.flac"}}
	if !localAlbumNeedsCatalogCheck(small) {
		t.Fatal("two-track folder was not checked for missing album tracks")
	}
	full := []localWorkflowTrack{{Path: "01 A.flac"}, {Path: "02 B.flac"}, {Path: "03 C.flac"}, {Path: "04 D.flac"}}
	if localAlbumNeedsCatalogCheck(full) {
		t.Fatal("complete four-track folder caused an unbounded catalogue lookup")
	}
	gapped := []localWorkflowTrack{{Path: "01 A.flac"}, {Path: "02 B.flac"}, {Path: "04 D.flac"}, {Path: "05 E.flac"}}
	if !localAlbumNeedsCatalogCheck(gapped) {
		t.Fatal("numbered track gap was not sent for catalogue verification")
	}
}

func TestAlbumExpansionSupportsPartialMultiTrackDirectory(t *testing.T) {
	application := newApp(t)
	detail := `{"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","title":"First Album","date":"2019-01-02","artist-credit":[{"artist":{"name":"Some Artist"},"name":"Some Artist"}],"release-group":{"id":"bbbbbbbb-cccc-dddd-eeee-ffffffffffff"},"media":[{"position":1,"track-count":3,"tracks":[{"id":"t1","title":"Opening","position":1,"length":240250,"artist-credit":[{"artist":{"name":"Some Artist"},"name":"Some Artist"}]},{"id":"t2","title":"Closing","position":2,"length":180500,"artist-credit":[{"artist":{"name":"Some Artist"},"name":"Some Artist"}]},{"id":"t3","title":"Encore","position":3,"length":150000,"artist-credit":[{"artist":{"name":"Some Artist"},"name":"Some Artist"}]}]}]}`
	catalog := newFakeMusicBrainz(t, catalogCompletionSearch, detail, http.StatusOK)
	tracks := []localWorkflowTrack{
		{Path: "01 Opening.flac", Codec: "flac", Artist: "Some Artist", Album: "First Album", Title: "Opening", Duration: 240.25},
		{Path: "02 Closing.flac", Codec: "flac", Artist: "Some Artist", Album: "First Album", Title: "Closing", Duration: 180.5},
	}
	expand, err := application.albumNeedsExpansion(context.Background(), catalogClientForTest(catalog.server.URL), tracks)
	if err != nil || !expand {
		t.Fatalf("two present tracks of a three-track release were not expandable: expand=%v err=%v", expand, err)
	}
}

func TestLocalWorkflowAlbumArtistPreservesCompilationIdentity(t *testing.T) {
	tracks := []localWorkflowTrack{
		{Path: filepath.Join("Various Artists", "Shared Album", "01 First.flac"), Artist: "First Artist", AlbumArtist: "Various Artists", Album: "Shared Album"},
		{Path: filepath.Join("Various Artists", "Shared Album", "02 Second.flac"), Artist: "Second Artist", AlbumArtist: "Various Artists", Album: "Shared Album"},
	}
	if got := localWorkflowAlbumArtist(tracks); got != "Various Artists" {
		t.Fatalf("album artist=%q", got)
	}

	folderOnly := append([]localWorkflowTrack(nil), tracks...)
	for i := range folderOnly {
		folderOnly[i].AlbumArtist = ""
	}
	if got := localWorkflowAlbumArtist(folderOnly); got != "Various Artists" {
		t.Fatalf("folder album artist=%q", got)
	}

	flat := append([]localWorkflowTrack(nil), folderOnly...)
	flat[0].Path = "01 First.flac"
	flat[1].Path = "02 Second.flac"
	if got := localWorkflowAlbumArtist(flat); got != "First Artist" {
		t.Fatalf("fallback track artist=%q", got)
	}
}

func TestDuplicateScanWithUnchangedCacheProducesNoCheckpointUpdates(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	firstWAV := filepath.Join(incoming, "cache-a.wav")
	secondWAV := filepath.Join(incoming, "cache-b.wav")
	writeToneWAV(t, firstWAV, 440)
	writeToneWAV(t, secondWAV, 880)
	writeFLACFromWAV(t, firstWAV, filepath.Join(incoming, "cache-a.flac"))
	releaseLibraryFile(t, application, "cache-a.flac", "cache-key-1", "Artist", "Album", "Song", 1)
	releaseLibraryFile(t, application, "cache-b.wav", "cache-key-2", "Artist", "Album", "Song", 1)
	root := application.libraryDir()
	empty := application.loadLibraryAutomationScanCache(root)
	_, _, _, updates, err := application.scanDuplicatesWithOptions(context.Background(), 0, root, empty.Files, nil, nil)
	if err != nil || len(updates) == 0 {
		t.Fatalf("initial duplicate scan updates=%d err=%v", len(updates), err)
	}
	mergeDuplicateFingerprintCache(&empty, updates)
	_, _, _, secondUpdates, err := application.scanDuplicatesWithOptions(context.Background(), 0, root, empty.Files, nil, nil)
	if err != nil || len(secondUpdates) != 0 {
		t.Fatalf("unchanged cache generated %d checkpoint updates: %v", len(secondUpdates), err)
	}
}

func TestIncompleteAlbumFollowupRunsOnlyExpansion(t *testing.T) {
	got := incompleteAlbumOnlySettings(libraryAutomationSettings{Enabled: false, Scrape: true, Dedupe: true, Upgrade: true, Expand: false, IntervalHours: 2})
	if !got.Enabled || got.Scrape || got.Dedupe || got.Upgrade || !got.Expand || got.IntervalHours != 2 {
		t.Fatalf("partial-album settings = %+v", got)
	}
}

func TestMaintenanceWorkflowSkipsVanishedSourceWithoutReportingFailure(t *testing.T) {
	application := newApp(t)
	missing := filepath.Join(application.libraryDir(), "already-moved.mp3")
	if application.enqueueMaintenanceWorkflowKey(context.Background(), "upgrade", []localWorkflowTrack{{
		Path: missing, Artist: "Artist", Album: "Album", Title: "Title", Codec: "mp3", Duration: 180,
	}}, filepath.Dir(missing), "stale-scan") {
		t.Fatal("missing maintenance source created a workflow")
	}
	_, report := application.libraryAutomation.snapshot()
	if report.ErrorCount != 0 {
		t.Fatalf("stale scan row became a user-visible failure: %+v", report.Errors)
	}
}

func TestMaintenanceWorkflowSkipsCorruptRepairWithoutUsableIdentity(t *testing.T) {
	application := newApp(t)
	path := filepath.Join(application.libraryDir(), "broken.flac")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	if application.enqueueMaintenanceWorkflowKey(context.Background(), "upgrade", []localWorkflowTrack{{
		Path: path, Codec: "invalid", Duration: 1,
	}}, filepath.Dir(path), "corrupt-repair-v2") {
		t.Fatal("identity-less corrupt repair created a workflow")
	}
	_, report := application.libraryAutomation.snapshot()
	if report.ErrorCount != 0 {
		t.Fatalf("identity-less corrupt repair became a user-visible failure: %+v", report.Errors)
	}
}

func TestMaintenanceWorkflowTreatsStaleLossyCandidateAsCompletedUpgrade(t *testing.T) {
	application := newApp(t)
	source := filepath.Join(application.libraryDir(), "Artist", "Album", "source.wav")
	target := filepath.Join(application.libraryDir(), "Artist", "Album", "01 Song.flac")
	writeToneWAV(t, source, 523)
	writeFLACFromWAV(t, source, target)
	if application.enqueueMaintenanceWorkflowKey(context.Background(), "upgrade", []localWorkflowTrack{{
		Path: target, Artist: "Artist", Album: "Album", Title: "Song", Codec: "mp3", Duration: 1,
	}}, filepath.Dir(target), "corrupt-repair-v2") {
		t.Fatal("already-lossless stale candidate created an upgrade")
	}
	_, report := application.libraryAutomation.snapshot()
	if report.ErrorCount != 0 {
		t.Fatalf("completed lossless transition became a maintenance failure: %+v", report.Errors)
	}
}
