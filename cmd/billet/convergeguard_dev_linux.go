package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// devOf is the device number as the hook records it; already 64 bits here.
func devOf(st *syscall.Stat_t) uint64 { return st.Dev }

// modeOf is a stat's mode bits as the classifier reads them; already 32 bits
// here.
func modeOf(st *unix.Stat_t) uint32 { return st.Mode }
