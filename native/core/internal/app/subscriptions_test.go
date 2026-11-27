package app

import (
	"testing"
	"time"
)

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

func TestSubscriptionAlbumCompletionExpandsSingleTrackPlan(t *testing.T) {
	application := newApp(t)
	jobID := "sub-netease-590536461-complete-album"
	tracks := []playlistTrack{{
		Artist: "Artist", Title: "Song", Album: "Album", Duration: "180",
		NeteaseSongID: 42, NeteaseAlbumID: 84,
	}}
	if err := application.queueSubscriptionAlbum(jobID, "netease-590536461", "Album", tracks, true); err != nil {
		t.Fatal(err)
	}
	job, err := application.flows.Job(jobID)
	if err != nil || job == nil || job.ActiveTaskID == "" {
		t.Fatal(job, err)
	}
	task, err := application.tasks.Get(job.ActiveTaskID)
	if err != nil || task == nil {
		t.Fatal(task, err)
	}
	raw, found, err := application.tasks.Data(task.ID, "arguments", 0)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	plan, err := decodePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NeteaseOnly || plan.NeteaseTrackOnly || plan.NeteaseAlbumID != 84 || plan.NeteaseSongID != 42 {
		t.Fatalf("album completion plan was reduced to one track: %+v", plan)
	}
}

func TestManualSubscriptionCheckBypassesAlbumRetryAndKeepsFullAlbumPlan(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	if response := post(t, application, "/v1/settings/library", map[string]any{"library_dir": library}); response["ok"] != true {
		t.Fatal(response)
	}
	indexPlaylistTestFile(t, application, library,
		"Compilation/Artist A - Song One.flac",
		"Artist A", "Compilation", "Song One")
	application.invalidatePlaylistLibraryCache()

	playlistID := "netease-590536461-manual"
	track := playlistTrack{
		Artist: "Artist A", Album: "Single Album", Title: "Song One", Duration: "200",
		NeteaseSongID: 12, NeteaseAlbumID: 34,
	}
	tracks := []playlistTrack{track}
	if err := application.writeSavedPlaylist(playlistID, "Album target", tracks); err != nil {
		t.Fatal(err)
	}
	key := trackKey(track)
	future := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	lastChecked := time.Now().UTC().Format(time.RFC3339)
	if err := application.storeSubscriptions([]playlistSubscription{{
		PlaylistID: playlistID, Name: "Album target", IntervalHours: 24, IncludeExisting: true,
		Seen: []string{key}, RetryMissing: map[string]string{key: future},
		AlbumRetry: map[string]string{key: future}, LastChecked: lastChecked,
	}}); err != nil {
		t.Fatal(err)
	}

	automatic, err := application.runSubscriptionCheck(playlistID, false)
	if err != nil || automatic["queued"] != 0 {
		t.Fatalf("automatic check ignored album retry: result=%+v err=%v", automatic, err)
	}
	manual, err := application.runSubscriptionCheck(playlistID, true)
	if err != nil || manual["queued"] != 1 {
		t.Fatalf("manual check did not bypass album retry: result=%+v err=%v", manual, err)
	}
	ids := manual["job_ids"].([]string)
	job, err := application.flows.Job(ids[0])
	if err != nil || job == nil || job.ActiveTaskID == "" {
		t.Fatal(job, err)
	}
	task, err := application.tasks.Get(job.ActiveTaskID)
	if err != nil || task == nil {
		t.Fatal(task, err)
	}
	raw, found, err := application.tasks.Data(task.ID, "arguments", 0)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	plan, err := decodePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NeteaseOnly || plan.NeteaseTrackOnly || plan.NeteaseAlbumID != 34 || plan.NeteaseSongID != 12 {
		t.Fatalf("manual album completion was reduced to one track: %+v", plan)
	}
}
