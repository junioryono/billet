package replay

import (
	"testing"

	"github.com/junioryono/billet/internal/importcheck"
)

// importPath is this package, as an importer would spell it.
const importPath = "github.com/junioryono/billet/internal/replay"

// NOTHING OUTSIDE A TEST IMPORTS THIS PACKAGE.
//
// The harness stands billet up over a scripted GitHub and a backend that
// fabricates completions, and is the one caller of both outside their own tests.
// A production path that reached it would reach them; the walk over the module's
// import graph is what makes "test-side consumer" a fact rather than a sentence.
func TestNothingOutsideATestImportsThisPackage(t *testing.T) {
	t.Parallel()

	if importers := importcheck.ProductionImporters(t, importPath); len(importers) > 0 {
		t.Fatalf("the replay harness is imported by production code: %v", importers)
	}
}
