// Catalog identify wires the offline releaseidentity.Catalogue to the real
// MusicBrainz API: search for candidate releases, fetch the best one's
// detail, turn it into an observation and let Catalogue.Resolve classify
// each requested track.
//
//   - confirmed here means only that the local track matched the
//     MusicBrainz release's tracklist offline (title, artists, duration
//     within 3s, complete tracklist). It is NOT a confirmed online release
//     identity, and a local tag grouping is never presented as one
//     (AGENTS.md). The response says so in plain text.
//   - MusicBrainz is the only live source. Discogs needs a user token
//     (Python CatalogResolver refuses Discogs queries without one) and
//     NetEase has no verified production catalog API (its Python provider
//     only carries the musicops.fixture.v1 fixture protocol), so both are
//     reported available:false with the reason, and no candidates are
//     invented for them.
//   - No setting is persisted: budget and TTL are fixed defaults below.
//     The budget is spent per release group inside Catalogue.Resolve, and
//     the TTL is how long a group's verdict stays cached in Pebble.
package app

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
	"github.com/wsdbb72/MusicOps/native/core/internal/releaseidentity"
)

// catalogBudget is the lookup budget handed to Catalogue.Resolve per
// identify call. One unit is spent per release group (album + artists +
// edition + recording version), so a normal single-album request spends 1
// and 8 leaves room for multi-version tracklists; anything beyond it is
// honestly reported as deferred, never confirmed.
const catalogBudget = 8

// catalogTTL mirrors the Python CatalogResolver cache_ttl default (900s):
// how long a group's Resolve verdict stays cached in Pebble before the
// next identify call looks it up again.
const catalogTTL = 15 * time.Minute

// catalogMaxCandidates caps how many search hits get a detail lookup, in
// ranked order. Each detail lookup costs one rate-limited MusicBrainz
// request (1 req/s), so this stays small.
const catalogMaxCandidates = 3

// catalogMaxTracks bounds one request; the body itself is already capped
// at 1 MiB by decode.
const catalogMaxTracks = 100

// catalogClient is the single shared MusicBrainz client for identify.
// The client's rate limiter is per instance (mu + lastCall, 1 req/s), so
// sharing one instance is what keeps sequential and concurrent identify
// calls inside the MusicBrainz policy; a fresh client per request would
// reset the limiter and burst. Tests replace this variable (and restore
// it) to point at a fake service; production code never replaces it and
// App's structure is untouched.
var catalogClient = musicbrainz.NewClient()

// errCatalogInvalidRequest marks a request that fails validation, as
// opposed to a MusicBrainz lookup failure (which is a 502).
var errCatalogInvalidRequest = errors.New("catalog: invalid identify request")

type catalogTrackRequest struct {
	Title    string   `json:"title"`
	Artists  []string `json:"artists"`
	Duration *float64 `json:"duration"`
}

type catalogIdentifyRequest struct {
	Artist string                `json:"artist"`
	Album  string                `json:"album"`
	Title  string                `json:"title"`
	MBID   string                `json:"mbid"`
	Tracks []catalogTrackRequest `json:"tracks"`
}

