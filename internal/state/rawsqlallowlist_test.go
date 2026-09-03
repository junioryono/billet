package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// THE ALLOWLIST TABLE AND THE EXEMPTIONS IN THE CODE MUST DESCRIBE THE SAME
// STATEMENTS.
//
// internal/state/queries/README.md carries a table of the statements billet
// deliberately does not generate, one row per site: the file, the CALL that
// executes it, and the reason it cannot be a generated query. The `rawsql`
// analyzer enforces the same thing mechanically, by requiring a
// `//billet:ignore rawsql // <why>` at each of those sites.
//
// TWO RECORDS OF ONE FACT DRIFT, and each direction is its own failure. A
// suppressed call with no row is an exemption nobody reviewing that table would
// know exists -- and the table is the specification the ban is written against. A
// row with no call is worse in a quieter way: it describes an exemption that is no
// longer taken, so a reader believes a statement is still raw when it has been
// generated, and the next person to add one copies a pattern nobody uses.
//
// # Four things this got wrong before, all the same shape
//
// Each version was attacked and each fix was still gameable by a smaller edit.
// They are worth listing because the shape recurs: a check that compares
// something PRESENT rather than something that IDENTIFIES.
//
//   - COUNTS. Swapping one exemption for a different raw call in the same file
//     preserves the count.
//   - REASONS. Five of state.go's exemptions are pragmas sharing one sentence word
//     for word, so the swap survived wherever reasons repeat.
//   - A HUMAN-TYPED STATEMENT LABEL in the directive. Nothing tied the label to
//     the code, so changing the SQL at a site and leaving its comment alone
//     satisfied every check at once.
//   - LOOKING IN ONE PACKAGE. The analyzer honours a directive anywhere in the
//     module, so an exemption in internal/alloc was allowed and invisible here.
//
// SO THE IDENTITY IS DERIVED FROM THE SOURCE, not written beside it: the
// enclosing function, the method called, a readable rendering of the statement
// argument, and a digest of that argument's EXACT bytes. Changing what a
// suppressed site executes changes its key, and no edit to a comment can hide
// that -- the digest is what makes it a key rather than a description.
func TestTheAllowlistTableNamesEveryRawStatement(t *testing.T) {
	t.Parallel()

	sites := moduleExemptions(t)
	if len(sites) == 0 {
		t.Fatal("no rawsql exemption was found anywhere in the module, so this " +
			"comparison would check nothing. If the last raw statement really has been " +
			"generated, delete the table and this test together")
	}

	table := allowlistRows(t)

	keys := make([]string, 0, len(sites))
	for k := range sites {
		keys = append(keys, k)
	}

	for k := range table {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}

	slices.Sort(keys)

	for _, k := range keys {
		switch got, want := sites[k], table[k]; {
		case want == "":
			t.Errorf("%s is a rawsql exemption the allowlist table in queries/README.md "+
				"does not mention, with the reason %q. Every exemption is specified there, "+
				"and one outside internal/state cannot be", k, got)
		case got == "":
			t.Errorf("the allowlist table names %s and no such exemption exists; the "+
				"statement was generated, or the call changed, and the row is stale", k)
		case got != want:
			t.Errorf("%s gives the reason %q and the allowlist table gives %q. The table's "+
				"`Why it cannot be generated` column is the directive's own reason "+
				"verbatim, so these must be equal", k, got, want)
		}
	}
}

// directive is the SAME SHAPE tools/lint/suppress accepts, not a stricter one.
//
// THAT DIFFERENCE WAS A HOLE. This test used to insist on the exact spelling
// `//billet:ignore rawsql // `, while the real parser tolerates whitespace
// variants -- so `// billet:ignore  rawsql // reason` was a live exemption the
// analyzer honoured and this comparison never saw. A checker stricter than the
// thing it checks has a blind spot shaped like the difference.
var directive = regexp.MustCompile(`\A//\s*billet:ignore\s+rawsql\s*//\s*(.+?)\s*\z`)

