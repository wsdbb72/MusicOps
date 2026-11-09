//go:build windows

package vault

import (
	"errors"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modCrypt32        = windows.NewLazySystemDLL("crypt32.dll")
	procProtectData   = modCrypt32.NewProc("CryptProtectData")
	procUnprotectData = modCrypt32.NewProc("CryptUnprotectData")
	modKernel32       = windows.NewLazySystemDLL("kernel32.dll")
	procLocalFree     = modKernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

// platformProtect is the DPAPI whole-blob protection, matching Python
// clients.vault.protect on Windows: CryptProtectData with description
// "MusicOps" and CRYPTPROTECT_UI_FORBIDDEN (0x1), no entropy.
func platformProtect(data []byte, decrypt bool) ([]byte, error) {
	var in dataBlob
	if len(data) > 0 {
		in.cbData = uint32(len(data))
		in.pbData = &data[0]
	}
	desc, err := windows.UTF16PtrFromString("MusicOps")
	if err != nil {
		return nil, err
	}
	var out dataBlob
	var proc *windows.LazyProc
	if decrypt {
		proc = procUnprotectData
	} else {
		proc = procProtectData
	}
	r1, _, errno := proc.Call(
		uintptr(unsafe.Pointer(&in)),
		uintptr(unsafe.Pointer(desc)),
		0, 0, 0, 1,
		uintptr(unsafe.Pointer(&out)),
	)
	if r1 == 0 {
		return nil, errno
	}
	if out.pbData == nil || out.cbData == 0 {
		return nil, errors.New("DPAPI returned an empty blob")
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	buf := make([]byte, out.cbData)
	copy(buf, unsafe.Slice(out.pbData, out.cbData))
	return buf, nil
}

// tryLock takes a non-blocking exclusive lock on the first byte of f using
// LockFileEx, retrying for up to 5 seconds. This is the same byte-range lock
// the Python store takes with msvcrt.locking.
func tryLock(f *os.File) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := windows.LockFileEx(
			windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, 1, 0,
			&windows.Overlapped{},
		)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func unlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
