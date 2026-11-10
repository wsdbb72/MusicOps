package configmigrate

import (
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
)

// fixtureScript builds a real Python profile with the venv interpreter:
// a ConfigManager config with streaming on, two tracker sites enabled and
// updates off, plus two vault references. It is the same code path the
// desktop writes, not a hand-made sqlite file.
const fixtureScript = `
import copy, sys
sys.path.insert(0, sys.argv[2])
from pathlib import Path
from musicops.state.store import StateStore
from musicops.config.schema import ConfigManager
from musicops.clients.vault import Vault

base = Path(sys.argv[1])
store = StateStore(base / "state" / "musicops.sqlite")
mgr = ConfigManager(store, base)
cur = mgr.current()
values = copy.deepcopy(cur["values"])
values["streaming"]["enabled"] = True
values["providers"]["dicmusic"]["enabled"] = True
values["providers"]["open.cd"]["enabled"] = True
values["providers"]["orpheus"]["enabled"] = False
values["providers"]["redacted"]["enabled"] = False
values["updates"]["enabled"] = False
mgr.save(values, cur["version"])
vault = Vault(base)
vault.set("vault://qbt/local/credentials", "qbt-secret", "127.0.0.1")
vault.set("vault://ai/default/key", "ai-secret", "ai.example.invalid")
print("fixture-ok")
`

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "src", "musicops", "__init__.py")); err != nil {
		t.Skipf("repo root not found from %s: %v", wd, err)
	}
	return root
}

func venvPython(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("MUSICOPS_VENV_PYTHON"); p != "" {
		return p
	}
	p := "/home/hatch/.venv-musicops/bin/python"
	if _, err := os.Stat(p); err != nil {
		t.Skip("venv python unavailable; set MUSICOPS_VENV_PYTHON")
	}
	return p
}

