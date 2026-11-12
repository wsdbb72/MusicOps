package app

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

// catalogCompletionDetail is one release whose only track is 240.25s long. The
// old completion compared the file against a hardcoded 200s, so a real track
// could never match; these tests use a duration far enough from 200s that only
// the file's own STREAMINFO can match it (P1-15).
const catalogCompletionDetail = `{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02", "country": "JP", "status": "Official",
  "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
  "release-group": {"id": "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"},
  "genres": [{"name": "Ambient", "count": 4}, {"name": "Electronic", "count": 2}],
  "media": [{"position": 1, "track-count": 1, "tracks": [
    {"id": "t1", "title": "Opening", "position": 1, "length": 240250,
     "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
     "recording": {"id": "22222222-3333-4444-5555-666666666666", "title": "Opening", "length": 240250}}
  ]}]}`

const catalogCompletionSearch = `{"releases": [
  {"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02", "country": "JP", "status": "Official",
   "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
   "media": [{"track-count": 1}], "score": 100}
]}`

// fakeCompletionCatalog serves the search, the release detail and the
// release-group genre fallback. groupGenres is served only when set, so a test
// can prove the fallback path is what fills GENRE.
type fakeCompletionCatalog struct {
	server      *httptest.Server
	groupPath   atomic.Int32
	groupGenres string
}

func newFakeCompletionCatalog(t *testing.T, detail, groupGenres string) *fakeCompletionCatalog {
	t.Helper()
	fake := &fakeCompletionCatalog{groupGenres: groupGenres}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ws/2/release":
			_, _ = w.Write([]byte(catalogCompletionSearch))
		case strings.HasPrefix(r.URL.Path, "/ws/2/release-group/"):
			fake.groupPath.Add(1)
			if fake.groupGenres == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(fake.groupGenres))
		case strings.HasPrefix(r.URL.Path, "/ws/2/release/"):
			_, _ = w.Write([]byte(detail))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// flacWithDuration builds a FLAC whose STREAMINFO carries a real sample rate
// and total sample count, so flacDuration can read the true length.
func flacWithDuration(t *testing.T, seconds float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("fLaC")
	info := make([]byte, 34)
	binary.BigEndian.PutUint16(info[0:2], 4096)
	binary.BigEndian.PutUint16(info[2:4], 4096)
	const sampleRate = 44100
	total := uint64(seconds * sampleRate)
	packed := uint64(sampleRate)<<44 | uint64(1)<<41 | uint64(15)<<36 | total&((1<<36)-1)
	binary.BigEndian.PutUint64(info[10:18], packed)
	buf.Write([]byte{0x00, 0x00, 0x00, 34})
	buf.Write(info)
	// One empty VORBIS_COMMENT block, marked last.
	var comment bytes.Buffer
	comment.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	buf.Write([]byte{0x84, 0x00, 0x00, byte(comment.Len())})
	buf.Write(comment.Bytes())
	buf.WriteString("FAKEAUDIOBYTES")
	return buf.Bytes()
}

// writeTaggedFLAC places one completion fixture under the state directory and
// returns its API path plus the absolute path.
func writeTaggedFLAC(t *testing.T, application *App, name string, tags flactags.Tags) (string, string) {
	t.Helper()
	dir := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, flacWithDuration(t, 240.25), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := flactags.Write(full, tags); err != nil {
		t.Fatal(err)
	}
	return "incoming/" + name, full
}

