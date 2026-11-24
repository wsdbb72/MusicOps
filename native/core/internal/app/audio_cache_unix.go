//go:build !windows

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

func audioCacheFreeBytes(dir string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func cacheRemovalPath(path string) string {
	return canonicalPath(path)
}
func retireDownloadCacheDirectory(from, to string) error {
	return os.Rename(from, to)
}
