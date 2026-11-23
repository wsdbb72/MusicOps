package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

// writeToneWAV builds a one-second 44.1kHz mono 16-bit PCM WAV at freq Hz.
func writeToneWAV(t *testing.T, path string, freq float64) []byte {
	t.Helper()
	const rate = 44100
	samples := make([]int16, rate)
	for i := range samples {
		samples[i] = int16(12000 * math.Sin(2*math.Pi*freq*float64(i)/rate))
	}
	data := make([]byte, 44+len(samples)*2)
	copy(data[0:], "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVE")
	copy(data[12:], "fmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], rate)
	binary.LittleEndian.PutUint32(data[28:], rate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(samples)*2))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(sample))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFLACFromWAV(t *testing.T, source, destination string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", source, "-c:a", "flac", destination)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg convert %s: %v %s", destination, err, output)
	}
	return destination
}

func writeHighResFLAC(t *testing.T, source, destination string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", source,
		"-af", "aformat=sample_rates=96000:sample_fmts=s32", "-c:a", "flac", "-bits_per_raw_sample", "24", destination)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg high-res convert %s: %v %s", destination, err, output)
	}
	return destination
}

// releaseLibraryFile ingests one incoming file into the library through
// audiorelease.Release and returns the stored entry.
func releaseLibraryFile(t *testing.T, application *App, sourceName, rootID, artist, album, title string, track int) audiorelease.Entry {
	t.Helper()
	return releaseLibraryFileWithOptions(t, application, sourceName, rootID, artist, album, title, track, nil)
}

