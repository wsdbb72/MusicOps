package smoke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNoPython(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "musicops"), "binary")
	writeFile(t, filepath.Join(root, "tools", "ffmpeg"), "binary")
	// A frozen engine may contain Python internals; the no-Python gate
	// covers MusicOps' own runtime, not the third-party engines.
	services := filepath.Join(root, "tools", "services")
	writeFile(t, filepath.Join(services, "flaresolverr", "_internal", "base.pyc"), "bytecode")
	if err := CheckNoPython(root, services); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "helper.py"), "x=1")
	if err := CheckNoPython(root, services); err == nil {
		t.Fatal("expected a .py file at the bundle root to fail")
	}
	writeFile(t, filepath.Join(root, "helper.py"), "x=1")
	_ = os.Remove(filepath.Join(root, "helper.py"))
	writeFile(t, filepath.Join(root, "_internal", "python312.dll"), "dll")
	if err := CheckNoPython(root, services); err == nil {
		t.Fatal("expected _internal/python312.dll to fail")
	}
}

func TestCheckManifest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "musicops"), "core-binary")
	writeFile(t, filepath.Join(root, "tools", "ffmpeg"), "ffmpeg-binary")
	entries := []map[string]any{}
	for _, rel := range []string{"musicops", filepath.Join("tools", "ffmpeg")} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		entries = append(entries, map[string]any{
			"path": filepath.ToSlash(rel), "size": len(data),
			"sha256": hex.EncodeToString(sum[:]),
		})
	}
	raw, _ := json.Marshal(entries)
	writeFile(t, filepath.Join(root, "manifest.json"), string(raw))
	if err := CheckManifest(root, "manifest.json", true); err != nil {
		t.Fatal(err)
	}
	// Tamper with one file: the hash must no longer match.
	writeFile(t, filepath.Join(root, "musicops"), "tampered")
	if err := CheckManifest(root, "manifest.json", true); err == nil {
		t.Fatal("expected a tampered file to fail the manifest check")
	}
	// No manifest at all: not applicable when not required, an error
	// when required.
	empty := t.TempDir()
	if err := CheckManifest(empty, "manifest.json", false); !errors.Is(err, ErrNotApplicable) {
		t.Fatalf("got %v, want ErrNotApplicable", err)
	}
	if err := CheckManifest(empty, "manifest.json", true); err == nil {
		t.Fatal("expected a missing manifest to fail when required")
	}
}

func TestCheckManifestSkipsSignedMachO(t *testing.T) {
	root := t.TempDir()
	// A 64-bit Mach-O binary whose manifest entry records the
	// pre-signing size/hash, then "grows" under ad-hoc codesigning:
	// the check must still pass (integrity of signed binaries is
	// covered by codesign --verify, not the manifest).
	macho := append([]byte{0xFE, 0xED, 0xFA, 0xCF}, []byte("signed-binary-payload")...)
	writeFile(t, filepath.Join(root, "MusicOps"), string(macho))
	fat := append([]byte{0xCA, 0xFE, 0xBA, 0xBE}, []byte("fat-binary")...)
	writeFile(t, filepath.Join(root, "driver"), string(fat))
	// A regular file with a deliberately wrong hash must still fail.
	writeFile(t, filepath.Join(root, "notes.txt"), "plain-text")
	entries := []map[string]any{
		{"path": "MusicOps", "size": 1, "sha256": strings.Repeat("0", 64)},
		{"path": "driver", "size": 1, "sha256": strings.Repeat("0", 64)},
		{"path": "notes.txt", "size": 10, "sha256": strings.Repeat("0", 64)},
	}
	raw, _ := json.Marshal(entries)
	writeFile(t, filepath.Join(root, "manifest.json"), string(raw))
	if err := CheckManifest(root, "manifest.json", true); err == nil {
		t.Fatal("expected a tampered plain file to fail the manifest check")
	}
	// Without the tampered plain file, the stale Mach-O entries pass.
	entries = entries[:2]
	raw, _ = json.Marshal(entries)
	writeFile(t, filepath.Join(root, "manifest.json"), string(raw))
	if err := CheckManifest(root, "manifest.json", true); err != nil {
		t.Fatalf("stale Mach-O entries must be skipped: %v", err)
	}
	// A missing Mach-O file is still an error: the manifest must at
	// least prove the binary exists.
	if err := os.Remove(filepath.Join(root, "MusicOps")); err != nil {
		t.Fatal(err)
	}
	if err := CheckManifest(root, "manifest.json", true); err == nil {
		t.Fatal("expected a missing Mach-O entry to fail the manifest check")
	}
}

