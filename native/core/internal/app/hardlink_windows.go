//go:build windows

package app

import (
	"errors"
	"syscall"
)

// checkNotHardlinked refuses files with more than one hard link, as
// Python apply_tag_changes does: editing one name would silently change
// another. os.Stat does not expose a link count on Windows, and
// syscall.Stat_t does not exist there, so the count is read with
// GetFileInformationByHandle instead.
func checkNotHardlinked(path string) error {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return errors.New("file has more than one hard link")
	}
	return nil
}
