//go:build windows || darwin

package desktop

import "testing"

func TestSingleInstanceKeySupportsIsolatedProfiles(t *testing.T) {
	t.Setenv("MUSICOPS_SINGLE_INSTANCE_ID", "")
	if got := singleInstanceKey(); got != "musicops-desktop" {
		t.Fatal(got)
	}
	t.Setenv("MUSICOPS_SINGLE_INSTANCE_ID", "MusicOps.QA.abc123")
	first := singleInstanceKey()
	second := singleInstanceKey()
	if first == "musicops-desktop" || first != second || len(first) > 64 {
		t.Fatal(first, second)
	}
	t.Setenv("MUSICOPS_SINGLE_INSTANCE_ID", "MusicOps.QA.def456")
	if singleInstanceKey() == first {
		t.Fatal("different QA identities shared one window lock")
	}
}