// executors are the method names rawsql matches. Repeated here deliberately: this
// test cannot import the analyzer, which is a separate module, and a list that
// went stale would show up as an exemption with no row rather than as silence.
var executors = map[string]bool{
	"Exec": true, "ExecContext": true,
	"Query": true, "QueryContext": true,
	"QueryRow": true, "QueryRowContext": true,
	"Prepare": true, "PrepareContext": true,
}

// moduleExemptions maps a derived site identity to the reason recorded there.
func moduleExemptions(t *testing.T) map[string]string {
	t.Helper()

	root := filepath.Join("..", "..")

	// THE WALK HAS TO START AT THE MODULE ROOT, and `../..` is only that while this
	// package sits two levels down. Proving it rather than assuming it means a
	// moved package fails here instead of quietly walking some other tree and
	// reporting that every exemption is undocumented.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root, so this walk would cover the wrong tree: %v",
			root, err)
	}

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		if d.IsDir() {
			if skipDir(rel) {
				return fs.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		exemptionsIn(t, path, rel, out)

		return nil
	})
	if err != nil {
		t.Fatalf("walk the module for rawsql exemptions: %v", err)
	}

	return out
}

// exemptionsIn records every suppressed SQL call in one file.
//
// THE DIRECTIVE AND THE CALL ARE MATCHED BY LINE, the way suppress does it: on
// the call's own line, or alone on the line above. A directive that matches no
// call is left to the analyzer, which reports an unused one; what this needs is
// the calls that ARE suppressed, because those are the exemptions.
func exemptionsIn(t *testing.T, path, rel string, out map[string]string) {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	reasons := map[int]string{}

	for _, group := range file.Comments {
		for _, c := range group.List {
			if m := directive.FindStringSubmatch(c.Text); m != nil {
				reasons[fset.Position(c.Pos()).Line] = strings.TrimSpace(m[1])
			}
		}
	}

	if len(reasons) == 0 {
		return
	}

	// EVERY DIRECTIVE HAS TO BE ACCOUNTED FOR, not only the ones that turn out to
	// suppress a call. Deriving identity from the CALL was the fix for a
	// human-typed label, and it opened a gap of its own: a directive sitting above
	// something this does not recognise as a SQL call became invisible here.
	// billetlint would report it as unused -- but "another gate covers it" is how
	// a hole survives, and the claim this test makes is that no rawsql exemption
	// exists that the table does not describe.
	attributed := map[int]bool{}

	defer func() {
		for line := range reasons {
			if !attributed[line] && !attributed[line+1] {
				t.Errorf("%s:%d carries a rawsql exemption that suppresses no SQL call. "+
					"billetlint reports an unused directive; it is named here too, because "+
					"a standing exemption with nothing under it is a licence for whatever "+
					"lands on that line next", rel, line)
			}
		}
	}()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}

	lines := strings.Split(string(src), "\n")

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !executors[sel.Sel.Name] {
				return true
			}

			line := fset.Position(sel.Sel.Pos()).Line

			reason, ok := reasons[line]
			if !ok {
				reason, ok = reasons[line-1]
			}

			if !ok {
				return true
			}

			site, readable := siteOf(sel.Sel.Name, call, src, lines, fset)
			key := rel + ": " + fn.Name.Name + ": " + site

			// A KEY THE DIGEST COULD NOT BE TAKEN FOR IS REPORTED, not merely made
			// distinctive. `#unreadable` fails today because no row carries it -- and
			// a row COULD be written carrying it, which would enshrine the fallback
			// and give that site no exact-byte identity at all. Saying so here means
			// the table can never quietly absorb one.
			//
			// ANSWERED BY siteOf RATHER THAN SNIFFED OUT OF THE KEY, because a path,
			// a function or a statement may legitimately contain the word and would
			// then be reported as unreadable while being perfectly fine.
			if !readable {
				t.Errorf("%s: could not take a digest of the statement argument at line "+
					"%d, so this site has no exact identity. Do not write a table row "+
					"for it -- fix the derivation", rel, line)
			}
			if _, seen := out[key]; seen {
				t.Errorf("%s has two exemptions that derive the same identity, %q. Neither "+
					"the table nor this comparison can tell them apart", rel, key)
			}

			attributed[line] = true
			out[key] = reason

			return true
		})
	}
}

