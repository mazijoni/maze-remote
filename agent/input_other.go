//go:build !windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Linux/macOS fallback: drives input through xdotool (X11). Good enough for
// testing; Windows is the primary target.

func platformInit()            {}
func hideWindow(cmd *exec.Cmd) {}

func displayName() string {
	if d := os.Getenv("DISPLAY"); d != "" {
		return d
	}
	return ":0"
}

type xdoInjector struct {
	mu     sync.Mutex
	w, h   int
	warned bool
}

func NewInputInjector(monitor int) InputInjector {
	x := &xdoInjector{w: 1920, h: 1080}
	if out, err := exec.Command("xdotool", "getdisplaygeometry").Output(); err == nil {
		fmt.Sscanf(string(out), "%d %d", &x.w, &x.h)
	}
	return x
}

func (x *xdoInjector) run(args ...string) {
	if err := exec.Command("xdotool", args...).Run(); err != nil && !x.warned {
		x.warned = true
		log.Printf("input disabled: xdotool not available (%v)", err)
	}
}

var xButtons = map[int]string{0: "1", 1: "2", 2: "3", 3: "8", 4: "9"}

func (x *xdoInjector) Handle(ev InputEvent) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch ev.T {
	case "mm":
		x.run("mousemove", fmt.Sprint(int(clamp01(ev.X)*float64(x.w-1))), fmt.Sprint(int(clamp01(ev.Y)*float64(x.h-1))))
	case "md":
		x.run("mousedown", xButtons[ev.B])
	case "mu":
		x.run("mouseup", xButtons[ev.B])
	case "wh":
		if ev.DY > 0 {
			x.run("click", "5")
		} else if ev.DY < 0 {
			x.run("click", "4")
		}
	case "kd", "ku":
		if k := xKey(ev.C); k != "" {
			verb := "keydown"
			if ev.T == "ku" {
				verb = "keyup"
			}
			x.run(verb, k)
		}
	case "text":
		x.run("type", "--", ev.S)
	}
}

func (x *xdoInjector) ReleaseAll() {}

func xKey(code string) string {
	switch {
	case strings.HasPrefix(code, "Key"):
		return strings.ToLower(code[3:])
	case strings.HasPrefix(code, "Digit"):
		return code[5:]
	case strings.HasPrefix(code, "F") && len(code) <= 3:
		return code
	}
	m := map[string]string{
		"Enter": "Return", "Escape": "Escape", "Backspace": "BackSpace", "Tab": "Tab", "Space": "space",
		"ShiftLeft": "Shift_L", "ShiftRight": "Shift_R", "ControlLeft": "Control_L", "ControlRight": "Control_R",
		"AltLeft": "Alt_L", "AltRight": "Alt_R", "MetaLeft": "Super_L", "MetaRight": "Super_R",
		"ArrowUp": "Up", "ArrowDown": "Down", "ArrowLeft": "Left", "ArrowRight": "Right",
		"Delete": "Delete", "Home": "Home", "End": "End", "PageUp": "Prior", "PageDown": "Next",
		"Minus": "minus", "Equal": "equal", "Comma": "comma", "Period": "period", "Slash": "slash",
		"Semicolon": "semicolon", "Quote": "apostrophe", "BracketLeft": "bracketleft", "BracketRight": "bracketright",
		"Backslash": "backslash", "Backquote": "grave", "CapsLock": "Caps_Lock",
	}
	return m[code]
}
