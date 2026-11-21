package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// atomicReplaceRename is a test seam so failure paths can prove that an
// existing file and its backup survive when the final rename cannot happen.
var atomicReplaceRename = os.Rename

// replaceFileAtomically writes data to a same-directory temporary file and
// renames it over path. A failed write, Sync, Close or rename leaves the old
// file untouched. mode 0 preserves the target's current mode.
func replaceFileAtomically(path string, data []byte, mode os.FileMode) error {
	if path == "" {
		return errors.New("atomic replace: empty path")
	}
	if mode == 0 {
		mode = 0o600
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("atomic replace: stat %s: %w", path, err)
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".musicops-atomic-*")
	if err != nil {
		return fmt.Errorf("atomic replace: create temp beside %s: %w", path, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	written, err := temp.Write(data)
	if err != nil {
		_ = temp.Close()
		return fmt.Errorf("atomic replace: write temp for %s: %w", path, err)
	}
	if written != len(data) {
		_ = temp.Close()
		return fmt.Errorf("atomic replace: write temp for %s: wrote %d of %d bytes", path, written, len(data))
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("atomic replace: chmod temp for %s: %w", path, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("atomic replace: sync temp for %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("atomic replace: close temp for %s: %w", path, err)
	}
	if err := atomicReplaceRename(tempPath, path); err != nil {
		return fmt.Errorf("atomic replace: rename temp to %s: %w", path, err)
	}
	// Persist the directory entry where the platform supports syncing a
	// directory. Windows commonly rejects opening directories this way; the
	// file itself was still fully synced before the atomic rename.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// fileSnapshot preserves one existing sidecar, or remembers that the sidecar
// did not exist, so a multi-file metadata update can roll every path back as
// one transaction. Backups remain beside their source until Commit succeeds.
type fileSnapshot struct {
	path    string
	backup  string
	existed bool
}

func snapshotFiles(paths ...string) ([]*fileSnapshot, error) {
	snapshots := make([]*fileSnapshot, 0, len(paths))
	seen := map[string]bool{}
	cleanup := func() {
		for _, snapshot := range snapshots {
			snapshot.discardBackup()
		}
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		key := filepath.Clean(path)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, &fileSnapshot{path: path})
			continue
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		if !info.Mode().IsRegular() {
			cleanup()
			return nil, fmt.Errorf("snapshot %s: not a regular file", path)
		}
		backup, err := backupFile(path)
		if err != nil {
			cleanup()
			return nil, err
		}
		snapshots = append(snapshots, &fileSnapshot{path: path, backup: backup, existed: true})
	}
	return snapshots, nil
}

func (snapshot *fileSnapshot) discardBackup() {
	if snapshot != nil && snapshot.backup != "" {
		_ = os.Remove(snapshot.backup)
		snapshot.backup = ""
	}
}

func (snapshot *fileSnapshot) restore() error {
	if snapshot == nil {
		return nil
	}
	if snapshot.existed {
		if snapshot.backup == "" {
			return fmt.Errorf("restore sidecar %s: backup is missing", snapshot.path)
		}
		return restoreBackup(snapshot.path, snapshot.backup)
	}
	if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove newly written sidecar %s: %w", snapshot.path, err)
	}
	return nil
}

func restoreFileSnapshots(snapshots []*fileSnapshot) error {
	var result error
	for i := len(snapshots) - 1; i >= 0; i-- {
		if err := snapshots[i].restore(); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func commitFileSnapshots(snapshots []*fileSnapshot) error {
	var result error
	for _, snapshot := range snapshots {
		if snapshot == nil || snapshot.backup == "" {
			continue
		}
		if err := os.Remove(snapshot.backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove sidecar backup %s: %w", snapshot.backup, err))
		} else {
			snapshot.backup = ""
		}
	}
	return result
}

// restoreBackupOnFailure restores an original file after a partial operation.
// It reports whether restoration committed and combines the original failure
// with any restore failure. The backup remains in place when restoration fails.
func restoreBackupOnFailure(path, backup string, cause error) (bool, error) {
	if backup == "" {
		return false, cause
	}
	if err := restoreBackup(path, backup); err != nil {
		return false, errors.Join(cause, fmt.Errorf("restore original file %s: %w", path, err))
	}
	return true, cause
}
