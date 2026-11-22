//go:build windows || darwin

package desktop

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

func TestApplyNativeWindowIconRetriesUntilWindowExists(t *testing.T) {
	calls := 0
	err := applyNativeWindowIconWithRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errors.New("window is not ready")
		}
		return nil
	}, 5, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want 3", calls)
	}
}

func TestApplyNativeWindowIconHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := applyNativeWindowIconWithRetry(ctx, func() error {
		return errors.New("window is not ready")
	}, 10, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}
