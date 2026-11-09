package desktop

import (
	"os"
	"strings"
)

// buildDesktopEntry renders the Linux XDG autostart entry for exe.
func buildDesktopEntry(exe string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=MusicOps\n" +
		"Exec=" + quoteDesktopExec(exe) + "\n" +
		"X-GNOME-Autostart-enabled=true\n"
}

// parseDesktopEntry extracts Exec= and X-GNOME-Autostart-enabled from a
// desktop entry file.
func parseDesktopEntry(data string) (exe string, enabled bool, err error) {
	seen := false
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Exec":
			exe = unquoteDesktopExec(value)
			seen = true
		case "X-GNOME-Autostart-enabled":
			enabled = strings.EqualFold(strings.TrimSpace(value), "true")
			seen = true
		}
	}
	if !seen {
		return "", false, os.ErrInvalid
	}
	return exe, enabled, nil
}