func releaseLibraryFileWithOptions(t *testing.T, application *App, sourceName, rootID, artist, album, title string, track int, mutate func(*audiorelease.Request)) audiorelease.Entry {
	t.Helper()
	incoming := filepath.Join(application.Dir, "incoming")
	library := filepath.Join(application.Dir, "library-audio")
	request := audiorelease.Request{
		RequestID: "rel-" + rootID, RootID: rootID, Artist: artist, Album: album, Title: title,
		Track: track, SourceName: sourceName, Incoming: incoming, Library: library,
		Decoder: audiorelease.SystemDecoder(),
	}
	if mutate != nil {
		mutate(&request)
	}
	result, err := audiorelease.Release(context.Background(), application.engine, request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LibraryPublished {
		t.Fatal(result)
	}
	entries, err := audiorelease.List(application.engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.RequestID == "rel-"+rootID {
			return entry
		}
	}
	t.Fatal("released entry not listed")
	return audiorelease.Entry{}
}

func duplicateGroups(t *testing.T, application *App) []duplicateGroup {
	t.Helper()
	groups, _, _, err := application.scanDuplicates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

func TestDuplicateRankUsesRealQualityAndValidDecode(t *testing.T) {
	hiRes := duplicateRank(duplicateFile{Valid: true, Lossless: true, Codec: "flac", Format: "flac", BitsPerSample: 24, SampleRate: 96000})
	cd := duplicateRank(duplicateFile{Valid: true, Lossless: true, Codec: "flac", Format: "flac", BitsPerSample: 16, SampleRate: 44100})
	mp3High := duplicateRank(duplicateFile{Valid: true, Codec: "mp3", Format: "mp3", BitRate: 320000})
	mp3Low := duplicateRank(duplicateFile{Valid: true, Codec: "mp3", Format: "mp3", BitRate: 128000})
	damaged := duplicateRank(duplicateFile{Valid: false, Lossless: true, Codec: "flac", Format: "flac", BitsPerSample: 24, SampleRate: 192000})
	if compareRank(hiRes, cd) <= 0 || compareRank(cd, mp3High) <= 0 || compareRank(mp3High, mp3Low) <= 0 || compareRank(mp3Low, damaged) <= 0 {
		t.Fatalf("quality order wrong: hires=%v cd=%v mp3high=%v mp3low=%v damaged=%v", hiRes, cd, mp3High, mp3Low, damaged)
	}
}

func TestDuplicateRecordingTitleKeepsVersionSuffixes(t *testing.T) {
	studio := duplicateRecordingTitle("Artist/Album/01 Song.flac", "Artist")
	live := duplicateRecordingTitle("Artist/Album/01 Song (Live).flac", "Artist")
	remix := duplicateRecordingTitle("Artist/Album/01 Song - Remix.flac", "Artist")
	if studio == live || studio == remix || live == remix {
		t.Fatalf("version identities collapsed: studio=%q live=%q remix=%q", studio, live, remix)
	}
}

func TestDuplicateRankTreatsAPEAsLossless(t *testing.T) {
	ape := duplicateRank(duplicateFile{Format: "ape", Lossless: true})
	mp3 := duplicateRank(duplicateFile{Format: "mp3"})
	if compareRank(ape, mp3) <= 0 {
		t.Fatalf("APE rank %v did not beat MP3 rank %v", ape, mp3)
	}
}

// ---------------------------------------------------------------------------
// Duplicate scan / merge / restore

func TestDuplicateScanGroupsSameRecordingAndIgnoresDifferentTitle(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a.wav"), 440)
	writeFLACFromWAV(t, filepath.Join(incoming, "a.wav"), filepath.Join(incoming, "b.flac"))
	writeToneWAV(t, filepath.Join(incoming, "c.wav"), 880)
	releaseLibraryFile(t, application, "a.wav", "dup-1", "Artist", "Album", "Song", 1)
	releaseLibraryFile(t, application, "b.flac", "dup-2", "Artist", "Album", "Song", 1)
	releaseLibraryFile(t, application, "c.wav", "dup-3", "Artist", "Album", "Other", 1)

	groups := duplicateGroups(t, application)
	if len(groups) != 1 {
		t.Fatal(groups)
	}
	group := groups[0]
	if group.State != "ready" || group.Keep == nil || len(group.Files) != 2 || group.Keep.Valid != true {
		t.Fatal(group)
	}
}

func TestDuplicateScanLimitStopsAndReportsTruncation(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	for i := 0; i < 3; i++ {
		wav := filepath.Join(incoming, fmt.Sprintf("limit-%d.wav", i))
		flac := filepath.Join(incoming, fmt.Sprintf("limit-%d.flac", i))
		writeToneWAV(t, wav, 440+float64(i))
		writeFLACFromWAV(t, wav, flac)
		title := fmt.Sprintf("Song %d", i)
		releaseLibraryFile(t, application, filepath.Base(wav), fmt.Sprintf("limit-wav-%d", i), "Artist", "Limit Album", title, i+1)
		releaseLibraryFile(t, application, filepath.Base(flac), fmt.Sprintf("limit-flac-%d", i), "Artist", "Limit Album", title, i+1)
	}
	groups, _, truncated, _, err := application.scanDuplicatesWithOptions(context.Background(), 2, "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || !truncated {
		t.Fatalf("groups=%d truncated=%v", len(groups), truncated)
	}
}

func TestDuplicateScanDifferentContentStaysReview(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a.wav"), 440)
	writeToneWAV(t, filepath.Join(incoming, "b-source.wav"), 880)
	writeFLACFromWAV(t, filepath.Join(incoming, "b-source.wav"), filepath.Join(incoming, "b.flac"))
	releaseLibraryFile(t, application, "a.wav", "rev-1", "Artist", "Album2", "Song", 1)
	releaseLibraryFile(t, application, "b.flac", "rev-2", "Artist", "Album2", "Song", 1)

	groups := duplicateGroups(t, application)
	if len(groups) != 1 {
		t.Fatal(groups)
	}
	if groups[0].State != "review" || groups[0].Keep != nil {
		t.Fatal(groups[0])
	}
	if !strings.Contains(groups[0].Reason, "解码内容不同") {
		t.Fatal(groups[0])
	}
}

func TestDuplicateScanStructuralWithoutDecoder(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a.wav"), 440)
	writeFLACFromWAV(t, filepath.Join(incoming, "a.wav"), filepath.Join(incoming, "b.flac"))
	writeToneWAV(t, filepath.Join(incoming, "c-source.wav"), 880)
	writeFLACFromWAV(t, filepath.Join(incoming, "c-source.wav"), filepath.Join(incoming, "c.flac"))
	// Ingest without a decoder so the records are structural-only.
	for _, item := range []struct{ src, root, title string }{
		{"a.wav", "st-1", "Song"}, {"b.flac", "st-2", "Song"}, {"c.flac", "st-3", "Other"},
	} {
		_, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
			RequestID: "rel-" + item.root, RootID: item.root, Artist: "Artist", Album: "Album",
			Title: item.title, Track: 1, SourceName: item.src,
			Incoming: incoming, Library: filepath.Join(application.Dir, "library-audio"),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := audiorelease.List(application.engine)
	if err != nil {
		t.Fatal(err)
	}
	group, err := application.fingerprintGroup(context.Background(), entries[:2], nil)
	if err != nil {
		t.Fatal(err)
	}
	if group == nil || group.State != "review" || group.IdentityUsed != "structural" {
		t.Fatal(group)
	}
	if !strings.Contains(group.Reason, "结构级比对") {
		t.Fatal(group)
	}
}

func TestDuplicateScanKeepsHighestResolutionForSameRecording(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	source := filepath.Join(incoming, "source.wav")
	writeToneWAV(t, source, 440)
	writeHighResFLAC(t, source, filepath.Join(incoming, "high.flac"))
	releaseLibraryFile(t, application, "source.wav", "hires-1", "Artist", "Album", "Song", 1)
	releaseLibraryFile(t, application, "high.flac", "hires-2", "Artist", "Album", "Song", 1)
	groups := duplicateGroups(t, application)
	if len(groups) != 1 || groups[0].State != "ready" || groups[0].Keep == nil {
		t.Fatal(groups)
	}
	keep := groups[0].Keep
	if keep.Format != "flac" || keep.SampleRate != 96000 || keep.BitsPerSample < 24 || !keep.Lossless {
		t.Fatalf("kept wrong quality: %+v", keep)
	}
}

func TestDuplicateMergeAndRestoreRoundTrip(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a-source.wav"), 440)
	writeFLACFromWAV(t, filepath.Join(incoming, "a-source.wav"), filepath.Join(incoming, "a.flac"))
	writeToneWAV(t, filepath.Join(incoming, "b.wav"), 440)
	keep := releaseLibraryFile(t, application, "a.flac", "mrg-1", "Artist", "Album", "Song", 1)
	loser := releaseLibraryFile(t, application, "b.wav", "mrg-2", "Artist", "Album", "Song", 1)
	// A sidecar next to the loser must travel into quarantine with it.
	loserPath := filepath.Join(application.Dir, "library-audio", loser.File)
	if err := os.WriteFile(loserPath+".lrc", []byte("[00:01.00] B side\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	groups := duplicateGroups(t, application)
	if len(groups) != 1 || groups[0].State != "ready" {
		t.Fatal(groups)
	}
	groupID := groups[0].GroupID
	code, decoded := mediaPost(t, application, "/v1/library/duplicates/merge", map[string]any{"group_id": groupID})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	mergeID, _ := decoded["merge_id"].(string)
	if mergeID == "" {
		t.Fatal(decoded)
	}
	kept, _ := decoded["kept"].(string)
	if kept == "" {
		t.Fatal(decoded)
	}
	// Exactly one of the two files stays; the other is quarantined,
	// never deleted.
	quarantine, err := filepath.Glob(filepath.Join(application.Dir, "quarantine", mergeID, "Artist", "Album", "*.wav"))
	if err != nil || len(quarantine) != 1 {
		t.Fatal(quarantine, err)
	}
	// The quarantined bytes are the loser audio, and its sidecar went too.
	quarantined, err := os.ReadFile(quarantine[0])
	if err != nil {
		t.Fatal(err)
	}
	loserBytes, err := os.ReadFile(filepath.Join(incoming, "b.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(quarantined, loserBytes) {
		t.Fatal("quarantined bytes are not the loser audio")
	}
	// Exactly one of the two files stays in the library.
	entries, err := audiorelease.List(application.engine)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.RequestID] = entry.FilePresent
	}
	staying := 0
	for _, id := range []string{keep.RequestID, loser.RequestID} {
		if present[id] {
			staying++
		}
	}
	if staying != 1 {
		t.Fatal("expected exactly one file to stay in the library", present)
	}
	if _, err := os.Stat(quarantine[0] + ".lrc"); err != nil {
		t.Fatal("sidecar did not travel into quarantine", err)
	}
	if _, err := os.Stat(loserPath); !os.IsNotExist(err) {
		t.Fatal("loser still in the library")
	}
	// Merging the same group again is a conflict (the group is gone).
	code, decoded = mediaPost(t, application, "/v1/library/duplicates/merge", map[string]any{"group_id": groupID})
	if code != http.StatusNotFound {
		t.Fatal(code, decoded)
	}
	// Restore brings the loser back byte-identical, with its sidecar.
	code, decoded = mediaPost(t, application, "/v1/library/duplicates/restore", map[string]any{"merge_id": mergeID})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	restored, err := os.ReadFile(loserPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, loserBytes) {
		t.Fatal("restored bytes differ from the quarantined file")
	}
	if _, err := os.Stat(loserPath + ".lrc"); err != nil {
		t.Fatal("sidecar did not come back", err)
	}
	// A second restore replays.
	code, decoded = mediaPost(t, application, "/v1/library/duplicates/restore", map[string]any{"merge_id": mergeID})
	if code != http.StatusOK || decoded["replayed"] != true {
		t.Fatal(code, decoded)
	}
	// And the group is ready again.
	groups = duplicateGroups(t, application)
	if len(groups) != 1 || groups[0].State != "ready" {
		t.Fatal(groups)
	}
}

func TestDuplicateMergeRefusesReviewGroup(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a.wav"), 440)
	writeToneWAV(t, filepath.Join(incoming, "b-source.wav"), 880)
	writeFLACFromWAV(t, filepath.Join(incoming, "b-source.wav"), filepath.Join(incoming, "b.flac"))
	releaseLibraryFile(t, application, "a.wav", "nmg-1", "Artist", "Album2", "Song", 1)
	releaseLibraryFile(t, application, "b.flac", "nmg-2", "Artist", "Album2", "Song", 1)
	groups := duplicateGroups(t, application)
	if len(groups) != 1 || groups[0].State != "review" {
		t.Fatal(groups)
	}
	code, decoded := mediaPost(t, application, "/v1/library/duplicates/merge", map[string]any{"group_id": groups[0].GroupID})
	if code != http.StatusConflict || decoded["code"] != "MERGE_NOT_READY" {
		t.Fatal(code, decoded)
	}
}

// ---------------------------------------------------------------------------
// Artist folders

func TestFolderArtist(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"A feat. B"}, "A／B"},
		{[]string{"A", "B"}, "A／B"},
		{[]string{"A & B"}, "A／B"},
		{[]string{"A feat. B feat. C"}, "A／B／C"},
		{[]string{"A", "a"}, "A"},
		{[]string{"陈奕迅"}, "陈奕迅"},
		{[]string{""}, ""},
	}
	for _, tc := range cases {
		if got := folderArtist(tc.in); got != tc.want {
			t.Fatalf("folderArtist(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func releaseTaggedFLAC(t *testing.T, application *App, rootID, artist, album, title string, track int) audiorelease.Entry {
	t.Helper()
	incoming := filepath.Join(application.Dir, "incoming")
	sourceName := rootID + ".flac"
	path := genTestFLAC(t, incoming, sourceName)
	if err := flactags.Write(path, flactags.Tags{Raw: map[string][]string{
		"ARTIST": {artist}, "ALBUM": {album}, "TITLE": {title},
	}}); err != nil {
		t.Fatal(err)
	}
	return releaseLibraryFile(t, application, sourceName, rootID, artist, album, title, track)
}

func TestArtistFolderPlanAndRelocate(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(application.Dir, "library-audio")
	entry := releaseTaggedFLAC(t, application, "af-1", "Solo Artist", "My Album", "Track One", 1)
	// Already organized: no plan.
	plans, err := application.planArtistFolders()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].State != "none" {
		t.Fatal(plans)
	}
	// Move the album folder to the library root, the way a loose album
	// arrives, and point the record at it.
	organized := filepath.Join(library, "Solo Artist", "My Album")
	loose := filepath.Join(library, "My Album")
	if err := os.Rename(organized, loose); err != nil {
		t.Fatal(err)
	}
	looseFile := filepath.Join(loose, filepath.Base(entry.File))
	if err := audiorelease.Relocate(application.engine, entry.RequestID, looseFile); err != nil {
		t.Fatal(err)
	}
	code, decoded := mediaGet(t, application, "/v1/library/artist-folders")
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	plansRaw, ok := decoded["plans"].([]any)
	if !ok || len(plansRaw) != 1 {
		t.Fatal(decoded)
	}
	plan := plansRaw[0].(map[string]any)
	if plan["state"] != "intent" {
		t.Fatal(plan)
	}
	if plan["from"] != loose || !strings.HasSuffix(plan["to"].(string), filepath.Join("Solo Artist", "My Album")) {
		t.Fatal(plan)
	}
	// Execute: the folder moves back and the record follows it.
	code, decoded = mediaPost(t, application, "/v1/library/artist-folders", map[string]any{})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	results, ok := decoded["results"].([]any)
	if !ok || len(results) != 1 || results[0].(map[string]any)["state"] != "done" {
		t.Fatal(decoded)
	}
	entries, err := audiorelease.List(application.engine)
	if err != nil {
		t.Fatal(err)
	}
	// Entry.File is slash-separated on every OS (see libraryFile), so
	// compare in slash form rather than against filepath.Join, which
	// is backslash-joined on Windows.
	if len(entries) != 1 || filepath.ToSlash(entries[0].File) != "Solo Artist/My Album/"+filepath.Base(entry.File) {
		t.Fatal(entries)
	}
	if _, err := os.Stat(filepath.Join(library, "Solo Artist", "My Album", filepath.Base(entry.File))); err != nil {
		t.Fatal(err)
	}
	// Now the plan is empty again.
	plans, err = application.planArtistFolders()
	if err != nil || len(plans) != 1 || plans[0].State != "none" {
		t.Fatal(plans, err)
	}
}

func TestArtistFolderTargetExistsIsSkipped(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(application.Dir, "library-audio")
	entry := releaseTaggedFLAC(t, application, "af-2", "Other Artist", "Other Album", "Track One", 1)
	organized := filepath.Join(library, "Other Artist", "Other Album")
	loose := filepath.Join(library, "Other Album")
	if err := os.Rename(organized, loose); err != nil {
		t.Fatal(err)
	}
	if err := audiorelease.Relocate(application.engine, entry.RequestID, filepath.Join(loose, filepath.Base(entry.File))); err != nil {
		t.Fatal(err)
	}
	// Pre-create the target with content: the plan must skip, never merge.
	target := filepath.Join(library, "Other Artist", "Other Album")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "existing.flac"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	plans, err := application.planArtistFolders()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].State != "skipped" || !strings.Contains(plans[0].Reason, "目标文件夹已存在") {
		t.Fatal(plans)
	}
	// Execute does not move anything.
	code, decoded := mediaPost(t, application, "/v1/library/artist-folders", map[string]any{})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	results := decoded["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["state"] != "skipped" {
		t.Fatal(decoded)
	}
	if _, err := os.Stat(filepath.Join(loose, filepath.Base(entry.File))); err != nil {
		t.Fatal("loose file moved despite the skip", err)
	}
}

func TestArtistFolderLooseFileMovesWithSidecar(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(application.Dir, "library-audio")
	entry := releaseTaggedFLAC(t, application, "af-3", "Solo Artist", "Single Album", "Track One", 1)
	organized := filepath.Join(library, "Solo Artist", "Single Album", filepath.Base(entry.File))
	looseFile := filepath.Join(library, filepath.Base(entry.File))
	if err := os.Rename(organized, looseFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(looseFile+".lrc", []byte("[00:01.00] Loose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := audiorelease.Relocate(application.engine, entry.RequestID, looseFile); err != nil {
		t.Fatal(err)
	}
	plans, err := application.planArtistFolders()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].State != "intent" || !plans[0].LooseFile {
		t.Fatal(plans)
	}
	code, decoded := mediaPost(t, application, "/v1/library/artist-folders", map[string]any{"request_ids": []string{entry.RequestID}})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	results := decoded["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["state"] != "done" {
		t.Fatal(decoded)
	}
	moved := filepath.Join(library, "Solo Artist", "Single Album", filepath.Base(entry.File))
	if _, err := os.Stat(moved); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.TrimSuffix(moved, filepath.Ext(moved)) + ".lrc"); err != nil {
		t.Fatal("sidecar did not travel with the loose file", err)
	}
	if _, err := os.Stat(looseFile); !os.IsNotExist(err) {
		t.Fatal("loose file still at the root")
	}
}

func TestArtistFolderSkipsWAVWithoutTags(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(application.Dir, "library-audio")
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "tone.wav"), 440)
	entry := releaseLibraryFile(t, application, "tone.wav", "af-4", "Some Artist", "Some Album", "Tone", 1)
	_ = entry
	// A WAV carries no tags to read the artist/album from, so the plan
	// skips it instead of guessing from the folder name.
	organized := filepath.Join(library, "Some Artist", "Some Album")
	loose := filepath.Join(library, "Some Album")
	if err := os.Rename(organized, loose); err != nil {
		t.Fatal(err)
	}
	if err := audiorelease.Relocate(application.engine, entry.RequestID, filepath.Join(loose, filepath.Base(entry.File))); err != nil {
		t.Fatal(err)
	}
	plans, err := application.planArtistFolders()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].State != "skipped" || !strings.Contains(plans[0].Reason, "缺少可靠的专辑艺人") {
		t.Fatal(plans)
	}
}

func TestResolveTargetInDirRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	if _, err := resolveTargetInDir(root, filepath.Join(link, "Album")); err == nil {
		t.Fatal("symlinked target escape accepted")
	}
	// A plain target inside the root resolves fine.
	got, err := resolveTargetInDir(root, filepath.Join(root, "Artist", "Album"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "Artist", "Album") {
		t.Fatal(got)
	}
}

// TestArtistFolderPlanUnderSymlinkedParent is the regression test for
// the macOS artist-folder failures: the state directory sits under a
// symlinked parent (/var points at /private/var), so the plan must
// still find a loose album (state "intent", from/to spelled through
// the state directory) instead of reporting it already organized
// (state "none", empty "to").
func TestArtistFolderPlanUnderSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	// t.TempDir follows GOTMPDIR first, then TMPDIR: point both at the
	// symlinked parent so the app below really lives under it.
	t.Setenv("GOTMPDIR", link)
	t.Setenv("TMPDIR", link)
	application := newApp(t)
	library := filepath.Join(application.Dir, "library-audio")
	entry := releaseTaggedFLAC(t, application, "af-sym", "Solo Artist", "My Album", "Track One", 1)
	organized := filepath.Join(library, "Solo Artist", "My Album")
	loose := filepath.Join(library, "My Album")
	if err := os.Rename(organized, loose); err != nil {
		t.Fatal(err)
	}
	if err := audiorelease.Relocate(application.engine, entry.RequestID, filepath.Join(loose, filepath.Base(entry.File))); err != nil {
		t.Fatal(err)
	}
	plans, err := application.planArtistFolders()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].State != "intent" {
		t.Fatal(plans)
	}
	if plans[0].From != loose || !strings.HasSuffix(plans[0].To, filepath.Join("Solo Artist", "My Album")) {
		t.Fatal(plans)
	}
	// Execute: the folder moves back and the record follows it.
	code, decoded := mediaPost(t, application, "/v1/library/artist-folders", map[string]any{})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	results, ok := decoded["results"].([]any)
	if !ok || len(results) != 1 || results[0].(map[string]any)["state"] != "done" {
		t.Fatal(decoded)
	}
	entries, err := audiorelease.List(application.engine)
	if err != nil {
		t.Fatal(err)
	}
	// Entry.File is slash-separated on every OS (see libraryFile), so
	// compare in slash form rather than against filepath.Join, which
	// is backslash-joined on Windows.
	if len(entries) != 1 || filepath.ToSlash(entries[0].File) != "Solo Artist/My Album/"+filepath.Base(entry.File) {
		t.Fatal(entries)
	}
}

