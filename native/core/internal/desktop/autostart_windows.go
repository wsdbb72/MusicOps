//go:build windows

package desktop

import "golang.org/x/sys/windows/registry"

// HKCU\Software\Microsoft\Windows\CurrentVersion\Run, value name MusicOps,
// value "exe path" (quoted).
const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = autostartLabel
)

func enablePlatform(exe string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue(runValueName, quoteExePath(exe))
}

func disablePlatform() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return nil // no Run key at all: already disabled
	}
	defer key.Close()
	if err := key.DeleteValue(runValueName); err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	return nil
}

func enabledPlatform() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false, nil // no Run key at all: disabled
	}
	defer key.Close()
	value, _, err := key.GetStringValue(runValueName)
	if err != nil {
		return false, nil
	}
	return value != "", nil
}
