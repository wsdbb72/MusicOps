package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

// A batch that asks for artwork writes the lyrics into the tag and embeds the
// cover in one pass, with the sidecar the saved mode asks for.
func TestScrapeBatchFetchesLyricsAndCover(t *testing.T) {
	application := newApp(t)
	catalog := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	path, full := writeScrapeFLAC(t, application, "opening.flac", 240.25, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	cover := jpegBody()
	service, _ := lrcAPIFake(t, "[00:01.00]hello\n[00:02.00]world", cover, "Authorization", "")
	saveLyricsService(t, application, service.URL, "Authorization", "")

	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{path}, "catalog_base_url": catalog.server.URL, "overwrite": true, "with_artwork": true,
	})
	if code != http.StatusOK || decoded["written"] != float64(1) {
		t.Fatal(code, decoded)
	}
	if decoded["lyrics_written"] != float64(1) || decoded["cover_written"] != float64(1) {
		t.Fatal(decoded)
	}
	tags := readTags(t, full)
	if !strings.Contains(firstTagValue(tags.Raw, "LYRICS"), "[00:01.00]hello") {
		t.Fatal(tags.Raw["LYRICS"])
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(flactags.FrontCover(raw), cover) {
		t.Fatal("the cover was not embedded")
	}
	sidecar, err := os.ReadFile(strings.TrimSuffix(full, filepath.Ext(full)) + ".jpg")
	if err != nil || !bytes.Equal(sidecar, cover) {
		t.Fatal("the sidecar was not written", err)
	}
	// Roon, Plex and Navidrome read "<track>.lrc", not the tag.
	lyricsFile, err := os.ReadFile(strings.TrimSuffix(full, filepath.Ext(full)) + ".lrc")
	if err != nil || !strings.Contains(string(lyricsFile), "[00:01.00]hello") {
		t.Fatal("the lrc sidecar was not written", err, string(lyricsFile))
	}
	if legacy := full + ".lrc"; legacy != "" {
		if _, err := os.Stat(legacy); err == nil {
			t.Fatal("an audio-extension lrc was written as well")
		}
	}
}

// A failing artwork service refuses the file and leaves it byte-identical:
// half a write is worse than no write.
func TestScrapeBatchArtworkFailureLeavesTheFile(t *testing.T) {
	application := newApp(t)
	catalog := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	path, full := writeScrapeFLAC(t, application, "opening.flac", 240.25, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	before, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lyrics" {
			_, _ = io.WriteString(w, "[00:01.00]hello")
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	saveLyricsService(t, application, broken.URL, "Authorization", "")

	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{path}, "catalog_base_url": catalog.server.URL, "with_artwork": true,
	})
	if code != http.StatusOK || decoded["failed"] != float64(1) {
		t.Fatal(code, decoded)
	}
	files, _ := decoded["files"].([]any)
	row, _ := files[0].(map[string]any)
	if row["code"] != "ARTWORK_UNAVAILABLE" || !strings.Contains(row["note"].(string), "500") {
		t.Fatal(row)
	}
	after, err := os.ReadFile(full)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("a refused batch changed the file")
	}
}

