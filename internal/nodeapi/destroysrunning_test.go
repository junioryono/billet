package nodeapi_test

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A POLL NAMING AS MANY DESTROYS AS A NODE HOLDS FITS THE NODE WIRE'S HEADERS with
// room to spare: the node wire's server refuses a request whose headers pass 16 KiB,
// and a poll refused for its header would leave every command behind it untaken.
func TestTheMostDestroysAPollNamesFitItsHeader(t *testing.T) {
	t.Parallel()

	ids := make([]string, nodeapi.MaxDestroysRunning)
	for i := range ids {
		ids[i] = strings.Repeat("f", 32) // a command id: 16 random bytes in hex
	}

	if n := len(nodeapi.HeaderDestroysRunning) + len(strings.Join(ids, ",")); n > 8<<10 {
		t.Fatalf("a poll naming %d destroys carries %d header bytes, more than half the wire's 16 KiB",
			nodeapi.MaxDestroysRunning, n)
	}
}
