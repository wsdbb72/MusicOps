package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"

	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
	"path/filepath"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestUpgradeSelectsOnlyMatchingRealFLAC(t *testing.T) {
	plan := downloadPlan{UpgradeTracks: []localWorkflowTrack{{Title: "Song", Artist: "Artist", Album: "Album", Codec: "mp3"}}}
	dir := t.TempDir()
	raw, err := flactags.WriteBytes(syntheticFLAC(t, false), flactags.Tags{Title: "Song", Artist: "Artist", Album: "Album"})
	if err != nil {
		t.Fatal(err)
	}
	tags, err := flactags.ReadBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	plan.UpgradeTracks[0].Duration = float64(tags.StreamInfo.TotalSamples) / float64(tags.StreamInfo.SampleRate)
	if err = os.WriteFile(filepath.Join(dir, "song.flac"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	set := qbittorrent.Metainfo{Files: []qbittorrent.MetainfoFile{{Path: "song.flac"}, {Path: "other.mp3"}}}
	got, err := selectUpgradeTracks(plan, set, dir)
	if err != nil || len(got) != 1 || !got["song.flac"] {
		t.Fatal(got, err)
	}
	plan.UpgradeTracks[0].Duration += 30
	if _, err := selectUpgradeTracks(plan, set, dir); err == nil {
		t.Fatal("a different recording was accepted")
	}
	plan.UpgradeTracks[0].Codec = "invalid"
	plan.UpgradeTracks[0].Duration = 0
	got, err = selectUpgradeTracks(plan, set, dir)
	if err != nil || len(got) != 1 || !got["song.flac"] {
		t.Fatalf("corrupt source without duration did not select the exact FLAC: %v %v", got, err)
	}
}

func TestUpgradeDoesNotTreatContainerChangeAsImprovement(t *testing.T) {
	plan := SearchPlan{Scene: "upgrade", LibraryTracks: []localWorkflowTrack{{Codec: "flac"}}}
	if localUpgradeAdmits(plan, reviewmatch.Hit{Format: "FLAC"}) {
		t.Fatal("unproven upgrade")
	}
	plan.LibraryTracks[0].Codec = "mp3"
	if localUpgradeAdmits(plan, reviewmatch.Hit{Format: "MP3"}) || !localUpgradeAdmits(plan, reviewmatch.Hit{Format: "FLAC"}) {
		t.Fatal("format gate failed")
	}
	plan.LibraryTracks[0].Codec = "invalid"
	if !localUpgradeAdmits(plan, reviewmatch.Hit{Format: "FLAC"}) {
		t.Fatal("corrupt source was not eligible for exact replacement")
	}
}

func TestLossyNeteaseFLACContainerRemainsUpgradeEligible(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	path := filepath.Join(t.TempDir(), "song.flac")
	command := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-metadata", "artist=Artist", "-metadata", "album=Album", "-metadata", "title=Song", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate FLAC: %v %s", err, output)
	}
	tags, err := flactags.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tags.Raw["COMMENT"] = []string{"MusicOps source=netease-lossy"}
	if err := flactags.Write(path, *tags); err != nil {
		t.Fatal(err)
	}
	track, err := probeLocalWorkflowTrack(context.Background(), path)
	if err != nil || track.Codec != "flac-lossy" || !isLossyUpgradeSource(track.Codec) {
		t.Fatal(track, err)
	}
}

func TestLocalWorkflowIdentityFromPathRecoversRepairCandidates(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		artist, album string
		title         string
	}{
		{
			name: "flat artist album folder", path: filepath.Join("library", "Katy Perry - Prism", "09 Ghost.flac"),
			artist: "Katy Perry", album: "Prism", title: "Ghost",
		},
		{
			name: "nested artist album folder", path: filepath.Join("library", "周慧敏", "离开忧郁的习惯", "周慧敏 - 陪在你身边.flac"),
			artist: "周慧敏", album: "离开忧郁的习惯", title: "陪在你身边",
		},
		{
			name: "compilation album folder", path: filepath.Join("library", "Various Artists - One Day OST", "06. Rocks.flac"),
			artist: "Various Artists", album: "One Day OST", title: "Rocks",
		},
		{
			name: "track number without separating space", path: filepath.Join("library", "苏打绿 - 十年一刻", "16.I Don't Care (苏打绿版).flac"),
			artist: "苏打绿", album: "十年一刻", title: "I Don't Care (苏打绿版)",
		},
		{
			name: "per-track artist in compilation name", path: filepath.Join("library", "Various - Kamen Rider", "01. Artist - Song.flac"),
			artist: "Artist", album: "Kamen Rider", title: "Song",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artist, album, title, ok := localWorkflowIdentityFromPath(test.path)
			if !ok || artist != test.artist || album != test.album || title != test.title {
				t.Fatalf("identity=(%q,%q,%q,%v)", artist, album, title, ok)
			}
		})
	}
}

func TestCorruptFLACPathStillCreatesUpgradeIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Artist - Album")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "03 Song.flac")
	if err := os.WriteFile(path, make([]byte, 128), 0o600); err != nil {
		t.Fatal(err)
	}
	track, err := localWorkflowTrackFromPath(path, "invalid", 0)
	if err != nil || track.Artist != "Artist" || track.Album != "Album" || track.Title != "Song" || track.Codec != "invalid" {
		t.Fatalf("track=%+v err=%v", track, err)
	}
	if !isLossyUpgradeSource(track.Codec) {
		t.Fatal("corrupt FLAC was not eligible for replacement")
	}
}

func TestTransientProbeFailureIsNotTreatedAsCorruption(t *testing.T) {
	for _, message := range []string{"Access is denied.", "sharing violation", "The process cannot access the file because it is being used by another process."} {
		if !transientAudioProbeFailure(errors.New(message)) {
			t.Fatalf("transient failure was treated as corruption: %q", message)
		}
	}
	if transientAudioProbeFailure(errors.New("invalid data found when processing input")) {
		t.Fatal("a genuine decode failure was treated as transient")
	}
}

func TestLibrarySelectionBuildsDurableSnapshotPlan(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	music := httptest.NewServer(http.NotFoundHandler())
	defer music.Close()
	client := musicbrainz.NewClient()
	client.BaseURL = music.URL
	withCatalogClient(t, client)
	application := newApp(t)
	library := t.TempDir()
	path := filepath.Join(library, "song.flac")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-metadata", "artist=Example Artist", "-metadata", "album=Example Album", "-metadata", "title=Example Song", path).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	r := post(t, application, "/v1/library/workflow", map[string]any{"scene": "expand", "path": path, "confirmed": true})
	id, _ := r["job_id"].(string)
	if id == "" || r["started"] != true || r["downloaded"] != false {
		t.Fatal(r)
	}
	deadline := time.Now().Add(8 * time.Second)
	var job *workflow.Job
	for time.Now().Before(deadline) {
		job, _ = application.flows.Job(id)
		if job != nil && job.SearchJobID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job == nil || job.SearchJobID == "" {
		t.Fatalf("local identity did not advance to search: %+v", job)
	}
	raw, found, err := application.tasks.Data(job.SearchJobID, "arguments", 0)
	var plan SearchPlan
	if err != nil || !found || json.Unmarshal(raw, &plan) != nil || len(plan.LibraryTracks) != 1 || plan.ExpectedTitle != "Example Album" {
		t.Fatal(plan, found, err)
	}
	if !plan.Identity.IdentityConfirmed || plan.Identity.Source != "local-library" || !plan.StreamingEnabled {
		t.Fatalf("local identity plan=%+v", plan.Identity)
	}
}
