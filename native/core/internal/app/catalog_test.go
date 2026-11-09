package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
)

// The route is registered by the parent (POST /v1/catalog/identify), so
// these tests call a.identifyCatalog directly with a real HTTP request and
// response recorder against a real App (app.Open + Pebble). That is the
// full handler path minus the mux line.

const catalogSearchJSON = `{"releases": [
  {"id": "11111111-2222-3333-4444-555555555555", "title": "Second Album", "date": "2020-05-01", "country": "US", "status": "Official",
   "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}, " & ", {"artist": {"name": "Other Artist"}, "name": "Other Artist"}],
   "media": [{"track-count": 10}], "score": 80},
  {"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02", "country": "JP", "status": "Official",
   "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
   "media": [{"track-count": 2}], "score": 100}
]}`

const catalogDetailJSON = `{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "title": "First Album", "date": "2019-01-02", "country": "JP", "status": "Official",
  "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
  "media": [{"position": 1, "track-count": 2, "tracks": [
    {"id": "t1", "title": "Opening", "position": 1, "length": 200000,
     "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
     "recording": {"id": "22222222-3333-4444-5555-666666666666", "title": "Opening", "length": 200000}},
    {"id": "t2", "title": "Closing", "position": 2, "length": 180500,
     "artist-credit": [{"artist": {"name": "Some Artist"}, "name": "Some Artist"}],
     "recording": {"id": "33333333-4444-5555-6666-777777777777", "title": "Closing", "length": 180500}}
  ]}]}`

// fakeMusicBrainz serves fixed search/detail payloads and counts calls per
// endpoint, so tests can assert the search-then-detail flow and the MBID
// direct path (which must not search at all).
type fakeMusicBrainz struct {
	server         *httptest.Server
	searches       atomic.Int32
	details        atomic.Int32
	searchResponse string
	detailResponse string
	detailStatus   int
}

func newFakeMusicBrainz(t *testing.T, searchResponse, detailResponse string, detailStatus int) *fakeMusicBrainz {
	t.Helper()
	fake := &fakeMusicBrainz{searchResponse: searchResponse, detailResponse: detailResponse, detailStatus: detailStatus}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws/2/release" {
			fake.searches.Add(1)
			_, _ = w.Write([]byte(fake.searchResponse))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ws/2/release/") {
			fake.details.Add(1)
			w.WriteHeader(fake.detailStatus)
			_, _ = w.Write([]byte(fake.detailResponse))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// catalogFakeClock advances 2s on every Now call, so the shared client's
// 1 req/s limiter never blocks in tests that only need the flow (the
// limiter itself is covered by TestIdentifyCatalogRateLimiterShared).
type catalogFakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *catalogFakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(2 * time.Second)
	return f.t
}

func newCatalogTestClient(t *testing.T, fake *fakeMusicBrainz, now func() time.Time) *musicbrainz.Client {
	t.Helper()
	client := musicbrainz.NewClient()
	client.BaseURL = fake.server.URL
	client.HTTPClient = fake.server.Client()
	if now != nil {
		client.Now = now
	}
	return client
}

func withCatalogClient(t *testing.T, client *musicbrainz.Client) {
	t.Helper()
	old := catalogClient
	catalogClient = client
	t.Cleanup(func() { catalogClient = old })
}

func callIdentify(t *testing.T, application *App, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/catalog/identify", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	application.identifyCatalog(recorder, request)
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(recorder.Code, recorder.Body.String(), err)
	}
	return recorder.Code, decoded
}

func catalogBody(tracks ...map[string]any) map[string]any {
	return map[string]any{
		"artist": "Some Artist", "album": "First Album", "tracks": tracks,
	}
}

func catalogTrack(title string, duration float64) map[string]any {
	return map[string]any{"title": title, "artists": []string{"Some Artist"}, "duration": duration}
}

func resultsOf(t *testing.T, decoded map[string]any) []any {
	t.Helper()
	results, ok := decoded["results"].([]any)
	if !ok {
		t.Fatal(decoded)
	}
	return results
}

