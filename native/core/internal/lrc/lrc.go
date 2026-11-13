// Package lrc parses and formats LRC lyrics: plain-text timed lyrics with
// [mm:ss.xx] timestamps, [offset:+/-ms] and [ti:]/[ar:]/[al:]/[by:]
// metadata tags, plus "<audio>.lrc" sidecar files.
//
// The package is pure text parsing: it never reads audio and never embeds
// lyrics into an audio file. Embedding is the caller's job (for FLAC, wire
// the lyrics through flactags as the LYRICS Vorbis comment).
//
// Parsing rules:
//   - Timestamps are [mm:ss.xx] (centiseconds) or [mm:ss.xxx]
//     (milliseconds); a one-digit fraction and a bare [mm:ss] are also
//     accepted. Several timestamps may prefix one line; each produces a
//     Line with the same text.
//   - [offset:+/-ms] records OffsetMS. Times in Lines are the raw times as
//     written; the offset is NOT applied to them, so a parse -> format ->
//     parse round-trip is exact. Use ShiftedLines for offset-applied times.
//   - [key:value] tags other than offset are metadata (key lower-cased);
//     ti, ar, al and by are the conventional ones, but any key is kept.
//   - A non-empty line that is not a timestamped line, a metadata tag or
//     an offset tag is malformed: it is reported in Malformed, not
//     silently dropped. A bracket that looks like a timestamp or tag but
//     is invalid is likewise reported, and any valid timestamps on the
//     same line are still kept.
//   - Input with zero valid timed lines is rejected.
package lrc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Line is one timed lyric line.
type Line struct {
	TimeMS int64  // milliseconds from the start of the track
	Text   string // lyric text (may be empty for an instrumental marker)
}

// MalformedLine is a line (or bracket) that could not be interpreted,
// reported so callers can see what was ignored and why.
type MalformedLine struct {
	Text   string // the raw line as it appeared in the input
	Reason string // what was wrong with it
}

// Lyrics is a parsed LRC document.
type Lyrics struct {
	// Lines is ordered by TimeMS ascending (stable for equal times).
	Lines []Line
	// Meta holds [key:value] tags with lower-cased keys (ti, ar, al, by, ...).
	Meta map[string]string
	// OffsetMS is the [offset:+/-ms] value in milliseconds (0 when absent).
	// It is recorded, not applied to Lines; see ShiftedLines.
	OffsetMS int64
	// Malformed lists every ignored line or bracket and why.
	Malformed []MalformedLine
}

// timestampRe matches the inside of a timestamp bracket: minutes,
// seconds (00-59) and an optional 1-3 digit fraction.
var timestampRe = regexp.MustCompile(`^(\d+):(\d{1,2})(?:\.(\d{1,3}))?$`)