func readTags(t *testing.T, path string) *flactags.Tags {
	t.Helper()
	tags, err := flactags.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

// A completion must write the whole catalogue answer, classification tag
// included. Before this change it wrote only ARTIST/ALBUM/TITLE from the
// request body and never GENRE, so "scraping" left the file without a
// classification (the tag problem reported after B0).
func TestCompleteMetadataWritesCatalogFieldsIncludingGenre(t *testing.T) {
	application := newApp(t)
	fake := newFakeCompletionCatalog(t, catalogCompletionDetail, "")
	apiPath, full := writeTaggedFLAC(t, application, "opening.flac", flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	code, decoded := mediaPost(t, application, "/v1/library/metadata", map[string]any{
		"path": apiPath, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": fake.server.URL,
	})
	if code != http.StatusOK || decoded["written"] != true {
		t.Fatal(code, decoded)
	}
	tags := readTags(t, full)
	if tags.Genre != "Ambient" {
		t.Fatal("GENRE was not written:", tags.Genre, decoded)
	}
	if tags.Date != "2019-01-02" || tags.AlbumArtist != "Some Artist" {
		t.Fatal(tags.Date, tags.AlbumArtist)
	}
	if tags.TrackNumber != 1 || firstTagValue(tags.Raw, "DISCNUMBER") != "" {
		t.Fatal("track/disc:", tags.TrackNumber, tags.Raw["DISCNUMBER"])
	}
	written, _ := decoded["written_fields"].([]any)
	if !containsString(written, "genre") || !containsString(written, "albumartist") || !containsString(written, "date") {
		t.Fatal(written)
	}
	catalog, _ := decoded["catalog"].(map[string]any)
	genres, _ := catalog["genres"].([]any)
	if len(genres) != 2 || genres[0] != "Ambient" {
		t.Fatal(catalog)
	}
	// The cover/lyrics services are not configured: the file must still be
	// written and reported as a completed tag write.
	if decoded["cover"] != false || decoded["lyrics"] != false {
		t.Fatal(decoded)
	}
}

// A release with no genre of its own is common on MusicBrainz; the release
// group usually carries the classification. The fallback must fill GENRE.
func TestCompleteMetadataFallsBackToReleaseGroupGenre(t *testing.T) {
	application := newApp(t)
	detail := `{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02",
	  "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
	  "release-group": {"id": "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"},
	  "media": [{"position": 1, "track-count": 1, "tracks": [
	    {"id": "t1", "title": "Opening", "position": 1, "length": 240250,
	     "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}]}]}]}`
	fake := newFakeCompletionCatalog(t, detail, `{"id": "bbbbbbbb-cccc-dddd-eeee-ffffffffffff", "genres": [{"name": "Jazz", "count": 2}]}`)
	apiPath, full := writeTaggedFLAC(t, application, "jazz.flac", flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
	})
	code, decoded := mediaPost(t, application, "/v1/library/metadata", map[string]any{
		"path": apiPath, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": fake.server.URL,
	})
	if code != http.StatusOK || decoded["written"] != true {
		t.Fatal(code, decoded)
	}
	if tags := readTags(t, full); tags.Genre != "Jazz" {
		t.Fatal("group genre was not written:", tags.Genre)
	}
	if fake.groupPath.Load() == 0 {
		t.Fatal("the release-group fallback was not requested")
	}
}

// A genre the user curated is a conflict, not a defect: it is reported and
// kept unless overwrite is set. Silent replacement would destroy curation.
func TestCompleteMetadataKeepsCuratedGenreUnlessOverwrite(t *testing.T) {
	application := newApp(t)
	fake := newFakeCompletionCatalog(t, catalogCompletionDetail, "")
	apiPath, full := writeTaggedFLAC(t, application, "curated.flac", flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", TrackNumber: 1,
		Genre: "Chinese Rock",
	})
	code, decoded := mediaPost(t, application, "/v1/library/metadata", map[string]any{
		"path": apiPath, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": fake.server.URL,
	})
	if code != http.StatusOK || decoded["written"] != true {
		t.Fatal(code, decoded)
	}
	if tags := readTags(t, full); tags.Genre != "Chinese Rock" {
		t.Fatal("curated genre was overwritten:", tags.Genre)
	}
	kept, _ := decoded["kept_fields"].([]any)
	found := false
	for _, item := range kept {
		entry, _ := item.(map[string]any)
		if entry["field"] == "genre" && entry["catalog"] == "Ambient" && entry["current"] == "Chinese Rock" {
			found = true
		}
	}
	if !found {
		t.Fatal("genre conflict was not reported:", kept)
	}
	if note, _ := decoded["note"].(string); !strings.Contains(note, "分类保留为「Chinese Rock」") || !strings.Contains(note, "Ambient") {
		t.Fatal(note)
	}

	// With overwrite the catalogue value wins.
	code, decoded = mediaPost(t, application, "/v1/library/metadata", map[string]any{
		"path": apiPath, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": fake.server.URL, "overwrite": true,
	})
	if code != http.StatusOK || decoded["written"] != true {
		t.Fatal(code, decoded)
	}
	if tags := readTags(t, full); tags.Genre != "Ambient" {
		t.Fatal("overwrite did not replace the genre:", tags.Genre)
	}
}

