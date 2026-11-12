package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With a dedicated staging directory the upload, the import source and the
// library copy are three different roots: staging on the SSD, library on the
// HDD. Nothing is moved after the fact, so a task that already started keeps
// writing where it started.
func TestWorkDirStagesOnTheConfiguredDisk(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "library")
	work := filepath.Join(t.TempDir(), "ssd-work")
	saved := postCode(t, application, "/v1/settings/library", map[string]any{
		"library_dir": library, "work_dir": work,
	})
	if saved.Code != http.StatusOK {
		t.Fatal(saved.Code, saved.Body.String())
	}
	decoded := decodeBody(t, saved)
	info, _ := decoded["library"].(map[string]any)
	if info["effective_work_dir"] != work || info["effective_library_dir"] != library {
		t.Fatal(info)
	}
	if application.workDir() != work || application.libraryDir() != library {
		t.Fatal("the settings did not take effect:", application.workDir(), application.libraryDir())
	}

	incoming := filepath.Join(work, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	writeToneWAV(t, filepath.Join(incoming, "ssd.wav"), 440)

	// The API resolves "incoming/x" against the work directory now.
	recorder := postRaw(t, application, "/v1/open/library/import", map[string]any{
		"source": "incoming/ssd.wav", "artist": "Artist", "album": "Album", "title": "Song", "track": 1,
	})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	placed, _ := decodeBody(t, recorder)["path"].(string)
	if !strings.HasPrefix(placed, library) {
		t.Fatal("the verified copy did not land in the library:", placed)
	}
	if _, err := os.Stat(placed); err != nil {
		t.Fatal(err)
	}
	// The staging copy stays where it was: releasing never moves the source.
	if _, err := os.Stat(filepath.Join(incoming, "ssd.wav")); err != nil {
		t.Fatal("the staging copy disappeared:", err)
	}
}

// A staging directory inside the library would put partial downloads where the
// library scan and dedup walk expect finished files, so it is refused.
func TestWorkDirCannotOverlapTheLibrary(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "library")
	if recorder := postCode(t, application, "/v1/settings/library", map[string]any{"library_dir": library}); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	for _, work := range []string{
		filepath.Join(library, "work"),
		library,
		filepath.Dir(library),
	} {
		recorder := postCode(t, application, "/v1/settings/library", map[string]any{"library_dir": library, "work_dir": work})
		if recorder.Code != http.StatusBadRequest {
			t.Fatal("overlapping work dir accepted:", work, recorder.Code, recorder.Body.String())
		}
	}
	if recorder := postCode(t, application, "/v1/settings/library", map[string]any{"library_dir": library, "work_dir": "relative/work"}); recorder.Code != http.StatusBadRequest {
		t.Fatal("a relative work dir was accepted:", recorder.Code)
	}
	if application.workDir() != application.Dir {
		t.Fatal("a refused setting changed the effective work dir:", application.workDir())
	}
}