// tagRe matches the inside of a metadata bracket: key:value.
var tagRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):(.*)$`)

// offsetRe matches the inside of an offset bracket.
var offsetRe = regexp.MustCompile(`^(?i:offset):\s*([+-]?\d+)\s*$`)

// Parse parses LRC text. It returns an error when the input contains no
// valid timed line.
func Parse(text string) (*Lyrics, error) {
	return parse(text)
}

// ParseBytes parses LRC text from bytes.
func ParseBytes(b []byte) (*Lyrics, error) {
	return parse(string(b))
}

func parse(text string) (*Lyrics, error) {
	lyr := &Lyrics{Meta: map[string]string{}}
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue // blank lines are skipped, not malformed
		}
		parseLine(lyr, raw, trimmed)
	}
	if len(lyr.Lines) == 0 {
		return nil, fmt.Errorf("lrc: no valid timed lines (ignored %d malformed line(s))", len(lyr.Malformed))
	}
	sort.SliceStable(lyr.Lines, func(i, j int) bool { return lyr.Lines[i].TimeMS < lyr.Lines[j].TimeMS })
	return lyr, nil
}

// parseLine processes one non-blank line, appending timed lines, metadata
// or malformed reports to lyr.
func parseLine(lyr *Lyrics, raw, trimmed string) {
	rest := trimmed
	var times []int64
	sawTag := false
	sawMalformed := false
	for strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			lyr.Malformed = append(lyr.Malformed, MalformedLine{Text: raw, Reason: "unterminated '[' bracket"})
			return
		}
		inside := rest[1:end]
		rest = strings.TrimSpace(rest[end+1:])
		if ms, ok, malformed := parseTimestamp(inside); malformed != "" {
			lyr.Malformed = append(lyr.Malformed, MalformedLine{Text: raw, Reason: malformed})
			sawMalformed = true
			continue
		} else if ok {
			times = append(times, ms)
			continue
		}
		if m := offsetRe.FindStringSubmatch(inside); m != nil {
			v, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				lyr.Malformed = append(lyr.Malformed, MalformedLine{Text: raw, Reason: fmt.Sprintf("invalid offset %q", inside)})
				sawMalformed = true
				continue
			}
			lyr.OffsetMS = v
			sawTag = true
			continue
		}
		if m := tagRe.FindStringSubmatch(inside); m != nil {
			lyr.Meta[strings.ToLower(strings.TrimSpace(m[1]))] = strings.TrimSpace(m[2])
			sawTag = true
			continue
		}
		lyr.Malformed = append(lyr.Malformed, MalformedLine{Text: raw, Reason: fmt.Sprintf("unrecognized bracket [%s]", inside)})
		sawMalformed = true
	}
	if len(times) > 0 {
		for _, ms := range times {
			lyr.Lines = append(lyr.Lines, Line{TimeMS: ms, Text: rest})
		}
		return
	}
	if sawTag && rest == "" {
		return // a metadata/offset-only line
	}
	if sawMalformed {
		return // already reported; do not add a second generic report
	}
	if sawTag {
		lyr.Malformed = append(lyr.Malformed, MalformedLine{Text: raw, Reason: "text without a timestamp"})
		return
	}
	lyr.Malformed = append(lyr.Malformed, MalformedLine{Text: raw, Reason: "line has no timestamp"})
}

// parseTimestamp parses the inside of a bracket as a timestamp. It returns
// the time in milliseconds and ok=true when it is a timestamp; ok=false
// with malformed=="" when it is not timestamp-shaped at all; and ok=false
// with malformed set when it looks like a timestamp but is invalid.
func parseTimestamp(inside string) (ms int64, ok bool, malformed string) {
	m := timestampRe.FindStringSubmatch(inside)
	if m == nil {
		// Timestamp-shaped but invalid (e.g. [12:34:56], [ab:cd]) is
		// malformed, not just "not a timestamp": report it.
		if looksLikeTimestamp(inside) {
			return 0, false, fmt.Sprintf("invalid timestamp [%s]", inside)
		}
		return 0, false, ""
	}
	minutes, _ := strconv.ParseInt(m[1], 10, 64)
	seconds, _ := strconv.ParseInt(m[2], 10, 64)
	if seconds > 59 {
		return 0, false, fmt.Sprintf("invalid timestamp [%s]: seconds out of range", inside)
	}
	var fracMS int64
	if m[3] != "" {
		frac, _ := strconv.ParseInt(m[3], 10, 64)
		scale := int64(1)
		for i := len(m[3]); i < 3; i++ {
			scale *= 10
		}
		fracMS = frac * scale
	}
	return (minutes*60+seconds)*1000 + fracMS, true, ""
}

// looksLikeTimestamp reports whether s has the rough shape of a timestamp
// (digits, a colon, then digits/colon/dot) so an invalid one can be
// reported as malformed instead of silently treated as a tag.
func looksLikeTimestamp(s string) bool {
	if s == "" {
		return false
	}
	hasColon := false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c == ':':
			hasColon = true
		case c == '.':
		default:
			return false
		}
	}
	return hasColon
}

// Format serializes lines to canonical LRC: one [mm:ss.cc] timestamp per
// line (centiseconds, truncated), lines ordered by time ascending. Times
// before zero are clamped to zero.
func Format(lines []Line) string {
	sorted := append([]Line(nil), lines...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TimeMS < sorted[j].TimeMS })
	var b strings.Builder
	for _, ln := range sorted {
		ms := ln.TimeMS
		if ms < 0 {
			ms = 0
		}
		totalSeconds := ms / 1000
		fmt.Fprintf(&b, "[%02d:%02d.%02d]", totalSeconds/60, totalSeconds%60, (ms%1000)/10)
		if ln.Text != "" {
			b.WriteByte(' ')
			b.WriteString(ln.Text)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Format serializes the whole document: metadata tags, then the offset
// tag when set, then the timed lines. A parse -> Format -> parse round-trip
// reproduces the same lines, meta and offset.
func (l *Lyrics) Format() string {
	var b strings.Builder
	// Conventional tags first, in a stable order, then any other keys sorted.
	ordered := []string{"ti", "ar", "al", "by", "au", "length", "re", "ve"}
	seen := map[string]bool{}
	for _, k := range ordered {
		if v, ok := l.Meta[k]; ok {
			fmt.Fprintf(&b, "[%s:%s]\n", k, v)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(l.Meta))
	for k := range l.Meta {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		fmt.Fprintf(&b, "[%s:%s]\n", k, l.Meta[k])
	}
	if l.OffsetMS != 0 {
		fmt.Fprintf(&b, "[offset:%+d]\n", l.OffsetMS)
	}
	b.WriteString(Format(l.Lines))
	return b.String()
}

// ShiftedLines returns the lines with OffsetMS applied (time + offset),
// clamped at zero, in time order. The stored Lines are unaffected.
func (l *Lyrics) ShiftedLines() []Line {
	out := make([]Line, len(l.Lines))
	for i, ln := range l.Lines {
		ms := ln.TimeMS + l.OffsetMS
		if ms < 0 {
			ms = 0
		}
		out[i] = Line{TimeMS: ms, Text: ln.Text}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeMS < out[j].TimeMS })
	return out
}

// SidecarPath returns the sidecar path for an audio file: the audio file's
// name without its extension, plus ".lrc" (e.g. "01 Song.flac" ->
// "01 Song.lrc").
//
// The audio extension is stripped because that is what Roon, Plex and
// Navidrome look for; "<audio>.flac.lrc" is read by none of them.
func SidecarPath(audioPath string) string {
	base := strings.TrimSuffix(audioPath, filepath.Ext(audioPath))
	if base == "" {
		base = audioPath
	}
	return base + ".lrc"
}

// legacySidecarPath is the older "<audio>.flac.lrc" spelling this program
// used. Reading falls back to it so lyrics written by an earlier build are
// not lost; writing always uses SidecarPath.
func legacySidecarPath(audioPath string) string {
	return audioPath + ".lrc"
}

// WriteSidecar writes lines as "<audio>.lrc" (canonical Format output).
// It refuses to overwrite an existing sidecar with different content; an
// existing sidecar with identical content is left in place and reported
// as success. The write goes through a temporary file plus rename.
func WriteSidecar(audioPath string, lines []Line) error {
	return writeSidecarFile(SidecarPath(audioPath), Format(lines))
}

// WriteSidecarLyrics writes a whole parsed document (metadata, offset and
// lines) as "<audio>.lrc", with the same overwrite refusal as WriteSidecar.
func WriteSidecarLyrics(audioPath string, lyr *Lyrics) error {
	if lyr == nil {
		return errors.New("lrc: nil lyrics")
	}
	return writeSidecarFile(SidecarPath(audioPath), lyr.Format())
}

func writeSidecarFile(path, content string) error {
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == content {
			return nil
		}
		return fmt.Errorf("lrc: sidecar %s already exists with different content", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lrc: read sidecar %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".lrc-*")
	if err != nil {
		return fmt.Errorf("lrc: create temp sidecar in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("lrc: write temp sidecar: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("lrc: sync temp sidecar: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("lrc: close temp sidecar: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("lrc: rename temp sidecar to %s: %w", path, err)
	}
	return nil
}

// ReadSidecar reads "<audio>.lrc" next to audioPath and parses it. An older
// "<audio>.flac.lrc" is accepted when the standard name is absent. It returns
// an error wrapping os.ErrNotExist when no sidecar is present.
func ReadSidecar(audioPath string) (*Lyrics, error) {
	path := SidecarPath(audioPath)
	data, err := os.ReadFile(path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		if legacy := legacySidecarPath(audioPath); legacy != path {
			if raw, legacyErr := os.ReadFile(legacy); legacyErr == nil {
				path, data, err = legacy, raw, nil
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("lrc: read sidecar %s: %w", path, err)
	}
	lyr, err := ParseBytes(data)
	if err != nil {
		return nil, fmt.Errorf("lrc: parse sidecar %s: %w", path, err)
	}
	return lyr, nil
}