type catalogSourceStatus struct {
	Source    string `json:"source"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type catalogReleaseView struct {
	ReleaseID  string   `json:"release_id"`
	Title      string   `json:"title"`
	Date       string   `json:"date,omitempty"`
	Artists    []string `json:"artists"`
	TrackCount int      `json:"track_count"`
	Score      int      `json:"score"`
}

type catalogTrackResult struct {
	Title             string   `json:"title"`
	Artists           []string `json:"artists"`
	Status            string   `json:"status"`
	Reason            string   `json:"reason,omitempty"`
	FailureClass      string   `json:"failure_class,omitempty"`
	ReleaseID         string   `json:"release_id,omitempty"`
	IdentityConfirmed bool     `json:"identity_confirmed"`
	CacheHit          bool     `json:"cache_hit"`
	Reused            bool     `json:"reused"`
}

type catalogIdentifyResponse struct {
	OK                 bool                    `json:"ok"`
	Artist             string                  `json:"artist"`
	Album              string                  `json:"album"`
	MusicBrainz        *catalogReleaseView     `json:"musicbrainz"`
	LookupError        string                  `json:"lookup_error,omitempty"`
	Results            []catalogTrackResult    `json:"results"`
	Summary            releaseidentity.Summary `json:"summary"`
	Sources            []catalogSourceStatus   `json:"sources"`
	ProductionVerified bool                    `json:"production_verified"`
	Note               string                  `json:"note"`
}

func catalogSources() []catalogSourceStatus {
	return []catalogSourceStatus{
		{Source: "musicbrainz", Available: true},
		{Source: "discogs", Available: false, Reason: "需要用户 Discogs Token，当前未配置凭据；Python CatalogResolver 在无 Token 时拒绝 Discogs 查询（AUTH_REQUIRED），不返回候选。"},
		{Source: "netease", Available: false, Reason: "生产 API/权限契约待现场核实，Python NetEaseProvider 仅有 musicops.fixture.v1 夹具协议，无可用线上目录接口，不返回候选。"},
	}
}

// identifyCatalog is the POST /v1/catalog/identify handler. The route is
// registered by the parent (see the route line in the slice report); this
// method is complete on its own. A MusicBrainz search failure is a 502:
// the lookup did not happen, so there is nothing to classify. A failed
// detail lookup is not a 502: it becomes a failed observation and
// Catalogue.Resolve reports it as isolated_failure.
func (a *App) identifyCatalog(w http.ResponseWriter, r *http.Request) {
	var body catalogIdentifyRequest
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	resp, err := identifyCatalogLookup(catalogClient, a.engine, body, catalogBudget, catalogTTL)
	if err != nil {
		status, code := http.StatusBadGateway, "CATALOG_UNAVAILABLE"
		if errors.Is(err, errCatalogInvalidRequest) || errors.Is(err, musicbrainz.ErrInvalidMBID) || errors.Is(err, musicbrainz.ErrEmptyQuery) {
			status, code = http.StatusBadRequest, "INVALID_REQUEST"
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": code})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// identifyCatalogLookup runs one identify request against a MusicBrainz
// client and a Catalogue backed by engine. It is split out of the handler
// so tests can drive it with a small budget (to observe exhaustion) without
// going through HTTP.
func identifyCatalogLookup(client *musicbrainz.Client, engine *dataengine.Engine, req catalogIdentifyRequest, budget int, ttl time.Duration) (catalogIdentifyResponse, error) {
	artist := strings.TrimSpace(req.Artist)
	album := strings.TrimSpace(req.Album)
	if artist == "" || album == "" || len(req.Tracks) > catalogMaxTracks {
		return catalogIdentifyResponse{}, errCatalogInvalidRequest
	}
	for _, field := range []string{artist, album, req.Title} {
		if len(field) > 500 {
			return catalogIdentifyResponse{}, errCatalogInvalidRequest
		}
	}
	tracks, err := catalogTrackQueries(req, artist)
	if err != nil {
		return catalogIdentifyResponse{}, err
	}
	if len(tracks) == 0 {
		return catalogIdentifyResponse{}, errCatalogInvalidRequest
	}

	detail, fallback, searchTop, lookupErr, err := catalogLookup(client, req, artist, album)
	if err != nil {
		return catalogIdentifyResponse{}, err
	}
	observation := catalogObservation(detail, fallback, lookupErr)
	items := make([]releaseidentity.Item, len(tracks))
	for i, query := range tracks {
		// Query.Edition must be the release date: Observation.Edition
		// carries the MusicBrainz release date, and releaseidentity
		// treats any edition difference as a release conflict, so a
		// blank edition would conflict with every real release. The
		// date is taken from the looked-up release itself, never
		// invented; album, artists, track titles and durations do the
		// actual matching. A failed or missing lookup leaves the
		// edition blank and Resolve reports that honestly.
		if detail != nil {
			query.Edition = detail.Date
		} else if fallback != nil {
			query.Edition = fallback.Date
		}
		items[i] = releaseidentity.Item{Query: query, Observation: observation}
	}
	if req.MBID != "" {
		// Namespaced the same way Observation does ("mbid:<MBID>") so
		// a caller-supplied MBID that disagrees with the looked-up
		// release is a conflict, not a silent override.
		mbid := strings.ToLower(strings.TrimSpace(req.MBID))
		for i := range items {
			items[i].Query.ReleaseID = "mbid:" + mbid
		}
	}
	catalogue := releaseidentity.New(engine, budget, ttl)
	results, summary, err := catalogue.Resolve(time.Now(), items)
	if err != nil {
		return catalogIdentifyResponse{}, err
	}
	resp := catalogIdentifyResponse{
		OK: true, Artist: artist, Album: album,
		Results:            catalogTrackResults(tracks, results),
		Summary:            summary,
		Sources:            catalogSources(),
		ProductionVerified: false,
		Note:               "confirmed 仅表示本地曲目与 MusicBrainz 发行曲目离线匹配通过（曲名、艺人、时长在 3 秒内、曲目表完整），不是已确认的线上发行身份；本地标签分组不作线上身份。",
	}
	if lookupErr != nil {
		resp.LookupError = lookupErr.Error()
	}
	if detail != nil {
		resp.MusicBrainz = &catalogReleaseView{ReleaseID: "mbid:" + detail.MBID, Title: detail.Title, Date: detail.Date, Artists: detail.Artists, TrackCount: len(detail.Tracks)}
	} else if searchTop != nil {
		resp.MusicBrainz = &catalogReleaseView{ReleaseID: "mbid:" + searchTop.MBID, Title: searchTop.Title, Date: searchTop.Date, Artists: searchTop.Artists, TrackCount: searchTop.TrackCount, Score: searchTop.Score}
	}
	return resp, nil
}

// catalogTrackQueries builds one releaseidentity query per requested track.
// With no tracks, the top-level title (if any) becomes a single track;
// a track without a duration keeps HasDuration false, and
// releaseidentity then cannot confirm it (duration is part of the match),
// which Resolve reports as unresolved rather than as a match.
func catalogTrackQueries(req catalogIdentifyRequest, artist string) ([]releaseidentity.Query, error) {
	inputs := req.Tracks
	if len(inputs) == 0 {
		if strings.TrimSpace(req.Title) == "" {
			return nil, nil
		}
		inputs = []catalogTrackRequest{{Title: req.Title}}
	}
	queries := make([]releaseidentity.Query, 0, len(inputs))
	for _, track := range inputs {
		title := strings.TrimSpace(track.Title)
		if title == "" || len(title) > 500 {
			return nil, errCatalogInvalidRequest
		}
		artists := catalogCleanArtists(track.Artists)
		if len(artists) == 0 {
			artists = []string{artist}
		}
		query := releaseidentity.Query{Album: strings.TrimSpace(req.Album), Title: title, Artists: artists}
		if track.Duration != nil {
			if *track.Duration <= 0 || *track.Duration > 24*3600 {
				return nil, errCatalogInvalidRequest
			}
			query.Duration = *track.Duration
			query.HasDuration = true
		}
		queries = append(queries, query)
	}
	return queries, nil
}

func catalogCleanArtists(artists []string) []string {
	var out []string
	for _, artist := range artists {
		if trimmed := strings.TrimSpace(artist); trimmed != "" && len(trimmed) <= 200 {
			out = append(out, trimmed)
		}
	}
	return out
}

// catalogLookup fetches the MusicBrainz release to match against. An MBID
// skips the search and looks the release up directly. Otherwise it searches
// (SearchRelease builds the Lucene query itself), ranks the hits the way
// Python CatalogResolver._rank ranks releases (album match 60 + artists
// match 30, MusicBrainz score breaks ties), and fetches details in ranked
// order until one passes the pre-check (same album and artist set, complete
// tracklist). The first successful detail is kept as a fallback so a
// mismatching best hit still reaches Resolve, which then reports the real
// conflict/unresolved reason instead of hiding the lookup. When every
// detail fetch fails, lookupErr is returned alongside a nil detail and the
// caller turns it into a failed observation.
func catalogLookup(client *musicbrainz.Client, req catalogIdentifyRequest, artist, album string) (detail, fallback *musicbrainz.ReleaseDetail, searchTop *musicbrainz.Release, lookupErr error, err error) {
	if strings.TrimSpace(req.MBID) != "" {
		got, fetchErr := client.GetRelease(req.MBID)
		if fetchErr != nil {
			if errors.Is(fetchErr, musicbrainz.ErrInvalidMBID) {
				return nil, nil, nil, nil, fetchErr
			}
			return nil, nil, nil, fetchErr, nil
		}
		return &got, nil, nil, nil, nil
	}
	releases, searchErr := client.SearchRelease(artist, album, strings.TrimSpace(req.Title))
	if searchErr != nil {
		return nil, nil, nil, nil, searchErr
	}
	if len(releases) == 0 {
		return nil, nil, nil, nil, nil
	}
	top := releases[0]
	ranked := catalogRank(releases, artist, album)
	for i, candidate := range ranked {
		if i >= catalogMaxCandidates {
			break
		}
		got, fetchErr := client.GetRelease(candidate.MBID)
		if fetchErr != nil {
			lookupErr = fetchErr
			continue
		}
		if fallback == nil {
			copy := got
			fallback = &copy
		}
		if catalogDetailMatches(got, album, artist) {
			return &got, fallback, &top, nil, nil
		}
	}
	if fallback != nil {
		return fallback, fallback, &top, nil, nil
	}
	return nil, nil, &top, lookupErr, nil
}

// catalogObservation converts a looked-up detail into the observation
// releaseidentity expects, field for field. No Catalogue (label number)
// is filled: this lookup never fetches label data, so none is invented.
func catalogObservation(detail, fallback *musicbrainz.ReleaseDetail, lookupErr error) *releaseidentity.Observation {
	chosen := detail
	if chosen == nil {
		chosen = fallback
	}
	if chosen != nil {
		mapped := chosen.Observation()
		obs := &releaseidentity.Observation{
			Source: mapped.Source, Kind: mapped.Kind, ReleaseID: mapped.ReleaseID,
			Album: mapped.Album, Edition: mapped.Edition, Artists: mapped.Artists,
			TracklistComplete: mapped.TracklistComplete,
		}
		for _, track := range mapped.Tracks {
			obs.Tracks = append(obs.Tracks, releaseidentity.Track{
				Title: track.Title, Artists: track.Artists,
				Duration: track.DurationSec, HasDuration: track.HasDuration,
			})
		}
		return obs
	}
	if lookupErr != nil {
		return &releaseidentity.Observation{Failed: true, FailureClass: catalogFailureClass(lookupErr)}
	}
	return nil
}

// catalogFailureClass maps a MusicBrainz error onto the failure class
// Catalogue understands. Only rate_limited changes Catalogue behaviour
// (it halts the rest of the batch); the rest stay isolated failures.
func catalogFailureClass(err error) string {
	var rateErr *musicbrainz.RateLimitError
	if errors.As(err, &rateErr) {
		return "rate_limited"
	}
	var clientErr *musicbrainz.ClientError
	if errors.As(err, &clientErr) {
		if clientErr.StatusCode == http.StatusNotFound {
			return "not_found"
		}
		return "network"
	}
	return "network"
}

// catalogRank orders search hits by the Python release ranking (album
// match 60 + artist-set match 30); the MusicBrainz score, already
// descending from the service, breaks ties via a stable sort.
func catalogRank(releases []musicbrainz.Release, artist, album string) []musicbrainz.Release {
	type scored struct {
		release musicbrainz.Release
		points  int
	}
	ranked := make([]scored, len(releases))
	for i, release := range releases {
		points := 0
		if catalogFoldEqual(release.Title, album) {
			points += 60
		}
		if catalogArtistSetEqual(release.Artists, []string{artist}) {
			points += 30
		}
		ranked[i] = scored{release: release, points: points}
	}
	// Stable: equal points keep the service's score-descending order.
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].points > ranked[j].points
	})
	out := make([]musicbrainz.Release, len(ranked))
	for i, item := range ranked {
		out[i] = item.release
	}
	return out
}

// catalogDetailMatches is the pre-check before a detail is handed to
// Resolve: the release title must be the requested album, the artist set
// must be the requested artist, and the tracklist must be complete. It
// only selects a candidate; confirmation is still Catalogue.Resolve's job.
func catalogDetailMatches(detail musicbrainz.ReleaseDetail, album, artist string) bool {
	return catalogFoldEqual(detail.Title, album) &&
		catalogArtistSetEqual(detail.Artists, []string{artist}) &&
		detail.TracklistComplete
}

// catalogFoldEqual folds text for ranking and candidate selection only
// (fullwidth ASCII, case, whitespace). It is not identity matching:
// releaseidentity does the real matching with its own rules.
func catalogFoldEqual(left, right string) bool {
	return catalogFold(left) == catalogFold(right) && catalogFold(left) != ""
}

func catalogFold(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if r == 0x3000 {
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func catalogArtistSetEqual(left, right []string) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	folded := func(artists []string) map[string]bool {
		set := map[string]bool{}
		for _, artist := range artists {
			if folded := catalogFold(artist); folded != "" {
				set[folded] = true
			}
		}
		return set
	}
	a, b := folded(left), folded(right)
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

func catalogTrackResults(queries []releaseidentity.Query, results []releaseidentity.Result) []catalogTrackResult {
	out := make([]catalogTrackResult, len(results))
	for i, result := range results {
		out[i] = catalogTrackResult{
			Title: resultTitle(queries, i), Artists: resultArtists(queries, i),
			Status: result.Status, Reason: result.Reason, FailureClass: result.FailureClass,
			ReleaseID: result.ReleaseID, IdentityConfirmed: result.IdentityConfirmed,
			CacheHit: result.CacheHit, Reused: result.Reused,
		}
	}
	return out
}

func resultTitle(queries []releaseidentity.Query, i int) string {
	if i < len(queries) {
		return queries[i].Title
	}
	return ""
}

func resultArtists(queries []releaseidentity.Query, i int) []string {
	if i < len(queries) {
		return queries[i].Artists
	}
	return nil
}