// makePythonProfile runs the fixture and returns the profile directory.
func makePythonProfile(t *testing.T) string {
	t.Helper()
	profile := filepath.Join(t.TempDir(), "profile")
	script := filepath.Join(t.TempDir(), "fixture.py")
	if err := os.WriteFile(script, []byte(fixtureScript), 0600); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", filepath.Join(keyDir, "master.key"))
	cmd := exec.Command(venvPython(t), script, profile, filepath.Join(repoRoot(t), "src"))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(repoRoot(t), "src"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python fixture failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "fixture-ok") {
		t.Fatalf("python fixture unexpected output: %s", out)
	}
	return profile
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readNative(t *testing.T, profile, key string) map[string]any {
	t.Helper()
	engine, err := dataengine.Open(filepath.Join(profile, "state", "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	var raw []byte
	var ok bool
	if err := engine.View(func(v *dataengine.View) error {
		var err error
		raw, ok, err = v.Get([]byte(key))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("native key %q missing", key)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMaybeMigrateNoPythonProfile(t *testing.T) {
	profile := t.TempDir()
	report, err := MaybeMigrate(profile)
	if err != nil {
		t.Fatalf("MaybeMigrate(empty): %v", err)
	}
	if len(report.Skipped) != 1 || report.Skipped[0] != "no python profile" {
		t.Fatalf("Skipped = %v, want [no python profile]", report.Skipped)
	}
	if _, err := os.Stat(filepath.Join(profile, "state", "migration-report.json")); !os.IsNotExist(err) {
		t.Fatal("receipt written for a profile with no Python state")
	}
}

func TestMaybeMigratePythonFixture(t *testing.T) {
	profile := makePythonProfile(t)
	sqlite := filepath.Join(profile, "state", "musicops.sqlite")
	vaultFile := filepath.Join(profile, "vault", "secrets.aesgcm")
	beforeSQLite := sha256File(t, sqlite)
	beforeVault := sha256File(t, vaultFile)

	report, err := MaybeMigrate(profile)
	if err != nil {
		t.Fatalf("MaybeMigrate: %v", err)
	}
	if report.ConfigVersion != 2 {
		t.Fatalf("ConfigVersion = %d, want 2 (default config v1 + one save)", report.ConfigVersion)
	}
	wantMapped := []string{"app/settings/download", "app/settings/sites", "app/settings/updates", "app/settings/qbittorrent", "app/settings/ai", "app/settings/library"}
	if len(report.Mapped) != len(wantMapped) {
		t.Fatalf("Mapped = %v, want %v", report.Mapped, wantMapped)
	}
	for i, key := range wantMapped {
		if report.Mapped[i] != key {
			t.Fatalf("Mapped = %v, want %v", report.Mapped, wantMapped)
		}
	}
	wantRefs := []string{"vault://ai/default/key", "vault://qbt/local/credentials"}
	if len(report.VaultRefs) != len(wantRefs) {
		t.Fatalf("VaultRefs = %v, want %v", report.VaultRefs, wantRefs)
	}
	for i, ref := range wantRefs {
		if report.VaultRefs[i] != ref {
			t.Fatalf("VaultRefs = %v, want %v", report.VaultRefs, wantRefs)
		}
	}
	if len(report.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want none", report.Skipped)
	}
	notes := strings.Join(report.Notes, "\n")
	for _, want := range []string{
		"Python 音乐库位于",
		"offline=true",
		"workflow.setup_done=false",
		"本次不搬运音频文件",
		"本次不迁移音频文件与任务数据库",
		"sqlitemigrate",
	} {
		if !strings.Contains(notes, want) {
			t.Fatalf("Notes missing %q:\n%s", want, notes)
		}
	}

	download := readNative(t, profile, "app/settings/download")
	if download["streaming_enabled"] != true {
		t.Fatalf("download settings = %v, want streaming_enabled true", download)
	}
	sites := readNative(t, profile, "app/settings/sites")
	enabled, ok := sites["enabled_sites"].([]any)
	if !ok || len(enabled) != 2 || enabled[0] != "open.cd" || enabled[1] != "dicmusic" {
		t.Fatalf("sites settings = %v, want enabled_sites [open.cd dicmusic]", sites)
	}
	updates := readNative(t, profile, "app/settings/updates")
	if updates["enabled"] != false || updates["last_check_ns"] != float64(0) || updates["last_version"] != "" {
		t.Fatalf("updates settings = %v, want enabled false, last_check_ns 0, last_version empty", updates)
	}

	// The receipt on disk matches the report, and the Python files are
	// byte-for-byte untouched.
	raw, err := os.ReadFile(filepath.Join(profile, "state", "migration-report.json"))
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	var receipt Report
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("receipt JSON: %v", err)
	}
	if receipt.ConfigVersion != 2 || len(receipt.Mapped) != 6 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := sha256File(t, sqlite); got != beforeSQLite {
		t.Fatal("migration modified the Python sqlite file")
	}
	if got := sha256File(t, vaultFile); got != beforeVault {
		t.Fatal("migration modified the Python vault file")
	}

	// Second run is idempotent: already migrated, nothing rewritten.
	report2, err := MaybeMigrate(profile)
	if err != nil {
		t.Fatalf("MaybeMigrate again: %v", err)
	}
	if len(report2.Skipped) != 1 || report2.Skipped[0] != "already migrated" {
		t.Fatalf("second run Skipped = %v, want [already migrated]", report2.Skipped)
	}
	if len(report2.Mapped) != 0 {
		t.Fatalf("second run Mapped = %v, want none", report2.Mapped)
	}
}

func TestMaybeMigrateDoesNotOverwriteGoSettings(t *testing.T) {
	profile := makePythonProfile(t)
	// Seed Go settings first, as a user who already changed them would have.
	engine, err := dataengine.Open(filepath.Join(profile, "state", "native"))
	if err != nil {
		t.Fatal(err)
	}
	seedDownload := `{"streaming_enabled":false,"updated_ns":123}`
	seedSites := `{"enabled_sites":["redacted"],"updated_ns":123}`
	for _, kv := range []struct {
		key, value string
	}{
		{"app/settings/download", seedDownload},
		{"app/settings/sites", seedSites},
	} {
		ok, err := engine.CompareAndSwap(
			[]dataengine.Condition{{Key: []byte(kv.key), Absent: true}},
			[]dataengine.Mutation{{Key: []byte(kv.key), Value: []byte(kv.value)}},
		)
		if err != nil || !ok {
			t.Fatalf("seed %s: ok=%v err=%v", kv.key, ok, err)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := MaybeMigrate(profile)
	if err != nil {
		t.Fatalf("MaybeMigrate: %v", err)
	}
	wantMapped := []string{"app/settings/updates", "app/settings/qbittorrent", "app/settings/ai", "app/settings/library"}
	if len(report.Mapped) != len(wantMapped) {
		t.Fatalf("Mapped = %v, want %v", report.Mapped, wantMapped)
	}
	for i, key := range wantMapped {
		if report.Mapped[i] != key {
			t.Fatalf("Mapped = %v, want %v", report.Mapped, wantMapped)
		}
	}
	download := readNative(t, profile, "app/settings/download")
	if download["streaming_enabled"] != false || download["updated_ns"] != float64(123) {
		t.Fatalf("download settings overwritten: %v", download)
	}
	sites := readNative(t, profile, "app/settings/sites")
	enabled, _ := sites["enabled_sites"].([]any)
	if len(enabled) != 1 || enabled[0] != "redacted" {
		t.Fatalf("sites settings overwritten: %v", sites)
	}
}

func TestMaybeMigrateCopiesServiceSettingsWithoutSecrets(t *testing.T) {
	profile := t.TempDir()
	state := filepath.Join(profile, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(state, "musicops.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	payload := `{
		"version": 2,
		"values": {
			"streaming": {"enabled": false},
			"providers": {"dicmusic": {"enabled": true}},
			"updates": {"enabled": true},
			"paths": {"library": "/library/music"},
			"offline": false,
			"workflow": {"setup_done": true},
			"qbt": {"base_url": "http://127.0.0.1:8085", "category": "MusicOps", "tag_prefix": "Seed", "credential_ref": "vault://user/qbt/abcdefabcdef", "password": "must-not-copy"},
			"ai": {"enabled": true, "base_url": "https://ai.example.invalid/v1", "model": "review", "timeout": 60, "max_output_tokens": 2048, "temperature": 0.2, "auth_secret_ref": "vault://ai/default/key", "api_key": "must-not-copy"}
		}
	}`
	if _, err = db.Exec(`CREATE TABLE config_versions (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO config_versions(id, payload, updated_at) VALUES('2', ?, '2026-01-01T00:00:00Z')`, payload); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := MaybeMigrate(profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"app/settings/qbittorrent", "app/settings/ai", "app/settings/library"} {
		found := false
		for _, mapped := range report.Mapped {
			if mapped == key {
				found = true
			}
		}
		if !found {
			t.Fatal(report.Mapped)
		}
	}
	qbt := readNative(t, profile, "app/settings/qbittorrent")
	if qbt["base_url"] != "http://127.0.0.1:8085" || qbt["category"] != "MusicOps" || qbt["tags"] != "Seed" {
		t.Fatal(qbt)
	}
	ai := readNative(t, profile, "app/settings/ai")
	if ai["enabled"] != true || ai["base_url"] != "https://ai.example.invalid/v1" || ai["model"] != "review" || ai["timeout"] != float64(60) {
		t.Fatal(ai)
	}
	library := readNative(t, profile, "app/settings/library")
	if library["library_dir"] != "/library/music" {
		t.Fatal(library)
	}
	encoded, err := json.Marshal([]any{qbt, ai, library, report})
	if err != nil || strings.Contains(string(encoded), "must-not-copy") || strings.Contains(string(encoded), "vault://") {
		t.Fatal(string(encoded), err)
	}
}
