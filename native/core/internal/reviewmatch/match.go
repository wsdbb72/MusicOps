// Package reviewmatch compares one expected release with search hits.
// Normalization does not remove edition, compilation, or artist differences.
package reviewmatch

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hit is evidence already present on a candidate. Aliases must come from that record.
// GroupID, Format and Year are the release-identity evidence the record carries:
// the tracker group, the media format (for example FLAC), and the release year.
// A zero value means the record carries no such evidence. GroupID is site-local
// (the same album has different groups on different trackers) and Format is a
// per-torrent property, so neither is compared across sites; they are carried so
// the model cache key and later checks cover them. Year is a property of the
// release itself, so conflicting years veto a merge.
type Hit struct {
	ID      string
	Title   string
	Artists []string
	Aliases []string
	Edition string
	GroupID string
	Format  string
	Year    int
}

// Result is one comparison. Pass is false whenever the evidence is short or conflicting.
type Result struct {
	Pass   bool
	Reason string
}

var (
	editionPatterns = []struct {
		name string
		expr *regexp.Regexp
	}{
		{"live", regexp.MustCompile(`(?i)\blive\b|现场`)},
		{"remaster", regexp.MustCompile(`(?i)\bremaster(?:ed)?\b|重制`)},
		{"remix", regexp.MustCompile(`(?i)\bremix(?:ed)?\b|混音`)},
		{"deluxe", regexp.MustCompile(`(?i)\bdeluxe\b|豪华`)},
		{"instrumental", regexp.MustCompile(`(?i)\binstrumental\b`)},
		{"karaoke", regexp.MustCompile(`(?i)\bkaraoke\b`)},
		{"rerecord", regexp.MustCompile(`(?i)\bre[- ]?record(?:ed|ing)\b|重录`)},
	}
	compilation = regexp.MustCompile(`(?i)(?:\bcompilation\b|\bvarious artists\b|^va$)|合辑|精选|群星`)
)

// Compare decides whether the hit is the expected release.
func Compare(expectedTitle, expectedArtist string, hit Hit) Result {
	if strings.TrimSpace(expectedTitle) == "" || strings.TrimSpace(hit.Title) == "" {
		return Result{Reason: "missing_title"}
	}
	if compilation.MatchString(fold(hit.Title)+" "+fold(hit.Edition)+" "+fold(strings.Join(hit.Artists, " "))) || compilation.MatchString(fold(expectedTitle)+" "+fold(expectedArtist)) {
		return Result{Reason: "compilation"}
	}
	if editionKey(expectedTitle, "") != editionKey(hit.Title, hit.Edition) {
		return Result{Reason: "edition"}
	}
	titleOK := fold(hit.Title) == fold(expectedTitle)
	if !titleOK {
		for _, alias := range hit.Aliases {
			if fold(alias) == fold(expectedTitle) {
				titleOK = true
				break
			}
		}
	}
	if !titleOK {
		return Result{Reason: "title"}
	}
	if !artistsAgree(expectedArtist, hit.Artists) {
		return Result{Reason: "artist"}
	}
	if titleOK && fold(hit.Title) != fold(expectedTitle) {
		return Result{Pass: true, Reason: "alias"}
	}
	return Result{Pass: true, Reason: "title"}
}

// Same reports whether two hits that already matched the expectation are one release.
// Title, edition and artist must agree, and release-year evidence must not
// conflict: different release years veto the merge. GroupID is site-local and
// Format is a per-torrent property, so neither vetoes a cross-site merge; they
// are carried on the hit so the model cache key covers them.
func Same(left, right Hit) bool {
	if fold(left.Title) != fold(right.Title) || editionKey(left.Title, left.Edition) != editionKey(right.Title, right.Edition) {
		return false
	}
	if left.Year != 0 && right.Year != 0 && left.Year != right.Year {
		return false
	}
	return artistKey(left.Artists) == artistKey(right.Artists)
}

func artistsAgree(expected string, observed []string) bool {
	wanted := artistKey([]string{expected})
	// A caller who did not name an artist is not asking for an artist check.
	// A named artist with no artist on the hit is missing evidence, not a match.
	if wanted == "" {
		return true
	}
	if len(observed) == 0 {
		return false
	}
	for _, name := range observed {
		if fold(name) == wanted {
			return true
		}
	}
	return false
}

func editionKey(title, edition string) string {
	text := fold(title + " " + edition)
	var names []string
	for _, item := range editionPatterns {
		if item.expr.MatchString(text) {
			names = append(names, item.name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "+")
}

func artistKey(names []string) string {
	seen := map[string]bool{}
	var keys []string
	for _, name := range names {
		text := fold(name)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		keys = append(keys, text)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

func fold(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
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
