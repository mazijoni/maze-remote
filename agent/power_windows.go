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
	procGetCursorInfo           = user32.NewProc("GetCursorInfo")
	procLoadCursorW             = user32.NewProc("LoadCursorW")
	procPostMessageW            = user32.NewProc("PostMessageW")
	procRedrawWindow            = user32.NewProc("RedrawWindow")
)

// kickScreen makes Windows repaint every window once. Desktop Duplication only
// delivers a frame when something on screen changes, so on an idle desktop the
// first picture can otherwise take forever.
func kickScreen() {
	const rdwInvalidate, rdwErase, rdwAllChildren, rdwFrame = 0x1, 0x4, 0x80, 0x400
	procRedrawWindow.Call(0, 0, 0, rdwInvalidate|rdwErase|rdwAllChildren|rdwFrame)
	in := inputM{Type: inputMouse, Mi: mouseInput{Dx: 1, Flags: mouseMove}}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	in.Mi.Dx = -1
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// Standard Windows cursors -> CSS cursor names.
var (
	cursorOnce  sync.Once
	cursorNames map[uintptr]string
)

func loadCursorNames() {
	ids := map[uintptr]string{
		32512: "default", 32513: "text", 32514: "wait", 32515: "crosshair",
		32642: "nwse-resize", 32643: "nesw-resize", 32644: "ew-resize", 32645: "ns-resize",
		32646: "move", 32648: "not-allowed", 32649: "pointer", 32650: "progress", 32651: "help",
	}
	cursorNames = map[uintptr]string{}
	for id, name := range ids {
		if h, _, _ := procLoadCursorW.Call(0, id); h != 0 {
			cursorNames[h] = name
		}
	}
}

// cursorShape returns the CSS name for the pointer Windows is showing right now
// ("none" when hidden, "default" for app-specific cursors we can't match).
func cursorShape() string {
	cursorOnce.Do(loadCursorNames)
	var ci struct {
		Size   uint32
		Flags  uint32
		Cursor uintptr
		X, Y   int32
	}
	ci.Size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return "default"
	}
	if ci.Flags&1 == 0 { // CURSOR_SHOWING
		return "none"
	}
	if n, ok := cursorNames[ci.Cursor]; ok {
		return n
	}
	return "default"
}

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
	// 1. a tiny mouse jiggle (1px and back)
	in := inputM{Type: inputMouse, Mi: mouseInput{Dx: 1, Flags: mouseMove}}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	in.Mi.Dx = -1
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	// 2. tap F15 — a real key no keyboard has and no app uses, but it counts as activity
	for _, f := range []uint32{0, keyUp} {
		k := inputK{Type: inputKeyboard, Ki: kbdInput{Vk: 0x7E, Flags: f}}
		procSendInput.Call(1, uintptr(unsafe.Pointer(&k)), unsafe.Sizeof(k))
	}
	// 3. ask Windows directly to power the monitors on (WM_SYSCOMMAND / SC_MONITORPOWER, -1 = on)
	procPostMessageW.Call(0xFFFF, 0x0112, 0xF170, ^uintptr(0))
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
