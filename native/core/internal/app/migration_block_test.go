package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A damaged legacy profile must not make the new build unstartable. Before the
// fix, both the config migration and the live import returned an error from
// Open, so the core process exited before serving anything and the user had no
// way to reach the migration page (P0-4).
func TestBrokenLegacyProfileDoesNotBlockStartup(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	if err := os.MkdirAll(filepath.Join(profile, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(profile, "state", "musicops.sqlite")
	if err := os.WriteFile(legacy, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(filepath.Join(profile, "state", "native"), filepath.Join(root, "backups"), "test-password-16b")
	if err != nil {
		t.Fatalf("startup was blocked by a broken legacy profile: %v", err)
	}
	t.Cleanup(func() { application.Close() })
	recorder := getAuth(t, application, "/v1/migration")
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"failed":true`) || !strings.Contains(body, "旧库导入失败") {
		t.Fatal(body)
	}
	// The old database is input only: it must still hold the same bytes.
	data, err := os.ReadFile(legacy)
	if err != nil || string(data) != "this is not a sqlite database" {
		t.Fatal("the legacy database was modified")
	}
}

// A healthy profile leaves the failure flag clear, so the page keeps its
// current "nothing to migrate" behaviour.
func TestHealthyProfileReportsNoMigrationFailure(t *testing.T) {
	application := newApp(t)
	recorder := getAuth(t, application, "/v1/migration")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"failed":false`) {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}
