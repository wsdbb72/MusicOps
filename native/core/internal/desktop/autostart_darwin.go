//go:build darwin

package desktop

import (
	"os"
	"path/filepath"
)

// ~/Library/LaunchAgents/com.musicops.desktop.plist
// (RunAtLoad true, ProgramArguments [exe], Label com.musicops.desktop).
func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.musicops.desktop.plist"), nil
}

func enablePlatform(exe string) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicWriteFile(path, []byte(buildLaunchAgentPlist(exe)), 0644)
}

func disablePlatform() error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func enabledPlatform() (bool, error) {
	path, err := launchAgentPath()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	exe, runAtLoad, err := parseLaunchAgentPlist(raw)
	if err != nil {
		return false, err
	}
	return runAtLoad && exe != "", nil
}
