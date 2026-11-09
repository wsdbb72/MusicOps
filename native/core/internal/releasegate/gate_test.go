package releasegate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The tests build the real cmd/musicops binary once and copy it into
// each test's bundle; building it per test would cost ~6s each.
var testCoreBinary string

func TestMain(m *testing.M) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		home, _ := os.UserHomeDir()
		goBin = filepath.Join(home, ".local", "go", "bin", "go")
	}
	dir, err := os.MkdirTemp("", "musicops-releasegate-test-core-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	testCoreBinary = filepath.Join(dir, "musicops")
	if runtime.GOOS == "windows" {
		testCoreBinary += ".exe"
	}
	cmd := exec.Command(goBin, "build", "-trimpath",
		"-ldflags", "-X github.com/wsdbb72/MusicOps/native/core/internal/version.Version=9.9.9-gate",
		"-o", testCoreBinary, "./cmd/musicops")
	cmd.Dir = filepath.Join("..", "..")
	goTmpDir := filepath.Join(os.TempDir(), "musicops-go-tmp")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		goTmpDir = filepath.Join(home, ".cache", "go-tmp")
	}
	if err := os.MkdirAll(goTmpDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create GOTMPDIR:", err)
		os.Exit(1)
	}
	cmd.Env = append(os.Environ(), "GOTMPDIR="+goTmpDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot build the test core binary:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func copyCore(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(testCoreBinary)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, filepath.Base(testCoreBinary))
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// goodBundle returns a linux-layout bundle with a versioned core,
// a matching manifest and an engines fixture.
func goodBundle(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	core := copyCore(t, root)
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
	// Manifest over the core binary only.
	data, err := os.ReadFile(core)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	entries := []map[string]any{{
		"path": filepath.ToSlash("musicops" + exe), "size": len(data),
		"sha256": hex.EncodeToString(sum[:]),
	}}
	raw, _ := json.Marshal(entries)
	writeFile(t, filepath.Join(root, "manifest.json"), string(raw))
	return root
}

func passingSmokeReport(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "smoke.json")
	raw, _ := json.Marshal(map[string]any{
		"version": version, "evidence": "go_bundle_smoke", "passed": true,
		"checks": map[string]bool{"bundle_no_python": true, "core_is_go_built": true},
	})
	writeFile(t, path, string(raw))
	return path
}

// exeSuffix is the platform executable suffix the fixture uses for the
// core and engine binaries.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// testLayout is the gate layout the fixture matches: the fixture builds
// a linux-layout bundle on Unix and a windows-layout bundle on Windows
// (the gate's windows layout looks for musicops.exe, its linux layout
// for musicops). A linux layout on Windows would look for a file the
// fixture never creates.
func testLayout() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "linux"
}

func TestGatePassesGoodBundle(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	gate := Gate{Version: "9.9.9-gate", Layout: testLayout(), RequireEngines: true, RequireManifest: true, SmokeReportPath: passingSmokeReport(t, "9.9.9-gate")}
	result := gate.Check(root)
	raw, _ := json.MarshalIndent(result, "", "  ")
	t.Logf("gate result:\n%s", raw)
	if !result.Passed {
		t.Fatalf("gate failed: %s", result.Error)
	}
}

func TestGateFailsVersionMismatch(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	gate := Gate{Version: "9.9.8-gate", Layout: testLayout()}
	result := gate.Check(root)
	if result.Passed || result.Checks["core_version"] {
		t.Fatalf("gate passed with a version mismatch: %+v", result)
	}
}

func TestGateFailsPythonInBundle(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	writeFile(t, filepath.Join(root, "helper.py"), "x=1")
	gate := Gate{Version: "9.9.9-gate", Layout: testLayout()}
	result := gate.Check(root)
	if result.Passed || result.Checks["bundle_no_python"] {
		t.Fatalf("gate passed with Python in the bundle: %+v", result)
	}
}

func TestGateFailsTamperedManifest(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	writeFile(t, filepath.Join(root, "musicops"+exeSuffix()), "tampered")
	gate := Gate{Version: "9.9.9-gate", Layout: testLayout(), RequireManifest: true}
	result := gate.Check(root)
	if result.Passed || result.Checks["manifest_hashes_match"] {
		t.Fatalf("gate passed with a tampered manifest: %+v", result)
	}
}

func TestGateFailsMissingEngine(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	if err := os.Remove(filepath.Join(root, "tools", "services", "go-music-dl", "go-music-dl"+exeSuffix())); err != nil {
		t.Fatal(err)
	}
	gate := Gate{Version: "9.9.9-gate", Layout: testLayout(), RequireEngines: true}
	result := gate.Check(root)
	if result.Passed || result.Checks["engines_present"] {
		t.Fatalf("gate passed with a missing engine: %+v", result)
	}
}

func TestGateFailsSmokeReport(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	path := filepath.Join(t.TempDir(), "smoke.json")
	raw, _ := json.Marshal(map[string]any{
		"version": "9.9.9-gate", "evidence": "go_bundle_smoke", "passed": false,
		"checks": map[string]bool{"bundle_no_python": false}, "error": "bundle_no_python: python files in bundle: helper.py",
	})
	writeFile(t, path, string(raw))
	gate := Gate{Version: "9.9.9-gate", Layout: testLayout(), SmokeReportPath: path}
	result := gate.Check(root)
	if result.Passed || result.Checks["smoke_evidence_passed"] {
		t.Fatalf("gate passed with a failing smoke report: %+v", result)
	}
}

func TestGateFailsBuildJSONMismatch(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	writeFile(t, filepath.Join(root, "build.json"), `{"version":"9.9.8-gate"}`)
	gate := Gate{Version: "9.9.9-gate", Layout: testLayout()}
	result := gate.Check(root)
	if result.Passed || result.Checks["build_json_version"] {
		t.Fatalf("gate passed with a build.json mismatch: %+v", result)
	}
}

func TestGateFailsMissingInstaller(t *testing.T) {
	root := goodBundle(t, "9.9.9-gate")
	gate := Gate{Version: "9.9.9-gate", Layout: testLayout(), Installers: []string{"MusicOps-9.9.9-gate-Setup.exe"}}
	result := gate.Check(root)
	if result.Passed {
		t.Fatalf("gate passed with a missing installer: %+v", result)
	}
}
