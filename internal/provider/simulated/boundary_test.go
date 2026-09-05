package simulated

import (
	"testing"

	"github.com/junioryono/billet/internal/importcheck"
)

// importPath is this package, as an importer would spell it.
const importPath = "github.com/junioryono/billet/internal/provider/simulated"

// NOTHING OUTSIDE A TEST IMPORTS THIS PACKAGE.
//
// config.Load refuses the kind and cmd/billet refuses to construct it, and both
// are behaviour a later change can undo one line at a time. What cannot be
// undone quietly is the import graph: a backend that fabricates completions with
// no production importer cannot be reached by a production path at all. The walk
// reads every non-test Go file in the module, so the first production caller
// fails here rather than in somebody's fleet.
//
// A STRUCTURAL TEST BECAUSE THE HAZARD IS A NEW CALLER, the same argument as
// internal/server's force-destroy caller test.
//
// THE REPLAY HARNESS IS THE ONE NAMED IMPORTER. It is a package of non-test
// files that stands billet up over this backend, and it is exactly as test-side
// as this package: it needs a *testing.T to run and carries this same test for
// itself, so the chain ends at a package nothing in production reaches.
func TestNothingOutsideATestImportsThisPackage(t *testing.T) {
	t.Parallel()

	importers := importcheck.ProductionImporters(t, importPath,
		"github.com/junioryono/billet/internal/replay")
	if len(importers) > 0 {
		t.Fatalf("the simulated backend is imported by production code, which could reach a "+
			"backend that fabricates completions: %v", importers)
	}
}
