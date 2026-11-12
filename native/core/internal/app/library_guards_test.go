package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

// A locked field is the user's decision: the catalogue must not write it even
// with overwrite on, and the response has to say so instead of silently
// skipping it.
func TestFieldLockBlocksTheCatalogueWrite(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(application.Dir, "library-audio")
	albumDir := filepath.Join(library, "Some Artist", "First Album")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(albumDir, "01 opening.flac")
	if err := os.WriteFile(path, syntheticFLAC(t, false), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	if recorder := postCode(t, application, "/v1/library/guards", map[string]any{"dir": albumDir, "fields": []string{"date"}}); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	catalog := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	written := post(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": catalog.server.URL, "overwrite": true,
	})
	if written["written"] != true {
		t.Fatal(written)
	}
	locked, _ := written["locked_fields"].([]any)
	if len(locked) != 1 || locked[0] != "date" {
		t.Fatalf("locked_fields = %v", written["locked_fields"])
	}
	tags := readTags(t, path)
	if strings.TrimSpace(firstTagValue(tags.Raw, "DATE")) != "" {
		t.Fatal("a locked date was written:", tags.Raw["DATE"])
	}
	if strings.TrimSpace(firstTagValue(tags.Raw, "ALBUM")) == "" {
		t.Fatal("the unlocked fields were not written")
	}
	// Clearing the guard lets the catalogue write the date again.
	if recorder := postCode(t, application, "/v1/library/guards", map[string]any{"dir": albumDir, "fields": []string{}}); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code)
	}
	again := post(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": catalog.server.URL, "overwrite": true,
	})
	if again["written"] != true {
		t.Fatal(again)
	}
	if value := firstTagValue(readTags(t, path).Raw, "DATE"); value != "2019-01-02" {
		t.Fatalf("date = %q", value)
	}
}

// The same lock applies to the batch path.
func TestFieldLockBlocksBatchScrape(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(application.Dir, "library-audio")
	albumDir := filepath.Join(library, "Some Artist", "First Album")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The batch path reads artist/album/title and the duration from the file,
	// so the fixture carries tags and the duration the catalogue expects.
	path := filepath.Join(albumDir, "01 opening.flac")
	if err := os.WriteFile(path, flacWithDuration(t, 240.25), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := flactags.Write(path, flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	}); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	post(t, application, "/v1/library/guards", map[string]any{"dir": albumDir, "fields": []string{"albumartist"}})
	catalog := newFakeMusicBrainz(t, catalogCompletionSearch, scrapeDetailJSON, http.StatusOK)
	code, decoded := scrapePost(t, application, map[string]any{
		"paths": []string{path}, "catalog_base_url": catalog.server.URL, "overwrite": true,
	})
	if code != http.StatusOK || decoded["written"] != float64(1) {
		t.Fatal(code, decoded)
	}
	files, _ := decoded["files"].([]any)
	if len(files) != 1 {
		t.Fatal(files)
	}
	row, _ := files[0].(map[string]any)
	locked, _ := row["locked_fields"].([]any)
	if len(locked) != 1 || locked[0] != "albumartist" {
		t.Fatalf("locked_fields = %v", row["locked_fields"])
	}
	tags := readTags(t, path)
	if value := firstTagValue(tags.Raw, "ALBUMARTIST"); value != "" {
		t.Fatal("a locked albumartist was written:", value)
	}
	if value := firstTagValue(tags.Raw, "GENRE"); value != "Ambient" {
		t.Fatalf("genre = %q, the unlocked fields were not written", value)
	}
}

// A suppressed album is hidden from the duplicate scan but the merge endpoint
// still finds it and answers with the reason, not a 404.
func TestSuppressedAlbumIsHiddenAndRefusesMerge(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a.wav"), 440)
	writeToneWAV(t, filepath.Join(incoming, "b.wav"), 440)
	first := releaseLibraryFile(t, application, "a.wav", "guard-1", "Artist", "Album", "Song A", 1)
	releaseLibraryFile(t, application, "b.wav", "guard-2", "Artist", "Album", "Song B", 1)
	albumDir := filepath.Join(first.LibraryDir, filepath.Dir(first.File))
	if groups := duplicateGroups(t, application); len(groups) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
	if recorder := postCode(t, application, "/v1/library/guards", map[string]any{"dir": albumDir, "suppressed": true}); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	scan := getAuth(t, application, "/v1/library/duplicates")
	if scan.Code != http.StatusOK || !strings.Contains(scan.Body.String(), `"count":0`) || !strings.Contains(scan.Body.String(), `"suppressed":1`) {
		t.Fatal(scan.Body.String())
	}
	groups, _, _, err := application.scanDuplicates(context.Background())
	if err != nil || len(groups) != 1 {
		t.Fatal(len(groups), err)
	}
	code, decoded := mediaPost(t, application, "/v1/library/duplicates/merge", map[string]any{"group_id": groups[0].GroupID})
	if code != http.StatusConflict || decoded["code"] != "MERGE_SUPPRESSED" {
		t.Fatal(code, decoded)
	}
}

