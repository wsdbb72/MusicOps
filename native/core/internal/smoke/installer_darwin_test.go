//go:build darwin

package smoke

// Installer acceptance for the Go macOS PKG, ported from
// packaging/smoke_macos_installer.py: install the real PKG on a
// disposable macOS runner, verify the payload, the signature and the
// receipt, prove a running core blocks reinstall, reinstall over a
// stale file, and run the Go bundle smoke against the installed .app.
//
// Gated on MUSICOPS_SMOKE_PKG (the .pkg), MUSICOPS_SMOKE_APP (the
// built .app it must match) and MUSICOPS_SMOKE_WORK (scratch dir).
// Without those variables the test skips, like TestBundle.
//
// This test only runs on a disposable GitHub macOS runner: it refuses
// to run when /Applications/MusicOps.app or the user profile already
// exists, and it refuses to run outside GitHub Actions. It never
// removes the installed app afterwards; the runner is discarded.
//
// Only the standard library and os/exec are used. The vault roundtrip
// goes through internal/vault, which on macOS uses the Keychain master
// key; the release workflow provides an isolated unlocked test
// keychain via MUSICOPS_KEYCHAIN_PATH.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/vault"
)

const installerPkgID = "com.wsdbb72.MusicOps.pkg"

const installerAppID = "com.wsdbb72.MusicOps"

func runCmd(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func runSudo(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCmd(t, "/usr/bin/sudo", append([]string{"-n"}, args...)...)
}

// digestTree returns sha256 hex digests of every non-directory entry
// under root, keyed by slash-separated path relative to root. Symlinks
// are hashed by the content they point to (read through the link);
// the build script's audit already rejects links escaping the bundle.
func digestTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

func digestsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// plistStringValue reads one <string> value out of an XML plist,
// matched by the preceding <key>. Both pkgutil --pkg-info-plist and
// Info.plist are XML plists on macOS.
func plistStringValue(data []byte, wantKey string) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var lastKey string
	var inKey, inString bool
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			inKey = t.Name.Local == "key"
			inString = t.Name.Local == "string"
		case xml.CharData:
			text := strings.TrimSpace(string(t))
			if text == "" {
				continue
			}
			if inKey {
				lastKey = text
			} else if inString && lastKey == wantKey {
				return text, nil
			}
		}
	}
	return "", fmt.Errorf("key %q not found in plist", wantKey)
}

func randomSecret(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}

// startInstalledCore launches the core from the installed .app against
// a scratch profile and waits for its ready line. It returns the
// command, its stdout reader drain, and a stop function.
func startInstalledCore(t *testing.T, corePath, work string) (*exec.Cmd, func()) {
	t.Helper()
	stateDir := filepath.Join(work, "busy-profile", "state")
	backupDir := filepath.Join(work, "busy-profile", "backup")
	password, err := randomSecret(32)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(corePath,
		"--state-dir", stateDir, "--backup-dir", backupDir, "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "MUSICOPS_APP_PASSWORD="+password)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start installed core: %v", err)
	}
	stop := func() {
		if cmd.ProcessState != nil {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	// The core prints one ready JSON line, then serves.
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		var ready struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(line), &ready); err != nil || ready.URL == "" {
			stop()
			t.Fatalf("installed core ready line unreadable: %s (stderr: %s)", line, stderr.String())
		}
		t.Logf("installed core ready at %s", ready.URL)
	case err := <-errCh:
		stop()
		t.Fatalf("installed core printed no ready line: %v (stderr: %s)", err, stderr.String())
	case <-time.After(30 * time.Second):
		stop()
		t.Fatalf("installed core did not become ready within 30s (stderr: %s)", stderr.String())
	}
	return cmd, stop
}

