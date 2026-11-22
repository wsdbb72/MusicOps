//go:build windows

package desktop

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmSetIcon          = 0x0080
	iconSmall          = 0
	iconBig            = 1
	imageIcon          = 1
	smCXIcon           = 11
	smCYIcon           = 12
	smCXSmIcon         = 49
	smCYSmIcon         = 50
	lrDefaultColor     = 0x00000000
	lrShared           = 0x00008000
	idiApplicationIcon = 32512
	iconResourceVer    = 0x00030000
)

//go:embed assets/musicops.ico
var windowIconICO []byte

var (
	windowIconUser32            = windows.NewLazySystemDLL("user32.dll")
	windowIconKernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procWindowIconSendMessageW  = windowIconUser32.NewProc("SendMessageW")
	procWindowIconGetClassNameW = windowIconUser32.NewProc("GetClassNameW")
	procWindowIconLoadImageW    = windowIconUser32.NewProc("LoadImageW")
	procWindowIconGetMetric     = windowIconUser32.NewProc("GetSystemMetrics")
	procWindowIconGetModule     = windowIconKernel32.NewProc("GetModuleHandleW")
	procWindowIconCreateIcon    = windowIconUser32.NewProc("CreateIconFromResourceEx")
)

// applyNativeWindowIcon pins the packaged brand icon to every top-level Wails
// window owned by this process. Wails 2.17 tries PE resource ID 3 at window
// creation, while goversioninfo exposes the application icon as
// IDI_APPLICATION (32512). Relying on the PE icon alone therefore leaves the
// running window and taskbar with the default icon even though Explorer shows
// the correct file icon. The embedded ICO is a development-build fallback for
// executables built without a generated Windows resource.
func applyNativeWindowIcon() error {
	processID := windows.GetCurrentProcessId()
	handles := make([]windows.HWND, 0, 2)
	callback := windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var ownerID uint32
		if _, err := windows.GetWindowThreadProcessId(windows.HWND(hwnd), &ownerID); err == nil &&
			ownerID == processID && isWailsWindow(windows.HWND(hwnd)) {
			handles = append(handles, windows.HWND(hwnd))
		}
		return 1
	})
	if err := windows.EnumWindows(callback, nil); err != nil {
		return fmt.Errorf("enumerate MusicOps windows: %w", err)
	}
	if len(handles) == 0 {
		return fmt.Errorf("MusicOps Wails window is not ready")
	}

	module, _, _ := procWindowIconGetModule.Call(0)
	smallWidth := metric(procWindowIconGetMetric, smCXSmIcon)
	smallHeight := metric(procWindowIconGetMetric, smCYSmIcon)
	bigWidth := metric(procWindowIconGetMetric, smCXIcon)
	bigHeight := metric(procWindowIconGetMetric, smCYIcon)
	small, err := loadWindowIcon(module, smallWidth, smallHeight)
	if err != nil {
		return err
	}
	large, err := loadWindowIcon(module, bigWidth, bigHeight)
	if err != nil {
		return err
	}

	// Send synchronously off the Wails UI thread. A successful return means
	// the title bar and taskbar have observed both handles.
	for _, hwnd := range handles {
		procWindowIconSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconSmall, small)
		procWindowIconSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconBig, large)
	}
	return nil
}

func isWailsWindow(hwnd windows.HWND) bool {
	var class [128]uint16
	length, _, _ := procWindowIconGetClassNameW.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&class[0])),
		uintptr(len(class)),
	)
	if length == 0 || length >= uintptr(len(class)) {
		return false
	}
	return windows.UTF16ToString(class[:int(length)]) == "wailsWindow"
}

func loadWindowIcon(module uintptr, width, height int) (uintptr, error) {
	if module != 0 {
		icon, _, _ := procWindowIconLoadImageW.Call(
			module,
			uintptr(idiApplicationIcon),
			imageIcon,
			uintptr(width),
			uintptr(height),
			lrDefaultColor|lrShared,
		)
		if icon != 0 {
			return icon, nil
		}
	}
	return loadEmbeddedWindowIcon(width, height)
}

func loadEmbeddedWindowIcon(width, height int) (uintptr, error) {
	bits, err := embeddedIconBits(windowIconICO, width)
	if err != nil {
		return 0, err
	}
	icon, _, callErr := procWindowIconCreateIcon.Call(
		uintptr(unsafe.Pointer(&bits[0])),
		uintptr(len(bits)),
		1, // fIcon
		iconResourceVer,
		uintptr(width),
		uintptr(height),
		lrDefaultColor,
	)
	if icon == 0 {
		return 0, fmt.Errorf("create embedded MusicOps window icon: %v", callErr)
	}
	return icon, nil
}

// embeddedIconBits selects the smallest ICO frame that covers the requested
// logical size, falling back to the largest frame. Both DIB and PNG ICO frames
// are accepted by CreateIconFromResourceEx with version 0x00030000.
func embeddedIconBits(ico []byte, target int) ([]byte, error) {
	if len(ico) < 6 || binary.LittleEndian.Uint16(ico[0:2]) != 0 || binary.LittleEndian.Uint16(ico[2:4]) != 1 {
		return nil, fmt.Errorf("embedded MusicOps icon is not a valid ICO file")
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	if count < 1 || len(ico) < 6+16*count {
		return nil, fmt.Errorf("embedded MusicOps icon has no image frames")
	}
	bestSize := -1
	bestOffset := uint32(0)
	bestLength := uint32(0)
	for index := 0; index < count; index++ {
		entry := ico[6+16*index : 6+16*(index+1)]
		size := int(entry[0])
		if size == 0 {
			size = 256
		}
		length := binary.LittleEndian.Uint32(entry[8:12])
		offset := binary.LittleEndian.Uint32(entry[12:16])
		if length == 0 || uint64(offset)+uint64(length) > uint64(len(ico)) {
			continue
		}
		if bestSize < 0 ||
			(size >= target && (bestSize < target || size < bestSize)) ||
			(bestSize < target && size > bestSize) {
			bestSize = size
			bestOffset = offset
			bestLength = length
		}
	}
	if bestSize < 0 {
		return nil, fmt.Errorf("embedded MusicOps icon has no readable image frame")
	}
	return ico[int(bestOffset):int(bestOffset+bestLength)], nil
}

func metric(proc *windows.LazyProc, index int) int {
	value, _, _ := proc.Call(uintptr(index))
	if value == 0 {
		// A zero dimension would make icon creation choose the wrong frame.
		switch index {
		case smCXSmIcon, smCYSmIcon:
			return 16
		default:
			return 32
		}
	}
	return int(value)
}
