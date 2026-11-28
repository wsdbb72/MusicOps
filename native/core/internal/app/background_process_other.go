//go:build !windows

package app

import "os/exec"

func prepareBackgroundCommand(_ *exec.Cmd) {}
