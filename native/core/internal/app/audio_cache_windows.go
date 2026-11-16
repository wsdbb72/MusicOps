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
