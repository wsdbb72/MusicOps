package reviewmatch

import (
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
)

// The set is hand-built. It is not a dump of production reviews, and a pass
// here is not a live-site success rate. Each row states the evidence the
// candidate record actually carries.
//
// The comparison baseline in this test is a simplified title-only comparator
// defined below (simplifiedTitleAccept). It is not the production legacy
// matcher, so the baseline numbers below compare the new rules against that
// simplified comparator only; they do not claim the old algorithm in
// production behaved the same way.
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
		{name: "remaster", title: "Album", hit: Hit{Title: "Album", Edition: "Remastered"}, want: true},
		{name: "chinese-live", title: "专辑", hit: Hit{Title: "专辑 现场"}, want: false},
		{name: "remix", title: "Album", hit: Hit{Title: "Album Remix"}, want: false},
		{name: "compilation-artist", title: "Example", artist: "Artist", hit: Hit{Title: "Example", Artists: []string{"群星"}}, want: false},
		{name: "compilation-title", title: "Example", hit: Hit{Title: "Example 合辑"}, want: false},
		{name: "other-artist", title: "Example", artist: "Artist", hit: Hit{Title: "Example", Artists: []string{"Tribute Band"}}, want: false},
		{name: "alias-on-live", title: "Blue", artist: "Artist", hit: Hit{Title: "蓝 Live", Artists: []string{"Artist"}, Aliases: []string{"Blue"}}, want: false},
		{name: "missing-title", title: "Example", hit: Hit{Title: " "}, want: false},
		{name: "deluxe", title: "Album", hit: Hit{Title: "Album Deluxe"}, want: true},
		{name: "rerecord", title: "Album", hit: Hit{Title: "Album", Edition: "重录"}, want: false},
	}
	baselinePass, newPass, baselineFalse, newFalse, wantPass := 0, 0, 0, 0, 0
	for _, item := range cases {
		old := simplifiedTitleAccept(item.title, item.hit.Title)
		next := Compare(item.title, item.artist, item.hit).Pass
		if item.want {
			wantPass++
			if old {
				baselinePass++
			}
			if next {
				newPass++
			}
		} else if old {
			baselineFalse++
		}
		if !item.want && next {
			newFalse++
		}
		if next != item.want {
			t.Fatalf("%s pass=%v reason=%s", item.name, next, Compare(item.title, item.artist, item.hit).Reason)
		}
	}
	reviewBaseline := len(cases) - baselinePass - baselineFalse
	reviewNew := len(cases) - newPass - newFalse
	t.Logf("synthetic labeled set n=%d source=hand-built limitations=no production dump and no live tracker; want_pass=%d baseline_correct_pass=%d new_correct_pass=%d baseline_false_pass=%d new_false_pass=%d baseline_review=%d new_review=%d",
		len(cases), wantPass, baselinePass, newPass, baselineFalse, newFalse, reviewBaseline, reviewNew)
	if newFalse != 0 || newPass <= baselinePass || reviewNew >= reviewBaseline {
		t.Fatalf("coverage or review ratio regressed: %+v", cases)
	}
	left := Hit{Title: "Example", Artists: []string{"Artist"}}
	right := Hit{Title: "Ｅｘａｍｐｌｅ", Artists: []string{"Artist"}}
	other := Hit{Title: "Example", Artists: []string{"Someone Else"}}
	if !Same(left, right) || Same(left, other) {
		t.Fatal(Same(left, right), Same(left, other))
	}
}

// simplifiedTitleAccept is the simplified title-only baseline used for
// comparison in this test. It is not the production legacy matcher.
func TestMergeVetoesConflictingReleaseEvidence(t *testing.T) {
	base := Hit{Title: "Example", Artists: []string{"Artist"}, GroupID: "g1", Format: "FLAC", Year: 2020}
	same := Hit{Title: "Example", Artists: []string{"Artist"}, GroupID: "g1", Format: "FLAC", Year: 2020}
	if !Same(base, same) {
		t.Fatal("identical evidence should merge")
	}
	otherYear := Hit{Title: "Example", Artists: []string{"Artist"}, GroupID: "g1", Format: "FLAC", Year: 2021}
	if Same(base, otherYear) {
		t.Fatal("different release year must veto the merge")
	}
	// GroupID is site-local and Format is a per-torrent property: the same album
	// on another tracker has another group, and another format does not make it
	// another release, so neither vetoes a cross-site merge.
	otherSite := Hit{Title: "Example", Artists: []string{"Artist"}, GroupID: "g2", Format: "MP3", Year: 2020}
	if !Same(base, otherSite) {
		t.Fatal("different site-local group or torrent format must not veto the merge")
	}
	// Evidence a side does not carry cannot veto: a website-parsed hit with no
	// group or year still merges on title, edition and artist.
	bare := Hit{Title: "Example", Artists: []string{"Artist"}}
	if !Same(base, bare) {
		t.Fatal("missing evidence must not veto the merge")
	}
}

func TestNamedArtistWithoutHitArtistsDoesNotPass(t *testing.T) {
	missing := Compare("Example", "Artist", Hit{Title: "Example"})
	if missing.Pass || missing.Reason != "artist" {
		t.Fatal(missing)
	}
	blank := Compare("Example", "Artist", Hit{Title: "Example", Artists: []string{" "}})
	if blank.Pass || blank.Reason != "artist" {
		t.Fatal(blank)
	}
	unnamed := Compare("Example", "", Hit{Title: "Example", Artists: []string{"Artist"}})
	if !unnamed.Pass {
		t.Fatal(unnamed)
	}
}

func TestOpenCDPrefixCoversFormatAndYearOnly(t *testing.T) {
	hit := Hit{Title: "Artist - Album [FLAC]", Artists: []string{}}
	got := CompareSource("open.cd", "Album", "Artist", hit)
	if !got.Pass || got.Reason != "open_cd_prefix" {
		t.Fatal(got)
	}
	if CompareSource("dicmusic", "Album", "Artist", hit).Pass {
		t.Fatal("prefix cover is only for open.cd")
	}
	live := Hit{Title: "Artist - Album Live"}
	if CompareSource("open.cd", "Album", "Artist", live).Pass {
		t.Fatal(live)
	}
	if Compare("Album", "", Hit{Title: "Album Deluxe Edition"}).Pass != true {
		t.Fatal("deluxe edition base should match the studio title")
	}
	if Compare("Album", "", Hit{Title: "Album Live"}).Pass {
		t.Fatal("live must stay a different recording")
	}
}

func simplifiedTitleAccept(expected, observed string) bool {
	expected = strings.TrimSpace(expected)
	observed = strings.TrimSpace(observed)
	if expected == "" || observed == "" || !strings.EqualFold(observed, expected) {
		return false
	}
	return !searchdecision.TitlesConflict(observed, expected)
}