// Merging fills empty fields from the duplicate, but never a locked one.
func TestMergeFillSkipsLockedFields(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	first := genTestFLAC(t, incoming, "guard-fill-a.flac")
	second := genTestFLAC(t, incoming, "guard-fill-b.flac")
	if err := flactags.Write(first, flactags.Tags{
		Title: "Song A", Artists: []string{"Artist"}, Album: "Album", AlbumArtist: "Artist", TrackNumber: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := flactags.Write(second, flactags.Tags{
		Title: "Song B", Artists: []string{"Artist"}, Album: "Album", AlbumArtist: "Artist", TrackNumber: 1,
		Genre: "Ambient", Date: "2019-01-02",
	}); err != nil {
		t.Fatal(err)
	}
	keep := releaseLibraryFile(t, application, "guard-fill-a.flac", "guard-fill-1", "Artist", "Album", "Song A", 1)
	other := releaseLibraryFile(t, application, "guard-fill-b.flac", "guard-fill-2", "Artist", "Album", "Song B", 1)
	albumDir := filepath.Join(keep.LibraryDir, filepath.Dir(keep.File))
	if recorder := postCode(t, application, "/v1/library/guards", map[string]any{"dir": albumDir, "fields": []string{"date"}}); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	group := &duplicateGroup{
		Keep: &duplicateFile{RequestID: keep.RequestID, File: keep.File, LibraryDir: keep.LibraryDir},
		Files: []duplicateFile{
			{RequestID: keep.RequestID, File: keep.File, LibraryDir: keep.LibraryDir},
			{RequestID: other.RequestID, File: other.File, LibraryDir: other.LibraryDir},
		},
	}
	filled, err := application.fillMissingTags(group)
	if err != nil {
		t.Fatal(err)
	}
	if len(filled) != 1 || filled[0] != "genre" {
		t.Fatalf("filled = %v", filled)
	}
	tags := readTags(t, filepath.Join(keep.LibraryDir, keep.File))
	if tags.Genre != "Ambient" || tags.Date != "" {
		t.Fatalf("genre=%q date=%q", tags.Genre, tags.Date)
	}
}

// The guard API refuses a directory outside the library and ignores unknown
// field names instead of storing them.
func TestGuardApiValidatesInput(t *testing.T) {
	application := newApp(t)
	if recorder := postCode(t, application, "/v1/library/guards", map[string]any{"dir": "../../etc", "fields": []string{"title"}}); recorder.Code != http.StatusBadRequest {
		t.Fatal("a path outside the library was accepted", recorder.Code)
	}
	library := filepath.Join(application.Dir, "library-audio")
	if err := os.MkdirAll(filepath.Join(library, "A", "B"), 0o755); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	recorder := postCode(t, application, "/v1/library/guards", map[string]any{
		"dir": filepath.Join(library, "A", "B"), "fields": []string{"title", "not-a-field", "TITLE"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	decoded := decodeBody(t, recorder)
	guard, _ := decoded["guard"].(map[string]any)
	fields, _ := guard["fields"].([]any)
	if len(fields) != 1 || fields[0] != "title" {
		t.Fatal(decoded)
	}
	if got := application.guardFor(filepath.Join(library, "A", "B")); len(got.Fields) != 1 || got.Fields[0] != "title" {
		t.Fatal(got)
	}
	// Windows path case must not create a second entry for the same album.
	post(t, application, "/v1/library/guards", map[string]any{"dir": strings.ToUpper(filepath.Join(library, "A", "B")), "suppressed": true})
	list := getAuth(t, application, "/v1/library/guards")
	if !strings.Contains(list.Body.String(), `"suppressed":true`) {
		t.Fatal(list.Body.String())
	}
	guards, err := application.loadGuards()
	if err != nil || len(guards.Albums) != 1 {
		t.Fatalf("%d entries, %v", len(guards.Albums), err)
	}
}
