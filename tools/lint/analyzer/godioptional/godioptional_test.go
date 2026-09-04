package godioptional_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/junioryono/billet/tools/lint/analyzer/godioptional"
)

// AN ANALYZER THAT STOPS DETECTING ANYTHING STILL REPORTS ZERO, which reads
// exactly like a clean tree -- so the fixture carries both halves. Every `want`
// in testdata is an optional field the analyzer must find, and the cases after
// them are shapes it must stay quiet about: a required field, an optional tag on
// a struct that embeds nothing godi reads, a reasoned suppression on the line
// above, and a bare directive, which is itself reported.
func TestGodiOptionalFindsUnexplainedFieldsAndStaysQuietOtherwise(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), godioptional.Analyzer, "a")
}
