package suppress_test

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"

	"github.com/junioryono/billet/tools/lint/suppress"
)

// A BARE DIRECTIVE IS ITSELF A FINDING, and it is tested here rather than in the
// analyzer's own fixture because it CANNOT be: analysistest declares an expected
// diagnostic with a `// want` comment on the diagnostic's line, and on this line
// that comment is exactly what the directive would read as its reason. Testing it
// through analysistest would therefore assert the opposite of the rule.
func TestABareDirectiveIsReported(t *testing.T) {
	src := `package p

func f() {
	//billet:ignore parallelshared
	_ = 1
}
`
	diags := indexOf(t, src)

	if len(diags) != 1 {
		t.Fatalf("a directive with no reason produced %d diagnostics, want 1: %v", len(diags), diags)
	}

	if !strings.Contains(diags[0], "needs a reason") {
		t.Errorf("the diagnostic was %q; it has to say what is missing", diags[0])
	}
}

// A directive whose "reason" is only whitespace is the same mistake wearing a
// comment marker.
func TestAnEmptyReasonIsReported(t *testing.T) {
	src := `package p

func f() {
	//billet:ignore parallelshared //
	_ = 1
}
`
	if diags := indexOf(t, src); len(diags) != 1 {
		t.Fatalf("an empty reason produced %d diagnostics, want 1: %v", len(diags), diags)
	}
}

func TestAReasonedDirectiveIsAccepted(t *testing.T) {
	src := `package p

func f() {
	//billet:ignore parallelshared // guarded by mu
	_ = 1
}
`
	if diags := indexOf(t, src); len(diags) != 0 {
		t.Fatalf("a reasoned directive was reported: %v", diags)
	}
}

// THE DIRECTIVE COVERS ITS OWN LINE AND THE ONE BELOW IT, and both are asserted:
// a rule that only worked inline would force it onto a line that is often
// already long, and one that only worked above would read as covering the whole
// block that follows.
func TestADirectiveCoversItsOwnLineAndTheNextOne(t *testing.T) {
	src := `package p

func f() {
	//billet:ignore parallelshared // above the write
	_ = 1
	_ = 2 //billet:ignore parallelshared // on the write
	_ = 3
}
`
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	pass := &analysis.Pass{Fset: fset, Report: func(analysis.Diagnostic) {}}
	lines := suppress.Index(pass, file, []byte(src))

	// Lines are 1-based: 5 is `_ = 1`, 6 is `_ = 2`, 7 is `_ = 3`.
	for _, tc := range []struct {
		line int
		want bool
		why  string
	}{
		{line: 5, want: true, why: "the line below a directive"},
		{line: 6, want: true, why: "the line carrying a directive"},
		{line: 7, want: false, why: "a line a directive does not reach"},
	} {
		pos := fset.File(file.Pos()).LineStart(tc.line)
		if got := lines.Skip(pass, pos, "parallelshared"); got != tc.want {
			t.Errorf("%s: Skip = %v, want %v", tc.why, got, tc.want)
		}
	}
}

// A directive naming ANOTHER analyzer does not silence this one.
func TestADirectiveIsScopedToItsAnalyzer(t *testing.T) {
	src := `package p

func f() {
	_ = 1 //billet:ignore somethingelse // not this analyzer
}
`
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	pass := &analysis.Pass{Fset: fset, Report: func(analysis.Diagnostic) {}}
	lines := suppress.Index(pass, file, []byte(src))

	pos := fset.File(file.Pos()).LineStart(4)
	if lines.Skip(pass, pos, "parallelshared") {
		t.Error("a directive naming another analyzer silenced this one")
	}
}

// indexOf runs Index over src and returns the diagnostics it reported.
func indexOf(t *testing.T, src string) []string {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var got []string

	pass := &analysis.Pass{
		Fset:   fset,
		Report: func(d analysis.Diagnostic) { got = append(got, d.Message) },
	}

	suppress.Index(pass, file, []byte(src))

	return got
}

// AN UNUSED DIRECTIVE IS REPORTED, which is the rule .golangci.yml already sets
// for nolintlint. A licence that covers no finding today is one that will cover
// whatever appears on that line next, with a reason written for something else.
func TestAnUnusedDirectiveIsReported(t *testing.T) {
	src := `package p

func f() {
	//billet:ignore parallelshared // nothing on this line is ever flagged
	_ = 1
}
`
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var got []string

	pass := &analysis.Pass{
		Fset:   fset,
		Report: func(d analysis.Diagnostic) { got = append(got, d.Message) },
	}

	lines := suppress.Index(pass, file, []byte(src))

	if len(got) != 0 {
		t.Fatalf("indexing a reasoned directive reported %v", got)
	}

	lines.ReportUnused(pass, "parallelshared")

	if len(got) != 1 {
		t.Fatalf("an unused directive produced %d diagnostics, want 1: %v", len(got), got)
	}

	if !strings.Contains(got[0], "suppresses nothing") {
		t.Errorf("the diagnostic was %q; it has to say the directive covers no finding", got[0])
	}
}

// And one that DID suppress something is not reported, or the rule would be
// unusable: every legitimate exemption would also be a finding.
func TestAUsedDirectiveIsNotReported(t *testing.T) {
	src := `package p

func f() {
	//billet:ignore parallelshared // this one is consumed below
	_ = 1
}
`
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var got []string

	pass := &analysis.Pass{
		Fset:   fset,
		Report: func(d analysis.Diagnostic) { got = append(got, d.Message) },
	}

	lines := suppress.Index(pass, file, []byte(src))

	if !lines.Skip(pass, fset.File(file.Pos()).LineStart(5), "parallelshared") {
		t.Fatal("the directive did not cover the line below it")
	}

	lines.ReportUnused(pass, "parallelshared")

	if len(got) != 0 {
		t.Errorf("a consumed directive was reported as unused: %v", got)
	}
}
