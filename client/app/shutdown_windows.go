//go:build windows

package main

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procFindWindow               = user32.NewProc("FindWindowW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procBlockReasonCreate        = user32.NewProc("ShutdownBlockReasonCreate")
	procBlockReasonDestroy       = user32.NewProc("ShutdownBlockReasonDestroy")

	windowMutex  sync.Mutex
	windowHandle uintptr
	blocking     bool
)

func ownWindow() uintptr {
	windowMutex.Lock()
	defer windowMutex.Unlock()
	if windowHandle != 0 {
		return windowHandle
	}
	class, _ := windows.UTF16PtrFromString("wailsWindow")
	title, _ := windows.UTF16PtrFromString("peerly")
	handle, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
	if handle == 0 {
		return 0
	}
	var ownerPID uint32
	procGetWindowThreadProcessID.Call(handle, uintptr(unsafe.Pointer(&ownerPID)))
	if ownerPID != windows.GetCurrentProcessId() {
		return 0
	}
	windowHandle = handle
	return handle
}

func setShutdownReason(reason string) {
	handle := ownWindow()
	if handle == 0 {
		return
	}
	windowMutex.Lock()
	defer windowMutex.Unlock()
	if reason == "" {
		if blocking {
			procBlockReasonDestroy.Call(handle)
			blocking = false
		}
		return
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}
	text, err := windows.UTF16PtrFromString(reason)
	if err != nil {
		return
	}
	procBlockReasonCreate.Call(handle, uintptr(unsafe.Pointer(text)))
	blocking = true
}
