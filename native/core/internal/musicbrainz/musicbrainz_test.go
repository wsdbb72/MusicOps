package musicbrainz

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a controllable clock: Now returns the current fake time and
// Sleep advances it, so the rate limiter can be tested without waiting a
// real second.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
	f.slept += d
}

func (f *fakeClock) Slept() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.slept
}

func newTestClient(t *testing.T, srv *httptest.Server, clock *fakeClock) *Client {
	t.Helper()
	c := NewClient()
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()
	c.Now = clock.Now
	c.sleep = clock.Sleep
	return c
}

const searchJSON = `{"releases": [
  {"id": "11111111-2222-3333-4444-555555555555", "title": "Second Album", "date": "2020-05-01", "country": "US", "status": "Official",
   "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}, " & ", {"artist": {"name": "Other Artist"}, "name": "Other Artist"}],
   "media": [{"track-count": 10}, {"track-count": 2}], "score": 80},
  {"id": "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", "title": "First Album", "date": "2019-01-02", "country": "JP", "status": "Official",
   "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
   "media": [{"track-count": 12}], "score": 100},
  {"title": "No ID Album", "date": "2018-01-01", "score": 99}
]}`

const detailJSON = `{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02", "country": "JP", "status": "Official",
  "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
  "release-group": {"id": "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"},
  "genres": [{"name": "Ambient", "count": 3}, {"name": "Electronic", "count": 1}, {"name": "ambient", "count": 1}],
  "media": [{"position": 1, "track-count": 2, "tracks": [
    {"id": "t1", "title": "Opening", "position": 1, "length": 200000,
     "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
     "recording": {"id": "22222222-3333-4444-5555-666666666666", "title": "Opening", "length": 200000}},
    {"id": "t2", "title": "Closing", "position": 2, "length": 180500,
     "recording": {"id": "33333333-4444-5555-6666-777777777777", "title": "Closing", "length": 180500}}
  ]}]}`

const incompleteDetailJSON = `{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02", "country": "JP", "status": "Official",
  "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
  "media": [{"position": 1, "track-count": 2, "tracks": [
    {"id": "t1", "title": "Opening", "position": 1, "length": 200000,
     "recording": {"id": "22222222-3333-4444-5555-666666666666", "title": "Opening", "length": 200000}},
    {"id": "t2", "title": "Closing", "position": 2,
     "recording": {"id": "33333333-4444-5555-6666-777777777777", "title": "Closing"}}
  ]}]}`

func TestBuildQuery(t *testing.T) {
	cases := []struct {
		name                 string
		artist, album, title string
		want                 string
	}{
		{"album only", "", "Kind of Blue", "", `release:"Kind of Blue"`},
		{"all three", "Miles Davis", "Kind of Blue", "So What",
			`release:"Kind of Blue" AND recording:"So What" AND artist:"Miles Davis"`},
		{"artist only", "Miles Davis", "", "", `artist:"Miles Davis"`},
		{"escapes lucene specials", "", "A+B: C/D", "",
			`release:"A\+B\: C\/D"`},
		{"escapes operators", "", "Tom && Jerry || Spike", "",
			`release:"Tom \&& Jerry \|| Spike"`},
		{"escapes quotes and parens", "", `Say "Hi" (Live)`, "",
			`release:"Say \"Hi\" \(Live\)"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildQuery(tc.artist, tc.album, tc.title)
			if err != nil {
				t.Fatalf("buildQuery returned error: %v", err)
			}
			if got != tc.want {
				t.Errorf("buildQuery = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildQueryEmpty(t *testing.T) {
	if _, err := buildQuery("", "", ""); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("buildQuery empty = %v, want ErrEmptyQuery", err)
	}
	if _, err := buildQuery("  ", " ", ""); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("buildQuery whitespace-only = %v, want ErrEmptyQuery", err)
	}
}

func TestSearchReleaseMappingOrderingAndSkips(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		if r.URL.Query().Get("fmt") != "json" {
			t.Errorf("fmt = %q, want json", r.URL.Query().Get("fmt"))
		}
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Error("User-Agent header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchJSON))
	}))
	defer srv.Close()

	clock := newFakeClock()
	client := newTestClient(t, srv, clock)
	releases, err := client.SearchRelease("Some Artist", "First Album", "")
	if err != nil {
		t.Fatalf("SearchRelease: %v", err)
	}
	if !strings.Contains(gotQuery, `release:"First Album"`) || !strings.Contains(gotQuery, `artist:"Some Artist"`) {
		t.Errorf("sent query = %q, want release and artist clauses", gotQuery)
	}
	// The row missing its id is skipped; the rest sort by score desc.
	if len(releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(releases))
	}
	first, second := releases[0], releases[1]
	if first.Title != "First Album" || first.Score != 100 {
		t.Errorf("first = %+v, want First Album score 100", first)
	}
	if second.Title != "Second Album" || second.Score != 80 {
		t.Errorf("second = %+v, want Second Album score 80", second)
	}
	if first.MBID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("first MBID = %q, want lower-cased MBID", first.MBID)
	}
	if first.Date != "2019-01-02" || first.Country != "JP" || first.Status != "Official" {
		t.Errorf("first metadata = date %q country %q status %q", first.Date, first.Country, first.Status)
	}
	if len(first.Artists) != 1 || first.Artists[0] != "Some Artist" {
		t.Errorf("first artists = %v, want [Some Artist]", first.Artists)
	}
	if first.TrackCount != 12 {
		t.Errorf("first TrackCount = %d, want 12", first.TrackCount)
	}
	// Join phrases (" & ") are not artists; media track-counts sum.
	if len(second.Artists) != 2 || second.Artists[1] != "Other Artist" {
		t.Errorf("second artists = %v, want [Some Artist Other Artist]", second.Artists)
	}
	if second.TrackCount != 12 {
		t.Errorf("second TrackCount = %d, want 10+2=12", second.TrackCount)
	}
}

func TestSearchReleaseSkipsBadMBIDAndEmptyTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"releases": [
			{"id": "not-a-mbid", "title": "Bad ID", "score": 100},
			{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "", "score": 90},
			{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "Good", "score": 10}
		]}`))
	}))
	defer srv.Close()

	clock := newFakeClock()
	releases, err := newTestClient(t, srv, clock).SearchRelease("", "Good", "")
	if err != nil {
		t.Fatalf("SearchRelease: %v", err)
	}
	if len(releases) != 1 || releases[0].Title != "Good" {
		t.Fatalf("releases = %+v, want only the valid row", releases)
	}
}

func TestSearchReleaseEmptyResultIsNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"releases": []}`))
	}))
	defer srv.Close()

	clock := newFakeClock()
	releases, err := newTestClient(t, srv, clock).SearchRelease("", "Nothing Here", "")
	if err != nil {
		t.Fatalf("SearchRelease: %v", err)
	}
	if releases == nil || len(releases) != 0 {
		t.Errorf("releases = %v, want empty non-nil slice", releases)
	}
}

func TestGetReleaseMapping(t *testing.T) {
	var gotPath, gotInc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotInc = r.URL.Path, r.URL.Query().Get("inc")
		_, _ = w.Write([]byte(detailJSON))
	}))
	defer srv.Close()

	clock := newFakeClock()
	detail, err := newTestClient(t, srv, clock).GetRelease("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if gotPath != "/ws/2/release/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("path = %q, want lower-cased MBID path", gotPath)
	}
	if gotInc != "artists+recordings+genres" {
		t.Errorf("inc = %q, want artists+recordings+genres", gotInc)
	}
	if detail.Title != "First Album" || detail.Date != "2019-01-02" || detail.Country != "JP" {
		t.Errorf("detail = %+v", detail)
	}
	if !detail.TracklistComplete {
		t.Error("TracklistComplete = false, want true for a full tracklist")
	}
	if got := strings.Join(detail.Genres, ","); got != "Ambient,Electronic" {
		t.Errorf("Genres = %q, want the most-voted names without duplicates", got)
	}
	if detail.ReleaseGroupID != "bbbbbbbb-cccc-dddd-eeee-ffffffffffff" {
		t.Errorf("ReleaseGroupID = %q", detail.ReleaseGroupID)
	}
	if len(detail.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(detail.Tracks))
	}
	first, second := detail.Tracks[0], detail.Tracks[1]
	if first.Title != "Opening" || first.Position != 1 || first.Disc != 1 {
		t.Errorf("first track = %+v", first)
	}
	if first.LengthMs != 200000 || first.DurationSec != 200 || !first.HasDuration {
		t.Errorf("first duration = %dms %.1fs has=%v, want 200000ms 200s true",
			first.LengthMs, first.DurationSec, first.HasDuration)
	}
	if first.RecordingMBID != "22222222-3333-4444-5555-666666666666" {
		t.Errorf("first recording MBID = %q", first.RecordingMBID)
	}
	// Second track has no track-level artist-credit: it falls back to the
	// recording's credit, then to the release artists.
	if second.DurationSec != 180.5 || !second.HasDuration {
		t.Errorf("second duration = %.1fs has=%v, want 180.5s true", second.DurationSec, second.HasDuration)
	}
	if len(second.Artists) != 1 || second.Artists[0] != "Some Artist" {
		t.Errorf("second artists = %v, want release-artist fallback", second.Artists)
	}

	obs := detail.Observation()
	if obs.Source != "musicbrainz" || obs.Kind != "api" {
		t.Errorf("observation source/kind = %q/%q, want musicbrainz/api", obs.Source, obs.Kind)
	}
	if obs.ReleaseID != "mbid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("observation ReleaseID = %q", obs.ReleaseID)
	}
	if obs.Album != "First Album" || obs.Edition != "2019-01-02" || !obs.TracklistComplete {
		t.Errorf("observation = %+v", obs)
	}
	if len(obs.Tracks) != 2 || obs.Tracks[0].DurationSec != 200 || !obs.Tracks[0].HasDuration {
		t.Errorf("observation tracks = %+v", obs.Tracks)
	}
}

func TestGetReleaseIncompleteTracklist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(incompleteDetailJSON))
	}))
	defer srv.Close()

	clock := newFakeClock()
	detail, err := newTestClient(t, srv, clock).GetRelease("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if detail.TracklistComplete {
		t.Error("TracklistComplete = true, want false when a track lacks a length")
	}
	if len(detail.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2 (the incomplete track is kept, not dropped)", len(detail.Tracks))
	}
	missing := detail.Tracks[1]
	if missing.HasDuration || missing.DurationSec != 0 || missing.LengthMs != 0 {
		t.Errorf("track without length = %+v, want no duration", missing)
	}
	if detail.Observation().TracklistComplete {
		t.Error("Observation().TracklistComplete = true, want false")
	}
}

func TestRateLimitingWithFakeClock(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"releases": []}`))
	}))
	defer srv.Close()

	clock := newFakeClock()
	client := newTestClient(t, srv, clock)
	for i := 0; i < 2; i++ {
		if _, err := client.SearchRelease("", "Album", ""); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 2 {
		t.Fatalf("server saw %d calls, want 2", calls)
	}
	if got := clock.Slept(); got != time.Second {
		t.Errorf("limiter slept %v total, want exactly 1s (first call free, second waits out the interval)", got)
	}
}

