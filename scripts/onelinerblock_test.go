package scripts_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A CONTROL-FLOW BODY ON ONE LINE IS CONTROL FLOW THAT DOES NOT REVIEW.
//
// `if err != nil { return err }` puts the consequence off to the right of the
// condition, where a line diff shows one changed line for two changed decisions
// and a reader scanning the left margin sees a test with no body. billet's code
// already reads the other way everywhere -- measured at ZERO violations across
// internal/, cmd/ and scripts/ before this test existed, which is the bar for
// adding a gate at all: a rule that arrives with a backlog is a rule people
// learn to route around.
//
// A TEST RATHER THAN A LINTER because golangci-lint cannot express it. wsl_v5
// covers the neighbouring ground -- blank lines around blocks -- and was measured
// on this tree at 5882 findings, which is a rewrite rather than a rule, so it
// stays off.
//
// EMPTY BLOCKS ARE EXEMPT. `for range ch {}` and a deliberately empty branch say
// exactly what they mean on one line, and revive's empty-block rule already has
// an opinion about the ones that are mistakes. Generated code is exempt because
// nobody reviews it; test files are NOT, because a test is read more often than
// the code it covers.
func TestNoGoBodyIsWrittenOnOneLine(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}

	var offences []string

	files := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// .claude holds git worktrees of OTHER branches, whose code this
			// checkout is not responsible for -- the same reason .golangci.yml
			// excludes the path.
			switch d.Name() {
			case ".git", ".claude", "node_modules", "vendor", "bin":
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()

		// A FILE THAT DOES NOT PARSE IS REPORTED, NOT SKIPPED. Skipping is how a
		// walk quietly stops examining the thing it was pointed at, and every
		// .go file in this repository is expected to parse -- the compiler would
		// say so anyway, but only for files it is asked to build.
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}

		if isGenerated(file) {
			return nil
		}

		files++

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		ast.Inspect(file, func(n ast.Node) bool {
			body, kind := blockOf(n)
			if body == nil || len(body.List) == 0 {
				return true
			}

			open := fset.Position(body.Lbrace)
			closed := fset.Position(body.Rbrace)

			if open.Line == closed.Line {
				offences = append(offences,
					rel+":"+strconv.Itoa(open.Line)+": "+kind+" body written on one line")
			}

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// A WALK THAT FOUND NOTHING TO PARSE PASSES FOR THE WRONG REASON, which is
	// the failure this whole file is arranged against elsewhere.
	if files < 100 {
		t.Fatalf("only %d Go files were examined; this guard checked almost nothing", files)
	}

	for _, o := range offences {
		t.Error(o + "\n\tExpand it: a one-line body hides the consequence off to the " +
			"right of the condition, and a line diff shows one change for two decisions.")
	}
}

// blockOf returns the body a one-line rule applies to, and what to call it.
//
// CONTROL FLOW ONLY, and function literals are deliberately absent. A one-line
// `func() { close(ch) }` passed as an option or a cleanup is not a branch: there
// is no condition for its consequence to hide behind, which is the entire
// argument above. Including them would take this rule from zero violations to a
// backlog, and a rule with a backlog is one people learn to route around.
func blockOf(n ast.Node) (*ast.BlockStmt, string) {
	switch s := n.(type) {
	case *ast.IfStmt:
		return s.Body, "if"
	case *ast.ForStmt:
		return s.Body, "for"
	case *ast.RangeStmt:
		return s.Body, "range"
	case *ast.SwitchStmt:
		return s.Body, "switch"
	case *ast.TypeSwitchStmt:
		return s.Body, "type switch"
	case *ast.SelectStmt:
		return s.Body, "select"
	default:
		return nil, ""
	}
}

// isGenerated reports whether a file carries the standard generated marker.
func isGenerated(file *ast.File) bool {
	for _, group := range file.Comments {
		for _, c := range group.List {
			if strings.HasPrefix(c.Text, "// Code generated ") &&
				strings.HasSuffix(c.Text, " DO NOT EDIT.") {
				return true
			}
		}
	}

	return false
}
