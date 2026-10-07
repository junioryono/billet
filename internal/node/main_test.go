package node

import (
	"fmt"
	"os"
	"testing"
)

// TestMain runs this package's tests with the cache's fill threshold out of the
// way: casFull asks the filesystem under t.TempDir, so on a disk more than 90%
// full every test that stores a cache object failed with 507 (measured on a
// development Mac at 91%, 2026-10-05) while the code was right. The tests of the
// threshold itself set casFill on their service. The production threshold is
// checked first, against its own literal as well as the constant, so this
// override cannot hide a change to either; a constructor that stopped reading
// casFillDefault is not caught here.
func TestMain(m *testing.M) {
	if casFullFraction != 0.9 {
		fmt.Fprintf(os.Stderr, "casFullFraction is %v, want 0.9: a node would stop caching at "+
			"another fill than the one the tests below were written against; change this "+
			"check in the same commit, on purpose\n", casFullFraction)
		os.Exit(1)
	}

	if casFillDefault != casFullFraction {
		fmt.Fprintf(os.Stderr, "casFillDefault is %v, want casFullFraction (%v): a new CacheService "+
			"would no longer stop caching where the volume is %v full\n",
			casFillDefault, casFullFraction, casFullFraction)
		os.Exit(1)
	}

	casFillDefault = 1

	m.Run()
}