func TestScrapeBatchRestoresSidecarsWhenCoverWriteFails(t *testing.T) {
	application := newApp(t)
	catalog := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	path, full := writeScrapeFLAC(t, application, "sidecar-rollback.flac", 240.25, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	originalAudio, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	lyricsPath := lyricsSidecarPath(full)
	coverPath := strings.TrimSuffix(full, filepath.Ext(full)) + ".jpg"
	oldLyrics := []byte("[00:01.00]old batch lyrics\n")
	oldCover := []byte("old batch cover")
	if err := os.WriteFile(lyricsPath, oldLyrics, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coverPath, oldCover, 0o644); err != nil {
		t.Fatal(err)
	}
	newCover := jpegBody()
	service, _ := lrcAPIFake(t, "[00:02.00]new batch lyrics", newCover, "Authorization", "")
	saveLyricsService(t, application, service.URL, "Authorization", "")

	previousRename := atomicReplaceRename
	blockCoverOnce := true
	atomicReplaceRename = func(oldPath, newPath string) error {
		if blockCoverOnce && strings.EqualFold(filepath.Ext(newPath), ".jpg") {
			blockCoverOnce = false
			return errors.New("injected batch sidecar rename failure")
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { atomicReplaceRename = previousRename })

	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{path}, "catalog_base_url": catalog.server.URL,
		"overwrite": true, "with_artwork": true,
	})
	if code != http.StatusOK || decoded["failed"] != float64(1) {
		t.Fatal(code, decoded)
	}
	files, _ := decoded["files"].([]any)
	row, _ := files[0].(map[string]any)
	if row["code"] != "COVER_WRITE_FAILED" {
		t.Fatal(row)
	}
	if body, err := os.ReadFile(full); err != nil || !bytes.Equal(body, originalAudio) {
		t.Fatal("batch audio was not restored", err)
	}
	if body, err := os.ReadFile(lyricsPath); err != nil || !bytes.Equal(body, oldLyrics) {
		t.Fatalf("old batch lyrics were not restored: %q err=%v", body, err)
	}
	if body, err := os.ReadFile(coverPath); err != nil || !bytes.Equal(body, oldCover) {
		t.Fatalf("old batch cover was not restored: %q err=%v", body, err)
	}
}

// The artwork batch is deliberately smaller: two extra reads per file.
func TestScrapeBatchArtworkCap(t *testing.T) {
	application := newApp(t)
	paths := make([]string, 0, scrapeArtworkMaxFiles+1)
	for i := 0; i <= scrapeArtworkMaxFiles; i++ {
		paths = append(paths, fmt.Sprintf("incoming/track-%02d.flac", i))
	}
	code, decoded := scrapePost(t, application, map[string]any{"paths": paths, "with_artwork": true})
	if code != http.StatusUnprocessableEntity || decoded["code"] != "ARTWORK_BATCH_TOO_LARGE" {
		t.Fatal(code, decoded)
	}
}

// A catalogue miss must not suppress independently available artwork. The
// existing tags are enough to ask the lyrics service, and the audio stream stays
// untouched while its .lrc and .jpg sidecars are filled.
func TestScrapeBatchFetchesArtworkWhenCatalogIsUnconfirmed(t *testing.T) {
	application := newApp(t)
	catalog := newFakeMusicBrainz(t, `{"releases":[]}`, `{}`, http.StatusOK)
	path, full := writeScrapeFLAC(t, application, "opening.flac", 240.25, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	before, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	cover := jpegBody()
	service, _ := lrcAPIFake(t, "[00:01.00]fallback lyrics", cover, "Authorization", "")
	saveLyricsService(t, application, service.URL, "Authorization", "")

	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{path}, "catalog_base_url": catalog.server.URL, "with_artwork": true,
	})
	if code != http.StatusOK || decoded["written"] != float64(1) || decoded["failed"] != float64(0) || decoded["lyrics_written"] != float64(1) || decoded["cover_written"] != float64(1) {
		t.Fatal(code, decoded)
	}
	files, _ := decoded["files"].([]any)
	row, _ := files[0].(map[string]any)
	if row["code"] != "CATALOG_UNCONFIRMED" || row["status"] != "written" {
		t.Fatal(row)
	}
	after, err := os.ReadFile(full)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("catalogue fallback changed the audio stream", err)
	}
	lyricsPath := strings.TrimSuffix(full, filepath.Ext(full)) + ".lrc"
	lyrics, err := os.ReadFile(lyricsPath)
	if err != nil || !strings.Contains(string(lyrics), "fallback lyrics") {
		t.Fatalf("lyrics=%q err=%v", lyrics, err)
	}
	sidecar, err := os.ReadFile(strings.TrimSuffix(full, filepath.Ext(full)) + ".jpg")
	if err != nil || !bytes.Equal(sidecar, cover) {
		t.Fatal("cover sidecar was not written", err)
	}
}