// unreadableDigest stands in when a statement argument's source span cannot be
// taken. The caller reports it; it is never a value a table row may carry.
const unreadableDigest = "unreadable"

// siteOf renders one suppressed call as `Method(statement #digest)`.
//
// TWO PARTS, AND THEY DO DIFFERENT JOBS. The rendered statement is for a person
// reading the allowlist table; the digest is what makes the identity INJECTIVE.
// Three versions of the readable part in a row were lossy in a way that let
// changed bytes derive an unchanged key -- the first line only, then all
// whitespace collapsed (which reaches inside string literals), then continuation
// indentation dropped (which is bytes of a raw Go string, and so possibly of a
// SQL literal) and every backtick rendered as a single quote (which conflates a
// backtick-quoted SQLite IDENTIFIER with a single-quoted VALUE). Each fix was a
// smaller version of the same mistake: a rendering chosen for legibility being
// asked to serve as an identity.
//
// So legibility and identity are separated. The digest is sha256 over the
// argument's EXACT source bytes, which cannot be lossy about anything, and twelve
// hex characters of it are enough for a guard on a documentation table. The
// readable part may be as lossy as it likes.
//
// WHAT IT CANNOT SEE IS AN ARGUMENT THAT IS AN IDENTIFIER, and that is FIVE of
// the thirteen sites rather than the one it is tempting to name: `PeekLedger`'s
// `q`, `migrate`'s `stmt` and `bootstrapSchemaMigrations`, and both of
// `readOnlyDBTX`'s forwarded `query` parameters. The digest covers the argument's
// source, so it sees `q` change to `r` and not what either holds -- and
// `bootstrapSchemaMigrations` is a package CONSTANT whose whole value could be
// rewritten under an unchanged key.
//
// Resolving it needs the type checker or data-flow analysis, and it is
// deliberately not written: this test guards a piece of DOCUMENTATION, every one
// of those five is a statement whose text is assembled or supplied at run time,
// and what such a statement does is exactly what the Why column has to explain
// rather than what any derived key could capture. The analyzer itself is
// unaffected -- it reports the call whatever the argument is, and only a directive
// carrying a reason silences it.
//
// The *Context forms carry the context first, so the statement is the second
// argument there and the first otherwise.
func siteOf(
	method string,
	call *ast.CallExpr,
	src []byte,
	lines []string,
	fset *token.FileSet,
) (site string, readable bool) {
	at := 0
	if strings.HasSuffix(method, "Context") {
		at = 1
	}

	// A CALL WITH NO STATEMENT ARGUMENT IS READABLE, not unreadable: there is
	// nothing to digest, and reporting it would send somebody looking for a
	// derivation fault where the call simply has no SQL in it.
	if len(call.Args) <= at {
		return method + "()", true
	}

	arg := call.Args[at]
	pos, end := fset.Position(arg.Pos()), fset.Position(arg.End())

	// THE DIGEST COVERS THE EXACT BYTES, taken by offset rather than reassembled
	// from lines, so nothing the rendering does can reach it.
	//
	// THE FALLBACK IS REPORTED BY THE CALLER rather than left to fail through the
	// table comparison, because a row could be written carrying it -- which would
	// enshrine the fallback and leave that site with no exact-byte identity.
	if pos.Offset < 0 || end.Offset > len(src) || pos.Offset >= end.Offset {
		return method + "(#" + unreadableDigest + ")", false
	}

	h := sha256.Sum256(src[pos.Offset:end.Offset])
	sum := hex.EncodeToString(h[:])[:12]

	var text strings.Builder

	for n := pos.Line; n <= end.Line && n-1 < len(lines); n++ {
		line := lines[n-1]

		from, to := 0, len(line)
		if n == pos.Line {
			from = min(pos.Column-1, len(line))
		}

		if n == end.Line {
			to = min(end.Column-1, len(line))
		}

		if from >= to {
			continue
		}

		part := line[from:to]
		if n > pos.Line {
			text.WriteString(" ")

			part = strings.TrimLeft(part, " \t")
		}

		text.WriteString(part)
	}

	// A GO RAW STRING IS DELIMITED BY BACKTICKS AND A MARKDOWN CELL CANNOT HOLD
	// ONE. Rendering them as single quotes is safe HERE only because the digest
	// beside it is what the comparison turns on.
	rendered := strings.ReplaceAll(text.String(), "`", "'")

	return method + "(" + rendered + " #" + sum + ")", true
}