// ---------------------------------------------------------------------------
// End-to-end acceptance: a real HTTP server over the new endpoints.

func TestSlice7EndToEndOverHTTP(t *testing.T) {
	application := newApp(t)
	server := httptest.NewServer(mediaHandler(application))
	defer server.Close()
	client := server.Client()

	doJSON := func(method, path string, body map[string]any) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(raw)
		}
		request, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(response.StatusCode, string(data), err)
		}
		return response.StatusCode, decoded
	}

	incoming := filepath.Join(application.Dir, "incoming")
	wav := writeTestWAV(t, filepath.Join(incoming, "plain.wav"))
	meta := map[string]any{"musicName": "E2E Song", "artist": []any{map[string]any{"name": "E2E Artist"}}, "album": "E2E Album", "format": "wav"}
	container := buildTestNCM(t, wav, meta, nil)
	if err := os.WriteFile(filepath.Join(incoming, "e2e.ncm"), container, 0o600); err != nil {
		t.Fatal(err)
	}
	// NCM decrypt.
	code, decoded := doJSON(http.MethodPost, "/v1/library/ncm/decrypt", map[string]any{"source": "incoming/e2e.ncm"})
	if code != http.StatusOK || decoded["format"] != "wav" {
		t.Fatal(code, decoded)
	}
	// FLAC tags round-trip.
	flacPath := genTestFLAC(t, incoming, "e2e.flac")
	_ = flacPath
	code, decoded = doJSON(http.MethodPost, "/v1/library/tags", map[string]any{"path": "incoming/e2e.flac", "tags": map[string]any{"title": "E2E", "artist": "E2E Artist"}})
	if code != http.StatusOK {
		t.Fatal(code, decoded)
	}
	code, decoded = doJSON(http.MethodGet, "/v1/library/tags?path=incoming/e2e.flac", nil)
	if code != http.StatusOK || decoded["tags"].(map[string]any)["title"] != "E2E" {
		t.Fatal(code, decoded)
	}
	// LRC round-trip.
	code, decoded = doJSON(http.MethodPost, "/v1/library/lyrics", map[string]any{"path": "incoming/e2e.flac", "text": "[00:01.00] E2E line\n", "sidecar": true, "embed": true})
	if code != http.StatusOK || decoded["embedded"] != true {
		t.Fatal(code, decoded)
	}
	code, decoded = doJSON(http.MethodGet, "/v1/library/lyrics?path=incoming/e2e.flac", nil)
	if code != http.StatusOK || decoded["sidecar"] == nil || decoded["embedded"] == nil {
		t.Fatal(code, decoded)
	}
	// Duplicates scan (empty library: no groups).
	code, decoded = doJSON(http.MethodGet, "/v1/library/duplicates", nil)
	if code != http.StatusOK || decoded["method"] != "pcm" || decoded["count"] != float64(0) {
		t.Fatal(code, decoded)
	}
	// Artist-folder plan (empty library: no plans).
	code, decoded = doJSON(http.MethodGet, "/v1/library/artist-folders", nil)
	if code != http.StatusOK || decoded["count"] != float64(0) {
		t.Fatal(code, decoded)
	}
}

