//go:build !windows

package vault

import (
	"errors"
	"os"
	"syscall"
)

// checkDirOwnership verifies a private directory belongs to the current user.
func checkDirOwnership(dir string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot verify ownership of " + dir)
	}
	if int(stat.Uid) != os.Getuid() {
		return errors.New("private directory does not belong to the current user")
	}
	return nil
}
