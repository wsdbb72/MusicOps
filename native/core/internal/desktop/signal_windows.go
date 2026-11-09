//go:build windows

package desktop

import (
	"os"

	"golang.org/x/sys/windows"
)

// signal kinds for signalPID.
const (
	sigTerm = 1 // best effort: console break, then the caller falls back to kill
	sigKill = 2 // TerminateProcess
)

// pidAlive reports whether pid names a live process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	// STILL_ACTIVE (259) is the Win32 exit code of a running process;
	// x/sys/windows does not export it as a constant.
	return code == 259
}

// signalPID delivers a shutdown request to pid. Windows has no SIGTERM:
// sigTerm tries a console-break via the process handle's console (best
// effort, errors ignored by the caller), sigKill terminates immediately.
func signalPID(pid int, sig int) error {
	if pid <= 0 {
		return nil
	}
	if sig == sigKill {
		handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			return err
		}
		defer windows.CloseHandle(handle)
		return windows.TerminateProcess(handle, 1)
	}
	// Best effort graceful request: GenerateConsoleCtrlEvent only reaches
	// processes sharing our console, so failures are normal; the caller
	// falls back to sigKill after the grace period.
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = p.Signal(os.Interrupt)
	return nil
}

// acquireDesktopLock opens path and takes a non-blocking exclusive
// LockFileEx. A second desktop process gets an error instead of waiting.
func acquireDesktopLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(f.Fd())
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// releaseDesktopLock unlocks and closes a lock file acquired by
// acquireDesktopLock.
func releaseDesktopLock(f *os.File) {
	handle := windows.Handle(f.Fd())
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
	_ = f.Close()
}
