package godiconstructor_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/junioryono/billet/tools/lint/analyzer/godiconstructor"
)

// AN ANALYZER THAT STOPS DETECTING ANYTHING STILL REPORTS ZERO, which reads
// exactly like a clean tree -- so the fixture carries both halves. The `want`
// in package a is a constructor the analyzer must find; the cases after it are
// shapes it must stay quiet about: an interface result, an exported concrete
// result, an unexported constructor, a method, and a reasoned suppression.
// Package b imports no godi and is left alone whatever it returns.
func TestGodiConstructorFindsHiddenResultsAndStaysQuietOtherwise(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), godiconstructor.Analyzer, "a", "b")
}