// A second completion finds every field already correct and must not rewrite
// the file.
func TestCompleteMetadataReportsNoWriteWhenTagsAlreadyMatch(t *testing.T) {
	application := newApp(t)
	fake := newFakeCompletionCatalog(t, catalogCompletionDetail, "")
	apiPath, full := writeTaggedFLAC(t, application, "already.flac", flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", AlbumArtist: "Some Artist",
		TrackNumber: 1, Date: "2019-01-02", Genre: "Ambient",
	})
	before, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	code, decoded := mediaPost(t, application, "/v1/library/metadata", map[string]any{
		"path": apiPath, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": fake.server.URL,
	})
	if code != http.StatusOK || decoded["written"] != false {
		t.Fatal(code, decoded)
	}
	after, err := os.ReadFile(full)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("an already-correct file was rewritten")
	}
}

func containsString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// catalogDetailJSON from the identify tests must stay decodable: the view now
// carries album_artist, genres and the tracklist.
func TestCatalogResponseCarriesGenreAndTracklist(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogCompletionDetail, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))
	status, decoded := callIdentify(t, application, map[string]any{
		"artist": "Some Artist", "album": "First Album", "title": "Opening",
	})
	if status != http.StatusOK {
		t.Fatal(status, decoded)
	}
	view, _ := decoded["musicbrainz"].(map[string]any)
	if view["album_artist"] != "Some Artist" {
		t.Fatal(view)
	}
	genres, _ := view["genres"].([]any)
	if len(genres) != 2 || genres[0] != "Ambient" {
		t.Fatal(view)
	}
	tracks, _ := view["tracks"].([]any)
	if len(tracks) != 1 {
		t.Fatal(view)
	}
	first, _ := tracks[0].(map[string]any)
	if first["disc"] != float64(1) || first["position"] != float64(1) || first["title"] != "Opening" {
		t.Fatal(first)
	}
	if _, err := json.Marshal(decoded); err != nil {
		t.Fatal(err)
	}
}

// A completion with nothing to fix in the tags but a cover to add must still
// write the cover. Skipping the whole write because the tag plan is empty would
// silently drop the artwork the user asked for.
func TestCompleteMetadataWritesCoverWhenTagsAlreadyMatch(t *testing.T) {
	application := newApp(t)
	fake := newFakeCompletionCatalog(t, catalogCompletionDetail, "")
	apiPath, full := writeTaggedFLAC(t, application, "cover-only.flac", flactags.Tags{
		Title: "Opening", Artists: []string{"Some Artist"}, Album: "First Album", AlbumArtist: "Some Artist",
		TrackNumber: 1, Date: "2019-01-02", Genre: "Ambient",
	})
	coverBody := append([]byte{0xff, 0xd8}, bytes.Repeat([]byte{0x22}, 96)...)
	cover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(coverBody)
	}))
	defer cover.Close()
	code, decoded := mediaPost(t, application, "/v1/library/metadata", map[string]any{
		"path": apiPath, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": fake.server.URL, "cover_base_url": cover.URL, "cover_mode": "embedded",
	})
	if code != http.StatusOK || decoded["written"] != true || decoded["cover"] != true {
		t.Fatal(code, decoded)
	}
	data, err := os.ReadFile(full)
	if err != nil || !bytes.Equal(flactags.FrontCover(data), coverBody) {
		t.Fatal("cover was not embedded", err)
	}
	if tags := readTags(t, full); tags.Genre != "Ambient" {
		t.Fatal("a cover-only write changed the tags:", tags.Genre)
	}
}
