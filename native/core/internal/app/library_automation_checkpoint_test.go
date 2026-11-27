package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLibraryAutomationScheduleSurvivesRestartAndFailureRetry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if got := libraryAutomationNextRun(now, libraryAutomationSchedule{}, 6*time.Hour); !got.Equal(now.Add(libraryAutomationInitialDelay)) {
		t.Fatalf("initial next run=%s", got)
	}
	recent := libraryAutomationSchedule{LastAttemptNS: now.Add(-2 * time.Hour).UnixNano(), LastFinishedNS: now.Add(-time.Hour).UnixNano()}
	if got := libraryAutomationNextRun(now, recent, 6*time.Hour); !got.Equal(now.Add(5 * time.Hour)) {
		t.Fatalf("persisted next run=%s", got)
	}
	interrupted := libraryAutomationSchedule{LastAttemptNS: now.Add(-2 * time.Hour).UnixNano(), LastFinishedNS: now.Add(-time.Hour).UnixNano()}
	interrupted.LastAttemptNS = now.Add(-time.Minute).UnixNano()
	if got := libraryAutomationNextRun(now, libraryAutomationSchedule{LastAttemptNS: now.Add(-2 * time.Hour).UnixNano(), LastFinishedNS: now.Add(-time.Hour).UnixNano()}, 6*time.Hour); !got.Equal(now.Add(5 * time.Hour)) {
		t.Fatalf("unexpected persisted run=%s", got)
	}
	interrupted.LastFinishedNS = now.Add(-2 * time.Hour).UnixNano()
	if got := libraryAutomationNextRun(now, interrupted, 6*time.Hour); !got.Equal(now.Add(libraryAutomationInitialDelay)) {
		t.Fatalf("interrupted next run=%s", got)
	}
	retry := libraryAutomationSchedule{LastFinishedNS: now.UnixNano(), RetryAfterNS: now.Add(libraryAutomationFailureRetry).UnixNano()}
	if got := libraryAutomationNextRun(now, retry, 6*time.Hour); !got.Equal(time.Unix(0, retry.RetryAfterNS)) {
		t.Fatalf("failure retry=%s", got)
	}
}

func TestLibraryAutomationReindexIsDailyAndRootScoped(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	root := filepath.Join(t.TempDir(), "Music")
	if !libraryAutomationReindexDue(now, libraryAutomationSchedule{}, root) {
		t.Fatal("missing checkpoint did not require reindex")
	}
	fresh := libraryAutomationSchedule{LastReindexNS: now.Add(-time.Hour).UnixNano(), LastReindexRoot: root}
	if libraryAutomationReindexDue(now, fresh, root) {
		t.Fatal("fresh checkpoint forced reindex")
	}
	if libraryAutomationReindexDue(now, fresh, filepath.Join(filepath.Dir(root), "Other")) {
		// Different roots require their own first index.
	} else {
		t.Fatal("different root reused another library checkpoint")
	}
	old := libraryAutomationSchedule{LastReindexNS: now.Add(-libraryAutomationReindexInterval).UnixNano(), LastReindexRoot: root}
	if !libraryAutomationReindexDue(now, old, root) {
		t.Fatal("daily reindex did not become due")
	}
}

func TestIndexedLibraryAudioUsesDatabaseWithoutStattingFiles(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	folder := filepath.Join(library, "Artist", "Album")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(folder, "01 One.flac")
	second := filepath.Join(folder, "02 Two.flac")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("placeholder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := application.indexLibraryTree(context.Background(), library); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, second} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	paths, byDir, err := application.indexedLibraryAudio(library)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || len(byDir[folder]) != 2 {
		t.Fatalf("paths=%v byDir=%v", paths, byDir)
	}
}

func TestLibraryAutomationPassSkipsFreshFullReindex(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	if err := application.recordLibraryReindex(library); err != nil {
		t.Fatal(err)
	}
	settings := libraryAutomationSettings{Enabled: true, IntervalHours: 6}
	if err := application.runLibraryAutomationPass(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	_, report := application.libraryAutomation.snapshot()
	if report.IndexChecked != 0 {
		t.Fatalf("fresh index was rescanned: %+v", report)
	}
}
