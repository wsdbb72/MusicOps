//go:build windows

package app

import (
	"errors"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	managedChildJobOnce sync.Once
	managedChildJob     windows.Handle
	managedChildJobErr  error
)

// prepareManagedChild creates one job for every engine this core launches.
// KILL_ON_JOB_CLOSE is the crash-safe half of engine shutdown: when the core
// exits for any reason, Windows closes the only job handle and terminates the
// managed engines and every browser or helper they spawned.
func prepareManagedChild() error {
	managedChildJobOnce.Do(func() {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			managedChildJobErr = err
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err = windows.SetInformationJobObject(
			job,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			_ = windows.CloseHandle(job)
			managedChildJobErr = err
			return
		}
		managedChildJob = job
	})
	return managedChildJobErr
}

func attachManagedChild(process *os.Process) error {
	if process == nil {
		return errors.New("managed child process is nil")
	}
	if err := prepareManagedChild(); err != nil {
		return err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.AssignProcessToJobObject(managedChildJob, handle)
}
