package parallelshared_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/junioryono/billet/tools/lint/analyzer/parallelshared"
)

// AN ANALYZER THAT STOPS DETECTING ANYTHING STILL REPORTS ZERO, which reads
// exactly like a clean tree -- so the fixture carries both halves. Every `//
// want` below is a write the analyzer must find, and the cases after them are
// shapes it must stay quiet about: a sequential subtest, a subtest that owns its
// own state, per-iteration state in a range, a read, a shadowed name, and a
// write carrying a reasoned suppression.
func TestParallelSharedFindsWritesAndStaysQuietOtherwise(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), parallelshared.Analyzer, "a")
}
