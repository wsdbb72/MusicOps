//go:build windows

package desktop

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcAttr detaches a core from the short-lived parent's console, so
// console control events aimed at the parent do not reach it.
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
