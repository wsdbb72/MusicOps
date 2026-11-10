//go:build windows

package smoke

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestWindowsPEVersion checks the real packaged executables' version
// resources. Phase B removed the Python pytest that encoded this check
// for the old PyInstaller freeze; the Go build embeds the same facts
// with goversioninfo, and this test reads them back from the files that
// will be installed. It runs only when the packaging script points
// MUSICOPS_SMOKE_BUNDLE at that directory.
func TestWindowsPEVersion(t *testing.T) {
	bundle := os.Getenv("MUSICOPS_SMOKE_BUNDLE")
	version := os.Getenv("MUSICOPS_SMOKE_VERSION")
	if bundle == "" || version == "" {
		t.Skip("MUSICOPS_SMOKE_BUNDLE and MUSICOPS_SMOKE_VERSION are not set")
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("version %q is not X.Y.Z", version)
	}
	numbers := make([]uint16, 3)
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 65535 {
			t.Fatalf("version component %q", part)
		}
		numbers[i] = uint16(value)
	}
	report := map[string]any{}
	for _, name := range []string{"musicops-core.exe", "MusicOps.exe"} {
		path := filepath.Join(bundle, name)
		got, err := readPEVersion(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.FileMajor != numbers[0] || got.FileMinor != numbers[1] || got.FilePatch != numbers[2] || got.FileBuild != 0 {
			t.Fatalf("%s file version %d.%d.%d.%d, want %s.0", name, got.FileMajor, got.FileMinor, got.FilePatch, got.FileBuild, version)
		}
		if got.ProductMajor != numbers[0] || got.ProductMinor != numbers[1] || got.ProductPatch != numbers[2] || got.ProductBuild != 0 {
			t.Fatalf("%s product version numbers %d.%d.%d.%d", name, got.ProductMajor, got.ProductMinor, got.ProductPatch, got.ProductBuild)
		}
		if got.FileVersion != version+".0" {
			t.Fatalf("%s FileVersion string %q", name, got.FileVersion)
		}
		if got.ProductVersion != version {
			t.Fatalf("%s ProductVersion string %q", name, got.ProductVersion)
		}
		if got.OriginalFilename != name {
			t.Fatalf("%s OriginalFilename %q", name, got.OriginalFilename)
		}
		if got.Company != "wsdbb72" {
			t.Fatalf("%s CompanyName %q", name, got.Company)
		}
		if !strings.Contains(got.Description, "MusicOps") {
			t.Fatalf("%s FileDescription %q", name, got.Description)
		}
		if got.LangID != 2052 || got.Charset != 1200 {
			t.Fatalf("%s translation %d/%d, want 2052/1200", name, got.LangID, got.Charset)
		}
		if name == "MusicOps.exe" && got.Subsystem != 2 {
			t.Fatalf("desktop subsystem %d, want 2 (Windows GUI)", got.Subsystem)
		}
		report[name] = got
	}
	if path := os.Getenv("MUSICOPS_SMOKE_PE_REPORT"); path != "" {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("PE version resources match %s", version)
}
