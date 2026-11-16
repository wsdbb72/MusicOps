package app

import (
	"bytes"
	"context"
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
	newName, title, result, err := a.scrapePlaylistIncoming(context.Background(), dir, name, "Artist", "Album", "Example", 1)
	if err != nil || newName == name || title != "Example" || result == nil {
		t.Fatal(newName, title, result, err)
	}
	tags, err := flactags.ReadFile(filepath.Join(dir, newName))
	if err != nil || tags.Title != "Example" || tags.Album != "Album" {
		t.Fatal(tags, err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, newName))
	if _, _, _, err := a.scrapePlaylistIncoming(context.Background(), dir, name, "Artist", "Album", "Example", 1); err != nil {
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
