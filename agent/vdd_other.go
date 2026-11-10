//go:build !windows

package main

import "errors"

// Virtual screens are only supported on Windows (Parsec VDD driver).
func vscreenAvailable() bool   { return false }
func vscreenAdd() (int, error) { return -1, errors.New("virtual screens are Windows-only") }
func vscreenRemove(idx int)    {}
