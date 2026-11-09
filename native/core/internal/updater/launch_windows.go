//go:build windows

package updater

import (
	"os/exec"
	"syscall"
)

// applyLaunchAttr hides the launcher window on Windows, matching Python's
// CREATE_NO_WINDOW creation flag for the PowerShell handoff.
func applyLaunchAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
