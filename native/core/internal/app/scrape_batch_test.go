package app

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

// scrapeDetailJSON is one album with two tracks at the durations the fixtures
// write, plus the genre only the release carries.
const scrapeDetailJSON = `{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02",
  "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
  "release-group": {"id": "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"},
  "genres": [{"name": "Ambient", "count": 4}],
  "media": [{"position": 1, "track-count": 2, "tracks": [
    {"id": "t1", "title": "Opening", "position": 1, "length": 240250,
     "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}]},
    {"id": "t2", "title": "Closing", "position": 2, "length": 180500,
     "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}]}
  ]}]}`

// scrapePost calls the handler through the real mux so the route registration
// is exercised too.
func scrapePost(t *testing.T, application *App, body map[string]any) (int, map[string]any) {
	t.Helper()
	recorder := postRaw(t, application, "/v1/library/scrape", body)
	return recorder.Code, decodeBody(t, recorder)
}

func writeScrapeFLAC(t *testing.T, application *App, name string, seconds float64, tags flactags.Tags) (string, string) {
	t.Helper()
	dir := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, flacWithDuration(t, seconds), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := flactags.Write(full, tags); err != nil {
		t.Fatal(err)
	}
	return "incoming/" + name, full
}

// One call must identify the album once and write the catalogue's fields —
// including the classification tag — into every selected track. This is the
// batch half of F10; before it, the only way to scrape was one file at a time
// through the single-file completion.
func TestScrapeBatchWritesEverySelectedTrack(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	openingPath, openingFull := writeScrapeFLAC(t, application, "opening.flac", 240.25, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	closingPath, closingFull := writeScrapeFLAC(t, application, "closing.flac", 180.5, flactags.Tags{
		Title: "Closing", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 2,
	})
	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{openingPath, closingPath}, "catalog_base_url": fake.server.URL,
	})
	if code != http.StatusOK || decoded["ok"] != true {
		t.Fatal(code, decoded)
	}
	if decoded["written"] != float64(2) || decoded["failed"] != float64(0) || decoded["skipped"] != float64(0) {
		t.Fatal(decoded)
	}
	if decoded["genre_written"] != float64(2) {
		t.Fatal("genre_written:", decoded["genre_written"])
	}
	for _, full := range []string{openingFull, closingFull} {
		tags := readTags(t, full)
		if tags.Genre != "Ambient" || tags.AlbumArtist != "Some Artist" || tags.Date != "2019-01-02" {
			t.Fatal(tags.Genre, tags.AlbumArtist, tags.Date)
		}
	}
	if tags := readTags(t, openingFull); tags.TrackNumber != 1 {
		t.Fatal(tags.TrackNumber)
	}
	if tags := readTags(t, closingFull); tags.TrackNumber != 2 {
		t.Fatal(tags.TrackNumber)
	}
	albums, _ := decoded["albums"].([]any)
	if len(albums) != 1 {
		t.Fatal("one album must cost one lookup:", albums)
	}
	row, _ := albums[0].(map[string]any)
	if row["confirmed"] != true || row["release_id"] != "mbid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatal(row)
	}
	if fake.searches.Load() != 1 || fake.details.Load() != 1 {
		t.Fatal("the album was looked up more than once:", fake.searches.Load(), fake.details.Load())
	}
}

// A dry run reports the plan and leaves every byte alone.
func TestScrapeBatchDryRunDoesNotWrite(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	apiPath, full := writeScrapeFLAC(t, application, "opening.flac", 240.25, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	before, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{apiPath}, "catalog_base_url": fake.server.URL, "dry_run": true,
	})
	if code != http.StatusOK || decoded["planned"] != float64(1) || decoded["written"] != float64(0) {
		t.Fatal(code, decoded)
	}
	after, err := os.ReadFile(full)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("a dry run changed the file")
	}
	files, _ := decoded["files"].([]any)
	first, _ := files[0].(map[string]any)
	if first["status"] != "planned" {
		t.Fatal(first)
	}
	written, _ := first["written_fields"].([]any)
	if !containsString(written, "genre") {
		t.Fatal(written)
	}
}

// Files that cannot be scraped are reported per file, never silently dropped.
func TestScrapeBatchReportsSkippedFiles(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	untaggedPath, _ := writeScrapeFLAC(t, application, "untagged.flac", 240.25, flactags.Tags{})
	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{untaggedPath, "incoming/missing.flac"}, "catalog_base_url": fake.server.URL,
	})
	if code != http.StatusOK || decoded["skipped"] != float64(2) {
		t.Fatal(code, decoded)
	}
	files, _ := decoded["files"].([]any)
	if len(files) != 2 {
		t.Fatal(files)
	}
	codes := map[string]bool{}
	for _, item := range files {
		entry, _ := item.(map[string]any)
		codes[entry["code"].(string)] = true
	}
	if !codes["NO_TAGS"] || !codes["UNSUPPORTED"] {
		t.Fatal(codes)
	}
}

// An empty selection is a 400: nothing to do is not a successful batch.
func TestScrapeBatchRejectsAnEmptySelection(t *testing.T) {
	application := newApp(t)
	recorder := postRaw(t, application, "/v1/library/scrape", map[string]any{"paths": []string{"  "}})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "请先勾选") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}
