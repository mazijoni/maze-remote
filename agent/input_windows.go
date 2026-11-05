//go:build windows

package main

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32                        = syscall.NewLazyDLL("user32.dll")
	procSendInput                 = user32.NewProc("SendInput")
	procSetCursorPos              = user32.NewProc("SetCursorPos")
	procEnumDisplayMonitors       = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW           = user32.NewProc("GetMonitorInfoW")
	procSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")
	procVkKeyScanW                = user32.NewProc("VkKeyScanW")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseXDown      = 0x0080
	mouseXUp        = 0x0100
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004
	keyScancode = 0x0008
)

// 64-bit layouts (amd64/arm64): INPUT is 40 bytes.
type mouseInput struct {
	Dx, Dy                 int32
	MouseData, Flags, Time uint32
	ExtraInfo              uintptr
}
type kbdInput struct {
	Vk, Scan    uint16
	Flags, Time uint32
	ExtraInfo   uintptr
	_           [8]byte
}
type inputM struct {
	Type uint32
	_    uint32
	Mi   mouseInput
}
type inputK struct {
	Type uint32
	_    uint32
	Ki   kbdInput
}

type rect struct{ Left, Top, Right, Bottom int32 }
type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

func platformInit() {
	// Per-monitor DPI aware v2, so cursor coordinates are real pixels.
	procSetProcessDpiAwarenessCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

func monitors() []rect {
	var primary []rect
	var others []rect
	cb := syscall.NewCallback(func(h, hdc, lprc, lparam uintptr) uintptr {
		var mi monitorInfo
		mi.Size = uint32(unsafe.Sizeof(mi))
		procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi)))
		if mi.Flags&1 != 0 {
			primary = append(primary, mi.Monitor)
		} else {
			others = append(others, mi.Monitor)
		}
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	return append(primary, others...)
}

type winInjector struct {
	mu      sync.Mutex
	t       Screen
	keys    map[string]bool
	buttons map[int]bool
}

func NewInputInjector() InputInjector {
	return &winInjector{t: Screen{W: 1920, H: 1080}, keys: map[string]bool{}, buttons: map[int]bool{}}
}

func (w *winInjector) SetTarget(t Screen) {
	w.mu.Lock()
	w.t = t
	w.mu.Unlock()
}

