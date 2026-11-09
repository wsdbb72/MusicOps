package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// toolsDirFor resolves the bundled tools directory from the core binary
// path: <core-dir>/tools on Windows/Linux, and
// Contents/Resources/tools when the core sits in Contents/MacOS on macOS.
func TestToolsDirFor(t *testing.T) {
	root := t.TempDir()
	core := filepath.Join(root, "musicops")
	if runtime.GOOS == "windows" {
		core += ".exe"
	}
	if err := os.MkdirAll(filepath.Join(root, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := toolsDirFor(core); got != filepath.Join(root, "tools") {
		t.Fatalf("got %q, want %q", got, filepath.Join(root, "tools"))
	}
	// No tools directory: no PATH injection.
	bare := filepath.Join(t.TempDir(), "musicops")
	if got := toolsDirFor(bare); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if runtime.GOOS == "darwin" {
		app := filepath.Join(t.TempDir(), "MusicOps.app", "Contents")
		core := filepath.Join(app, "MacOS", "MusicOps")
		tools := filepath.Join(app, "Resources", "tools")
		if err := os.MkdirAll(tools, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := toolsDirFor(core); got != tools {
			t.Fatalf("got %q, want %q", got, tools)
		}
	}
}

func TestWithPathPrefix(t *testing.T) {
	env := []string{"HOME=/home/x", "PATH=/usr/bin:/bin", "LANG=C"}
	got := withPathPrefix(env, "/opt/musicops/tools")
	var path string
	count := 0
	for _, kv := range got {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			path, count = v, count+1
		}
	}
	if count != 1 {
		t.Fatalf("PATH appears %d times: %v", count, got)
	}
	if path != "/opt/musicops/tools"+string(os.PathListSeparator)+"/usr/bin:/bin" {
		t.Fatalf("PATH = %q", path)
	}
	// No existing PATH: one is appended, nothing else is dropped.
	got = withPathPrefix([]string{"HOME=/home/x"}, "/tools")
	if len(got) != 2 || got[0] != "HOME=/home/x" || got[1] != "PATH=/tools" {
		t.Fatalf("got %v", got)
	}
}
