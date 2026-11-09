// Autostart for the desktop shell.
//
// Python has no autostart implementation (the Qt workbench only toggles
// online autostart of the worker); this ports the standard platform
// mechanisms the packaged app needs: Windows Run key, macOS LaunchAgent,
// Linux XDG autostart entry.
//
// The file-format builders and parsers (plist, desktop entry) are pure
// functions in untagged files and are covered by tests; the platform
// Enable/Disable/Enabled entry points dispatch to enablePlatform and
// friends, one implementation per GOOS.
package desktop

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// autostartLabel is the value name / label used on every platform.
const autostartLabel = "MusicOps"

// Enable registers exe to start at login.
func Enable(exe string) error {
	if exe == "" {
		return errors.New("autostart executable path is required")
	}
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("autostart executable path must be absolute: %q", exe)
	}
	return enablePlatform(exe)
}

// Disable removes the login item. Removing a nonexistent entry is a no-op.
func Disable() error {
	return disablePlatform()
}

// Enabled reports whether a login item is currently registered.
func Enabled() (bool, error) {
	return enabledPlatform()
}

// quoteExePath renders exe as a single quoted command-line argument, as the
// Windows Run value requires: "C:\...\MusicOps.exe".
func quoteExePath(exe string) string {
	return `"` + strings.ReplaceAll(exe, `"`, `\"`) + `"`
}

// quoteDesktopExec renders exe for an XDG Exec= line: quoted only when it
// contains whitespace or quotes.
func quoteDesktopExec(exe string) string {
	if strings.ContainsAny(exe, " \t\"") {
		return `"` + strings.ReplaceAll(exe, `"`, `\"`) + `"`
	}
	return exe
}

// unquoteDesktopExec removes one layer of double quotes from an Exec=
// value, if present.
func unquoteDesktopExec(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return strings.ReplaceAll(value[1:len(value)-1], `\"`, `"`)
	}
	return value
}
