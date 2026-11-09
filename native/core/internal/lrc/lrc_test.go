package lrc

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sampleLRC = `[ti:Test Song]
[ar:Test Artist]
[al:Test Album]
[by:Someone]
[offset:+500]
[00:01.00] first line
[00:05.50][00:10.25] repeated line
[00:12.345] millisecond line
plain text with no timestamp
[99:99.99] bad seconds
[ti:Duplicate Title]
`

func TestParseBasic(t *testing.T) {
	lyr, err := Parse(sampleLRC)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if lyr.Meta["ti"] != "Duplicate Title" {
		t.Errorf("Meta[ti] = %q (last wins)", lyr.Meta["ti"])
	}
	if lyr.Meta["ar"] != "Test Artist" || lyr.Meta["al"] != "Test Album" || lyr.Meta["by"] != "Someone" {
		t.Errorf("Meta = %v", lyr.Meta)
	}
	if lyr.OffsetMS != 500 {
		t.Errorf("OffsetMS = %d, want 500", lyr.OffsetMS)
	}
	want := []Line{
		{TimeMS: 1000, Text: "first line"},
		{TimeMS: 5500, Text: "repeated line"},
		{TimeMS: 10250, Text: "repeated line"},
		{TimeMS: 12345, Text: "millisecond line"},
	}
	if !reflect.DeepEqual(lyr.Lines, want) {
		t.Errorf("Lines = %+v, want %+v", lyr.Lines, want)
	}
	// Malformed lines are reported, not silently dropped.
	if len(lyr.Malformed) != 2 {
		t.Fatalf("Malformed = %+v, want 2 entries", lyr.Malformed)
	}
	for _, m := range lyr.Malformed {
		if m.Text == "" || m.Reason == "" {
			t.Errorf("malformed entry missing text/reason: %+v", m)
		}
	}
	// Offset is recorded, not applied to Lines; ShiftedLines applies it.
	shifted := lyr.ShiftedLines()
	if shifted[0].TimeMS != 1500 || shifted[1].TimeMS != 6000 {
		t.Errorf("ShiftedLines = %+v, want first times 1500/6000", shifted)
	}
	if lyr.Lines[0].TimeMS != 1000 {
		t.Error("Parse applied the offset to Lines; it must keep raw times")
	}
}

func TestParseRejectsNoTimedLines(t *testing.T) {
	for _, input := range []string{
		"",
		"\n\n",
		"[ti:Only Meta]\n[ar:Someone]\n",
		"just some text\nmore text\n",
		"[99:99.99] bad seconds only\n",
		"[offset:+100]\n",
	} {
		if _, err := Parse(input); err == nil {
			t.Errorf("Parse(%q) accepted input with no valid timed lines", input)
		}
	}
}