func TestCheckEngines(t *testing.T) {
	root := t.TempDir()
	services := filepath.Join(root, "tools", "services")
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	writeFile(t, filepath.Join(services, "engines.json"), `{"sources":{"go-music-dl":{"repo":"guohuiyuan/go-music-dl"},"FlareSolverr":{"repo":"FlareSolverr/FlareSolverr"}},"chrome_version":"155.0.8059.84"}`)
	for _, p := range []string{
		filepath.Join(services, "go-music-dl", "go-music-dl"+exe),
		filepath.Join(services, "flaresolverr", "flaresolverr"+exe),
	} {
		writeFile(t, p, "binary")
		if runtime.GOOS != "windows" {
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := CheckEngines(services, true); err != nil {
		t.Fatal(err)
	}
	if err := CheckEngines(filepath.Join(root, "no-such-dir"), false); !errors.Is(err, ErrNotApplicable) {
		t.Fatalf("got %v, want ErrNotApplicable", err)
	}
	if err := CheckEngines(filepath.Join(root, "no-such-dir"), true); err == nil {
		t.Fatal("expected missing engines to fail when required")
	}
	if err := os.Remove(filepath.Join(services, "go-music-dl", "go-music-dl"+exe)); err != nil {
		t.Fatal(err)
	}
	if err := CheckEngines(services, true); err == nil {
		t.Fatal("expected a missing engine binary to fail")
	}
}

func TestGoBuiltAndVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "musicops-test")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X github.com/wsdbb72/MusicOps/native/core/internal/version.Version=9.9.9-smoke",
		"-o", bin, "./cmd/musicops")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if err := CheckGoBuilt(bin); err != nil {
		t.Fatal(err)
	}
	version, err := BinaryVersion(bin, "version")
	if err != nil {
		t.Fatal(err)
	}
	if version != "9.9.9-smoke" {
		t.Fatalf("version = %q, want 9.9.9-smoke", version)
	}
	if err := CheckGoBuilt(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected a missing binary to fail")
	}
	// A non-Go file is not a Go-built binary.
	plain := filepath.Join(dir, "plain.txt")
	writeFile(t, plain, "not a go binary")
	if err := CheckGoBuilt(plain); err == nil {
		t.Fatal("expected a non-Go file to fail")
	}
	if _, err := BinaryVersion(plain, "version"); err == nil {
		t.Fatal("expected a non-executable file to fail the version command")
	}
}

func TestWriteTestWAV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smoke.wav")
	if err := writeTestWAV(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatal("not a WAV file")
	}
	// ffprobe must decode it: the transcode check depends on this.
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,sample_rate,channels", "-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	if !strings.Contains(string(out), "pcm_s16le,8000,1") {
		t.Fatalf("ffprobe output %q", out)
	}
}

func TestCheckFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	if err := checkFFmpeg(""); err != nil {
		t.Fatal(err)
	}
	// ffmpeg on PATH is enough; a missing tools directory does not fail
	// the check when the system ffmpeg is present.
	if err := checkFFmpeg(filepath.Join(t.TempDir(), "no-tools")); err != nil {
		t.Fatal(err)
	}
}

