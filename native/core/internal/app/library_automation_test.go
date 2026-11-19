package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
		},
	}}
	if err := application.saveLibraryAutomationScanCache(cache); err != nil {
		t.Fatal(err)
	}
	application.libraryAutomation.update(func(report *libraryAutomationReport) { *report = libraryAutomationReport{} })
	application.runAutomatedScrape(context.Background(), library, []string{path})
	_, report := application.libraryAutomation.snapshot()
	if report.ScanChecked != 1 || report.ScanReused != 1 || report.ScanFailed != 0 || len(report.Errors) != 0 {
		t.Fatalf("unchanged cached file was probed or failed: %+v", report)
	}

	if err := os.WriteFile(path, []byte("changed-and-still-not-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	application.libraryAutomation.update(func(report *libraryAutomationReport) { *report = libraryAutomationReport{} })
	application.runAutomatedScrape(context.Background(), library, []string{path})
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

func TestLibraryAutomationMergesOnlyReadyPCMDuplicates(t *testing.T) {
	application := newApp(t)
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	first := genTestFLAC(t, firstDir, "first.flac")
	second := genTestFLAC(t, secondDir, "second.flac")
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "first.flac"), firstData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "second.flac"), secondData, 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLibraryFile(t, application, "first.flac", "auto-dup-1", "Artist", "Album", "Song A", 1)
	releaseLibraryFile(t, application, "second.flac", "auto-dup-2", "Artist", "Album", "Song B", 1)
	settings := libraryAutomationSettings{Enabled: true, Dedupe: true, IntervalHours: 6}
	if err := application.runLibraryAutomationPass(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	_, report := application.libraryAutomation.snapshot()
	if report.DuplicateGroups < 1 || report.Deduplicated < 1 || len(report.Errors) != 0 {
		t.Fatalf("report=%+v", report)
	}
}