func TestParseTimestamps(t *testing.T) {
	lyr, err := Parse("[00:01.5] half second\n[01:02.05] two digits\n[00:03] bare\n[65:00.00] long song\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{1500, 62050, 3000, 65 * 60 * 1000}
	// Lines are returned time-ordered.
	got := []int64{lyr.Lines[0].TimeMS, lyr.Lines[1].TimeMS, lyr.Lines[2].TimeMS, lyr.Lines[3].TimeMS}
	wantSorted := []int64{1500, 3000, 62050, 65 * 60 * 1000}
	if !reflect.DeepEqual(got, wantSorted) {
		t.Errorf("times = %v, want %v (unsorted want was %v)", got, wantSorted, want)
	}
	// Out-of-order input is sorted on output.
	lyr, err = Parse("[00:10.00] later\n[00:01.00] earlier\n")
	if err != nil {
		t.Fatal(err)
	}
	if lyr.Lines[0].Text != "earlier" || lyr.Lines[1].Text != "later" {
		t.Errorf("lines not sorted: %+v", lyr.Lines)
	}
}

func TestParseNegativeOffsetAndCRLF(t *testing.T) {
	lyr, err := Parse("[offset:-250]\r\n[00:02.00] line one\r\n[00:04.00] line two\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if lyr.OffsetMS != -250 {
		t.Errorf("OffsetMS = %d, want -250", lyr.OffsetMS)
	}
	if len(lyr.Lines) != 2 || lyr.Lines[0].Text != "line one" {
		t.Errorf("Lines = %+v", lyr.Lines)
	}
	shifted := lyr.ShiftedLines()
	if shifted[0].TimeMS != 1750 {
		t.Errorf("shifted first line = %d, want 1750", shifted[0].TimeMS)
	}
}

func TestFormatRoundTrip(t *testing.T) {
	// Centisecond-precision input only: Format serializes to [mm:ss.cc],
	// so a .xxx millisecond time truncates (12345ms -> 12340ms) and
	// cannot round-trip exactly.
	lyr, err := Parse("[ti:Test Song]\n[ar:Test Artist]\n[al:Test Album]\n[by:Someone]\n[offset:+500]\n[00:01.00] first line\n[00:05.50][00:10.25] repeated line\n[00:12.34] centisecond line\n")
	if err != nil {
		t.Fatal(err)
	}
	formatted := lyr.Format()
	reparsed, err := Parse(formatted)
	if err != nil {
		t.Fatalf("re-parse of Format output: %v", err)
	}
	if !reflect.DeepEqual(reparsed.Lines, lyr.Lines) {
		t.Errorf("round-trip lines differ:\n%+v\nvs\n%+v", reparsed.Lines, lyr.Lines)
	}
	if !reflect.DeepEqual(reparsed.Meta, lyr.Meta) {
		t.Errorf("round-trip meta differs: %v vs %v", reparsed.Meta, lyr.Meta)
	}
	if reparsed.OffsetMS != lyr.OffsetMS {
		t.Errorf("round-trip offset %d vs %d", reparsed.OffsetMS, lyr.OffsetMS)
	}
	// Formatting twice is stable.
	if again := reparsed.Format(); again != formatted {
		t.Errorf("Format not stable:\n%s\nvs\n%s", again, formatted)
	}
}

func TestFormatCanonical(t *testing.T) {
	got := Format([]Line{
		{TimeMS: 62340, Text: "second"},
		{TimeMS: 1000, Text: "first"},
		{TimeMS: 12345, Text: ""},
	})
	want := "[00:01.00] first\n[00:12.34]\n[01:02.34] second\n"
	if got != want {
		t.Errorf("Format =\n%s\nwant\n%s", got, want)
	}
	// Centiseconds truncate (not round): 1299ms -> .29.
	if got := Format([]Line{{TimeMS: 1299, Text: "x"}}); got != "[00:01.29] x\n" {
		t.Errorf("Format(1299ms) = %q", got)
	}
	if got := Format(nil); got != "" {
		t.Errorf("Format(nil) = %q, want empty", got)
	}
}

func TestSidecarWriteRead(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(audio, []byte("fake flac"), 0644); err != nil {
		t.Fatal(err)
	}
	lines := []Line{{TimeMS: 1000, Text: "first"}, {TimeMS: 5000, Text: "second"}}
	if err := WriteSidecar(audio, lines); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	if got := SidecarPath(audio); got != audio+".lrc" {
		t.Errorf("SidecarPath = %q", got)
	}
	lyr, err := ReadSidecar(audio)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if !reflect.DeepEqual(lyr.Lines, lines) {
		t.Errorf("sidecar lines = %+v, want %+v", lyr.Lines, lines)
	}
	// Writing identical content again succeeds (idempotent).
	if err := WriteSidecar(audio, lines); err != nil {
		t.Errorf("rewriting identical sidecar: %v", err)
	}
	// Writing different content is refused, and the old file is untouched.
	other := []Line{{TimeMS: 2000, Text: "different"}}
	if err := WriteSidecar(audio, other); err == nil {
		t.Error("WriteSidecar overwrote an existing different sidecar")
	}
	lyr, err = ReadSidecar(audio)
	if err != nil || !reflect.DeepEqual(lyr.Lines, lines) {
		t.Errorf("sidecar changed after refused write: %+v err=%v", lyr, err)
	}
	// Full-document write preserves meta and offset.
	full, err := Parse("[ti:Full]\n[offset:-100]\n[00:01.00] a\n")
	if err != nil {
		t.Fatal(err)
	}
	audio2 := filepath.Join(dir, "song2.flac")
	if err := os.WriteFile(audio2, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecarLyrics(audio2, full); err != nil {
		t.Fatalf("WriteSidecarLyrics: %v", err)
	}
	back, err := ReadSidecar(audio2)
	if err != nil {
		t.Fatal(err)
	}
	if back.Meta["ti"] != "Full" || back.OffsetMS != -100 || len(back.Lines) != 1 {
		t.Errorf("full sidecar round-trip: %+v", back)
	}
	if err := WriteSidecarLyrics(audio2, nil); err == nil {
		t.Error("WriteSidecarLyrics accepted nil lyrics")
	}
}

func TestReadSidecarMissing(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "nosuch.flac")
	if _, err := ReadSidecar(audio); err == nil {
		t.Error("ReadSidecar accepted a missing sidecar")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadSidecar error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestParseBytesAndMalformedBrackets(t *testing.T) {
	lyr, err := ParseBytes([]byte("[00:01.00] ok\n[12:34:56] nonsense\n[foo] mystery\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(lyr.Lines) != 1 {
		t.Fatalf("Lines = %+v", lyr.Lines)
	}
	if len(lyr.Malformed) != 2 {
		t.Fatalf("Malformed = %+v, want 2", lyr.Malformed)
	}
	if !strings.Contains(lyr.Malformed[0].Reason, "timestamp") {
		t.Errorf("first malformed reason = %q, want timestamp complaint", lyr.Malformed[0].Reason)
	}
}
