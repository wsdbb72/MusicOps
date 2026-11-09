package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file build the real cmd/musicops binary once and drive
// it as the supervised core — no shell-script stand-ins. The binary is
// built in TestMain into a temp directory shared by the package's tests.
var testCoreBinary string

func TestMain(m *testing.M) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		home, _ := os.UserHomeDir()
		goBin = filepath.Join(home, ".local", "go", "bin", "go")
	}
	dir, err := os.MkdirTemp("", "musicops-desktop-test-core-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	testCoreBinary = filepath.Join(dir, "musicops-test")
	cmd := exec.Command(goBin, "build", "-o", testCoreBinary, "./cmd/musicops")
	cmd.Dir = filepath.Join("..", "..") // native/core
	cmd.Env = append(os.Environ(), "GOTMPDIR="+filepath.Join(os.Getenv("HOME"), ".cache", "go-tmp"))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot build the test core binary:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// testProfile returns a fresh profile with a vault key file configured.
func testProfile(t *testing.T) string {
	t.Helper()
	profile := filepath.Join(t.TempDir(), "profile")
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", filepath.Join(t.TempDir(), "vault.key"))
	return profile
}

func coreHealth(t *testing.T, handle *CoreHandle) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, handle.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("musicops", handle.Password)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
}

// waitCoreStatus polls CoreStatus until Running matches want or the
// deadline passes.
func waitCoreStatus(t *testing.T, profile string, want bool, timeout time.Duration) CoreStatusInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := CoreStatus(profile)
		if err != nil {
			t.Fatal(err)
		}
		if status.Running == want {
			return status
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("CoreStatus did not become Running=%v within %s", want, timeout)
	return CoreStatusInfo{}
}

