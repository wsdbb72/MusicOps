//go:build !windows

package updater

import (
	"os/exec"
	"syscall"
)

// applyLaunchAttr detaches the launcher on Unix so the installer outlives
// the CLI process, matching Python's start_new_session for /usr/bin/open.
func applyLaunchAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
