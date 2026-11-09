//go:build windows

package main

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	genericRead        = 0x80000000
	genericWrite       = 0x40000000
	fileShareReadWrite = 0x00000003
	openExisting       = 3
	fileFlagOverlapped = 0x40000000
	ioctlHIDFlushQueue = 0x000B400C
	waitTimeout        = 0x00000102
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	cfgmgr32 = syscall.NewLazyDLL("cfgmgr32.dll")
	hidDLL   = syscall.NewLazyDLL("hid.dll")

	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
	procDeviceIoControl     = kernel32.NewProc("DeviceIoControl")
	procCreateEvent         = kernel32.NewProc("CreateEventW")
	procCMGetListSize       = cfgmgr32.NewProc("CM_Get_Device_Interface_List_SizeW")
	procCMGetList           = cfgmgr32.NewProc("CM_Get_Device_Interface_ListW")
	procHIDGetProduct       = hidDLL.NewProc("HidD_GetProductString")
	procHIDGetManufacturer  = hidDLL.NewProc("HidD_GetManufacturerString")
	procHIDGetSerial        = hidDLL.NewProc("HidD_GetSerialNumberString")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var hidInterfaceGUID = guid{
	Data1: 0x4D1E55B2, Data2: 0xF16F, Data3: 0x11CF,
	Data4: [8]byte{0x88, 0xCB, 0x00, 0x11, 0x11, 0x00, 0x00, 0x30},
}

func hidInterfacePaths() ([]string, error) {
	var size uint32
	rc, _, _ := procCMGetListSize.Call(uintptr(unsafe.Pointer(&size)),
		uintptr(unsafe.Pointer(&hidInterfaceGUID)), 0, 0)
	if rc != 0 {
		return nil, fmt.Errorf("CM_Get_Device_Interface_List_Size failed (config_ret=%d)", rc)
	}
	if size <= 1 {
		return nil, nil
	}
	buf := make([]uint16, size)
	rc, _, _ = procCMGetList.Call(uintptr(unsafe.Pointer(&hidInterfaceGUID)), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(size), 0)
	if rc != 0 {
		return nil, fmt.Errorf("CM_Get_Device_Interface_List failed (config_ret=%d)", rc)
	}
	var paths []string
	start := 0
	for i, c := range buf {
		if c == 0 {
			if i > start {
				paths = append(paths, syscall.UTF16ToString(buf[start:i]))
			}
			start = i + 1
		}
	}
	return paths, nil
}

type device struct {
	h syscall.Handle
}

func openDevice(path string) (*device, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, genericRead|genericWrite, fileShareReadWrite,
		nil, openExisting, fileFlagOverlapped, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &device{h: h}, nil
}

func (d *device) close() {
	if d.h != 0 && d.h != syscall.InvalidHandle {
		syscall.CloseHandle(d.h)
	}
	d.h = 0
}

// io must not return while the kernel still owns buf or ov: a pending request is
// canceled and awaited first.
func (d *device) io(write bool, buf []byte, timeout time.Duration) (int, error) {
	if d.h == 0 {
		return 0, errClosed
	}
	evHandle, _, callErr := procCreateEvent.Call(0, 1, 0, 0)
	if evHandle == 0 {
		return 0, fmt.Errorf("CreateEvent: %w", callErr)
	}
	ev := syscall.Handle(evHandle)
	defer syscall.CloseHandle(ev)

	ov := new(syscall.Overlapped)
	ov.HEvent = ev
	var done uint32
	var err error
	if write {
		err = syscall.WriteFile(d.h, buf, &done, ov)
	} else {
		err = syscall.ReadFile(d.h, buf, &done, ov)
	}
	if err != nil && err != syscall.ERROR_IO_PENDING {
		return 0, err
	}
	if err == syscall.ERROR_IO_PENDING {
		wait, werr := syscall.WaitForSingleObject(ev, uint32(timeout/time.Millisecond))
		if werr != nil {
			return 0, werr
		}
		if wait == waitTimeout {
			procCancelIoEx.Call(uintptr(d.h), uintptr(unsafe.Pointer(ov)))
			// Cancellation must complete the active request.
			var ignore uint32
			procGetOverlappedResult.Call(uintptr(d.h), uintptr(unsafe.Pointer(ov)),
				uintptr(unsafe.Pointer(&ignore)), 1)
			runtime.KeepAlive(buf)
			return 0, errTimeout
		}
	}
	var got uint32
	ok, _, callErr := procGetOverlappedResult.Call(uintptr(d.h),
		uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(&got)), 0)
	runtime.KeepAlive(buf)
	if ok == 0 {
		return 0, callErr
	}
	return int(got), nil
}

func (d *device) flushQueue() {
	var ret uint32
	procDeviceIoControl.Call(uintptr(d.h), ioctlHIDFlushQueue,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&ret)), 0)
}

func (d *device) hidString(proc *syscall.LazyProc) (string, error) {
	if d.h == 0 {
		return "", errClosed
	}
	const chars = 128
	buf := make([]uint16, chars)
	ok, _, callErr := proc.Call(uintptr(d.h),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(chars*2))
	if ok == 0 {
		return "", fmt.Errorf("%s failed: %w", proc.Name, callErr)
	}
	return syscall.UTF16ToString(buf), nil
}

// The interface path does not prove which device answered because 0483:5752 is a generic
// STM32 custom-HID pair.  Use openVerifiedDevice to check the identity strings.
func (d *device) Identity() (product, manufacturer, serial string, err error) {
	if product, err = d.hidString(procHIDGetProduct); err != nil {
		return "", "", "", err
	}
	if manufacturer, err = d.hidString(procHIDGetManufacturer); err != nil {
		return "", "", "", err
	}
	serial, _ = d.hidString(procHIDGetSerial)
	return strings.TrimSpace(product), strings.TrimSpace(manufacturer),
		strings.TrimSpace(serial), nil
}

func findInterface(kind interfaceKind) (string, error) {
	tag := "MI_00"
	if kind == ifaceEvent {
		tag = "MI_02"
	}
	paths, err := hidInterfacePaths()
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		if strings.Contains(p, vendorVIDPID) && strings.Contains(strings.ToUpper(p), tag) {
			return p, nil
		}
	}
	return "", fmt.Errorf("keypad not found (no %s interface for %s)", tag, vendorVIDPID)
}