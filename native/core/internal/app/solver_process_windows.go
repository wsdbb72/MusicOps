//go:build windows

package app

import (
	"os/exec"
	"syscall"
)

// prepareSolverCommand hides the console window of the solver, the way the
// legacy wizard started it without a shell window. The browser it spawns has
// its own window policy.
func prepareSolverCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
