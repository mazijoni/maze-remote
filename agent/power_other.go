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
