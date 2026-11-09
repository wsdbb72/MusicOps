//go:build !windows

package desktop

import "syscall"

// detachProcAttr puts a detached core in its own session, so signals aimed
// at the short-lived parent's process group (e.g. Ctrl-C in the terminal
// that ran the CLI) do not reach it.
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
