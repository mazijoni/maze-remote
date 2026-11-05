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
	t      Screen
	warned bool
}

func NewInputInjector() InputInjector { return &xdoInjector{t: Screen{W: 1920, H: 1080}} }

func (x *xdoInjector) SetTarget(t Screen) { x.mu.Lock(); x.t = t; x.mu.Unlock() }

// listScreens: one screen covering the X display (multi-monitor X11 shows up as one big desktop).
func listScreens() []Screen {
	s := Screen{Index: 0, Name: "Screen 1", W: 1920, H: 1080, Primary: true}
	if out, err := exec.Command("xdotool", "getdisplaygeometry").Output(); err == nil {
		fmt.Sscanf(string(out), "%d %d", &s.W, &s.H)
	}
	return []Screen{s}
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
		x.run("mousemove", fmt.Sprint(x.t.X+int(clamp01(ev.X)*float64(x.t.W-1))), fmt.Sprint(x.t.Y+int(clamp01(ev.Y)*float64(x.t.H-1))))
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
