// Package releaseidentity matches release, edition and track identity offline.
// It does not scrape, search, download or invent a release id.
package releaseidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const durationWindow = 3.0

var versionMarkers = regexp.MustCompile(`(?i)\blive\b|\bremix(?:ed)?\b|\binstrumental\b|\bacoustic\b|\bremaster(?:ed)?\b|\bdeluxe\b|现场|重录|重制|重製|伴奏`)

// Query is one track the caller already has. An empty release id means the
// release is still unknown.
type Query struct {
	Album       string
	Edition     string
	Catalogue   string
	Title       string
	Artists     []string
	Duration    float64
	ReleaseID   string
	HasDuration bool
}

// Track is one row inside an observation. It is not a library file.
type Track struct {
	Title       string
	Artists     []string
	Duration    float64
	HasDuration bool
}

// Observation is caller-supplied evidence. FailureClass keeps a failed lookup
// distinct from an empty result.
type Observation struct {
	Source            string
	Kind              string
	ReleaseID         string
	Album             string
	Edition           string
	Catalogue         string
	Artists           []string
	Tracks            []Track
	TracklistComplete bool
	Failed            bool
	FailureClass      string
}

// Result is a classification. ProductionVerified stays false: this package
// never sees a live site or a library file.
type Result struct {
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	FailureClass       string `json:"failure_class,omitempty"`
	ReleaseID          string `json:"release_id,omitempty"`
	IdentityConfirmed  bool   `json:"identity_confirmed,omitempty"`
	ProductionVerified bool   `json:"production_verified,omitempty"`
	Reused             bool   `json:"reused,omitempty"`
	CacheHit           bool   `json:"cache_hit,omitempty"`
}

// strictText folds fullwidth ASCII and ASCII case, then collapses whitespace.
// It is not Unicode NFKC or full casefold, and it does not claim equivalence
// with Python identity-1.
func strictText(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		if r >= 0xFF01 && r <= 0xFF5E {
			r = r - 0xFEE0
		}
		if r == 0x3000 {
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func artistKey(artists []string) string {
	seen := map[string]bool{}
	var keys []string
	for _, artist := range artists {
		text := strictText(artist)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		keys = append(keys, text)
	}
	sortStrings(keys)
	return strings.Join(keys, "\n")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		item := values[i]
		j := i
		for j > 0 && values[j-1] > item {
			values[j] = values[j-1]
			j--
		}
		values[j] = item
	}
}

func unknownArtist(artists []string) bool {
	if artistKey(artists) == "" {
		return true
	}
	for _, artist := range artists {
		switch strictText(artist) {
		case "unknown", "unknown artist", "未知", "未知艺人":
			return true
		}
	}
	return false
}

func recordingVersion(title, edition string) string {
	return strings.TrimSpace(versionMarkers.FindString(strictText(title) + " " + strictText(edition)))
}

func durationClose(left, right QueryDuration) bool {
	if !left.known || !right.known || left.value <= 0 || right.value <= 0 {
		return false
	}
	delta := left.value - right.value
	if delta < 0 {
		delta = -delta
	}
	return delta <= durationWindow
}

type QueryDuration struct {
	value float64
	known bool
}

func groupKey(query Query) (string, bool) {
	album := strictText(query.Album)
	artists := artistKey(query.Artists)
	if album == "" || artists == "" || unknownArtist(query.Artists) {
		return "", false
	}
	raw := strings.Join([]string{album, artists, strictText(query.Edition), strictText(query.Catalogue), recordingVersion(query.Title, query.Edition)}, "\x1f")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), true
}

func classify(query Query, observation *Observation) Result {
	base := observationLevel(query, observation)
	if base.Status != "ready" {
		return base
	}
	if observation == nil || !trackMatches(query, *observation) {
		return Result{Status: "unresolved", Reason: "track_not_on_release"}
	}
	return Result{Status: "confirmed", Reason: "track_release_agreed", ReleaseID: base.ReleaseID, IdentityConfirmed: true}
}

func observationLevel(query Query, observation *Observation) Result {
	if observation == nil {
		return Result{Status: "not_looked_up", Reason: "no_observation"}
	}
	if observation.Failed {
		class := observation.FailureClass
		if class == "" {
			class = "isolated_failure"
		}
		return Result{Status: "isolated_failure", Reason: "lookup_failed", FailureClass: class}
	}
	if observation.Kind == "ai" || observation.Kind == "synthetic_fixture" {
		return Result{Status: "unresolved", Reason: "evidence_cannot_confirm"}
	}
	if observation.Kind != "api" && observation.Kind != "website" && observation.Kind != "catalog" {
		return Result{Status: "unresolved", Reason: "unknown_evidence_kind"}
	}
	if strings.TrimSpace(observation.ReleaseID) == "" {
		return Result{Status: "unresolved", Reason: "unknown_release"}
	}
	if !observation.TracklistComplete {
		return Result{Status: "unresolved", Reason: "incomplete_tracklist"}
	}
	if unknownArtist(query.Artists) || unknownArtist(observation.Artists) {
		return Result{Status: "unresolved", Reason: "unknown_artist"}
	}
	if releaseConflict(query, *observation) || recordingVersion(query.Title, query.Edition) != recordingVersion(observation.Album, observation.Edition) {
		return Result{Status: "conflict", Reason: "edition_or_release_conflict"}
	}
	return Result{Status: "ready", ReleaseID: strings.TrimSpace(observation.ReleaseID)}
}

func releaseConflict(query Query, observation Observation) bool {
	if bothDiffer(query.ReleaseID, observation.ReleaseID) || bothDiffer(query.Album, observation.Album) || bothDiffer(query.Catalogue, observation.Catalogue) {
		return true
	}
	return strictText(query.Edition) != strictText(observation.Edition)
}

func bothDiffer(left, right string) bool {
	left, right = strictText(left), strictText(right)
	return left != "" && right != "" && left != right
}

func trackMatches(query Query, observation Observation) bool {
	title := strictText(query.Title)
	artists := artistKey(query.Artists)
	if title == "" || artists == "" {
		return false
	}
	for _, track := range observation.Tracks {
		if strictText(track.Title) != title || artistKey(track.Artists) != artists {
			continue
		}
		if !durationClose(QueryDuration{query.Duration, query.HasDuration}, QueryDuration{track.Duration, track.HasDuration}) {
			continue
		}
		return true
	}
	return false
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func formatCount(value int) string { return strconv.Itoa(value) }
