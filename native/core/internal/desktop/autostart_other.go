//go:build !windows && !darwin && !linux

package desktop

import (
	"fmt"
	"runtime"
)

func enablePlatform(exe string) error {
	return fmt.Errorf("autostart is not supported on %s", runtime.GOOS)
}

func disablePlatform() error {
	return fmt.Errorf("autostart is not supported on %s", runtime.GOOS)
}

func enabledPlatform() (bool, error) {
	return false, fmt.Errorf("autostart is not supported on %s", runtime.GOOS)
}
