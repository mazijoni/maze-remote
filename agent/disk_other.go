//go:build !linux

package main

// diskSpace is unknown here (motion recording only runs on the Pi).
func diskSpace(dir string) (free, total int64) { return 0, 0 }
