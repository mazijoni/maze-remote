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
)

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
