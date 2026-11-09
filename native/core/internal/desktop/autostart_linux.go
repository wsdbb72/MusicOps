//go:build linux

package desktop

import (
	"os"
	"path/filepath"
)

// ~/.config/autostart/musicops.desktop
// (XDG Desktop Entry, Exec=exe, Type=Application,
// X-GNOME-Autostart-enabled=true).
func autostartFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "autostart", "musicops.desktop"), nil
}

func enablePlatform(exe string) error {
	path, err := autostartFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicWriteFile(path, []byte(buildDesktopEntry(exe)), 0644)
}

func disablePlatform() error {
	path, err := autostartFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func enabledPlatform() (bool, error) {
	path, err := autostartFilePath()
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
	exe, enabled, err := parseDesktopEntry(string(raw))
	if err != nil {
		return false, err
	}
	return enabled && exe != "", nil
}
