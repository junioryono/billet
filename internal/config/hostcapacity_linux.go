package config

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// detectTotalMemory reports this machine's usable RAM.
//
// SYSINFO REPORTS A COUNT AND A UNIT, NOT A SIZE. Totalram is a number of
// mem_unit-sized blocks, so the answer is a product and reading Totalram alone
// under-reports by whatever the unit is — on a kernel where mem_unit is 4096
// that is a 64GiB host claiming 16MiB, which registers a node nothing can be
// placed on and reads like a scheduling bug rather than a units bug.
//
// /proc/meminfo would also work and is not used: it is a text file that has to
// be found, opened, parsed and unit-converted, and every one of those steps is a
// way to be wrong about a number this load-bearing.
func detectTotalMemory() (uint64, error) {
	var si unix.Sysinfo_t

	if err := unix.Sysinfo(&si); err != nil {
		return 0, fmt.Errorf("sysinfo: %w", err)
	}

	return bytesFromBlocks(si.Totalram, si.Unit)
}

// bytesFromBlocks turns sysinfo's count-and-unit into a byte count.
//
// SEPARATE FROM THE SYSCALL SO THE MULTIPLICATION CAN BE TESTED, and that is not
// ceremony: mem_unit is 1 on an ordinary 64-bit kernel, so a version of this
// that dropped the multiply entirely would agree with the real syscall on every
// machine billet is likely to be tested on, and be wrong by a factor of 4096 on
// the one where it is not. A test that can only observe this host cannot see
// that, so the arithmetic is exercised with units the host does not have.
func bytesFromBlocks(blocks uint64, memUnit uint32) (uint64, error) {
	// TREATED AS BYTES WHEN THE KERNEL SAYS NOTHING. mem_unit arrived in Linux
	// 2.3.23 and is 1 on anything current, but a zero here would multiply the
	// whole reading away and report a host with no memory at all.
	unit := uint64(memUnit)
	if unit == 0 {
		unit = 1
	}

	// CHECKED BEFORE IT IS DONE, because this product is the one place an
	// unsigned overflow could wrap to a small number and look like a plausible
	// reading rather than an error.
	if blocks > ^uint64(0)/unit {
		return 0, fmt.Errorf("sysinfo reports %d blocks of %d bytes, which overflows",
			blocks, unit)
	}

	return blocks * unit, nil
}
