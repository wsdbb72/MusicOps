// Command releasegate runs the release gate over a built bundle
// directory and exits 0 when the bundle may be released, 1 when it may
// not. It prints the gate Result as JSON on stdout.
//
// Usage:
//
//	releasegate --bundle <dir> [--version <ver>] [--layout windows|macos|linux|docker]
//	    [--desktop <name>] [--core <name>]
//	    [--require-engines] [--require-manifest]
//	    [--installer <name>]... [--smoke-report <path>]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/wsdbb72/MusicOps/native/core/internal/releasegate"
)

type stringList []string

func (s *stringList) String() string { return fmt.Sprintf("%v", []string(*s)) }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var installers stringList
	bundle := flag.String("bundle", "", "bundle directory to verify")
	version := flag.String("version", "", "expected release version")
	layout := flag.String("layout", "windows", "bundle layout: windows, macos, linux or docker")
	desktop := flag.String("desktop", "", "desktop shell binary name (empty: no desktop shell in the bundle)")
	core := flag.String("core", "", "core binary name (empty: layout default)")
	requireEngines := flag.Bool("require-engines", false, "require the managed engines to be present")
	requireManifest := flag.Bool("require-manifest", false, "require manifest.json to exist and verify")
	smokeReport := flag.String("smoke-report", "", "smoke report JSON for this bundle")
	flag.Var(&installers, "installer", "installer artifact filename expected in the bundle (repeatable)")
	flag.Parse()

	if *bundle == "" {
		fmt.Fprintln(os.Stderr, "--bundle is required")
		os.Exit(2)
	}
	gate := releasegate.Gate{
		Version: *version, Layout: *layout, DesktopName: *desktop, CoreName: *core,
		RequireEngines: *requireEngines, RequireManifest: *requireManifest,
		Installers: installers, SmokeReportPath: *smokeReport,
	}
	result := gate.Check(*bundle)
	raw, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(raw))
	if !result.Passed {
		os.Exit(1)
	}
}
