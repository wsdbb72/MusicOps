package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

func TestPlaylistReleaseErrorKeepsClassAndCause(t *testing.T) {
	cause := fmt.Errorf("strict check: %w", audiorelease.ErrRejected)
	err := playlistReleaseError(cause, audiorelease.Result{Class: "decode_failed"})
	if !strings.Contains(err.Error(), "decode_failed") || !strings.Contains(err.Error(), "strict check") {
		t.Fatal(err)
	}
	if !errors.Is(err, audiorelease.ErrRejected) {
		t.Fatal("release cause was lost:", err)
	}
	conflict := playlistReleaseError(audiorelease.ErrConflict, audiorelease.Result{})
	if !strings.Contains(conflict.Error(), "library_conflict") || !errors.Is(conflict, audiorelease.ErrConflict) {
		t.Fatal(conflict)
	}
}
