// Package textnorm normalizes catalogue text before matching. It removes
// display-only Unicode differences while preserving words, editions, numbers,
// scripts and meaningful ASCII punctuation.
package textnorm

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Normalize converts compatibility forms (full-width ASCII, half-width kana,
// ligatures and compatibility ideographs), composes decomposed accents and
// canonicalizes typographic punctuation. It preserves letter case.
func Normalize(value string) string {
	value = norm.NFKC.String(value)
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Cc, r), r == '\u00ad':
			continue
		case unicode.IsSpace(r):
			b.WriteByte(' ')
			continue
		}
		switch r {
		case '\u2018', '\u2019', '\u201a', '\u201b', '\u2032', '\u02bc':
			r = '\''
		case '\u201c', '\u201d', '\u201e', '\u201f', '\u2033', '\u00ab', '\u00bb':
			r = '"'
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			r = '-'
		case '\u2026':
			b.WriteString("...")
			continue
		case '\u3002':
			r = '.'
		case '\u3001':
			r = ','
		case '\u301c':
			r = '~'
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Fold is Normalize plus full Unicode case folding. Use it for identity and
// artist comparisons, never as the display value.
func Fold(value string) string {
	return cases.Fold().String(Normalize(value))
}
