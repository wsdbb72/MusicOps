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
	requireText(t, desktop, "pull_request:")
	publishIf := "if: github.event_name == 'push' || (github.event_name == 'workflow_dispatch' && inputs.publish)"
	if strings.Count(desktop, publishIf) != 2 {
		t.Fatalf("publish and Docker latest must both be limited to a real publication, count=%d", strings.Count(desktop, publishIf))
	}
	requireText(t, desktop, "needs: [windows, macos, linux]")
	requireText(t, desktop, "name: Publish only the complete verified desktop release")
	requireText(t, desktop, "needs: [publish, linux]")
	requireText(t, desktop, "name: Advance Docker latest after the complete release")
	requireText(t, desktop, "musicops-desktop-publication")
	requireText(t, desktop, "cancel-in-progress: ${{ github.event_name == 'pull_request' }}")
	requireText(t, desktop, "push_image: ${{ github.event_name == 'push' || (github.event_name == 'workflow_dispatch' && inputs.publish) }}")
	requireText(t, desktop, "'MusicOps '+version")
	requireText(t, desktop, "A release with this version already exists")
	requireText(t, macos, "arch: arm64")
	requireText(t, macos, "architecture: arm64")
	if strings.Contains(macos, "arch: amd64") || strings.Contains(macos, "architecture: x86_64") {
		t.Fatal("new macOS release matrix includes Intel")
	}
	requireText(t, docker, "platforms: linux/amd64")
	requireText(t, docker, "refusing to overwrite")
	requireText(t, docker, "push_image:")
	if strings.Count(docker, "if: inputs.push_image") < 2 {
		t.Fatal("Docker login and push must both stay behind push_image")
	}
	requireText(t, docker, "!inputs.push_image")
}
