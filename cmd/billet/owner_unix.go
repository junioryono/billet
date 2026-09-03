//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileOwner reads a stat result's uid/gid. os.FileInfo.Sys() holds a
// *syscall.Stat_t here — not the structurally identical *unix.Stat_t, which
// this assertion would silently miss.
func fileOwner(fi os.FileInfo) (int, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}

	return int(st.Uid), int(st.Gid), true
}
