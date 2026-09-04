package simulated

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// importPath is this package, as an importer would spell it.
const importPath = "github.com/junioryono/billet/internal/provider/simulated"

// NOTHING OUTSIDE A TEST IMPORTS THIS PACKAGE.
//
// config.Load refuses the kind and cmd/billet refuses to construct it, and both
// are behaviour a later change can undo one line at a time. What cannot be
// undone quietly is the import graph: a backend that fabricates completions with
// no production importer cannot be reached by a production path at all. This
// walks every non-test Go file in the module and reads its imports, so the
// first production caller fails here rather than in somebody's fleet.
//
// A STRUCTURAL TEST BECAUSE THE HAZARD IS A NEW CALLER, the same argument as
// internal/server's force-destroy caller test.
func TestNothingOutsideATestImportsThisPackage(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	fset := token.NewFileSet()

	var importers []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := d.Name()

		if d.IsDir() {
			if path == root {
				return nil
			}

			// NAMED EXCLUSIONS, NOT EVERY HIDDEN DIRECTORY. A hidden directory can
			// hold Go source that ships, so only what is provably not this module
			// is skipped: the object store, a vendored tree, and any directory
			// carrying its own go.mod, which is another checkout (sessions park
			// worktrees under .claude) or a nested module that cannot import this
			// one's internals without a replace directive of its own.
			if name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}

			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}

		for _, imp := range file.Imports {
			spelled, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}

			if spelled == importPath {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}

				importers = append(importers, rel)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}

	if len(importers) > 0 {
		t.Fatalf("the simulated backend is imported by production code, which could reach a "+
			"backend that fabricates completions: %v", importers)
	}
}

// moduleRoot finds the directory holding go.mod above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package")
		}

		dir = parent
	}
}
