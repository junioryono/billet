package blockfs

import "golang.org/x/sys/unix"

// rdevOf is a stat's device number, which linux reports as a uint64.
func rdevOf(st *unix.Stat_t) uint64 { return st.Rdev }
