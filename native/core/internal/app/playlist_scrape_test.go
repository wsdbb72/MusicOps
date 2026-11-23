package app

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

func TestPlaylistScrapePreservesInputAndStagesReplay(t *testing.T) {
	a := newApp(t)
	dir := t.TempDir()
	name := "01 Example.flac"
	original := syntheticFLAC(t, false)
	source := filepath.Join(dir, name)
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	newName, title, result, err := a.scrapePlaylistIncoming(context.Background(), dir, name, "Artist", "", "Album", "Example", 1)
	if err != nil || newName == name || title != "Example" || result == nil {
		t.Fatal(newName, title, result, err)
	}
	tags, err := flactags.ReadFile(filepath.Join(dir, newName))
	if err != nil || tags.Title != "Example" || tags.Album != "Album" {
		t.Fatal(tags, err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, newName))
	if _, _, _, err := a.scrapePlaylistIncoming(context.Background(), dir, name, "Artist", "", "Album", "Example", 1); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, newName))
	raw, _ := os.ReadFile(source)
	if !bytes.Equal(before, after) || !bytes.Equal(raw, original) {
		t.Fatal("staging replay changed an existing file")
	}
}

func TestPlaylistAlbumFillMatchesExistingTrackByTags(t *testing.T) {
	a := newApp(t)
	lib := t.TempDir()
	dir := filepath.Join(lib, "Artist", "Album")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "Artist - Track.flac")
	body, err := flactags.WriteBytes(syntheticFLAC(t, false), flactags.Tags{Title: "Track", Artist: "Artist", Album: "Album", TrackNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if added, _, _, err := a.indexLibraryTree(context.Background(), lib); err != nil || added != 1 {
		t.Fatal(added, err)
	}
	if tracks := a.verifiedPlaylistAlbumTracks(lib, "Artist", "Album", "Album"); len(tracks["track"]) != 1 {
		t.Fatal(tracks)
	}
	if err := os.WriteFile(name, append(body, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if tracks := a.verifiedPlaylistAlbumTracks(lib, "Artist", "Album", "Album"); len(tracks["track"]) != 0 {
		t.Fatal("changed file was treated as verified existing audio")
	}
}

func TestPlaylistAlbumFillUsesExpectedFolderWhenAlbumTagIsLocalized(t *testing.T) {
	a := newApp(t)
	lib := t.TempDir()
	post(t, a, "/v1/settings/library", map[string]any{"library_dir": lib})
	expected := "Ref:rain / 眩いばかり"
	dir := filepath.Join(lib, publishComponent("Aimer"), publishComponent(expected))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "01 Ref_rain.flac")
	body, err := flactags.WriteBytes(syntheticFLAC(t, false), flactags.Tags{
		Title: "Ref:rain", Artist: "Aimer", Album: "Ref:rain / Mabayui Bakari", TrackNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.indexReleasedPath(name); err != nil {
		t.Fatal(err)
	}
	tracks := a.verifiedPlaylistAlbumTracks(lib, publishComponent("Aimer"), publishComponent(expected), expected)
	if len(tracks[normalizeKey("Ref:rain")]) != 1 {
		t.Fatal(tracks)
	}
}

func TestPlaylistAlbumFillDoesNotConflateSameTitleDifferentPCM(t *testing.T) {
	dir := t.TempDir()
	source, same, different := filepath.Join(dir, "source.flac"), filepath.Join(dir, "same.flac"), filepath.Join(dir, "different.flac")
	body := syntheticFLAC(t, false)
	body[26] = 1 // nonzero decoded-audio checksum
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(same, body, 0o600); err != nil {
		t.Fatal(err)
	}
	body[26] = 2
	if err := os.WriteFile(different, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if !playlistSamePCM(source, []string{same}) || playlistSamePCM(source, []string{different}) {
		t.Fatal("album completion did not distinguish different audio checksums")
	}
}
func TestHotSlowCompilationKeepsArtistAndPerTrackCover(t *testing.T) {
	a := newApp(t)
	server, _ := lrcAPIFake(t, "", jpegBody(), "", "")
	defer server.Close()
	saveLyricsService(t, a, server.URL, "", "")
	dir := t.TempDir()
	name := "01 Song.flac"
	trackCover := solidJPEG(color.RGBA{R: 20, G: 40, B: 180, A: 255})
	body, err := flactags.WriteBytes(syntheticFLAC(t, false), flactags.Tags{Raw: map[string][]string{
		"TITLE": {"Song"}, "ARTIST": {"Artist"}, "ALBUM": {"Wrong Album"}, "ALBUMARTIST": {"Wrong Album Artist"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	body, err = flactags.EmbedFrontCover(body, trackCover)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, _, _, err := a.scrapePlaylistIncoming(context.Background(), dir, name, "Artist", hotSlowCompilationArtist, hotSlowCompilationAlbum, "Song", 1)
	if err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(dir, staged))
	tags, tagErr := flactags.ReadBytes(body)
	if err != nil || tagErr != nil || tags.AlbumArtist != hotSlowCompilationArtist || tags.Album != hotSlowCompilationAlbum || tags.Artist != "Artist" {
		t.Fatalf("body=%v tags=%+v err=%v tagErr=%v", len(body), tags, err, tagErr)
	}
	if cover := flactags.FrontCover(body); !bytes.Equal(cover, trackCover) {
		t.Fatal("the per-track cover was not retained")
	}
}
func TestHotSlowCompilationDoesNotUseProviderCoverWithoutTrackCover(t *testing.T) {
	a := newApp(t)
	server, _ := lrcAPIFake(t, "", jpegBody(), "", "")
	defer server.Close()
	saveLyricsService(t, a, server.URL, "", "")
	dir := t.TempDir()
	name := "01 Song.flac"
	body := syntheticFLAC(t, false)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, _, _, err := a.scrapePlaylistIncoming(context.Background(), dir, name, "Artist", hotSlowCompilationArtist, hotSlowCompilationAlbum, "Song", 1)
	if err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(dir, staged))
	if err != nil {
		t.Fatal(err)
	}
	if cover := flactags.FrontCover(body); len(cover) != 0 {
		t.Fatal("an unrelated per-track cover leaked into the compilation")
	}
}

func solidJPEG(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			img.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		panic(err)
	}
	return out.Bytes()
}

func TestPlaylistScrapePreservesFullLongTitleInFLACTags(t *testing.T) {
	a := newApp(t)
	dir := t.TempDir()
	name := "long-title.flac"
	if err := os.WriteFile(filepath.Join(dir, name), syntheticFLAC(t, false), 0o600); err != nil {
		t.Fatal(err)
	}
	longTitle := `The Four Seasons, Violin Concerto in F Minor, Op. 8 No. 4, RV 297 "Winter": I. Allegro non molto`
	if len(longTitle) <= 80 {
		t.Fatal("fixture title is not longer than the filename boundary")
	}
	staged, title, _, err := a.scrapePlaylistIncoming(context.Background(), dir, name, "Anne-Sophie Mutter", "Herbert von Karajan", "Vivaldi: The Four Seasons", longTitle, 10)
	if err != nil {
		t.Fatal(err)
	}
	if title != longTitle {
		t.Fatalf("returned title=%q want=%q", title, longTitle)
	}
	tags, err := flactags.ReadFile(filepath.Join(dir, staged))
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != longTitle {
		t.Fatalf("stored title=%q want=%q", tags.Title, longTitle)
	}
	if tags.Artist != "Anne-Sophie Mutter" || tags.Album != "Vivaldi: The Four Seasons" {
		t.Fatalf("stored identity=%+v", tags)
	}
	lib := t.TempDir()
	post(t, a, "/v1/settings/library", map[string]any{"library_dir": lib})
	folder := filepath.Join(lib, publishComponent("Anne-Sophie Mutter"), publishComponent("Vivaldi: The Four Seasons"))
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	libraryPath := filepath.Join(folder, "10 long-title.flac")
	body, err := os.ReadFile(filepath.Join(dir, staged))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libraryPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.indexReleasedPath(libraryPath); err != nil {
		t.Fatal(err)
	}
	probed, err := readFLACTagsPrefix(libraryPath)
	if err != nil || probed.StreamInfo.SampleRate == 0 {
		t.Fatal(probed, err)
	}
	durationMS := int64(probed.StreamInfo.TotalSamples) * 1000 / int64(probed.StreamInfo.SampleRate)
	local := a.verifiedPlaylistAlbumTracks(lib, publishComponent("Anne-Sophie Mutter"), publishComponent("Vivaldi: The Four Seasons"), "Vivaldi: The Four Seasons")
	song := neteaseSong{Name: longTitle, Artists: []string{"Anne-Sophie Mutter"}, DurationMS: int(durationMS)}
	if !verifiedNeteaseTrackPresent(local, &song) {
		t.Fatal("full long title was downloaded and indexed but did not satisfy playlist presence")
	}
	if fileName := publishComponent(title); len(fileName) > 80 {
		t.Fatalf("publish filename exceeds the bounded component: %d", len(fileName))
	}
}