func resultAt(t *testing.T, decoded map[string]any, i int) map[string]any {
	t.Helper()
	result, ok := resultsOf(t, decoded)[i].(map[string]any)
	if !ok {
		t.Fatal(decoded)
	}
	return result
}

func summaryOf(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	summary, ok := decoded["summary"].(map[string]any)
	if !ok {
		t.Fatal(decoded)
	}
	return summary
}

func sourceByName(t *testing.T, decoded map[string]any, name string) map[string]any {
	t.Helper()
	sources, ok := decoded["sources"].([]any)
	if !ok {
		t.Fatal(decoded)
	}
	for _, entry := range sources {
		source, ok := entry.(map[string]any)
		if ok && source["source"] == name {
			return source
		}
	}
	t.Fatalf("no source %q in %v", name, decoded)
	return nil
}

func TestIdentifyCatalogConfirmsTracksCachesAndReportsUnavailableSources(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))

	body := catalogBody(catalogTrack("Opening", 200), catalogTrack("Closing", 180.5))
	status, first := callIdentify(t, application, body)
	if status != http.StatusOK || first["ok"] != true {
		t.Fatal(status, first)
	}
	// Full flow: search ranks First Album above Second Album, detail is
	// fetched once, both tracks confirm offline against it.
	if got := resultAt(t, first, 0); got["status"] != "confirmed" || got["identity_confirmed"] != true || got["release_id"] != "mbid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" || got["cache_hit"] != false {
		t.Fatal(got)
	}
	if got := resultAt(t, first, 1); got["status"] != "confirmed" || got["reused"] != true {
		t.Fatal(got)
	}
	summary := summaryOf(t, first)
	if summary["confirmed"] != float64(2) || summary["budget_spent"] != float64(1) || summary["groups"] != float64(1) || summary["cache_hits"] != float64(0) {
		t.Fatal(summary)
	}
	release, ok := first["musicbrainz"].(map[string]any)
	if !ok || release["release_id"] != "mbid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" || release["title"] != "First Album" || release["track_count"] != float64(2) {
		t.Fatal(first["musicbrainz"])
	}
	if first["production_verified"] != false {
		t.Fatal(first)
	}
	if note, ok := first["note"].(string); !ok || !strings.Contains(note, "离线匹配通过") || !strings.Contains(note, "不是已确认的线上发行身份") {
		t.Fatal(first["note"])
	}
	// Discogs and NetEase are reported unavailable with a reason, and no
	// candidates are invented for them.
	for _, name := range []string{"discogs", "netease"} {
		source := sourceByName(t, first, name)
		if source["available"] != false {
			t.Fatal(source)
		}
		if reason, ok := source["reason"].(string); !ok || reason == "" {
			t.Fatal(source)
		}
	}
	if source := sourceByName(t, first, "musicbrainz"); source["available"] != true {
		t.Fatal(source)
	}
	if fake.searches.Load() != 1 || fake.details.Load() != 1 {
		t.Fatal(fake.searches.Load(), fake.details.Load())
	}

	// Second identical call: the group's verdict is cached in Pebble, so
	// both tracks are cache hits and no budget is spent.
	_, second := callIdentify(t, application, body)
	if got := resultAt(t, second, 0); got["cache_hit"] != true || got["status"] != "confirmed" {
		t.Fatal(got)
	}
	if summary := summaryOf(t, second); summary["budget_spent"] != float64(0) || summary["cache_hits"] != float64(2) {
		t.Fatal(summary)
	}
}