// Moving a file that qBittorrent still holds breaks the torrent. A configured
// client that cannot be reached refuses as well: "I could not check" is not
// "it is safe" (P0-3).
func TestDuplicateMergeRefusesFilesStillInQBittorrent(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a-source.wav"), 440)
	writeFLACFromWAV(t, filepath.Join(incoming, "a-source.wav"), filepath.Join(incoming, "a.flac"))
	writeToneWAV(t, filepath.Join(incoming, "b.wav"), 440)
	releaseLibraryFileWithOptions(t, application, "a.flac", "seed-1", "Artist", "Album", "Song", 1, func(request *audiorelease.Request) { request.TorrentManaged = true })
	loser := releaseLibraryFileWithOptions(t, application, "b.wav", "seed-2", "Artist", "Album", "Song", 1, func(request *audiorelease.Request) { request.TorrentManaged = true })
	loserPath := filepath.Join(application.Dir, "library-audio", loser.File)

	fake := newFakeQBT(t, map[string][]byte{}, 0)
	fake.row = &fakeQBTorrent{
		hash:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		savePath:    filepath.Join(application.Dir, "library-audio", "Artist"),
		contentPath: filepath.Join(application.Dir, "library-audio", "Artist", "Album"),
		started:     true,
	}
	post(t, application, "/v1/settings/qbittorrent", map[string]any{"base_url": fake.url(), "username": "user", "password": "pass"})

	groups := duplicateGroups(t, application)
	if len(groups) != 1 || groups[0].State != "ready" {
		t.Fatal(groups)
	}
	code, decoded := mediaPost(t, application, "/v1/library/duplicates/merge", map[string]any{"group_id": groups[0].GroupID})
	if code != http.StatusConflict || decoded["code"] != "MERGE_SEEDING" {
		t.Fatal(code, decoded)
	}
	if _, err := os.Stat(loserPath); err != nil {
		t.Fatal("a refused merge must not move the file:", err)
	}

	// The client is configured but cannot be reached: refuse, never guess.
	post(t, application, "/v1/settings/qbittorrent", map[string]any{"base_url": "http://127.0.0.1:1", "username": "user", "password": "pass"})
	code, decoded = mediaPost(t, application, "/v1/library/duplicates/merge", map[string]any{"group_id": groups[0].GroupID})
	if code != http.StatusConflict || decoded["code"] != "MERGE_QBITTORRENT_UNAVAILABLE" {
		t.Fatal(code, decoded)
	}
	if _, err := os.Stat(loserPath); err != nil {
		t.Fatal("a refused merge must not move the file:", err)
	}
}

