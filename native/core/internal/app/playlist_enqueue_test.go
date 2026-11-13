package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrackKeyOfPath(t *testing.T) {
	artist, album, title := trackKeyOfPath(`E:\music\周杰伦\范特西\03 简单爱.flac`)
	if artist != "周杰伦" || album != "范特西" || title != "简单爱" {
		t.Fatalf("%q %q %q", artist, album, title)
	}
	_, _, title = trackKeyOfPath(`E:\music\A\B\01 - Song Name.flac`)
	if title != "song name" {
		t.Fatalf("%q", title)
	}
}

func TestPlaylistLibraryIndexMatchesNames(t *testing.T) {
	index := playlistLibraryIndex{tracks: map[string]bool{}, albums: map[string]bool{}}
	artist, album, title := trackKeyOfPath(`E:\music\Artist A\Album X\01 - Song One.flac`)
	index.tracks[artist+"/"+title] = true
	index.albums[artist+"/"+album] = true
	if !index.hasTrack("Artist A", "Song One") {
		t.Fatal("track not matched")
	}
	if !index.hasAlbum("Artist A", "Album X") {
		t.Fatal("album not matched")
	}
	if index.hasTrack("Artist A", "Song Two") {
		t.Fatal("false positive")
	}
}

func TestPlaylistSelection(t *testing.T) {
	if got := playlistSelection(3, nil); len(got) != 3 {
		t.Fatal(got)
	}
	got := playlistSelection(5, []int{3, 1, 3, 9, -1})
	if len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatal(got)
	}
}

// Rows from one album become one job: the pipeline works per album, and a
// partially present album is completed rather than patched track by track.
func TestEnqueueGroupsRowsByAlbum(t *testing.T) {
	application := newApp(t)
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No catalogue answer: identity stays unconfirmed and the job parks at
		// the metadata checkpoint, which is the manual fallback.
		http.NotFound(w, r)
	}))
	defer catalog.Close()
	rows := []playlistTrack{
		{Artist: "Artist A", Title: "Song One", Album: "Album X"},
		{Artist: "Artist A", Title: "Song Two", Album: "Album X"},
		{Artist: "Artist B", Title: "Song Nine", Album: ""},
	}
	report, err := application.enqueuePlaylistTracks(context.Background(), "pl-test-group", rows, []int{0, 1, 2},
		playlistEnqueueOptions{AlbumCompletion: true, CatalogBaseURL: catalog.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 2 || report.Albums != 1 || report.Tracks != 3 {
		t.Fatalf("%+v", report)
	}
	first, err := application.flows.Job(report.Created[0])
	if err != nil {
		t.Fatal(err)
	}
	if first.Scene != "expand" || first.Stage != "identifying" || first.Label != "Artist A - Album X" {
		t.Fatalf("%+v", first)
	}
	second, err := application.flows.Job(report.Created[1])
	if err != nil {
		t.Fatal(err)
	}
	if second.Label != "Artist B - Song Nine" {
		t.Fatalf("%+v", second)
	}
}