func TestIdentifyCatalogLiveHTTPServer(t *testing.T) {
	// Acceptance: a real App (app.Open + temp state dir) behind a real
	// HTTP server on a live socket, with the exact route line the parent
	// will register in http.go, against a fake MusicBrainz service. The
	// only difference from the wired route is that this mux has no auth
	// wrapper; the handler itself is the production one.
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/catalog/identify", application.identifyCatalog)
	server := httptest.NewServer(mux)
	defer server.Close()

	raw, err := json.Marshal(catalogBody(catalogTrack("Opening", 200), catalogTrack("Closing", 180.5)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(server.URL+"/v1/catalog/identify", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if got := resultAt(t, decoded, 0); got["status"] != "confirmed" || got["release_id"] != "mbid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatal(got)
	}
	if summary := summaryOf(t, decoded); summary["confirmed"] != float64(2) || summary["budget_spent"] != float64(1) {
		t.Fatal(summary)
	}
	if fake.searches.Load() != 1 || fake.details.Load() != 1 {
		t.Fatal(fake.searches.Load(), fake.details.Load())
	}
}

func TestIdentifyCatalogEmptySearchIsNotAnError(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, `{"releases": []}`, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))

	status, decoded := callIdentify(t, application, catalogBody(catalogTrack("Opening", 200)))
	if status != http.StatusOK {
		t.Fatal(status, decoded)
	}
	if got := resultAt(t, decoded, 0); got["status"] != "not_looked_up" || got["identity_confirmed"] != false {
		t.Fatal(got)
	}
	if decoded["musicbrainz"] != nil {
		t.Fatal(decoded["musicbrainz"])
	}
	if fake.details.Load() != 0 {
		t.Fatal("detail must not be fetched when the search found nothing")
	}
}

func TestIdentifyCatalogMBIDDirectLookupSkipsSearch(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))

	body := catalogBody(catalogTrack("Opening", 200), catalogTrack("Closing", 180.5))
	body["mbid"] = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	status, decoded := callIdentify(t, application, body)
	if status != http.StatusOK {
		t.Fatal(status, decoded)
	}
	if got := resultAt(t, decoded, 0); got["status"] != "confirmed" || got["release_id"] != "mbid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatal(got)
	}
	if fake.searches.Load() != 0 || fake.details.Load() != 1 {
		t.Fatal(fake.searches.Load(), fake.details.Load())
	}
}

func TestIdentifyCatalogTitleOnlyTrackWithoutDurationStaysUnresolved(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))

	// No tracks, only a top-level title and no duration: the release is
	// found, but releaseidentity cannot confirm a track without a
	// duration, so the result stays unresolved instead of confirming.
	body := map[string]any{"artist": "Some Artist", "album": "First Album", "title": "Opening"}
	status, decoded := callIdentify(t, application, body)
	if status != http.StatusOK {
		t.Fatal(status, decoded)
	}
	if got := resultAt(t, decoded, 0); got["status"] != "unresolved" || got["reason"] != "track_not_on_release" || got["identity_confirmed"] == true {
		t.Fatal(got)
	}
	if release, ok := decoded["musicbrainz"].(map[string]any); !ok || release["release_id"] != "mbid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatal(decoded["musicbrainz"])
	}
}

func TestIdentifyCatalogDetailFailureIsAnIsolatedFailure(t *testing.T) {
	application := newApp(t)
	// Search finds the release, but every detail lookup 404s: the tracks
	// become isolated failures (not_found), not a 502, and the lookup
	// error is reported.
	fake := newFakeMusicBrainz(t, catalogSearchJSON, `{"error": "gone"}`, http.StatusNotFound)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))

	status, decoded := callIdentify(t, application, catalogBody(catalogTrack("Opening", 200)))
	if status != http.StatusOK {
		t.Fatal(status, decoded)
	}
	if got := resultAt(t, decoded, 0); got["status"] != "isolated_failure" || got["failure_class"] != "not_found" {
		t.Fatal(got)
	}
	if lookupErr, ok := decoded["lookup_error"].(string); !ok || !strings.Contains(lookupErr, "404") {
		t.Fatal(decoded["lookup_error"])
	}
}

func TestIdentifyCatalogSearchFailureIs502(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, `{"releases": []}`, catalogDetailJSON, http.StatusOK)
	fake.server.Close()
	// Point the client at a dead server: the search fails at the transport
	// level (after the client's one retry) and the handler reports 502.
	dead := newCatalogTestClient(t, fake, nil)
	dead.BaseURL = "http://127.0.0.1:9"
	withCatalogClient(t, dead)

	status, decoded := callIdentify(t, application, catalogBody(catalogTrack("Opening", 200)))
	if status != http.StatusBadGateway || decoded["code"] != "CATALOG_UNAVAILABLE" {
		t.Fatal(status, decoded)
	}
}

