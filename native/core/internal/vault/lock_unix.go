//go:build !windows

package vault

import (
	"os"
	"syscall"
	"time"
)

// tryLock takes a non-blocking exclusive flock on f, retrying for up to 5
// seconds. This is the same flock the Python store takes with fcntl.flock,
// so a Go locker and a Python locker exclude each other.
func tryLock(f *os.File) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