func TestInstallerDarwin(t *testing.T) {
	pkgPath := os.Getenv("MUSICOPS_SMOKE_PKG")
	appPath := os.Getenv("MUSICOPS_SMOKE_APP")
	work := os.Getenv("MUSICOPS_SMOKE_WORK")
	if pkgPath == "" || appPath == "" || work == "" {
		t.Skip("MUSICOPS_SMOKE_PKG, MUSICOPS_SMOKE_APP and MUSICOPS_SMOKE_WORK are not set")
	}
	// Deliberate runs set the variables; an accidental run on a real
	// machine must not touch /Applications or the user's profile.
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("installer acceptance only runs on a disposable GitHub macOS runner (GITHUB_ACTIONS=true)")
	}
	installed := "/Applications/MusicOps.app"
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(home, "Library", "Application Support", "MusicOps", "profile")
	if _, err := os.Lstat(installed); err == nil {
		t.Fatalf("refuse to overwrite a pre-existing app on the acceptance machine: %s", installed)
	}
	if _, err := os.Lstat(profile); err == nil {
		t.Fatal("installer acceptance requires a new disposable user profile")
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runSudo(t, "true"); err != nil {
		t.Fatalf("sudo -n is not passwordless on this runner, installer steps cannot run: %v", err)
	}

	reportPath := os.Getenv("MUSICOPS_SMOKE_INSTALLER_REPORT")
	if reportPath == "" {
		reportPath = filepath.Join(work, "installer-smoke.json")
	}
	checks := map[string]bool{}
	report := Report{Evidence: "go_pkg_install_reinstall_and_installed_app", Checks: checks}
	defer func() {
		report.Passed = len(checks) > 0
		for _, ok := range checks {
			if !ok {
				report.Passed = false
			}
		}
		raw, _ := json.MarshalIndent(report, "", "  ")
		_ = os.WriteFile(reportPath, append(raw, '\n'), 0o644)
		t.Logf("installer report:\n%s", raw)
	}()
	check := func(name string, ok bool) {
		checks[name] = ok
		if !ok {
			t.Errorf("installer check failed: %s", name)
		}
	}

	// Version from the built app's Info.plist.
	infoRaw, err := os.ReadFile(filepath.Join(appPath, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	version, err := plistStringValue(infoRaw, "CFBundleShortVersionString")
	if err != nil {
		t.Fatal(err)
	}
	report.Version = version

	// A pre-existing profile sentinel plus a Vault credential: both
	// must survive install and reinstall untouched.
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(profile, "installer-preservation.txt")
	const sentinelText = "synthetic profile must survive"
	if err := os.WriteFile(sentinel, []byte(sentinelText), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := randomSecret(32)
	if err != nil {
		t.Fatal(err)
	}
	const vaultRef = "vault://installer/preservation"
	const vaultHost = "installer.example.invalid"
	if _, err := v.Set(vaultRef, "synthetic-installer-"+secret, vaultHost); err != nil {
		t.Fatal(err)
	}
	secret = "synthetic-installer-" + secret
	encryptedBefore, err := os.ReadFile(v.Path())
	if err != nil {
		t.Fatal(err)
	}
	profileSurvives := func() bool {
		data, err := os.ReadFile(sentinel)
		if err != nil || string(data) != sentinelText {
			return false
		}
		after, err := os.ReadFile(v.Path())
		if err != nil || !bytes.Equal(after, encryptedBefore) {
			return false
		}
		got, ok, err := v.Get(vaultRef, vaultHost)
		return err == nil && ok && got == secret
	}

	// First install.
	choices, err := runCmd(t, "/usr/sbin/installer", "-showChoicesXML", "-pkg", pkgPath, "-target", "/")
	if err != nil {
		t.Fatalf("installer -showChoicesXML failed: %v", err)
	}
	check("installer_choices_readable", strings.Contains(choices, "musicops"))

	if _, err := runSudo(t, "/usr/sbin/installer", "-pkg", pkgPath, "-target", "/"); err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	expected, err := digestTree(appPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(installed)
	if err != nil {
		t.Fatal(err)
	}
	check("installed_to_applications", st.IsDir() && st.Mode()&os.ModeSymlink == 0)
	installedDigests, err := digestTree(installed)
	if err != nil {
		t.Fatal(err)
	}
	check("installed_payload_matches_built_app", digestsEqual(expected, installedDigests))
	// Root-owned and not writable by the (non-root) test user.
	statT, _ := st.Sys().(*syscall.Stat_t)
	ownedByRoot := statT != nil && statT.Uid == 0
	notWritable := syscall.Access(installed, 0x2) != nil // W_OK
	check("app_owned_by_root", ownedByRoot && notWritable)
	if _, err := runCmd(t, "/usr/bin/codesign", "--verify", "--deep", "--strict", installed); err != nil {
		check("installed_app_signature_valid", false)
		t.Errorf("codesign --verify failed: %v", err)
	} else {
		check("installed_app_signature_valid", true)
	}
	receiptRaw, err := runCmd(t, "/usr/sbin/pkgutil", "--pkg-info-plist", installerPkgID)
	if err != nil {
		t.Fatalf("pkgutil receipt read failed: %v", err)
	}
	receiptVersion, err := plistStringValue([]byte(receiptRaw), "pkg-version")
	if err != nil {
		t.Fatal(err)
	}
	installedInfoRaw, err := os.ReadFile(filepath.Join(installed, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	installedVersion, err := plistStringValue(installedInfoRaw, "CFBundleShortVersionString")
	if err != nil {
		t.Fatal(err)
	}
	check("receipt_version_matches_app", receiptVersion == installedVersion && installedVersion == version)
	check("profile_and_credentials_survive_first_install", profileSurvives())

	// A running core must block replacement, and the running core must
	// not be terminated by the refused install.
	corePath := filepath.Join(installed, "Contents", "MacOS", "musicops-core")
	core, stopCore := startInstalledCore(t, corePath, work)
	_, err = runSudo(t, "/usr/sbin/installer", "-pkg", pkgPath, "-target", "/")
	stillRunning := core.ProcessState == nil
	afterRefusal, derr := digestTree(installed)
	check("running_core_prevents_replacement", err != nil && stillRunning && derr == nil && digestsEqual(expected, afterRefusal))
	if err == nil {
		t.Errorf("installer unexpectedly succeeded while the core was running")
	}
	stopCore()

	// Reinstall over a stale file: the stale file goes, the payload
	// matches the build again, the profile survives.
	stale := filepath.Join(installed, "Contents", "obsolete-from-previous-version.txt")
	if _, err := runSudo(t, "/usr/bin/touch", stale); err != nil {
		t.Fatalf("cannot plant stale file: %v", err)
	}
	if _, err := runSudo(t, "/usr/sbin/installer", "-pkg", pkgPath, "-target", "/"); err != nil {
		t.Fatalf("reinstall failed: %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		check("reinstall_removes_obsolete_bundle_files", false)
	} else {
		reinstalled, err := digestTree(installed)
		if err != nil {
			t.Fatal(err)
		}
		check("reinstall_removes_obsolete_bundle_files", digestsEqual(expected, reinstalled))
	}
	check("reinstall_preserves_profile_and_credentials", profileSurvives())

	// The installed .app must pass the same bundle smoke as the built
	// one. smoke.Run joins BundleDir with CoreName/DesktopName, so the
	// binaries are named by their path inside the .app; BundleDir
	// stays the .app root so the manifest, tools and no-Python checks
	// cover the whole bundle.
	smokeReport := Run(context.Background(), Config{
		BundleDir:       installed,
		CoreName:        filepath.Join("Contents", "MacOS", "musicops-core"),
		DesktopName:     filepath.Join("Contents", "MacOS", "MusicOps"),
		ToolsDir:        filepath.Join(installed, "Contents", "Resources", "tools"),
		ServicesDir:     filepath.Join(installed, "Contents", "Resources", "tools", "services"),
		ManifestRel:     filepath.Join("Contents", "Resources", "manifest.json"),
		RequireEngines:  true,
		RequireManifest: true,
		Version:         version,
		WorkDir:         filepath.Join(work, "installed-app-smoke"),
		Timeout:         5 * time.Minute,
	})
	raw, _ := json.MarshalIndent(smokeReport, "", "  ")
	t.Logf("installed app smoke report:\n%s", raw)
	check("installed_app_acceptance", smokeReport.Passed)

	if !reportPassed(checks) {
		t.Fatalf("installer acceptance failed; report at %s", reportPath)
	}
}

func reportPassed(checks map[string]bool) bool {
	if len(checks) == 0 {
		return false
	}
	for _, ok := range checks {
		if !ok {
			return false
		}
	}
	return true
}
