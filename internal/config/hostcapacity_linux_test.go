package config

import (
	"math"
	"testing"
)

// THE MULTIPLICATION IS THE PART THAT CANNOT BE OBSERVED ON THIS HOST.
//
// sysinfo reports memory as a COUNT of mem_unit-sized blocks. On an ordinary
// 64-bit kernel mem_unit is 1, so an implementation that ignored it agrees with
// the syscall on every machine this is likely to be run on, and under-reports by
// a factor of 4096 on the one where it does not — a 64GiB host claiming 16MiB,
// which registers a node nothing can be placed on and reads like a scheduling
// bug rather than a units bug.
//
// So the units are supplied rather than measured. This file is linux-only and
// therefore executes in CI, which runs on ubuntu, rather than on a macOS
// workstation where it would not build at all.
func TestSysinfoBlocksAreScaledByTheirUnit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		blocks uint64
		unit   uint32
		want   uint64
		fails  bool
	}{
		{
			// The ordinary modern kernel, and the case that hides a missing multiply.
			name: "a unit of one is a byte count", blocks: 64 * 1024 * 1024 * 1024, unit: 1,
			want: 64 * 1024 * 1024 * 1024,
		},
		{
			// The case a host-only test can never produce.
			name: "a page-sized unit scales the count", blocks: 16 * 1024 * 1024, unit: 4096,
			want: 64 * 1024 * 1024 * 1024,
		},
		{
			name: "an unstated unit is one byte", blocks: 8 * 1024 * 1024 * 1024, unit: 0,
			want: 8 * 1024 * 1024 * 1024,
		},
		{
			// Exactly representable: the boundary must be ACCEPTED, so a '>' that
			// should be '>=' is caught from this side.
			name: "the largest exact product", blocks: math.MaxUint64 / 4096, unit: 4096,
			want: (math.MaxUint64 / 4096) * 4096,
		},
		{
			// One block past it, so the check is caught from the other side too.
			name: "one block too many", blocks: math.MaxUint64/4096 + 1, unit: 4096, fails: true,
		},
		{
			name: "every bit set against a real unit", blocks: math.MaxUint64, unit: 4096, fails: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := bytesFromBlocks(tc.blocks, tc.unit)

			if tc.fails {
				if err == nil {
					t.Fatalf("bytesFromBlocks(%d, %d) = %d, want an error rather than a wrapped count",
						tc.blocks, tc.unit, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("bytesFromBlocks(%d, %d): %v", tc.blocks, tc.unit, err)
			}

			if got != tc.want {
				t.Errorf("bytesFromBlocks(%d, %d) = %d, want %d", tc.blocks, tc.unit, got, tc.want)
			}
		})
	}
}