func sendMouse(flags uint32, data int32) {
	in := inputM{Type: inputMouse, Mi: mouseInput{Flags: flags, MouseData: uint32(data)}}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendKey(scan uint16, flags uint32) {
	in := inputK{Type: inputKeyboard, Ki: kbdInput{Scan: scan, Flags: flags | keyScancode}}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendUnicode(ch uint16) {
	for _, f := range []uint32{keyUnicode, keyUnicode | keyUp} {
		in := inputK{Type: inputKeyboard, Ki: kbdInput{Scan: ch, Flags: f}}
		procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	}
}

func (w *winInjector) move(x, y float64) {
	px := int32(w.t.X) + int32(clamp01(x)*float64(w.t.W-1))
	py := int32(w.t.Y) + int32(clamp01(y)*float64(w.t.H-1))
	procSetCursorPos.Call(uintptr(px), uintptr(py))
}

var buttonFlags = map[int][2]uint32{
	0: {mouseLeftDown, mouseLeftUp},
	1: {mouseMiddleDown, mouseMiddleUp},
	2: {mouseRightDown, mouseRightUp},
	3: {mouseXDown, mouseXUp}, // back  (XBUTTON1)
	4: {mouseXDown, mouseXUp}, // forward (XBUTTON2)
}

func (w *winInjector) button(b int, down bool) {
	f, ok := buttonFlags[b]
	if !ok {
		return
	}
	var data int32
	if b == 3 {
		data = 1
	} else if b == 4 {
		data = 2
	}
	if down {
		sendMouse(f[0], data)
		w.buttons[b] = true
	} else {
		sendMouse(f[1], data)
		delete(w.buttons, b)
	}
}

func (w *winInjector) key(code string, down bool) {
	k, ok := scanCodes[code]
	if !ok {
		return
	}
	flags := uint32(0)
	if k.ext {
		flags |= keyExtended
	}
	if !down {
		flags |= keyUp
		delete(w.keys, code)
	} else {
		w.keys[code] = true
	}
	sendKey(k.scan, flags)
}

func (w *winInjector) Handle(ev InputEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch ev.T {
	case "mm":
		w.move(ev.X, ev.Y)
	case "md", "mu":
		if ev.X != 0 || ev.Y != 0 {
			w.move(ev.X, ev.Y)
		}
		w.button(ev.B, ev.T == "md")
	case "wh":
		if ev.DY != 0 {
			sendMouse(mouseWheel, int32(-ev.DY*120/100))
		}
		if ev.DX != 0 {
			sendMouse(mouseHWheel, int32(ev.DX*120/100))
		}
	case "kd":
		w.key(ev.C, true)
	case "ku":
		w.key(ev.C, false)
	case "text":
		for _, ch := range syscall.StringToUTF16(ev.S) {
			if ch != 0 {
				sendUnicode(ch)
			}
		}
	}
}

func (w *winInjector) ReleaseAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for c := range w.keys {
		w.key(c, false)
	}
	for b := range w.buttons {
		w.button(b, false)
	}
}

type sc struct {
	scan uint16
	ext  bool
}

// KeyboardEvent.code -> PC/AT set-1 scan code. Layout-independent: the remote
// PC's own keyboard layout decides which character a physical key produces.
var scanCodes = map[string]sc{
	"Escape": {0x01, false}, "Digit1": {0x02, false}, "Digit2": {0x03, false}, "Digit3": {0x04, false},
	"Digit4": {0x05, false}, "Digit5": {0x06, false}, "Digit6": {0x07, false}, "Digit7": {0x08, false},
	"Digit8": {0x09, false}, "Digit9": {0x0A, false}, "Digit0": {0x0B, false}, "Minus": {0x0C, false},
	"Equal": {0x0D, false}, "Backspace": {0x0E, false}, "Tab": {0x0F, false},
	"KeyQ": {0x10, false}, "KeyW": {0x11, false}, "KeyE": {0x12, false}, "KeyR": {0x13, false},
	"KeyT": {0x14, false}, "KeyY": {0x15, false}, "KeyU": {0x16, false}, "KeyI": {0x17, false},
	"KeyO": {0x18, false}, "KeyP": {0x19, false}, "BracketLeft": {0x1A, false}, "BracketRight": {0x1B, false},
	"Enter": {0x1C, false}, "ControlLeft": {0x1D, false},
	"KeyA": {0x1E, false}, "KeyS": {0x1F, false}, "KeyD": {0x20, false}, "KeyF": {0x21, false},
	"KeyG": {0x22, false}, "KeyH": {0x23, false}, "KeyJ": {0x24, false}, "KeyK": {0x25, false},
	"KeyL": {0x26, false}, "Semicolon": {0x27, false}, "Quote": {0x28, false}, "Backquote": {0x29, false},
	"ShiftLeft": {0x2A, false}, "Backslash": {0x2B, false},
	"KeyZ": {0x2C, false}, "KeyX": {0x2D, false}, "KeyC": {0x2E, false}, "KeyV": {0x2F, false},
	"KeyB": {0x30, false}, "KeyN": {0x31, false}, "KeyM": {0x32, false}, "Comma": {0x33, false},
	"Period": {0x34, false}, "Slash": {0x35, false}, "ShiftRight": {0x36, false},
	"NumpadMultiply": {0x37, false}, "AltLeft": {0x38, false}, "Space": {0x39, false}, "CapsLock": {0x3A, false},
	"F1": {0x3B, false}, "F2": {0x3C, false}, "F3": {0x3D, false}, "F4": {0x3E, false}, "F5": {0x3F, false},
	"F6": {0x40, false}, "F7": {0x41, false}, "F8": {0x42, false}, "F9": {0x43, false}, "F10": {0x44, false},
	"NumLock": {0x45, false}, "ScrollLock": {0x46, false},
	"Numpad7": {0x47, false}, "Numpad8": {0x48, false}, "Numpad9": {0x49, false}, "NumpadSubtract": {0x4A, false},
	"Numpad4": {0x4B, false}, "Numpad5": {0x4C, false}, "Numpad6": {0x4D, false}, "NumpadAdd": {0x4E, false},
	"Numpad1": {0x4F, false}, "Numpad2": {0x50, false}, "Numpad3": {0x51, false}, "Numpad0": {0x52, false},
	"NumpadDecimal": {0x53, false}, "IntlBackslash": {0x56, false}, "F11": {0x57, false}, "F12": {0x58, false},
	"NumpadEnter": {0x1C, true}, "ControlRight": {0x1D, true}, "NumpadDivide": {0x35, true},
	"PrintScreen": {0x37, true}, "AltRight": {0x38, true}, "Home": {0x47, true}, "ArrowUp": {0x48, true},
	"PageUp": {0x49, true}, "ArrowLeft": {0x4B, true}, "ArrowRight": {0x4D, true}, "End": {0x4F, true},
	"ArrowDown": {0x50, true}, "PageDown": {0x51, true}, "Insert": {0x52, true}, "Delete": {0x53, true},
	"MetaLeft": {0x5B, true}, "MetaRight": {0x5C, true}, "ContextMenu": {0x5D, true},
	"Pause": {0x45, true}, // approximate
}

func displayName() string { return "" }
