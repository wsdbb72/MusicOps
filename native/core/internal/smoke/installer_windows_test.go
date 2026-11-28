//go:build windows

package smoke

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// TestWindowsInstaller installs, upgrades and uninstalls the real Go
// installer on a Windows runner, mirroring packaging/
// smoke_windows_installer.py. It is gated on environment variables so
// plain `go test ./...` stays green without Inno Setup:
//
//	MUSICOPS_SMOKE_INSTALLER_BUNDLE   the Go bundle directory
//	MUSICOPS_SMOKE_INSTALLER_COMPILER the ISCC compiler (or "" to skip compiling)
//	MUSICOPS_SMOKE_INSTALLER_EXE      a prebuilt installer (skips compiling)
//	MUSICOPS_SMOKE_INSTALLER_WORK     an empty work directory
//	MUSICOPS_SMOKE_INSTALLER_VERSION  the release version (default 9.9.9
//	                                    when compiling, else derived from
//	                                    the installer file name)
//	MUSICOPS_SMOKE_INSTALLER_REPORT   path for the JSON acceptance report
//	                                    {"version","passed","checks"} with
//	                                    seven named checks, read by the
//	                                    release workflow's archive step
//
// The installer runs under an isolated AppId (MusicOps.QA.<hex>) so it
// never touches a production installation. No real downloads and no
// writes to an existing library are involved.
func TestWindowsInstaller(t *testing.T) {
	bundle := os.Getenv("MUSICOPS_SMOKE_INSTALLER_BUNDLE")
	work := os.Getenv("MUSICOPS_SMOKE_INSTALLER_WORK")
	if bundle == "" || work == "" {
		t.Skip("MUSICOPS_SMOKE_INSTALLER_BUNDLE/MUSICOPS_SMOKE_INSTALLER_WORK are not set")
	}
	compiler := os.Getenv("MUSICOPS_SMOKE_INSTALLER_COMPILER")
	installerExe := os.Getenv("MUSICOPS_SMOKE_INSTALLER_EXE")
	if installerExe == "" && compiler == "" {
		t.Skip("no installer compiler or prebuilt installer given")
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Fatalf("installer acceptance requires a new empty work directory: %v", entries)
	}
	identity := "MusicOps.QA." + hex.EncodeToString(randomBytes(t, 6))
	target := filepath.Join(work, "installed-runtime")
	profile := filepath.Join(target, "profile")

	// Six named checks, written to MUSICOPS_SMOKE_INSTALLER_REPORT (if
	// set) even when the test fails, so the release workflow can tell
	// exactly which acceptance step broke.
	version := os.Getenv("MUSICOPS_SMOKE_INSTALLER_VERSION")
	reportPath := os.Getenv("MUSICOPS_SMOKE_INSTALLER_REPORT")
	checks := map[string]bool{
		"fresh_install":           false,
		"installed_files_match":   false,
		"upgrade_preserves":       false,
		"running_blocks_upgrade":  false,
		"upgrade_after_exit":      false,
		"orphaned_helper_cleanup": false,
		"uninstall_keeps":         false,
	}
	t.Cleanup(func() {
		if reportPath == "" {
			return
		}
		passed := true
		for _, ok := range checks {
			passed = passed && ok
		}
		data, err := json.MarshalIndent(map[string]any{
			"version": version,
			"passed":  passed,
			"checks":  checks,
		}, "", "  ")
		if err != nil {
			t.Logf("cannot marshal installer report: %v", err)
			return
		}
		if err := os.WriteFile(reportPath, append(data, '\n'), 0o644); err != nil {
			t.Logf("cannot write installer report %s: %v", reportPath, err)
		}
	})

	if installerExe == "" {
		if version == "" {
			// The synthetic registry version faked below is 0.4.1; the
			// test installer must be newer or the .iss downgrade guard
			// refuses the upgrade step.
			version = "9.9.9"
		}
		installerExe = filepath.Join(work, "qa-installer", "MusicOps-Setup.exe")
		outDir := filepath.Join(work, "qa-installer")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Resolve from the absolute bundle path instead of the test process's
		// working directory. `go test` runs package tests from internal/smoke,
		// while the release script historically assumed the module root, which
		// made isolated QA compilation look for the wrong .iss file.
		bundleAbs, err := filepath.Abs(bundle)
		if err != nil {
			t.Fatal(err)
		}
		repoRoot := filepath.Dir(filepath.Dir(bundleAbs))
		iss := filepath.Join(repoRoot, "packaging", "MusicOps-go.iss")
		cmd := exec.Command(compiler, "/DAppIdentity="+identity, "/DAppVersion="+version, "/DBundleDir="+bundle, "/O"+outDir, iss)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ISCC compile failed: %v\n%s", err, out)
		}
		matches, err := filepath.Glob(filepath.Join(outDir, "*-Setup.exe"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("expected one installer, got %v (%v)", matches, err)
		}
		installerExe = matches[0]
	} else {
		// Prebuilt installer: MusicOps-<version>-Setup.exe.
		if version == "" {
			base := filepath.Base(installerExe)
			version = strings.TrimSuffix(strings.TrimPrefix(base, "MusicOps-"), "-Setup.exe")
			if version == base {
				version = "0.0.0-dev"
			}
		}
		// The prebuilt installer is the shipping build, compiled with
		// the .iss default AppIdentity rather than the QA identity
		// above, so the uninstall key it creates is
		// <AppIdentity>_is1. Read the identity from the .iss (resolved
		// from the bundle path: the bundle is
		// <repo>/dist/MusicOps-Windows) so the upgrade step below opens
		// the key the installer actually wrote. On a CI runner there
		// is no production install to disturb, and the uninstall step
		// at the end removes the key again.
		identity = appIdentityFromISS(t, bundle)
	}

	runInstaller := func(dir string) (int, string) {
		args := []string{installerExe, "/SP-", "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/NOICONS", "/TASKS="}
		if dir != "" {
			args = append(args, "/DIR="+dir)
		}
		cmd := exec.Command(args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatalf("installer run failed: %v", err)
			}
		}
		return code, string(out)
	}
	sha := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}

	// Fresh install.
	if code, _ := runInstaller(target); code != 0 {
		t.Fatalf("fresh install exited %d", code)
	}
	checks["fresh_install"] = true
	for _, name := range []string{"MusicOps.exe", "musicops-core.exe", "LICENSE", "用户操作说明.md", "运维与恢复说明.md", "manifest.json"} {
		if sha(filepath.Join(target, name)) != sha(filepath.Join(bundle, name)) {
			t.Fatalf("installed %s does not match the bundle", name)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "MusicOps-worker.exe")); !os.IsNotExist(err) {
		t.Fatal("installed bundle still ships MusicOps-worker.exe")
	}
	if _, err := os.Stat(filepath.Join(target, "_internal")); !os.IsNotExist(err) {
		t.Fatal("installed bundle still ships _internal")
	}
	checks["installed_files_match"] = true

	// Seed a profile sentinel and a preserved tool, then upgrade.
	sentinel := filepath.Join(profile, "installer-preservation.txt")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("synthetic profile must survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(target, "tools", "preserved-tool.txt")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("synthetic preserved tool"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A crashed prior core can leave an app-owned headless helper running.
	// Put a harmless test helper under the same tools\services subtree and
	// require the updater to stop it before replacing bundled files.
	helperExe := filepath.Join(target, "tools", "services", "installer-helper.exe")
	sourceExe, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	helperOut, err := os.OpenFile(helperExe, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		sourceExe.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(helperOut, sourceExe)
	closeErr := helperOut.Close()
	_ = sourceExe.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("seed bundled helper: copy=%v close=%v", copyErr, closeErr)
	}
	helper := exec.Command(helperExe, "-test.run=^TestWindowsInstallerHelperProcess$")
	helper.Env = append(os.Environ(), "MUSICOPS_INSTALLER_SLEEP_HELPER=1")
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	helperPID := uint32(helper.Process.Pid)
	defer func() {
		if helper.Process != nil && processAlive(helperPID) {
			_ = helper.Process.Kill()
			_, _ = helper.Process.Wait()
		}
	}()
	// Pretend the registered install is an old version: the upgrade
	// must reuse the registered directory.
	keyPath := `Software\Microsoft\Windows\CurrentVersion\Uninstall\` + identity + `_is1`
	key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE|registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		t.Fatalf("cannot open uninstall key %s: %v", keyPath, err)
	}
	if err := key.SetStringValue("DisplayVersion", "0.4.1"); err != nil {
		t.Fatal(err)
	}
	key.Close()
	if code, _ := runInstaller(""); code != 0 {
		t.Fatalf("upgrade exited %d", code)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "synthetic profile must survive" {
		t.Fatalf("profile sentinel did not survive the upgrade: %q (%v)", got, err)
	}
	if got, err := os.ReadFile(tool); err != nil || string(got) != "synthetic preserved tool" {
		t.Fatalf("preserved tool did not survive the upgrade: %q (%v)", got, err)
	}
	if sha(filepath.Join(target, "MusicOps.exe")) != sha(filepath.Join(bundle, "MusicOps.exe")) {
		t.Fatal("MusicOps.exe changed across the upgrade")
	}
	helperDeadline := time.Now().Add(10 * time.Second)
	for processAlive(helperPID) && time.Now().Before(helperDeadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(helperPID) {
		t.Fatalf("bundled helper %d survived the upgrade", helperPID)
	}
	checks["orphaned_helper_cleanup"] = true
	checks["upgrade_preserves"] = true

	// The installed shell starts its core; while it runs, an upgrade
	// must be refused.
	shell := exec.Command(filepath.Join(target, "MusicOps.exe"), "--profile", profile)
	shell.Env = append(os.Environ(), "MUSICOPS_SINGLE_INSTANCE_ID="+identity)
	shell.Dir = target
	if err := shell.Start(); err != nil {
		t.Fatalf("cannot start the installed shell: %v", err)
	}
	defer func() {
		if shell.Process != nil {
			_ = shell.Process.Kill()
			_, _ = shell.Process.Wait()
		}
	}()
	coreURL := ""
	var corePID uint32
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(profile, "state", "core.json"))
		if err == nil {
			var rec struct {
				URL string `json:"url"`
				PID uint32 `json:"pid"`
			}
			if json.Unmarshal(raw, &rec) == nil && rec.URL != "" {
				resp, err := http.Get(rec.URL + "/healthz")
				if err == nil && resp.StatusCode == http.StatusOK {
					_ = resp.Body.Close()
					coreURL = rec.URL
					corePID = rec.PID
					break
				}
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if coreURL == "" {
		t.Fatal("installed shell did not bring its core up within 30s")
	}
	t.Logf("installed core is up at %s", coreURL)
	if code, _ := runInstaller(""); code == 0 {
		t.Fatal("upgrade succeeded while the app was running")
	}
	checks["running_blocks_upgrade"] = true
	if shell.Process != nil {
		_ = shell.Process.Kill()
		_, _ = shell.Process.Wait()
		shell.Process = nil
	}
	if corePID != 0 {
		deadline = time.Now().Add(60 * time.Second)
		for processAlive(corePID) && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		if processAlive(corePID) {
			t.Fatalf("installed core pid %d did not stop after the shell exited", corePID)
		}
	}
	// After a safe exit the upgrade succeeds again.
	deadline = time.Now().Add(30 * time.Second)
	upgraded := false
	lastCode, lastOutput := 0, ""
	for time.Now().Before(deadline) {
		lastCode, lastOutput = runInstaller("")
		if lastCode == 0 {
			upgraded = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !upgraded {
		t.Fatalf("upgrade after the app exited did not succeed (last exit %d): %s", lastCode, lastOutput)
	}
	checks["upgrade_after_exit"] = true

	// Uninstall keeps the profile and the preserved tool, removes the exes.
	uninstaller := filepath.Join(target, "unins000.exe")
	cmd := exec.Command(uninstaller, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("profile sentinel did not survive uninstall: %v", err)
	}
	if _, err := os.Stat(tool); err != nil {
		t.Fatalf("preserved tool did not survive uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "MusicOps.exe")); !os.IsNotExist(err) {
		t.Fatal("MusicOps.exe still present after uninstall")
	}
	checks["uninstall_keeps"] = true
}

func processAlive(pid uint32) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == 259 // STILL_ACTIVE
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

// appIdentityFromISS reads the default AppIdentity from the release .iss
// next to the bundle's repo root (the bundle is
// <repo>/dist/MusicOps-Windows).
func appIdentityFromISS(t *testing.T, bundle string) string {
	t.Helper()
	repoRoot := filepath.Dir(filepath.Dir(bundle))
	data, err := os.ReadFile(filepath.Join(repoRoot, "packaging", "MusicOps-go.iss"))
	if err != nil {
		t.Fatalf("cannot read MusicOps-go.iss: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*#define\s+AppIdentity\s+"([^"]+)"`).FindSubmatch(data)
	if m == nil {
		t.Fatal("MusicOps-go.iss has no #define AppIdentity")
	}
	return string(m[1])
}
func TestWindowsInstallerHelperProcess(t *testing.T) {
	if os.Getenv("MUSICOPS_INSTALLER_SLEEP_HELPER") != "1" {
		return
	}
	select {}
}
