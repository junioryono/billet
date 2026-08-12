package config

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// detectTotalMemory reports this machine's usable RAM.
//
// hw.memsize IS ALREADY BYTES, with no unit to apply — unlike Linux, where the
// reading is a count of blocks. The two platforms differ in kind rather than in
// spelling, which is why this is a file per platform and not one function with a
// switch.
//
// hw.memsize rather than hw.physmem: physmem is a 32-bit sysctl and saturates
// at 4GiB, so on every Mac billet would plausibly run on it reports exactly
// 4GiB and is wrong by an order of magnitude without ever failing.
func detectTotalMemory() (uint64, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}

	return total, nil
}
