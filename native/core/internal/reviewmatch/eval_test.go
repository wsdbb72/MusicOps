package reviewmatch

import (
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
)

// The set is hand-built. It is not a dump of production reviews, and a pass
// here is not a live-site success rate. Each row states the evidence the
// candidate record actually carries.
func TestLabeledComparisonKeepsConflictsAndAcceptsNormalizedEvidence(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		artist string
		hit    Hit
		want   bool
	}{
		{name: "exact", title: "Example & Test", hit: Hit{Title: "Example & Test", Artists: []string{"Artist"}}, want: true},
		{name: "ascii-case", title: "Example & Test", hit: Hit{Title: "example & test", Artists: []string{"Artist"}}, want: true},
		{name: "fullwidth", title: "Example & Test", hit: Hit{Title: "Ｅｘａｍｐｌｅ　＆　Ｔｅｓｔ", Artists: []string{"Artist"}}, want: true},
		{name: "spacing", title: "Example & Test", hit: Hit{Title: "Example   &  Test", Artists: []string{"Artist"}}, want: true},
		{name: "recorded-alias", title: "Blue", artist: "Artist", hit: Hit{Title: "蓝", Artists: []string{"Artist"}, Aliases: []string{"Blue"}}, want: true},
		{name: "same-live", title: "Album Live", hit: Hit{Title: "Album Live"}, want: true},
		{name: "spaced-live", title: "Album Live", hit: Hit{Title: "Album  Live"}, want: true},
		{name: "fullwidth-live", title: "Album Live", hit: Hit{Title: "Ａｌｂｕｍ　Ｌｉｖｅ"}, want: true},
		{name: "live-studio", title: "Album", hit: Hit{Title: "Album Live"}, want: false},
		{name: "studio-live-query", title: "Album Live", hit: Hit{Title: "Album"}, want: false},
		{name: "remaster", title: "Album", hit: Hit{Title: "Album", Edition: "Remastered"}, want: false},
		{name: "chinese-live", title: "专辑", hit: Hit{Title: "专辑 现场"}, want: false},
		{name: "remix", title: "Album", hit: Hit{Title: "Album Remix"}, want: false},
		{name: "compilation-artist", title: "Example", artist: "Artist", hit: Hit{Title: "Example", Artists: []string{"群星"}}, want: false},
		{name: "compilation-title", title: "Example", hit: Hit{Title: "Example 合辑"}, want: false},
		{name: "other-artist", title: "Example", artist: "Artist", hit: Hit{Title: "Example", Artists: []string{"Tribute Band"}}, want: false},
		{name: "alias-on-live", title: "Blue", artist: "Artist", hit: Hit{Title: "蓝 Live", Artists: []string{"Artist"}, Aliases: []string{"Blue"}}, want: false},
		{name: "missing-title", title: "Example", hit: Hit{Title: " "}, want: false},
		{name: "deluxe", title: "Album", hit: Hit{Title: "Album Deluxe"}, want: false},
		{name: "rerecord", title: "Album", hit: Hit{Title: "Album", Edition: "重录"}, want: false},
	}
	legacyPass, newPass, legacyFalse, newFalse, wantPass := 0, 0, 0, 0, 0
	for _, item := range cases {
		old := legacyAccept(item.title, item.hit.Title)
		next := Compare(item.title, item.artist, item.hit).Pass
		if item.want {
			wantPass++
			if old {
				legacyPass++
			}
			if next {
				newPass++
			}
		} else if old {
			legacyFalse++
		}
		if !item.want && next {
			newFalse++
		}
		if next != item.want {
			t.Fatalf("%s pass=%v reason=%s", item.name, next, Compare(item.title, item.artist, item.hit).Reason)
		}
	}
	reviewLegacy := len(cases) - legacyPass - legacyFalse
	reviewNew := len(cases) - newPass - newFalse
	t.Logf("synthetic labeled set n=%d source=hand-built limitations=no production dump and no live tracker; want_pass=%d legacy_correct_pass=%d new_correct_pass=%d legacy_false_pass=%d new_false_pass=%d legacy_review=%d new_review=%d",
		len(cases), wantPass, legacyPass, newPass, legacyFalse, newFalse, reviewLegacy, reviewNew)
	if newFalse != 0 || newPass <= legacyPass || reviewNew >= reviewLegacy {
		t.Fatalf("coverage or review ratio regressed: %+v", cases)
	}
	left := Hit{Title: "Example", Artists: []string{"Artist"}}
	right := Hit{Title: "Ｅｘａｍｐｌｅ", Artists: []string{"Artist"}}
	other := Hit{Title: "Example", Artists: []string{"Someone Else"}}
	if !Same(left, right) || Same(left, other) {
		t.Fatal(Same(left, right), Same(left, other))
	}
}

func legacyAccept(expected, observed string) bool {
	expected = strings.TrimSpace(expected)
	observed = strings.TrimSpace(observed)
	if expected == "" || observed == "" || !strings.EqualFold(observed, expected) {
		return false
	}
	return !searchdecision.TitlesConflict(observed, expected)
}
