//go:build windows

package app

import "golang.org/x/sys/windows"

func audioCacheFreeBytes(dir string) (uint64, error) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var available uint64
	err = windows.GetDiskFreeSpaceEx(path, &available, nil, nil)
	return available, err
}

func cacheRemovalPath(path string) string {
	return windowsLongPath(canonicalPath(path))
}
func retireDownloadCacheDirectory(from, to string) error {
	from = windowsLongPath(from)
	to = windowsLongPath(to)
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, 0)
}

func windowsLongPath(path string) string {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetLongPathName(pathPtr, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 || int(length) >= len(buffer) {
		return path
	}
	return windows.UTF16ToString(buffer[:length])
}
