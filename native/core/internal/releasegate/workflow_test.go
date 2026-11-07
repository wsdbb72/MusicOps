package releasegate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, ".github", "workflows", "desktop-release.yml")
		if _, err := os.Stat(candidate); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("desktop release workflow not found")
	return ""
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func requireText(t *testing.T, body, fragment string) {
	t.Helper()
	if !strings.Contains(body, fragment) {
		t.Fatalf("missing %q", fragment)
	}
}

func TestDesktopReleasePublishesOnlyAfterAllThreePlatforms(t *testing.T) {
	desktop := readWorkflow(t, "desktop-release.yml")
	macos := readWorkflow(t, "macos-release.yml")
	docker := readWorkflow(t, "docker-release.yml")
	requireText(t, desktop, "branches: ['codex/release-*']")
	requireText(t, desktop, "workflow_dispatch:")
	if strings.Contains(desktop, "pull_request:") {
		t.Fatal("desktop release is armed for pull requests")
	}
	requireText(t, desktop, "needs: [windows, macos, linux]")
	requireText(t, desktop, "name: Publish only the complete verified desktop release")
	requireText(t, desktop, "needs: [publish, linux]")
	requireText(t, desktop, "name: Advance Docker latest after the complete release")
	requireText(t, desktop, "group: musicops-desktop-publication")
	requireText(t, desktop, "cancel-in-progress: false")
	requireText(t, desktop, "'MusicOps '+version")
	requireText(t, desktop, "A release with this version already exists")
	requireText(t, macos, "arch: arm64")
	requireText(t, macos, "architecture: arm64")
	if strings.Contains(macos, "arch: amd64") || strings.Contains(macos, "architecture: x86_64") {
		t.Fatal("new macOS release matrix includes Intel")
	}
	requireText(t, docker, "platforms: linux/amd64")
	requireText(t, docker, "refusing to overwrite")
}
