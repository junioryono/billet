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
// threshold itself set casFill on their service. casFillDefault's value is
// checked first, so this override cannot hide a change to it; a constructor
// that stopped reading casFillDefault is not caught here.
func TestMain(m *testing.M) {
	if casFillDefault != casFullFraction {
		fmt.Fprintf(os.Stderr, "casFillDefault is %v, want casFullFraction (%v): a new CacheService "+
			"would no longer stop caching where the volume is %v full\n",
			casFillDefault, casFullFraction, casFullFraction)
		os.Exit(1)
	}

	casFillDefault = 1

	m.Run()
}
