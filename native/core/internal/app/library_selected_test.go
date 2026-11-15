package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"

	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
	"path/filepath"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
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
	job := waitJob(t, application, id, "needs_review")
	raw, found, err := application.tasks.Data(job.ActiveTaskID, "arguments", 0)
	var plan SearchPlan
	if err != nil || !found || json.Unmarshal(raw, &plan) != nil || len(plan.LibraryTracks) != 1 || plan.ExpectedTitle != "Example Album" {
		t.Fatal(plan, found, err)
	}
}
