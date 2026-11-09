// Package releasegate is the real release gate: it verifies a built
// bundle directory before a release may proceed. It replaces the old
// text-only workflow assertions (workflow_test.go in this package) with
// checks over the actual artifacts:
//
//   - no Python runtime in MusicOps' own files;
//   - the core and desktop binaries are Go-built and report the expected
//     version (via their real version commands, not a manifest);
//   - manifest hashes match (when the bundle carries one);
//   - the managed engines are present (when required);
//   - the expected installer artifacts exist;
//   - the smoke report for this bundle passed with the expected version;
//   - build.json, when present, names the expected version.
//
// The gate never trusts a claim the bundle does not prove: a missing
// file fails, a version mismatch fails, and a smoke report that passed
// vacuously (engines/manifest skipped) does not satisfy a gate that
// requires them, because the gate checks them itself.
//
// Layouts: "windows" (bundle root holds MusicOps.exe + musicops-core.exe),
// "macos" (a .app: binaries in Contents/MacOS, tools in
// Contents/Resources/tools, manifest in Contents/Resources), "linux"
// and "docker" (bundle root holds musicops, no desktop shell).
package releasegate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wsdbb72/MusicOps/native/core/internal/smoke"
)

// Gate configures one check run.
type Gate struct {
	// Version is the expected release version, e.g. "0.4.33". Empty
	// means the version is read from the core binary and only checked
	// for consistency across the bundle.
	Version string
	// Layout is windows, macos, linux or docker.
	Layout string
	// DesktopName is the desktop shell binary name; "" means the bundle
	// has no desktop shell (docker/linux).
	DesktopName string
	// CoreName overrides the core binary name; "" uses the layout default.
	CoreName string
	// RequireEngines requires the managed engines to be present.
	// RequireManifest requires manifest.json to exist and verify.
	RequireEngines  bool
	RequireManifest bool
	// Installers lists installer artifact filenames expected in the
	// bundle directory (e.g. MusicOps-0.4.33-Setup.exe).
	Installers []string
	// SmokeReportPath is the smoke report JSON for this bundle; "" skips
	// the smoke-evidence check.
	SmokeReportPath string
}

