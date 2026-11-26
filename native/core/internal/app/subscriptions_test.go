package app

import "testing"

func TestSubscriptionGroupsAlbumsByStableAlbumIdentity(t *testing.T) {
	guns := playlistTrack{Artist: "Guns N' Roses", Title: "Don't Cry", Album: "Greatest Hits", NeteaseAlbumID: 100}
	gunsOther := playlistTrack{Artist: "Axel Rose", Title: "Paradise City", Album: "Greatest Hits", NeteaseAlbumID: 100}
	joan := playlistTrack{Artist: "Joan Jett", Title: "I Hate Myself", Album: "Greatest Hits", NeteaseAlbumID: 200}

	firstKey, firstLabel := subscriptionTrackGroup(guns)
	secondKey, _ := subscriptionTrackGroup(gunsOther)
	otherKey, otherLabel := subscriptionTrackGroup(joan)
	if firstKey != secondKey || firstKey == otherKey {
		t.Fatalf("album-id groups first=%q second=%q other=%q", firstKey, secondKey, otherKey)
	}
	if firstLabel != "Greatest Hits" || otherLabel != "Greatest Hits" {
		t.Fatalf("labels first=%q other=%q", firstLabel, otherLabel)
	}
}

func TestSubscriptionAlbumNameCollisionIncludesArtistWithoutAlbumID(t *testing.T) {
	guns := playlistTrack{Artist: "Guns N' Roses", Album: "Greatest Hits", Title: "Don't Cry"}
	joan := playlistTrack{Artist: "Joan Jett", Album: "Greatest Hits", Title: "I Hate Myself"}
	firstKey, firstLabel := subscriptionTrackGroup(guns)
	secondKey, secondLabel := subscriptionTrackGroup(joan)
	if firstKey == secondKey {
		t.Fatalf("different artists with the same album name shared a group: %q", firstKey)
	}
	if firstLabel != "Guns N' Roses — Greatest Hits" || secondLabel != "Joan Jett — Greatest Hits" {
		t.Fatalf("labels first=%q second=%q", firstLabel, secondLabel)
	}
}

func TestSubscriptionMissingAlbumIdentityDoesNotMixTracks(t *testing.T) {
	first := playlistTrack{Title: "One", Album: "Greatest Hits"}
	second := playlistTrack{Title: "Two", Album: "Greatest Hits"}
	firstKey, _ := subscriptionTrackGroup(first)
	secondKey, _ := subscriptionTrackGroup(second)
	if firstKey == secondKey {
		t.Fatalf("unknown artist and no album id shared a group: %q", firstKey)
	}
}

func TestSubscriptionRequiresTargetAlbumCompletionWhenTrackExistsElsewhere(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	indexPlaylistTestFile(t, application, library,
		"Compilation/Artist A - Song One.flac",
		"Artist A", "Compilation", "Song One")
	application.invalidatePlaylistLibraryCache()
	index := application.playlistLibrary()
	track := playlistTrack{Artist: "Artist A", Album: "Single Album", Title: "Song One", Duration: "200", NeteaseSongID: 12}
	if !index.hasPlaylistRow(track) {
		t.Fatal("test recording is not present in the compilation")
	}
	if !playlistTrackNeedsSubscription(index, track) {
		t.Fatal("a recording in another release hid the missing target album from the subscription")
	}
}