// skipDir reports whether a directory holds something other than billet's own
// compiled source.
//
// BY PATH RATHER THAN BY BASE NAME, because a base-name rule silently grows: it
// would skip any future directory called `lint` or `ledgerdb` anywhere in the
// tree, and over-skipping means an exemption nobody sees, which is the failure
// this test exists to prevent.
//
// `.claude` IS THE ONE THAT WOULD ACTUALLY BITE. Sessions park git worktrees under
// it, so a run from the main checkout descends into another branch's in-progress
// source and fails on directives the committer cannot reach -- exactly the reason
// .golangci.yml and .gitignore exclude the same path. A parked worktree's `.git`
// is a FILE rather than a directory, so no rule about `.git` would have stopped
// it. A gate that fails for something nobody can fix is one that gets deleted.
func skipDir(rel string) bool {
	switch rel {
	case ".git", ".claude", "bin",
		// A SEPARATE MODULE, and its fixtures carry rawsql directives ON PURPOSE:
		// they are what proves the analyzer still reports and still honours a
		// suppression.
		filepath.Join("tools", "lint"),
		// GENERATED, which the analyzer skips too -- a directive there would
		// suppress nothing.
		filepath.Join("internal", "state", "ledgerdb"):
		return true
	}

	// The toolchain never compiles a testdata directory, so nothing in one can be
	// a live exemption.
	return filepath.Base(rel) == "testdata"
}

// allowlistRows maps the same derived identity to the Why column of each row.
//
// EVERY ROW IN THE TABLE MUST PARSE, and every row is whatever sits between the
// header and the blank line after it. Matching only the rows a strict pattern
// understands would let a malformed one -- a stray pipe in a cell, a `Where` cell
// that stopped being a path -- disappear from the comparison rather than fail it,
// which is the quiet direction.
func allowlistRows(t *testing.T) map[string]string {
	t.Helper()

	src, err := os.ReadFile(filepath.Join("queries", "README.md"))
	if err != nil {
		t.Fatalf("read the query README: %v", err)
	}

	const header = "| Where | Call | Why it cannot be generated |"

	at := bytes.Index(src, []byte(header))
	if at < 0 {
		t.Fatalf("queries/README.md has no allowlist table headed %q; if its shape "+
			"changed, update this test rather than dropping the comparison", header)
	}

	row := regexp.MustCompile("^\\| `([^`|]+)` \\| `([^`|]+)` \\| ([^|]+?) \\|$")

	out := map[string]string{}
	rows := 0

	for _, line := range strings.Split(string(src)[at:], "\n")[1:] {
		if !strings.HasPrefix(line, "|") {
			break
		}

		if strings.HasPrefix(line, "|---") {
			continue
		}

		rows++

		m := row.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("this allowlist row cannot be parsed, so it would silently drop out "+
				"of the comparison -- a cell most likely contains a `|`:\n  %s", line)

			continue
		}

		key := m[1] + ": " + m[2]
		if _, seen := out[key]; seen {
			t.Errorf("the allowlist table has two rows for %s; one row per site, or a "+
				"contradictory row can hide behind a correct one", key)
		}

		out[key] = strings.TrimSpace(m[3])
	}

	if rows == 0 {
		t.Fatal("the allowlist table has no rows, so this comparison would check nothing")
	}

	return out
}