// A duplicate often carries the genre or date the keeper lost. Merging fills
// the keeper's empty fields from the other copy (Python duplicates.py parity),
// never overwriting a value the keeper already has (P0-3/F9).
func TestDuplicateMergeFillsMissingTagsFromTheDuplicate(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	first := genTestFLAC(t, incoming, "fill-a.flac")
	second := genTestFLAC(t, incoming, "fill-b.flac")
	if err := flactags.Write(first, flactags.Tags{
		Title: "Song A", Artists: []string{"Artist"}, Album: "Album", AlbumArtist: "Artist", TrackNumber: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := flactags.Write(second, flactags.Tags{
		Title: "Song B", Artists: []string{"Artist"}, Album: "Album", AlbumArtist: "Artist", TrackNumber: 1,
		Genre: "Ambient", Date: "2019-01-02",
	}); err != nil {
		t.Fatal(err)
	}
	keep := releaseLibraryFile(t, application, "fill-a.flac", "fill-1", "Artist", "Album", "Song", 1)
	otherFile := "Artist/Album/01 Song.duplicate.flac"
	otherPath := filepath.Join(application.Dir, "library-audio", filepath.FromSlash(otherFile))
	if err := os.MkdirAll(filepath.Dir(otherPath), 0o700); err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, secondData, 0o600); err != nil {
		t.Fatal(err)
	}

	// The keeper is the copy without the two fields; the duplicate has them.
	group := &duplicateGroup{
		Keep: &duplicateFile{RequestID: keep.RequestID, File: keep.File, LibraryDir: keep.LibraryDir},
		Files: []duplicateFile{
			{RequestID: keep.RequestID, File: keep.File, LibraryDir: keep.LibraryDir},
			{RequestID: "manual-other", File: otherFile, LibraryDir: keep.LibraryDir},
		},
	}
	filled, err := application.fillMissingTags(group)
	if err != nil {
		t.Fatal(err)
	}
	if len(filled) != 2 || !containsStringValue(filled, "genre") || !containsStringValue(filled, "date") {
		t.Fatal(filled)
	}
	tags := readTags(t, filepath.Join(application.Dir, "library-audio", keep.File))
	if tags.Genre != "Ambient" || tags.Date != "2019-01-02" {
		t.Fatal(tags.Genre, tags.Date)
	}
	// A second run has nothing left to fill and never rewrites the file.
	again, err := application.fillMissingTags(group)
	if err != nil || len(again) != 0 {
		t.Fatal(again, err)
	}

	// The merge planner uses the same fillMissingTags path before moving any
	// loser, so the quality winner keeps the duplicate-only metadata.
}

func containsStringValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestDuplicateAcrossTrackSlotsWithSameTitleAndPCM(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "a.wav"), 440)
	writeToneWAV(t, filepath.Join(incoming, "b.wav"), 440)
	releaseLibraryFile(t, application, "a.wav", "slot-one", "Artist", "Album", "Song", 1)
	releaseLibraryFile(t, application, "b.wav", "slot-two", "Artist", "Album", "Artist - Song", 7)
	groups := duplicateGroups(t, application)
	if len(groups) != 1 || groups[0].State != "ready" || len(groups[0].Files) != 2 {
		t.Fatal(groups)
	}
}

