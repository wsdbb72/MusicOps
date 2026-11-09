//go:build !windows

package desktop

import (
	"os"
	"syscall"
)

// statUID returns the owning UID of info on Unix, for the
// private_directory same-owner check.
func statUID(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