func TestEnsureCoreStartsAndAttaches(t *testing.T) {
	profile := testProfile(t)
	handle, err := EnsureCore(profile, testCoreBinary)
	if err != nil {
		t.Fatal(err)
	}
	if handle.URL == "" || handle.PID <= 0 || len(handle.Password) < 16 || !handle.StartedByUs {
		t.Fatalf("bad handle: %+v", handle)
	}
	coreHealth(t, handle)

	// Rendezvous files are on disk, 0600.
	for _, path := range []string{coreJSONPath(profile), corePasswordPath(profile)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %o, want 0600", path, info.Mode().Perm())
		}
	}
	var rec rendezvous
	raw, err := os.ReadFile(coreJSONPath(profile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.URL != handle.URL || rec.PID != handle.PID || rec.StartedAtUnix <= 0 {
		t.Fatalf("core.json = %+v, handle = %+v", rec, handle)
	}

	// CoreStatus sees the same live core.
	status, err := CoreStatus(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.URL != handle.URL || status.PID != handle.PID {
		t.Fatalf("CoreStatus = %+v", status)
	}

	// A second EnsureCore attaches instead of starting a duplicate.
	attached, err := EnsureCore(profile, testCoreBinary)
	if err != nil {
		t.Fatal(err)
	}
	if attached.StartedByUs || attached.PID != handle.PID || attached.URL != handle.URL {
		t.Fatalf("attach returned %+v, want the same core", attached)
	}

	// Clean shutdown through StopCore, then idempotent again.
	if err := StopCore(profile); err != nil {
		t.Fatal(err)
	}
	waitCoreStatus(t, profile, false, 15*time.Second)
	if _, err := os.Stat(coreJSONPath(profile)); !os.IsNotExist(err) {
		t.Fatal("core.json was not cleaned up")
	}
	if _, err := os.Stat(corePasswordPath(profile)); !os.IsNotExist(err) {
		t.Fatal("core-password was not cleaned up")
	}
	if err := StopCore(profile); err != nil {
		t.Fatalf("second StopCore: %v", err)
	}
}

func TestEnsureCoreBadBinary(t *testing.T) {
	profile := testProfile(t)
	if _, err := EnsureCore(profile, "/nonexistent/musicops-test-binary"); err == nil {
		t.Fatal("expected an error for a missing core binary")
	}
}

// TestEnsureCoreDetached starts a core that survives its starter: no
// --parent-pipe, stdout/stderr in core.log, ready URL read from the log.
func TestEnsureCoreDetached(t *testing.T) {
	profile := testProfile(t)
	handle, err := EnsureCoreDetached(profile, testCoreBinary)
	if err != nil {
		t.Fatal(err)
	}
	if handle.URL == "" || handle.PID <= 0 || len(handle.Password) < 16 || !handle.StartedByUs {
		t.Fatalf("bad handle: %+v", handle)
	}
	coreHealth(t, handle)
	t.Cleanup(func() { _ = StopCore(profile) })

	// The detached core wrote its ready JSON to core.log, not a pipe.
	raw, err := os.ReadFile(coreLogPath(profile))
	if err != nil {
		t.Fatalf("core.log: %v", err)
	}
	if !strings.Contains(string(raw), `"ready":true`) || !strings.Contains(string(raw), handle.URL) {
		t.Fatalf("core.log does not contain the ready line for %s:\n%s", handle.URL, raw)
	}
	if info, err := os.Stat(coreLogPath(profile)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("core.log stat = %v, mode = %v, want 0600", err, info.Mode().Perm())
	}

	// CoreStatus sees the same live core.
	status, err := CoreStatus(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.URL != handle.URL || status.PID != handle.PID {
		t.Fatalf("CoreStatus = %+v", status)
	}

	// A second EnsureCoreDetached attaches instead of starting a duplicate.
	attached, err := EnsureCoreDetached(profile, testCoreBinary)
	if err != nil {
		t.Fatal(err)
	}
	if attached.StartedByUs || attached.PID != handle.PID || attached.URL != handle.URL {
		t.Fatalf("attach returned %+v, want the same core", attached)
	}

	// StopCore terminates the detached core and cleans the rendezvous.
	if err := StopCore(profile); err != nil {
		t.Fatal(err)
	}
	waitCoreStatus(t, profile, false, 15*time.Second)
	if _, err := os.Stat(coreJSONPath(profile)); !os.IsNotExist(err) {
		t.Fatal("core.json was not cleaned up")
	}
}

func TestSupervisorRestartsKilledCore(t *testing.T) {
	profile := testProfile(t)
	sup := New(profile, testCoreBinary)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sup.Stop() }()

	first := waitCoreStatus(t, profile, true, 30*time.Second)
	handle := sup.Handle()
	if handle == nil || handle.PID != first.PID {
		t.Fatalf("supervisor handle %+v vs status %+v", handle, first)
	}
	coreHealth(t, handle)

	// Kill the core out from under the supervisor.
	proc, err := os.FindProcess(first.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	// Wait for the death to be visible, then for the restart.
	waitCoreStatus(t, profile, false, 15*time.Second)
	// The supervisor restarts it on a new PID.
	second := waitCoreStatus(t, profile, true, 30*time.Second)
	if second.PID == first.PID {
		t.Fatal("core was not restarted with a new PID")
	}
	if second.URL == first.URL {
		t.Fatal("restarted core listens on the same URL (expected a fresh :0 port)")
	}
	// lifecycle.jsonl records the exit and the restart.
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, _ := os.ReadFile(lifecyclePath(profile))
		if strings.Contains(string(raw), "core_exited") && strings.Contains(string(raw), "core_restarted") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lifecycle.jsonl lacks exit/restart events: %q", raw)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The health file is being written with the new PID.
	deadline = time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(healthPath(profile))
		if err == nil {
			var rec healthRecord
			if json.Unmarshal(raw, &rec) == nil && rec.PID == second.PID {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("worker-health.json was not updated to the restarted core")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Clean stop: core dead, rendezvous gone, health kept, stop file set.
	if err := sup.Stop(); err != nil {
		t.Fatal(err)
	}
	waitCoreStatus(t, profile, false, 15*time.Second)
	if _, err := os.Stat(coreJSONPath(profile)); !os.IsNotExist(err) {
		t.Fatal("core.json was not cleaned up by Stop")
	}
	if _, err := os.Stat(corePasswordPath(profile)); !os.IsNotExist(err) {
		t.Fatal("core-password was not cleaned up by Stop")
	}
	if _, err := os.Stat(healthPath(profile)); err != nil {
		t.Fatalf("health file should be kept after Stop: %v", err)
	}
	if _, err := os.Stat(stopFilePath(profile)); err != nil {
		t.Fatalf("supervisor-stop missing after Stop: %v", err)
	}
	// Idempotent.
	if err := sup.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestSupervisorSecondInstanceRefused(t *testing.T) {
	profile := testProfile(t)
	first := New(profile, testCoreBinary)
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Stop() }()
	waitCoreStatus(t, profile, true, 30*time.Second)

	second := New(profile, testCoreBinary)
	if err := second.Start(context.Background()); err == nil {
		_ = second.Stop()
		t.Fatal("expected the second supervisor to be refused by the desktop lock")
	}
	// After the first stops, a new supervisor can start.
	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}
	third := New(profile, testCoreBinary)
	if err := third.Start(context.Background()); err != nil {
		t.Fatalf("supervisor after first stop: %v", err)
	}
	defer func() { _ = third.Stop() }()
	waitCoreStatus(t, profile, true, 30*time.Second)
}

func TestSupervisorStartFailsWithoutBinary(t *testing.T) {
	profile := testProfile(t)
	sup := New(profile, "/nonexistent/musicops-test-binary")
	if err := sup.Start(context.Background()); err == nil {
		_ = sup.Stop()
		t.Fatal("expected Start to fail with a missing core binary")
	}
	// The failed start must not leave the lock held.
	sup2 := New(profile, testCoreBinary)
	if err := sup2.Start(context.Background()); err != nil {
		t.Fatalf("supervisor after failed start: %v", err)
	}
	defer func() { _ = sup2.Stop() }()
	waitCoreStatus(t, profile, true, 30*time.Second)
}
