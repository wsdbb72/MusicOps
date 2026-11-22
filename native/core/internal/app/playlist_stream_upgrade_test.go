package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

func TestPlaylistStreamLossyUpgradeUsesFLACTargetAndRetiresOldSource(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})

	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(folder, "01 Song.mp3")
	newPath := filepath.Join(folder, "01 Song.flac")
	if err := os.WriteFile(oldPath, []byte("ID3 lossy source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, oneSecondFLAC(t, "Artist", "Song"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := application.indexReleasedPath(oldPath); err != nil {
		t.Fatal(err)
	}
	if err := application.indexReleasedPath(newPath); err != nil {
		t.Fatal(err)
	}

	plan := SearchPlan{Scene: "upgrade", LibraryTracks: []localWorkflowTrack{{Path: oldPath}}}
	if got := playlistStreamTargetPath(plan); got != newPath {
		t.Fatalf("target=%q want=%q", got, newPath)
	}
	if got := playlistStreamAuthorizedReplacement(plan); got != "" {
		t.Fatalf("cross-format target must not authorize same-path replacement, got %q", got)
	}
	if err := application.removeSupersededLibrarySource(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("lossy source survived cleanup: %v", err)
	}
	if info, err := os.Stat(newPath); err != nil || info.Size() == 0 {
		t.Fatalf("lossless target missing after cleanup: %v", err)
	}

	entries, err := audiorelease.List(application.engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if sameLibraryPath(entry.File, oldPath) {
			t.Fatalf("retired source remains indexed: %+v", entry)
		}
	}

	// Cleanup is retried after an interrupted job: both a missing source and
	// an already-removed index entry are successful no-ops.
	if err := application.removeSupersededLibrarySource(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
}

func TestPlaylistStreamFLACReplacementStaysPinnedToTheOriginalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Artist", "Album", "08 Song.flac")
	plan := SearchPlan{Scene: "upgrade", LibraryTracks: []localWorkflowTrack{{Path: path}}}
	if got := playlistStreamTargetPath(plan); got != path {
		t.Fatalf("target=%q want=%q", got, path)
	}
	if got := playlistStreamAuthorizedReplacement(plan); got != path {
		t.Fatalf("replacement=%q want=%q", got, path)
	}
	if got := playlistStreamTargetPath(SearchPlan{Scene: "expand", LibraryTracks: []localWorkflowTrack{{Path: path}}}); got != "" {
		t.Fatalf("expand unexpectedly chose a replacement target: %q", got)
	}
}

func TestSingleTrackAlbumStreamingFallbackCannotReplaceTheExistingTrack(t *testing.T) {
	plan := SearchPlan{Scene: "expand", LibraryTracks: []localWorkflowTrack{{Path: "Artist/Album/01 Song.flac"}}}
	if !singleTrackAlbumStreamingCannotComplete(plan, false) {
		t.Fatal("ordinary single-track streaming fallback cannot complete an album and must terminate instead of publishing the local track again")
	}
	if singleTrackAlbumStreamingCannotComplete(plan, true) {
		t.Fatal("NetEase album-level fallback must remain eligible")
	}
	plan.Scene = "upgrade"
	if singleTrackAlbumStreamingCannotComplete(plan, false) {
		t.Fatal("explicit quality upgrades must remain eligible for streaming")
	}
	if singleTrackAlbumStreamingCannotComplete(SearchPlan{Scene: "expand"}, false) {
		t.Fatal("album expansion without an existing library track must keep the ordinary fallback")
	}
}
