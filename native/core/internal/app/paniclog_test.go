package app

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// The full trace is written with the same redaction the task messages get, and
// a credential quoted in the panic value never reaches the file.
func TestRecordPanicWritesARedactedStack(t *testing.T) {
	application := newApp(t)
	application.recordPanic(
		"request failed: cookie=session-secret authkey=abc123",
		"goroutine 1 [running]:\nmain.executor()\n\t/app/executor.go:42 +0x1d",
	)
	raw, err := os.ReadFile(application.panicLogPath())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "executor.go:42") || !strings.Contains(body, "request failed") {
		t.Fatal(body)
	}
	if strings.Contains(body, "session-secret") || strings.Contains(body, "abc123") {
		t.Fatal("a credential reached panic.log:", body)
	}
	if !strings.Contains(body, "cookie=redacted") {
		t.Fatal(body)
	}
	// A second panic appends instead of replacing the first.
	application.recordPanic("second", "stack two")
	raw, err = os.ReadFile(application.panicLogPath())
	if err != nil || strings.Count(string(raw), "\n") != 2 {
		t.Fatal(len(raw), err)
	}
}

// The file rotates instead of growing without bound.
func TestRecordPanicRotatesTheLog(t *testing.T) {
	application := newApp(t)
	path := application.panicLogPath()
	if err := os.WriteFile(path, []byte(strings.Repeat("x", panicLogMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	application.recordPanic("after rotation", "stack")
	if _, err := os.Stat(path + ".previous"); err != nil {
		t.Fatal("the oversized log was not kept as .previous:", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "after rotation") || len(raw) > 4096 {
		t.Fatal(len(raw), err)
	}
}

// A very deep stack is clipped, and the clip does not split a multi-byte rune.
func TestRecordPanicClipsLongStacks(t *testing.T) {
	application := newApp(t)
	application.recordPanic("v", strings.Repeat("帧", panicStackMaxBytes))
	raw, err := os.ReadFile(application.panicLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > panicStackMaxBytes+2048 {
		t.Fatal(len(raw))
	}
	if !strings.Contains(string(raw), "已截断") {
		t.Fatal("the clip marker is missing")
	}
	if !utf8.Valid(raw) {
		t.Fatal("invalid utf-8")
	}
}
