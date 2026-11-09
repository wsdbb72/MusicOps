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

// hideConsoleAttr hides the console window of a core started by the desktop
// shell. The shell itself is a GUI-subsystem binary with no console to
// inherit, so without this flag Windows would create a new console window
// for the console-subsystem core every time the app opens.
func hideConsoleAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
