package scripts_test

import (
	"testing"

	"github.com/junioryono/billet/internal/pwshtest"
)

// TestMain gives every pwsh this package starts a cache of its own (see
// internal/pwshtest and #791): its tests start them in parallel, and a shared
// startup profile is what crashed them.
func TestMain(m *testing.M) {
	cleanup, err := pwshtest.Isolate()
	if err != nil {
		panic(err)
	}

	m.Run()
	cleanup()
}
