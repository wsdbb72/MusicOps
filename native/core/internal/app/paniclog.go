package app

// A recovered executor panic keeps only its first frame in the task row: a
// trace is too large for a row the UI renders on every list request, and a
// panic value can quote a credential. The full stack still has to be somewhere
// an operator can read it, so it goes to panic.log next to the state directory
// with the same redaction the task messages get.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

const (
	// panicLogMaxBytes bounds the file before it rotates to
	// panic.log.previous. One crash report is a few KiB, so this holds
	// hundreds of them.
	panicLogMaxBytes = 1 << 20
	// panicStackMaxBytes bounds one record. A deep stack is still useful at
	// 64 KiB, and the cap keeps a runaway goroutine dump from filling the file.
	panicStackMaxBytes = 64 << 10
)

// recordPanic appends one redacted crash report. It never fails the caller:
// crashing the scheduler because a log write failed would turn a contained
// panic into an outage.
func (a *App) recordPanic(value, stack string) {
	if a == nil || a.Dir == "" {
		return
	}
	path := filepath.Join(a.Dir, "panic.log")
	if info, err := os.Stat(path); err == nil && info.Size() > panicLogMaxBytes {
		previous := path + ".previous"
		_ = os.Remove(previous)
		_ = os.Rename(path, previous)
	}
	record := map[string]any{
		"at":    a.now().UTC().Format(time.RFC3339Nano),
		"value": clipText(scrub(value), 500),
		"stack": clipText(scrub(stack), panicStackMaxBytes),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

// panicLogPath is where recordPanic writes, for the health view and tests.
func (a *App) panicLogPath() string {
	if a == nil {
		return ""
	}
	return filepath.Join(a.Dir, "panic.log")
}

// clipText truncates on a rune boundary so a cut never leaves invalid UTF-8 in
// a JSON string.
func clipText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…（已截断）"
}
