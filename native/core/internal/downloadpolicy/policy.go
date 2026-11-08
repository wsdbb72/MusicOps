// Package downloadpolicy classifies search, token and streaming outcomes and
// orders the offline fallback switches. It does not call a site, spend a token,
// download audio, or import a library file.
//
// The production Python streaming.order still starts with NetEase. The order
// below is the requested lossless-first policy for this core, and it is not
// wired to an executor.
package downloadpolicy

import (
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
)

const (
	ClassNoResult      = "no_result"
	ClassMatch         = "match"
	ClassAuth          = "auth"
	ClassRateLimited   = "rate_limited"
	ClassAuthorization = "authorization"
	ClassNetwork       = "network"
	ClassDownload      = "download"
	ClassImport        = "import"
	ClassFixture       = "fixture_observed"
	ClassTokenBudget   = "token_budget"
)

// Source is one switch. Tier 1 is a lossless subscription platform, tier 2 is
// a domestic lossless platform, and tier 3 is the playlist file downloader.
type Source struct {
	Name    string
	Tier    int
	Quality string
	Enabled bool
}

// Track describes whether the caller already has a source or identifying
// information. Neither field is inferred from a title.
type Track struct {
	SourceKnown      bool
	InfoKnown        bool
	PlaylistPlatform string
}

// Step is one planned attempt. It is not a completed download.
type Step struct {
	Name    string `json:"name"`
	Tier    int    `json:"tier"`
	Quality string `json:"quality"`
	Role    string `json:"role"`
}

// Outcome is one observed attempt. A fixture observation must be marked
// synthetic and still does not count as a download or import.
type Outcome struct {
	Source    string
	Class     string
	Synthetic bool
}

// Gate is the decision to open streaming. Search action comes from the tracker
// classifier; token and download classes come from their own ledgers.
type Gate struct {
	SearchAction      string
	Scene             string
	IdentityConfirmed bool
	StreamingEnabled  bool
	DownloadClass     string
	TokenRecorded     bool
}

// Walk is the running tally. Downloaded, Imported and ProductionVerified stay false.
type Walk struct {
	FailureClass       string   `json:"failure_class,omitempty"`
	Attempts           []string `json:"attempts,omitempty"`
	Next               []Step   `json:"next,omitempty"`
	FixtureObserved    bool     `json:"fixture_observed,omitempty"`
	Recoverable        bool     `json:"recoverable,omitempty"`
	ReviewRequired     bool     `json:"review_required,omitempty"`
	RetryAfterSec      int      `json:"retry_after_sec,omitempty"`
	Downloaded         bool     `json:"downloaded"`
	Imported           bool     `json:"imported"`
	ProductionVerified bool     `json:"production_verified"`
}

// DefaultSources is the readable switch list. Qobuz and Deezer stay off until
// a caller enables them, so the default path is Tidal, Apple Music, then QQ
// and NetEase.
func DefaultSources() []Source {
	return []Source{
		{Name: "tidal", Tier: 1, Quality: "lossless", Enabled: true},
		{Name: "applemusic", Tier: 1, Quality: "lossless", Enabled: true},
		{Name: "qobuz", Tier: 1, Quality: "lossless", Enabled: false},
		{Name: "deezer", Tier: 1, Quality: "lossless", Enabled: false},
		{Name: "qqmusic", Tier: 2, Quality: "lossless", Enabled: true},
		{Name: "netease", Tier: 2, Quality: "lossless", Enabled: true},
		{Name: "playlist", Tier: 3, Quality: "source-file", Enabled: true},
	}
}

var canonical = []string{"tidal", "applemusic", "qobuz", "deezer", "qqmusic", "netease", "playlist"}

// Plan returns enabled steps in canonical order, ignoring the caller's slice
// order. The playlist downloader is last, and only when both a resolved source
// and identifying information are absent.
func Plan(sources []Source, track Track) ([]Step, error) {
	if len(sources) != len(canonical) {
		return nil, dataengine.ErrInvalidRequest
	}
	byName := map[string]Source{}
	for _, source := range sources {
		if _, ok := byName[source.Name]; ok || !knownSource(source.Name) {
			return nil, dataengine.ErrInvalidRequest
		}
		byName[source.Name] = source
	}
	var steps []Step
	for _, name := range canonical {
		source, ok := byName[name]
		if !ok {
			return nil, dataengine.ErrInvalidRequest
		}
		if !source.Enabled {
			continue
		}
		if name == "playlist" {
			if track.SourceKnown || track.InfoKnown || source.Tier != 3 || source.Quality != "source-file" {
				if source.Tier != 3 || source.Quality != "source-file" {
					return nil, dataengine.ErrInvalidRequest
				}
				continue
			}
			stepName := "playlist"
			if track.PlaylistPlatform != "" {
				if !playlistPlatform(track.PlaylistPlatform) {
					return nil, dataengine.ErrInvalidRequest
				}
				stepName = "playlist:" + track.PlaylistPlatform
			}
			steps = append(steps, Step{Name: stepName, Tier: 3, Quality: source.Quality, Role: "playlist_file"})
			continue
		}
		if source.Tier != tierOf(name) || source.Quality != "lossless" {
			return nil, dataengine.ErrInvalidRequest
		}
		steps = append(steps, Step{Name: name, Tier: source.Tier, Quality: source.Quality, Role: roleOf(source.Tier)})
	}
	return steps, nil
}

