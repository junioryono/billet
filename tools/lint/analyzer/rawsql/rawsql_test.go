package rawsql_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/junioryono/billet/tools/lint/analyzer/rawsql"
)

// AN ANALYZER THAT STOPS DETECTING ANYTHING STILL REPORTS ZERO, which reads
// exactly like a clean tree -- so the fixture carries both halves. Every
// expectation in testdata is a call the analyzer must find: on *sql.DB, on
// *sql.Tx, through a hand-rolled interface with database/sql's signature, in the
// non-context forms, and with a statement assembled at run time. The cases after
// them are shapes it must stay quiet about: a reasoned suppression, a
// url.Values.Query several packages here really do call, a search API whose Query
// takes a string and returns none of database/sql's types, and a generated file.
//
// TWO OF THE SKIPS ARE PROVED BY THE REAL RUN RATHER THAN HERE, and that is worth
// saying rather than leaving to be assumed. `make lint-custom` runs this analyzer
// over the whole repository, where internal/state's tests alone hold about fifty
// direct SQL calls and internal/state/sqlitedb holds about a hundred and eighty:
// if either the test-file skip or the generated-file skip broke, that run would
// report them by the dozen rather than reporting nothing. The generated case is
// exercised here as well, because a generated file can sit in an ordinary
// package.
func TestRawSQLFindsExecutedStatementsAndStaysQuietOtherwise(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), rawsql.Analyzer, "a")
}
