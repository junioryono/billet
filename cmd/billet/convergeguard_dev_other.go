//go:build !linux

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// devOf widens the device number, which is narrower than 64 bits here.
func devOf(st *syscall.Stat_t) uint64 { return uint64(st.Dev) }

// modeOf widens a stat's mode bits, which are 16 bits here.
func modeOf(st *unix.Stat_t) uint32 { return uint32(st.Mode) }