func TestRetryAfter429ThenSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(searchJSON))
	}))
	defer srv.Close()

	clock := newFakeClock()
	releases, err := newTestClient(t, srv, clock).SearchRelease("", "First Album", "")
	if err != nil {
		t.Fatalf("SearchRelease: %v", err)
	}
	if calls != 2 {
		t.Errorf("server saw %d calls, want 2 (one retry after 429)", calls)
	}
	if len(releases) != 2 {
		t.Errorf("got %d releases after retry, want 2", len(releases))
	}
	if clock.Slept() < time.Second {
		t.Errorf("slept %v after 429 with Retry-After: 1, want at least 1s", clock.Slept())
	}
}

func TestRetry500ThenSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"releases": []}`))
	}))
	defer srv.Close()

	clock := newFakeClock()
	if _, err := newTestClient(t, srv, clock).SearchRelease("", "Album", ""); err != nil {
		t.Fatalf("SearchRelease: %v", err)
	}
	if calls != 2 {
		t.Errorf("server saw %d calls, want 2 (one retry after 500)", calls)
	}
}

func TestRetryExhaustedReturnsTypedError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	clock := newFakeClock()
	_, err := newTestClient(t, srv, clock).SearchRelease("", "Album", "")
	var serverErr *ServerError
	if !errors.As(err, &serverErr) || serverErr.StatusCode != http.StatusBadGateway {
		t.Errorf("err = %v, want *ServerError with status 502", err)
	}
	if calls != 2 {
		t.Errorf("server saw %d calls, want exactly 2 (one retry, then give up)", calls)
	}
}

func TestClientErrorNoRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	clock := newFakeClock()
	_, err := newTestClient(t, srv, clock).GetRelease("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	var clientErr *ClientError
	if !errors.As(err, &clientErr) || clientErr.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v, want *ClientError with status 404", err)
	}
	if calls != 1 {
		t.Errorf("server saw %d calls, want 1 (4xx is not retried)", calls)
	}
}

func TestMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	clock := newFakeClock()
	_, err := newTestClient(t, srv, clock).SearchRelease("", "Album", "")
	var parseErr *ParseError
	if !errors.As(err, &parseErr) {
		t.Errorf("err = %v, want *ParseError", err)
	}
}

func TestEmptyInputsRejectedWithoutRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"releases": []}`))
	}))
	defer srv.Close()

	clock := newFakeClock()
	client := newTestClient(t, srv, clock)
	if _, err := client.SearchRelease("", "", ""); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("SearchRelease empty = %v, want ErrEmptyQuery", err)
	}
	if _, err := client.SearchRelease("  ", "", ""); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("SearchRelease whitespace-only = %v, want ErrEmptyQuery", err)
	}
	if _, err := client.GetRelease("not-an-mbid"); !errors.Is(err, ErrInvalidMBID) {
		t.Errorf("GetRelease bad MBID = %v, want ErrInvalidMBID", err)
	}
	if _, err := client.GetRelease(""); !errors.Is(err, ErrInvalidMBID) {
		t.Errorf("GetRelease empty MBID = %v, want ErrInvalidMBID", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (invalid inputs never reach the network)", calls)
	}
}

func TestGetReleaseIDMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "bbbbbbbb-cccc-dddd-eeee-ffffffffffff", "title": "Wrong One", "media": []}`))
	}))
	defer srv.Close()

	clock := newFakeClock()
	_, err := newTestClient(t, srv, clock).GetRelease("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	var parseErr *ParseError
	if !errors.As(err, &parseErr) {
		t.Errorf("err = %v, want *ParseError for an id mismatch", err)
	}
}