// Two sidecars are one user-visible transaction. If the cover cannot be
// committed after the lyrics write, the helper restores the prior state.
func TestScrapeArtworkSidecarsRollBackTogether(t *testing.T) {
	application := newApp(t)
	path := filepath.Join(t.TempDir(), "song.flac")
	cover := jpegBody()
	previousRename := atomicReplaceRename
	atomicReplaceRename = func(oldPath, newPath string) error {
		if strings.EqualFold(filepath.Ext(newPath), ".jpg") {
			return errors.New("injected jpg failure")
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { atomicReplaceRename = previousRename })
	lyrics, wroteCover, err := application.writeScrapeArtworkSidecars(path, scrapeArtworkData{
		Lyrics: "[00:01.00]lyrics", Cover: cover,
	}, false)
	if err == nil || lyrics || wroteCover {
		t.Fatalf("lyrics=%v cover=%v err=%v", lyrics, wroteCover, err)
	}
	for _, candidate := range []string{lyricsSidecarPath(path), strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"} {
		if _, statErr := os.Stat(candidate); !os.IsNotExist(statErr) {
			t.Fatalf("partial sidecar survived after failure: %s stat=%v", candidate, statErr)
		}
	}
}

func TestScrapeGenericArtworkWhenCatalogIsUnconfirmed(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	application := newApp(t)
	folder := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(folder, "opening.mp3")
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-metadata", "artist=Some Artist", "-metadata", "album=First Album", "-metadata", "title=Opening", full)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	before, err := genericAudioHash(context.Background(), full)
	if err != nil {
		t.Fatal(err)
	}
	catalog := newFakeMusicBrainz(t, `{"releases":[]}`, `{}`, http.StatusOK)
	cover := jpegBody()
	service, _ := lrcAPIFake(t, "[00:01.00]generic fallback", cover, "Authorization", "")
	saveLyricsService(t, application, service.URL, "Authorization", "")

	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{"incoming/opening.mp3"}, "catalog_base_url": catalog.server.URL, "with_artwork": true,
	})
	if code != http.StatusOK || decoded["written"] != float64(1) || decoded["failed"] != float64(0) {
		t.Fatal(code, decoded)
	}
	after, err := genericAudioHash(context.Background(), full)
	if err != nil || before != after {
		t.Fatalf("generic artwork fallback changed the stream: %s %s %v", before, after, err)
	}
	if _, err := os.Stat(strings.TrimSuffix(full, filepath.Ext(full)) + ".lrc"); err != nil {
		t.Fatal("lyrics sidecar missing", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(full, filepath.Ext(full)) + ".jpg"); err != nil {
		t.Fatal("cover sidecar missing", err)
	}
}

// A dry run reports the plan without spending a single artwork request.
func TestScrapeBatchDryRunDoesNotFetchArtwork(t *testing.T) {
	application := newApp(t)
	catalog := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	path, _ := writeScrapeFLAC(t, application, "opening.flac", 240.25, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	service, seen := lrcAPIFake(t, "[00:01.00]hello", jpegBody(), "", "")
	saveLyricsService(t, application, service.URL, "Authorization", "")

	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{path}, "catalog_base_url": catalog.server.URL, "with_artwork": true, "dry_run": true,
	})
	if code != http.StatusOK || decoded["planned"] != float64(1) {
		t.Fatal(code, decoded)
	}
	files, _ := decoded["files"].([]any)
	row, _ := files[0].(map[string]any)
	if row["artwork_planned"] != true {
		t.Fatal(row)
	}
	if len(*seen) != 0 {
		t.Fatalf("a dry run fetched artwork: %d requests", len(*seen))
	}
}
