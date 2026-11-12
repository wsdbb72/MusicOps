package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A detached core writes its stderr straight into core.log and nothing rotates
// it while it runs, so the next start keeps the oversized file instead of
// truncating the only evidence of what happened.
func TestRotateCoreLogKeepsThePreviousRun(t *testing.T) {
	profile := t.TempDir()
	if err := os.MkdirAll(filepath.Join(profile, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := coreLogPath(profile)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", coreLogMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateCoreLog(profile)
	if _, err := os.Stat(strings.TrimSuffix(path, ".log") + ".previous.log"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err == nil {
		t.Fatalf("the oversized log was left in place: %d bytes", info.Size())
	}
}

// A small log is left alone: rotating every start would lose the tail of a
// short run that ended in a crash.
func TestRotateCoreLogLeavesASmallLog(t *testing.T) {
	profile := t.TempDir()
	if err := os.MkdirAll(filepath.Join(profile, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := coreLogPath(profile)
	if err := os.WriteFile(path, []byte("short run"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateCoreLog(profile)
	if body, err := os.ReadFile(path); err != nil || string(body) != "short run" {
		t.Fatal(string(body), err)
	}
	if _, err := os.Stat(strings.TrimSuffix(path, ".log") + ".previous.log"); !os.IsNotExist(err) {
		t.Fatal("a small log was rotated")
	}
}
