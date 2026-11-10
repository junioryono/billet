//go:build !linux

package blockfs

import "golang.org/x/sys/unix"

// rdevOf is a stat's device number, which darwin reports as an int32.
func rdevOf(st *unix.Stat_t) uint64 { return uint64(st.Rdev) }