// TestAppLayoutMapping pins the .app layout the macOS build script
// relies on: a bundle root ending in .app resolves its binaries under
// Contents/MacOS, its tools under Contents/Resources/tools and its
// manifest under Contents/Resources/manifest.json, while an explicit
// relative path (the darwin installer test's spelling) is used as given.
func TestAppLayoutMapping(t *testing.T) {
	cfg := Config{BundleDir: "/tmp/MusicOps.app", CoreName: "musicops", DesktopName: "MusicOps"}
	cfg.defaults()
	if got := cfg.binRel(cfg.CoreName); got != filepath.Join("Contents", "MacOS", "musicops") {
		t.Fatalf("core binRel = %q", got)
	}
	if got := cfg.binRel(cfg.DesktopName); got != filepath.Join("Contents", "MacOS", "MusicOps") {
		t.Fatalf("desktop binRel = %q", got)
	}
	if cfg.ToolsDir != filepath.Join("/tmp/MusicOps.app", "Contents", "Resources", "tools") {
		t.Fatalf("ToolsDir = %q", cfg.ToolsDir)
	}
	if cfg.ServicesDir != filepath.Join(cfg.ToolsDir, "services") {
		t.Fatalf("ServicesDir = %q", cfg.ServicesDir)
	}
	if cfg.ManifestRel != filepath.Join("Contents", "Resources", "manifest.json") {
		t.Fatalf("ManifestRel = %q", cfg.ManifestRel)
	}
	// Explicit relative paths are never re-prefixed.
	explicit := Config{BundleDir: "/Applications/MusicOps.app",
		CoreName: filepath.Join("Contents", "MacOS", "musicops")}
	explicit.defaults()
	if got := explicit.binRel(explicit.CoreName); got != filepath.Join("Contents", "MacOS", "musicops") {
		t.Fatalf("explicit binRel = %q", got)
	}
	// A plain bundle is untouched.
	plain := Config{BundleDir: "/tmp/bundle", CoreName: "musicops"}
	plain.defaults()
	if got := plain.binRel(plain.CoreName); got != "musicops" {
		t.Fatalf("plain binRel = %q", got)
	}
	if plain.ToolsDir != filepath.Join("/tmp/bundle", "tools") {
		t.Fatalf("plain ToolsDir = %q", plain.ToolsDir)
	}
	if plain.ManifestRel != "manifest.json" {
		t.Fatalf("plain ManifestRel = %q", plain.ManifestRel)
	}
}

// TestBundle runs the full black-box smoke against a prebuilt bundle
// directory. It is gated on MUSICOPS_SMOKE_BUNDLE so plain `go test
// ./...` stays green on machines without a bundle: the release workflow
// builds the bundle, sets the variable and runs this test.
//
// The bundle needs the core (+ desktop shell when DesktopName is set)
// and the bundled tools; engines and manifest are optional here because
// a local dev bundle has none — the release workflow sets them.
func TestBundle(t *testing.T) {
	bundle := os.Getenv("MUSICOPS_SMOKE_BUNDLE")
	if bundle == "" {
		t.Skip("MUSICOPS_SMOKE_BUNDLE is not set")
	}
	cfg := Config{
		BundleDir:       bundle,
		Version:         os.Getenv("MUSICOPS_SMOKE_VERSION"),
		RequireManifest: os.Getenv("MUSICOPS_SMOKE_REQUIRE_MANIFEST") == "1",
		RequireEngines:  os.Getenv("MUSICOPS_SMOKE_REQUIRE_ENGINES") == "1",
		Timeout:         5 * time.Minute,
	}
	if name := os.Getenv("MUSICOPS_SMOKE_DESKTOP"); name != "" {
		cfg.DesktopName = name
	}
	report := Run(context.Background(), cfg)
	raw, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("smoke report:\n%s", raw)
	if path := os.Getenv("MUSICOPS_SMOKE_REPORT"); path != "" {
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("cannot write smoke report to %s: %v", path, err)
		}
	}
	if !report.Passed {
		t.Fatalf("smoke failed: %s", report.Error)
	}
}
