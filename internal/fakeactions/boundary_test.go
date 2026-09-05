package fakeactions

import (
	"testing"

	"github.com/junioryono/billet/internal/importcheck"
)

// importPath is this package, as an importer would spell it.
const importPath = "github.com/junioryono/billet/internal/fakeactions"

// NOTHING OUTSIDE A TEST IMPORTS THIS PACKAGE.
//
// The package comment has said so since it was written, and a comment is undone
// one line at a time. A production path that reached a service answering the
// App handshake with a throwaway key would be a control plane talking to a
// GitHub that is not there, so the claim is asserted over the import graph.
//
// THE REPLAY HARNESS IS THE ONE NAMED IMPORTER: non-test files that script this
// service for a whole workload, needing a *testing.T to run and carrying this
// same test for themselves, so the chain ends at a package nothing in production
// reaches.
func TestNothingOutsideATestImportsThisPackage(t *testing.T) {
	t.Parallel()

	importers := importcheck.ProductionImporters(t, importPath,
		"github.com/junioryono/billet/internal/replay")
	if len(importers) > 0 {
		t.Fatalf("the fake Actions service is imported by production code: %v", importers)
	}
}
