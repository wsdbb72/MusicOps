//go:build !windows

package desktop

import (
	"os"
	"syscall"
)

// signal kinds for signalPID.
const (
	sigTerm = 1 // SIGTERM: graceful shutdown request
	sigKill = 2 // SIGKILL: forced termination
)

// pidAlive reports whether pid names a live process (kill(pid, 0)).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// signalPID delivers SIGTERM or SIGKILL to pid.
func signalPID(pid int, sig int) error {
	if pid <= 0 {
		return nil
	}
	switch sig {
	case sigTerm:
		return syscall.Kill(pid, syscall.SIGTERM)
	default:
		return syscall.Kill(pid, syscall.SIGKILL)
	}
}

// acquireDesktopLock opens path and takes a non-blocking exclusive flock.
// A second desktop process gets an error instead of waiting.
func acquireDesktopLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// releaseDesktopLock unlocks and closes a lock file acquired by
// acquireDesktopLock. The file itself is kept (it is the lock's identity).
func releaseDesktopLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
