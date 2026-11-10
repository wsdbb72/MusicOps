package smoke

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"

	_ "modernc.org/sqlite"
)

// TestPackagedLegacyMigrate runs the packaged core's migrate command
// against a synthetic Python profile shaped like a 0.4.31 config store.
// The sqlite file is not a copy of anyone's library. The test checks
// that the original database is left unchanged, settings land in the
// native store, and a second run does not migrate again.
func TestPackagedLegacyMigrate(t *testing.T) {
	bundle := os.Getenv("MUSICOPS_SMOKE_BUNDLE")
	if bundle == "" {
		t.Skip("MUSICOPS_SMOKE_BUNDLE is not set")
	}
	cfg := Config{BundleDir: bundle}
	cfg.defaults()
	binary := filepath.Join(bundle, cfg.binRel(cfg.CoreName))
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	stateDir := filepath.Join(profile, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(t.TempDir(), "library")
	sqlitePath := filepath.Join(stateDir, "musicops.sqlite")
	writeSyntheticPythonConfig(t, sqlitePath, library)
	before := fileSHA256(t, sqlitePath)

	first := runMigrate(t, binary, profile)
	if first.ConfigVersion != 2 || len(first.Skipped) != 0 {
		t.Fatalf("first migrate: %+v", first)
	}
	for _, key := range []string{"app/settings/download", "app/settings/sites", "app/settings/updates"} {
		if !containsString(first.Mapped, key) {
			t.Fatalf("mapped %v missing %s", first.Mapped, key)
		}
	}
	if fileSHA256(t, sqlitePath) != before {
		t.Fatal("migrate rewrote the Python sqlite file")
	}
	rawReport, err := os.ReadFile(filepath.Join(stateDir, "migration-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawReport), "do-not-migrate-secret") {
		t.Fatal("migration report contains the synthetic secret marker")
	}
	assertNativeSettings(t, profile, library)

	second := runMigrate(t, binary, profile)
	if len(second.Skipped) != 1 || second.Skipped[0] != "already migrated" {
		t.Fatalf("second migrate: %+v", second)
	}
	if fileSHA256(t, sqlitePath) != before {
		t.Fatal("second migrate rewrote the Python sqlite file")
	}
	if path := os.Getenv("MUSICOPS_SMOKE_MIGRATE_REPORT"); path != "" {
		if err := os.WriteFile(path, append(rawReport, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type migrateReport struct {
	ConfigVersion int      `json:"config_version"`
	Mapped        []string `json:"mapped"`
	Skipped       []string `json:"skipped"`
}

func writeSyntheticPythonConfig(t *testing.T, sqlitePath, library string) {
	t.Helper()
	payload := map[string]any{
		"version": 2,
		"values": map[string]any{
			"streaming": map[string]any{"enabled": true},
			"providers": map[string]any{
				"open.cd":  map[string]any{"enabled": true},
				"dicmusic": map[string]any{"enabled": true},
				"orpheus":  map[string]any{"enabled": false},
				"redacted": map[string]any{"enabled": false},
			},
			"updates": map[string]any{"enabled": false},
			"paths":   map[string]any{"library": library},
			"offline": false,
			"marker":  "do-not-migrate-secret",
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE config_versions (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO config_versions (id, payload, updated_at) VALUES (?, ?, ?)`, "2", string(encoded), "2026-11-06T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

func runMigrate(t *testing.T, binary, profile string) migrateReport {
	t.Helper()
	cmd := exec.Command(binary, "migrate", "--base", profile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("migrate: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	var report migrateReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("migrate output: %v\n%s", err, stdout.String())
	}
	return report
}

func assertNativeSettings(t *testing.T, profile, library string) {
	t.Helper()
	engine, err := dataengine.Open(filepath.Join(profile, "state", "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	read := func(key string) map[string]any {
		t.Helper()
		var raw []byte
		var ok bool
		if err := engine.View(func(v *dataengine.View) error {
			var err error
			raw, ok, err = v.Get([]byte(key))
			return err
		}); err != nil || !ok {
			t.Fatalf("native %s: ok %v err %v", key, ok, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "do-not-migrate-secret") {
			t.Fatalf("native %s contains the synthetic secret marker", key)
		}
		return decoded
	}
	download := read("app/settings/download")
	if download["streaming_enabled"] != true {
		t.Fatalf("streaming_enabled = %v", download["streaming_enabled"])
	}
	sites := read("app/settings/sites")
	enabled, _ := sites["enabled_sites"].([]any)
	if len(enabled) != 2 || enabled[0] != "open.cd" || enabled[1] != "dicmusic" {
		t.Fatalf("enabled_sites = %v", enabled)
	}
	updates := read("app/settings/updates")
	if updates["enabled"] != false {
		t.Fatalf("updates.enabled = %v", updates["enabled"])
	}
	librarySettings := read("app/settings/library")
	if librarySettings["library_dir"] != library {
		t.Fatalf("library_dir = %v", librarySettings["library_dir"])
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