// AllowStreaming reports whether fallback may be queued. It does not start it.
func AllowStreaming(gate Gate) bool {
	if !gate.IdentityConfirmed || !gate.StreamingEnabled || !fallbackScene(gate.Scene) {
		return false
	}
	switch gate.DownloadClass {
	case ClassMatch, ClassAuth, ClassRateLimited, ClassAuthorization, ClassNetwork, ClassDownload, ClassImport:
		return false
	case ClassTokenBudget:
		return gate.TokenRecorded
	case "", ClassNoResult:
		return gate.SearchAction == searchdecision.ActionFallback
	default:
		return false
	}
}

var streamRetrySec = [3]int{600, 1800, 3600}

// Apply walks outcomes in plan order. Import stops the walk. A synthetic
// observation stops it too, without becoming a download or an import. A plan
// that ends only in network or rate-limit failures is retried up to three
// times, then marked for review instead of being left without a next step.
func Apply(plan []Step, outcomes []Outcome, retryCount int) (Walk, error) {
	if len(outcomes) > len(plan) || retryCount < 0 || retryCount > 3 {
		return Walk{}, dataengine.ErrInvalidRequest
	}
	walk := Walk{Next: append([]Step(nil), plan...)}
	transient := true
	for i, outcome := range outcomes {
		if outcome.Source != plan[i].Name {
			return Walk{}, dataengine.ErrInvalidRequest
		}
		class, err := classify(outcome)
		if err != nil {
			return Walk{}, err
		}
		walk.Attempts = append(walk.Attempts, plan[i].Name+":"+class)
		walk.Next = append([]Step(nil), plan[i+1:]...)
		if class == ClassFixture {
			walk.FixtureObserved = true
			walk.FailureClass = ""
			walk.Next = nil
			return walk, nil
		}
		if class != ClassNetwork && class != ClassRateLimited {
			transient = false
		}
		walk.FailureClass = class
		if class == ClassImport {
			walk.ReviewRequired = true
			walk.Next = nil
			return walk, nil
		}
	}
	if len(outcomes) == len(plan) && len(plan) > 0 && transient {
		if retryCount < 3 {
			walk.Recoverable = true
			walk.RetryAfterSec = streamRetrySec[retryCount]
			walk.Next = append([]Step(nil), plan...)
		} else {
			walk.ReviewRequired = true
			walk.Next = nil
		}
	}
	return walk, nil
}

func classify(outcome Outcome) (string, error) {
	switch outcome.Class {
	case ClassNoResult, ClassMatch, ClassAuth, ClassRateLimited, ClassAuthorization, ClassNetwork, ClassDownload, ClassImport:
		return outcome.Class, nil
	case ClassFixture:
		if !outcome.Synthetic {
			return "", dataengine.ErrInvalidRequest
		}
		return ClassFixture, nil
	default:
		return "", dataengine.ErrInvalidRequest
	}
}

func knownSource(name string) bool {
	switch name {
	case "tidal", "applemusic", "qobuz", "deezer", "qqmusic", "netease", "playlist":
		return true
	default:
		return false
	}
}

func tierOf(name string) int {
	switch name {
	case "tidal", "applemusic", "qobuz", "deezer":
		return 1
	case "qqmusic", "netease":
		return 2
	default:
		return 0
	}
}

func roleOf(tier int) string {
	if tier == 1 {
		return "subscription_lossless"
	}
	return "domestic_lossless"
}

func playlistPlatform(name string) bool {
	switch name {
	case "netease", "qqmusic", "qishui", "kugou", "kuwo", "migu", "bilibili", "applemusic", "spotify", "youtube":
		return true
	default:
		return false
	}
}

func fallbackScene(scene string) bool { return scene == "playlist" || scene == "fill" }