// Result is the gate outcome. Skipped lists checks that passed
// vacuously because the thing they verify is absent and not required
// (a core-only bundle has no engines or manifest).
type Result struct {
	Version string          `json:"version"`
	Passed  bool            `json:"passed"`
	Checks  map[string]bool `json:"checks"`
	Skipped []string        `json:"skipped,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func (g Gate) layout() string {
	if g.Layout == "" {
		return "windows"
	}
	return g.Layout
}

func (g Gate) coreName() string {
	if g.CoreName != "" {
		return g.CoreName
	}
	// Windows and macOS use musicops-core[.exe]: the desktop shell is
	// MusicOps[.exe] and those filesystems are case-insensitive, so a
	// core named musicops would be the same file as the shell.
	switch g.layout() {
	case "windows":
		return "musicops-core.exe"
	case "macos":
		return "musicops-core"
	default:
		return "musicops"
	}
}

// manifestRel is the manifest path relative to the bundle root.
func (g Gate) manifestRel() string {
	if g.layout() == "macos" {
		return "Contents/Resources/manifest.json"
	}
	return "manifest.json"
}

// paths resolves the bundle paths for the layout.
func (g Gate) paths(bundleDir string) (core, desktop, services string) {
	switch g.layout() {
	case "macos":
		core = filepath.Join(bundleDir, "Contents", "MacOS", g.coreName())
		if g.DesktopName != "" {
			desktop = filepath.Join(bundleDir, "Contents", "MacOS", g.DesktopName)
		}
		services = filepath.Join(bundleDir, "Contents", "Resources", "tools", "services")
	default:
		core = filepath.Join(bundleDir, g.coreName())
		if g.DesktopName != "" {
			desktop = filepath.Join(bundleDir, g.DesktopName)
		}
		services = filepath.Join(bundleDir, "tools", "services")
	}
	return core, desktop, services
}

// Check verifies the bundle and returns the result.
func (g Gate) Check(bundleDir string) Result {
	result := Result{Checks: map[string]bool{}}
	check := func(name string, fn func() error) {
		if err := fn(); err != nil {
			result.Checks[name] = false
			if result.Error == "" {
				result.Error = name + ": " + err.Error()
			}
			return
		}
		result.Checks[name] = true
	}
	core, desktop, services := g.paths(bundleDir)
	check("bundle_no_python", func() error { return smoke.CheckNoPython(bundleDir, services) })
	version := g.Version
	check("core_present_and_go_built", func() error {
		if _, err := os.Stat(core); err != nil {
			return err
		}
		return smoke.CheckGoBuilt(core)
	})
	check("core_version", func() error {
		v, err := smoke.BinaryVersion(core, "version")
		if err != nil {
			return err
		}
		if version == "" {
			version = v
			return nil
		}
		if v != version {
			return fmt.Errorf("core reports %q, want %q", v, version)
		}
		return nil
	})
	result.Version = version
	if desktop != "" {
		check("desktop_present_and_go_built", func() error {
			if _, err := os.Stat(desktop); err != nil {
				return err
			}
			return smoke.CheckGoBuilt(desktop)
		})
		check("desktop_version", func() error {
			v, err := smoke.BinaryVersion(desktop, "--version")
			if err != nil {
				return err
			}
			if version != "" && v != version {
				return fmt.Errorf("desktop reports %q, want %q", v, version)
			}
			return nil
		})
	}
	check("manifest_hashes_match", func() error {
		err := smoke.CheckManifest(bundleDir, g.manifestRel(), g.RequireManifest)
		if errors.Is(err, smoke.ErrNotApplicable) {
			result.Skipped = append(result.Skipped, "manifest_hashes_match")
			return nil
		}
		return err
	})
	check("engines_present", func() error {
		err := smoke.CheckEngines(services, g.RequireEngines)
		if errors.Is(err, smoke.ErrNotApplicable) {
			result.Skipped = append(result.Skipped, "engines_present")
			return nil
		}
		return err
	})
	for _, name := range g.Installers {
		name := name
		check("installer_present:"+name, func() error {
			path := filepath.Join(bundleDir, name)
			st, err := os.Stat(path)
			if err != nil {
				return err
			}
			if st.IsDir() || st.Size() == 0 {
				return fmt.Errorf("installer %s is empty or a directory", name)
			}
			return nil
		})
	}
	if g.SmokeReportPath != "" {
		check("smoke_evidence_passed", func() error { return checkSmokeReport(g.SmokeReportPath, version) })
	}
	// build.json, when present at the bundle root or in the macOS
	// resources directory, must name the expected version.
	for _, rel := range []string{"build.json", filepath.Join("Contents", "Resources", "build.json")} {
		if raw, err := os.ReadFile(filepath.Join(bundleDir, rel)); err == nil {
			check("build_json_version", func() error {
				var doc struct {
					Version string `json:"version"`
				}
				if err := json.Unmarshal(raw, &doc); err != nil {
					return err
				}
				if version != "" && doc.Version != "" && doc.Version != version {
					return fmt.Errorf("build.json version %q, want %q", doc.Version, version)
				}
				return nil
			})
			break
		}
	}
	result.Passed = len(result.Checks) > 0
	for _, ok := range result.Checks {
		if !ok {
			result.Passed = false
		}
	}
	return result
}

// checkSmokeReport requires the smoke report to have passed with the
// expected version and no failing check.
func checkSmokeReport(path, version string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var report smoke.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("smoke report unreadable: %w", err)
	}
	if !report.Passed {
		return fmt.Errorf("smoke report did not pass: %s", report.Error)
	}
	if version != "" && report.Version != "" && report.Version != version {
		return fmt.Errorf("smoke report version %q, want %q", report.Version, version)
	}
	for name, ok := range report.Checks {
		if !ok {
			return fmt.Errorf("smoke check %s failed", name)
		}
	}
	return nil
}
