package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUserDataRootLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only path rules")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "/data/home")
	if got := UserDataRoot(); got != "/data/home" {
		t.Fatalf("XDG_DATA_HOME: got %q", got)
	}
	t.Setenv("XDG_DATA_HOME", "")
	if got := UserDataRoot(); got != filepath.Join(home, ".local", "share") {
		t.Fatalf("default: got %q", got)
	}
}

func TestLinuxDefaultKeyFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", "")
	want := filepath.Join(home, ".local", "share", "MusicOps", "vault.key")
	if got := LinuxDefaultKeyFile(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := os.Getenv("MUSICOPS_VAULT_KEY_FILE"); got != want {
		t.Fatalf("env got %q, want %q", got, want)
	}
	// An explicit value is never overwritten.
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", "/explicit/vault.key")
	if got := LinuxDefaultKeyFile(); got != "/explicit/vault.key" {
		t.Fatalf("explicit: got %q", got)
	}
	if got := os.Getenv("MUSICOPS_VAULT_KEY_FILE"); got != "/explicit/vault.key" {
		t.Fatalf("explicit env got %q", got)
	}
}

// testTempDir returns a temp directory with symlinks resolved.
// ResolveProfile returns symlink-resolved paths, so expectations built
// from an unresolved temp dir would mismatch on macOS (where /var is a
// symlink to /private/var) and on Windows (8.3 short names in %TEMP%).
func testTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// testHome returns an isolated home directory (symlinks resolved) and
// points HOME — plus the platform data-root overrides — at it, so
// UserDataRoot() resolves inside the test temp dir on every OS:
// LOCALAPPDATA on Windows, XDG_DATA_HOME cleared on Linux so the
// ~/.local/share branch is used, HOME on macOS.
func testHome(t *testing.T) string {
	t.Helper()
	home := testTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", home)
	t.Setenv("XDG_DATA_HOME", "")
	return home
}

// fakeExe returns a path under dir pretending to be the desktop executable.
func fakeExe(t *testing.T, dir string, rel string) string {
	t.Helper()
	exe := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(exe), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestResolveProfilePortable(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("portable mode is not used on macOS")
	}
	testHome(t)
	root := testTempDir(t)
	exe := fakeExe(t, root, "musicops-desktop")
	if err := os.MkdirAll(filepath.Join(root, "profile"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveProfile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "profile") {
		t.Fatalf("got %q", got)
	}
}

func TestResolveProfileCheckout(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("checkout walk is not used on macOS")
	}
	testHome(t)
	root := testTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[x]"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profile", "state"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profile", "state", "musicops.sqlite"), []byte("sqlite"), 0644); err != nil {
		t.Fatal(err)
	}
	// Executable three levels below the checkout root.
	exe := fakeExe(t, root, filepath.Join("a", "b", "c", "musicops-desktop"))
	got, err := ResolveProfile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "profile") {
		t.Fatalf("got %q, want %q", got, filepath.Join(root, "profile"))
	}
}

func TestResolveProfileCheckoutNeedsSQLite(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("checkout walk is not used on macOS")
	}
	testHome(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[x]"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profile", "state"), 0755); err != nil {
		t.Fatal(err)
	}
	// No musicops.sqlite: must fall through to the default.
	exe := fakeExe(t, root, filepath.Join("a", "musicops-desktop"))
	got, err := ResolveProfile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(UserDataRoot(), "MusicOps", "profile") {
		t.Fatalf("got %q (expected the default profile)", got)
	}
}

func TestResolveProfileActiveJSON(t *testing.T) {
	home := testHome(t)
	remembered := filepath.Join(home, "myprofile")
	if err := os.MkdirAll(filepath.Join(remembered, "state"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remembered, "state", "musicops.sqlite"), []byte("sqlite"), 0644); err != nil {
		t.Fatal(err)
	}
	location := filepath.Join(UserDataRoot(), "MusicOps")
	if err := os.MkdirAll(location, 0700); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]string{"profile": remembered})
	if err := os.WriteFile(filepath.Join(location, "active-profile.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	exe := fakeExe(t, t.TempDir(), "musicops-desktop")
	got, err := ResolveProfile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != remembered {
		t.Fatalf("got %q, want %q", got, remembered)
	}
}

func TestResolveProfileActiveJSONOversizedIgnored(t *testing.T) {
	testHome(t)
	location := filepath.Join(UserDataRoot(), "MusicOps")
	if err := os.MkdirAll(location, 0700); err != nil {
		t.Fatal(err)
	}
	// >4096 bytes: ignored, like startup.py.
	if err := os.WriteFile(filepath.Join(location, "active-profile.json"), []byte(strings.Repeat("x", 5000)), 0600); err != nil {
		t.Fatal(err)
	}
	exe := fakeExe(t, t.TempDir(), "musicops-desktop")
	got, err := ResolveProfile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(UserDataRoot(), "MusicOps", "profile") {
		t.Fatalf("got %q (expected the default profile)", got)
	}
}

func TestResolveProfileDefault(t *testing.T) {
	testHome(t)
	exe := fakeExe(t, t.TempDir(), "musicops-desktop")
	got, err := ResolveProfile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(UserDataRoot(), "MusicOps", "profile") {
		t.Fatalf("got %q", got)
	}
}

func TestResolveProfileSymlinkRefused(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("portable mode is not used on macOS")
	}
	testHome(t)
	root := testTempDir(t)
	real := filepath.Join(root, "real-profile")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "profile")); err != nil {
		t.Fatal(err)
	}
	exe := fakeExe(t, root, "musicops-desktop")
	if _, err := ResolveProfile(exe); err == nil {
		t.Fatal("expected an error for a symlinked profile directory")
	}
}

func TestRememberProfile(t *testing.T) {
	home := testHome(t)
	base := filepath.Join(home, "base")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := RememberProfile(base); err != nil {
		t.Fatal(err)
	}
	location := filepath.Join(UserDataRoot(), "MusicOps")
	info, err := os.Stat(location)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("MusicOps dir mode = %o, want 0700", info.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Join(location, "active-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value.Profile != base {
		t.Fatalf("stored profile = %q, want %q", value.Profile, base)
	}
	// Remembering again overwrites atomically (no temp files left behind).
	if err := RememberProfile(base); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(location)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only active-profile.json, got %d entries", len(entries))
	}
}
