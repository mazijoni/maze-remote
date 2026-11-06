//go:build windows

package main

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
	procGetTickCount            = kernel32.NewProc("GetTickCount")
	procGetLastInputInfo        = user32.NewProc("GetLastInputInfo")
	procOpenInputDesktop        = user32.NewProc("OpenInputDesktop")
	procGetUserObjectInfoW      = user32.NewProc("GetUserObjectInformationW")
	procCloseDesktop            = user32.NewProc("CloseDesktop")
)

// inputDesktop names the desktop that currently gets keyboard/mouse input:
// "Default" for the normal desktop, "Winlogon" for the lock/sign-in screen.
// A normal (non-admin) app isn't allowed to open the secure desktop at all,
// so failing to open it also means the PC is locked or showing a UAC prompt.
func inputDesktop() string {
	h, _, _ := procOpenInputDesktop.Call(0, 0, 0x0001) // DESKTOP_READOBJECTS
	if h == 0 {
		return "Winlogon"
	}
	defer procCloseDesktop.Call(h)
	var buf [128]uint16
	var need uint32
	if r, _, _ := procGetUserObjectInfoW.Call(h, 2, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&need))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:])
}

const (
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
	mouseMove         = 0x0001
)

// idleMillis is how long since the last keyboard/mouse input on this PC.
func idleMillis() uint32 {
	var lii struct{ Size, Time uint32 }
	lii.Size = uint32(unsafe.Sizeof(lii))
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	return uint32(now) - lii.Time
}

// wakeDisplays turns sleeping monitors back on with a tiny mouse nudge
// (moves 1px and back, so the cursor doesn't actually go anywhere).
// Returns true if the PC had been idle long enough that they may have been off.
func wakeDisplays() bool {
	idle := idleMillis()
	in := inputM{Type: inputMouse, Mi: mouseInput{Dx: 1, Flags: mouseMove}}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	in.Mi.Dx = -1
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	procSetThreadExecutionState.Call(esDisplayRequired) // one-shot "someone is here"
	return idle > 30_000
}

// keepAwake stops the monitors and the PC from going to sleep while a viewer
// is connected. The setting belongs to a thread, so one locked OS thread holds it.
var (
	awakeMu sync.Mutex
	awakeCh chan bool
)

func keepAwake(on bool) {
	awakeMu.Lock()
	defer awakeMu.Unlock()
	if awakeCh == nil {
		awakeCh = make(chan bool)
		go func() {
			runtime.LockOSThread()
			for v := range awakeCh {
				if v {
					procSetThreadExecutionState.Call(esContinuous | esSystemRequired | esDisplayRequired)
				} else {
					procSetThreadExecutionState.Call(esContinuous)
				}
			}
		}()
	}
	awakeCh <- on
}