func TestIdentifyCatalogInvalidInputsAre400(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))

	cases := map[string]map[string]any{
		"missing artist":  {"album": "First Album", "tracks": []map[string]any{catalogTrack("Opening", 200)}},
		"missing album":   {"artist": "Some Artist", "tracks": []map[string]any{catalogTrack("Opening", 200)}},
		"no tracks/title": {"artist": "Some Artist", "album": "First Album"},
		"empty track":     catalogBody(map[string]any{"title": "", "duration": 200}),
		"bad mbid": func() map[string]any {
			body := catalogBody(catalogTrack("Opening", 200))
			body["mbid"] = "not-an-mbid"
			return body
		}(),
		"zero duration": catalogBody(map[string]any{"title": "Opening", "duration": 0}),
		"unknown field": func() map[string]any {
			body := catalogBody(catalogTrack("Opening", 200))
			body["surprise"] = true
			return body
		}(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, decoded := callIdentify(t, application, body)
			if status != http.StatusBadRequest || decoded["code"] != "INVALID_REQUEST" {
				t.Fatal(status, decoded)
			}
		})
	}
}

func TestIdentifyCatalogBudgetExhaustion(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	client := newCatalogTestClient(t, fake, clock.Now)

	// "Opening" and "Opening (Live)" fall in different release groups
	// (the recording version is part of the group key), so a budget of 1
	// spends on the first group and defers the second instead of
	// confirming it.
	req := catalogIdentifyRequest{
		Artist: "Some Artist", Album: "First Album",
		Tracks: []catalogTrackRequest{
			{Title: "Opening", Artists: []string{"Some Artist"}, Duration: ptrFloat(200)},
			{Title: "Opening (Live)", Artists: []string{"Some Artist"}, Duration: ptrFloat(200)},
		},
	}
	resp, err := identifyCatalogLookup(client, application.engine, req, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Results[0].Status != "confirmed" || resp.Results[1].Status != "deferred" || resp.Summary.BudgetSpent != 1 {
		t.Fatal(resp.Results, resp.Summary)
	}

	// A budget of 0 defers everything and confirms nothing. A fresh App
	// is used so the first call's cached verdict cannot answer instead.
	fresh := newApp(t)
	resp, err = identifyCatalogLookup(client, fresh.engine, req, 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Results[0].Status != "deferred" || resp.Results[1].Status != "deferred" || resp.Summary.BudgetSpent != 0 || resp.Summary.Confirmed != 0 {
		t.Fatal(resp.Results, resp.Summary)
	}
}

func ptrFloat(v float64) *float64 { return &v }

func TestIdentifyCatalogRateLimiterSharedAcrossRequests(t *testing.T) {
	application := newApp(t)
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	// A fixed clock: the client's own limiter has to enforce the 1 req/s
	// spacing with real sleeps; the test itself never sleeps.
	fixed := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	client := newCatalogTestClient(t, fake, func() time.Time { return fixed })
	withCatalogClient(t, client)

	body := catalogBody(catalogTrack("Opening", 200))
	start := time.Now()
	for i := 0; i < 2; i++ {
		status, decoded := callIdentify(t, application, body)
		if status != http.StatusOK {
			t.Fatal(status, decoded)
		}
		if got := resultAt(t, decoded, 0); got["status"] != "confirmed" {
			t.Fatal(got)
		}
	}
	elapsed := time.Since(start)
	// Two identifies are 4 MusicBrainz calls through one shared client:
	// the first is free and the other three each wait out the 1s
	// interval, so the pair takes ~3s. A fresh client per request would
	// take ~1s (one wait each) instead.
	if elapsed < 2*time.Second {
		t.Fatalf("two identifies took %v, want at least 2s: the shared limiter did not throttle across requests", elapsed)
	}
	if fake.searches.Load() != 2 || fake.details.Load() != 2 {
		t.Fatal(fake.searches.Load(), fake.details.Load())
	}
}
