package qbittorrent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckOwnershipAcceptsMissingDownloadPathUnderSymlink is the
// regression test for the macOS download ADD_FAILED: the fake client
// reports a download path that does not exist yet under a symlinked
// parent, and the ownership check must still accept it instead of
// reporting that the download path escapes the task source path.
func TestCheckOwnershipAcceptsMissingDownloadPathUnderSymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(filepath.Dir(real), "zzlink")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	// Save path exists (planted by the fake client); download path does not.
	savePath := filepath.Join(link, "downloads", "task-1")
	if err := os.MkdirAll(savePath, 0o700); err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecord("task-1", filepath.Join(link, "downloads"), "musicops")
	if err != nil {
		t.Fatal(err)
	}
	rec.CreationIntent = true
	row := &TorrentRow{
		Hash: "abc", State: "stoppedDL", Progress: 0, Category: "musicops",
		Tags: "MusicOps", SavePath: savePath,
		ContentPath:  filepath.Join(savePath, "album"),
		DownloadPath: filepath.Join(savePath, ".incomplete"),
		AutoTMM:      boolPtr(false),
	}
	// Content path must exist for the check to reach the download path.
	if err := os.MkdirAll(row.ContentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if reason := CheckOwnership(row, rec, false, Version{}); reason != "" {
		t.Fatalf("CheckOwnership rejected: %s (save=%q download=%q)", reason, row.SavePath, row.DownloadPath)
	}
}
