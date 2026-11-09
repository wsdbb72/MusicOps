//go:build !windows

package app

import (
	"errors"
	"os"
	"syscall"
)

// checkNotHardlinked refuses files with more than one hard link, as
// Python apply_tag_changes does: editing one name would silently change
// another.
func checkNotHardlinked(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return errors.New("file has more than one hard link")
	}
	return nil
}
