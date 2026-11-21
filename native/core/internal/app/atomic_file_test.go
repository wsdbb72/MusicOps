package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReplaceFileAtomicallyKeepsOriginalWhenRenameFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "track.flac")
	original := []byte("complete original audio")
	replacement := []byte("complete replacement audio")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := atomicReplaceRename
	atomicReplaceRename = func(_, _ string) error { return errors.New("rename blocked") }
	t.Cleanup(func() { atomicReplaceRename = previous })
	if err := replaceFileAtomically(path, replacement, 0); err == nil {
		t.Fatal("atomic replace unexpectedly succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original changed after failed rename: bytes=%q err=%v", got, err)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".musicops-atomic-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files remain: %v err=%v", temps, err)
	}
}

func TestRestoreBackupKeepsBackupUntilRenameCommits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.flac")
	backup := filepath.Join(dir, ".musicops-backup-test")
	original := []byte("original audio")
	current := []byte("partially replaced audio")
	if err := os.WriteFile(path, current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, original, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := atomicReplaceRename
	atomicReplaceRename = func(_, _ string) error { return errors.New("rename blocked") }
	t.Cleanup(func() { atomicReplaceRename = previous })
	if err := restoreBackup(path, backup); err == nil {
		t.Fatal("restore unexpectedly succeeded")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, current) {
		t.Fatalf("current target changed after failed restore: bytes=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(backup); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("backup was not preserved: bytes=%q err=%v", got, err)
	}
	atomicReplaceRename = previous
	if err := restoreBackup(path, backup); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("restored bytes=%q err=%v", got, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("committed backup remains: %v", err)
	}
}

func TestFileSnapshotsRestoreExistingAndRemoveNewSidecars(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "track.lrc")
	created := filepath.Join(dir, "track.jpg")
	if err := os.WriteFile(existing, []byte("old lyrics"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotFiles(existing, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceFileAtomically(existing, []byte("new lyrics"), 0); err != nil {
		t.Fatal(err)
	}
	if err := replaceFileAtomically(created, []byte("new cover"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreFileSnapshots(snapshots); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(existing); err != nil || string(body) != "old lyrics" {
		t.Fatalf("existing sidecar was not restored: %q err=%v", body, err)
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new sidecar was not removed: %v", err)
	}
}

func TestFileSnapshotsCommitRemovesBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "track.lrc")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].backup == "" {
		t.Fatalf("snapshot missing backup: %+v", snapshots)
	}
	backup := snapshots[0].backup
	if err := replaceFileAtomically(path, []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	if err := commitFileSnapshots(snapshots); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed backup remains: %v", err)
	}
}

func TestBackupFilePreservesPermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits faithfully")
	}
	path := filepath.Join(t.TempDir(), "track.lrc")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup, err := backupFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(backup)
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("backup mode=%#o want=%#o", got, 0o644)
	}
}
