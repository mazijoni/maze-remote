//go:build !windows

package main

import "os/exec"

// On Linux/X11 (e.g. the Raspberry Pi) use xset to wake the screen and to
// pause screen blanking while a viewer is connected.
func wakeDisplays() bool {
	_ = exec.Command("xset", "dpms", "force", "on").Run()
	_ = exec.Command("xset", "s", "reset").Run()
	return false
}

func keepAwake(on bool) {
	if on {
		_ = exec.Command("xset", "s", "off", "-dpms").Run()
	} else {
		_ = exec.Command("xset", "s", "on", "+dpms").Run()
	}
}

// inputDesktop is Windows-only; elsewhere we can't tell, so report nothing.
func inputDesktop() string { return "" }

// cursorShape: shape sync is Windows-only; elsewhere show a normal arrow.
func cursorShape() string { return "default" }

// kickScreen: only needed for Windows Desktop Duplication.
func kickScreen() {}
