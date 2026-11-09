package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchAgentPlistRoundTrip(t *testing.T) {
	exe := "/Applications/MusicOps.app/Contents/MacOS/MusicOps"
	built := buildLaunchAgentPlist(exe)
	got, runAtLoad, err := parseLaunchAgentPlist([]byte(built))
	if err != nil {
		t.Fatal(err)
	}
	if got != exe {
		t.Fatalf("exe = %q, want %q", got, exe)
	}
	if !runAtLoad {
		t.Fatal("RunAtLoad = false, want true")
	}
}

func TestLaunchAgentPlistEscapesSpecialChars(t *testing.T) {
	exe := `/Users/a & b/MusicOps <"x">.app/Contents/MacOS/MusicOps`
	built := buildLaunchAgentPlist(exe)
	got, _, err := parseLaunchAgentPlist([]byte(built))
	if err != nil {
		t.Fatal(err)
	}
	if got != exe {
		t.Fatalf("round trip: got %q, want %q", got, exe)
	}
}

func TestLaunchAgentPlistRunAtLoadFalse(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.musicops.desktop</string>
<key>RunAtLoad</key><false/>
<key>ProgramArguments</key><array><string>/opt/MusicOps</string></array>
</dict></plist>`
	got, runAtLoad, err := parseLaunchAgentPlist([]byte(plist))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/MusicOps" || runAtLoad {
		t.Fatalf("got exe=%q runAtLoad=%v", got, runAtLoad)
	}
}

func TestLaunchAgentPlistGarbage(t *testing.T) {
	if _, _, err := parseLaunchAgentPlist([]byte("not xml at all")); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestDesktopEntryRoundTrip(t *testing.T) {
	for _, exe := range []string{"/usr/bin/musicops", "/home/a b/musicops-desktop"} {
		built := buildDesktopEntry(exe)
		got, enabled, err := parseDesktopEntry(built)
		if err != nil {
			t.Fatal(err)
		}
		if got != exe {
			t.Fatalf("exe = %q, want %q", got, exe)
		}
		if !enabled {
			t.Fatal("enabled = false, want true")
		}
	}
}

func TestDesktopEntryDisabled(t *testing.T) {
	entry := "[Desktop Entry]\nType=Application\nExec=/usr/bin/musicops\nX-GNOME-Autostart-enabled=false\n"
	got, enabled, err := parseDesktopEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/musicops" || enabled {
		t.Fatalf("got exe=%q enabled=%v", got, enabled)
	}
}

func TestQuoteExePath(t *testing.T) {
	if got := quoteExePath(`C:\Program Files\MusicOps\MusicOps.exe`); got != `"C:\Program Files\MusicOps\MusicOps.exe"` {
		t.Fatalf("got %q", got)
	}
}

func TestAutostartLinuxEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	enabled, err := Enabled()
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("Enabled = true on a fresh HOME")
	}
	if err := Enable("/usr/bin/musicops"); err != nil {
		t.Fatal(err)
	}
	enabled, err = Enabled()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("Enabled = false after Enable")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "autostart", "musicops.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); !contains(got, "Exec=/usr/bin/musicops") || !contains(got, "Type=Application") {
		t.Fatalf("desktop entry = %q", got)
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	enabled, err = Enabled()
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("Enabled = true after Disable")
	}
	// Disabling twice is a no-op.
	if err := Disable(); err != nil {
		t.Fatalf("second Disable: %v", err)
	}
}

func TestAutostartRejectsRelativeExe(t *testing.T) {
	if err := Enable("relative/musicops"); err == nil {
		t.Fatal("expected an error for a relative exe path")
	}
	if err := Enable(""); err == nil {
		t.Fatal("expected an error for an empty exe path")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
