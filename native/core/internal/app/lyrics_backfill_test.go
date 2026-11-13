package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

// backfillTestLibrary builds three tracks: one with the old
// "<audio>.flac.lrc" name, one with only a LYRICS tag, one already correct.
func backfillTestLibrary(t *testing.T, application *App) (library string, legacy, tagged, done string) {
	t.Helper()
	library = filepath.Join(application.Dir, "lib-lyrics")
	album := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	audio := flacWithDuration(t, 120)
	makeTrack := func(name string, tags flactags.Tags) string {
		path := filepath.Join(album, name)
		if err := os.WriteFile(path, audio, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := flactags.Write(path, tags); err != nil {
			t.Fatal(err)
		}
		return path
	}
	legacy = makeTrack("01 legacy.flac", flactags.Tags{Title: "Legacy"})
	if err := os.WriteFile(legacy+".lrc", []byte("[00:01.00]old name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tagged = makeTrack("02 tagged.flac", flactags.Tags{Title: "Tagged", Lyrics: "[00:02.00]from tag"})
	done = makeTrack("03 done.flac", flactags.Tags{Title: "Done"})
	if err := os.WriteFile(strings.TrimSuffix(done, filepath.Ext(done))+".lrc", []byte("[00:03.00]already fine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	return library, legacy, tagged, done
}

// runBackfill starts a pass and waits for it, the way the page does.
func runBackfill(t *testing.T, application *App, dryRun bool) map[string]any {
	t.Helper()
	post(t, application, "/v1/library/lyrics/backfill", map[string]any{"dry_run": dryRun})
	for i := 0; i < 200; i++ {
		status := getAuth(t, application, "/v1/library/lyrics/backfill")
		if status.Code != 200 {
			t.Fatal(status.Code, status.Body.String())
		}
		var body map[string]any
		if err := jsonUnmarshalString(status.Body.String(), &body); err != nil {
			t.Fatal(err)
		}
		if running, _ := body["running"].(bool); !running {
			report, _ := body["report"].(map[string]any)
			return report
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the backfill did not finish")
	return nil
}

func TestLyricsBackfillRenamesAndWrites(t *testing.T) {
	application := newApp(t)
	_, legacy, tagged, done := backfillTestLibrary(t, application)

	// A dry run counts without touching a single file.
	report := runBackfill(t, application, true)
	if report["renamed"] != float64(1) || report["written"] != float64(1) || report["kept"] != float64(1) {
		t.Fatal(report)
	}
	if _, err := os.Stat(legacy + ".lrc"); err != nil {
		t.Fatal("a dry run renamed the legacy sidecar")
	}
	if _, err := os.Stat(strings.TrimSuffix(tagged, filepath.Ext(tagged)) + ".lrc"); err == nil {
		t.Fatal("a dry run wrote a sidecar")
	}

	report = runBackfill(t, application, false)
	if report["renamed"] != float64(1) || report["written"] != float64(1) || report["kept"] != float64(1) {
		t.Fatal(report)
	}
	// The old name became the standard one, content untouched.
	if _, err := os.Stat(legacy + ".lrc"); !os.IsNotExist(err) {
		t.Fatal("the legacy sidecar was left behind")
	}
	moved, err := os.ReadFile(strings.TrimSuffix(legacy, filepath.Ext(legacy)) + ".lrc")
	if err != nil || !strings.Contains(string(moved), "old name") {
		t.Fatal(err, string(moved))
	}
	// The tag was copied into the standard sidecar.
	written, err := os.ReadFile(strings.TrimSuffix(tagged, filepath.Ext(tagged)) + ".lrc")
	if err != nil || !strings.Contains(string(written), "from tag") {
		t.Fatal(err, string(written))
	}
	// An existing standard sidecar is never overwritten.
	kept, err := os.ReadFile(strings.TrimSuffix(done, filepath.Ext(done)) + ".lrc")
	if err != nil || !strings.Contains(string(kept), "already fine") {
		t.Fatal(err, string(kept))
	}
}

// A second pass finds nothing to do.
func TestLyricsBackfillIsIdempotent(t *testing.T) {
	application := newApp(t)
	backfillTestLibrary(t, application)
	runBackfill(t, application, false)
	report := runBackfill(t, application, true)
	if report["renamed"] != float64(0) || report["written"] != float64(0) || report["kept"] != float64(3) {
		t.Fatal(report)
	}
}

func jsonUnmarshalString(raw string, out any) error {
	return json.Unmarshal([]byte(raw), out)
}
