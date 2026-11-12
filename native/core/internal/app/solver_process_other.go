//go:build !windows

package app

import "os/exec"

// prepareSolverCommand is a no-op outside Windows: the managed path is
// Windows-only because the configured executable must be an absolute .exe.
func prepareSolverCommand(cmd *exec.Cmd) {}