func TestDuplicateMergeDoesNotRequireQBittorrentForStreamingImports(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "local-a.wav"), 440)
	writeFLACFromWAV(t, filepath.Join(incoming, "local-a.wav"), filepath.Join(incoming, "local-a.flac"))
	writeToneWAV(t, filepath.Join(incoming, "local-b.wav"), 440)
	releaseLibraryFile(t, application, "local-a.flac", "local-1", "Artist", "Album", "Song", 1)
	loser := releaseLibraryFile(t, application, "local-b.wav", "local-2", "Artist", "Album", "Song", 1)
	loserPath := filepath.Join(application.Dir, "library-audio", loser.File)
	post(t, application, "/v1/settings/qbittorrent", map[string]any{"base_url": "http://127.0.0.1:1", "username": "user", "password": "pass"})
	groups := duplicateGroups(t, application)
	if len(groups) != 1 || groups[0].State != "ready" {
		t.Fatal(groups)
	}
	code, decoded := mediaPost(t, application, "/v1/library/duplicates/merge", map[string]any{"group_id": groups[0].GroupID})
	if code != http.StatusOK || decoded["ok"] != true {
		t.Fatalf("local duplicate merge code=%d body=%v", code, decoded)
	}
	if _, err := os.Stat(loserPath); !os.IsNotExist(err) {
		t.Fatal("local duplicate was not quarantined", err)
	}
}
