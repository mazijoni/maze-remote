package main

import "syscall"

// diskSpace is the free and total bytes on the disk holding dir.
func diskSpace(dir string) (free, total int64) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, 0
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize)
}
