//go:build windows

package app

import (
	"runtime"

	"golang.org/x/sys/windows"
)

const (
	threadModeBackgroundBegin = 0x00010000
	threadModeBackgroundEnd   = 0x00020000
)

var setThreadPriority = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadPriority")

// runWithBackgroundIO puts the dedicated OS thread into Windows background
// mode. Windows then assigns foreground playback a higher I/O priority even
// when both read the same physical disk. If the platform rejects the mode, the
// caller still has the explicit byte-rate cap as a safe fallback.
func runWithBackgroundIO(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.GetCurrentThread()
	if err != nil {
		return fn()
	}
	started, _, _ := setThreadPriority.Call(uintptr(thread), uintptr(threadModeBackgroundBegin))
	if started == 0 {
		return fn()
	}
	defer setThreadPriority.Call(uintptr(thread), uintptr(threadModeBackgroundEnd))
	return fn()
}
