//go:build linux

package egress

import (
	"strings"
	"testing"
)

// ZERO IS EVERY CONNECTION NOBODY MARKED, the host's own among them, so it never reaches the tracker.
func TestForgettingConnectionsRefusesAMarkThatIsNotAGuests(t *testing.T) {
	t.Parallel()

	for _, mark := range []uint32{0, 1, markBase, markBase ^ 0x10000} {
		if err := forgetConnections(mark); err == nil || !strings.Contains(err.Error(), "is not a mark") {
			t.Errorf("forgetConnections(%#x) = %v", mark, err)
		}
	}
}
