// Package importcheck answers which non-test Go files in this module import a
// package.
//
// It exists for the packages that must have NO PRODUCTION CALLER: the scripted
// stand-in for GitHub, the backend that fabricates completions, the replay
// harness that drives both. Each says so in its doc comment, and a comment is
// undone one line at a time; what cannot be undone quietly is the import graph.
// One walker rather than one per package, because a second copy of the rules
// below (which directories are another checkout, which are a nested module) is
// the one that goes stale.
package importcheck

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ProductionImporters lists every non-test Go file in this module, relative to
// the module root, that imports importPath, except files in the testSide
// packages.
//
// A TEST-SIDE IMPORTER IS NAMED, NOT INFERRED. The replay harness is a package
// of non-test files that imports the scripted service and the simulated
// backend, and it is exactly as test-side as they are; what makes that safe to
// exempt is that the harness carries this same test for itself, so the chain
// ends at a package nothing in production reaches. A caller names each such
// package and takes on that obligation; nothing here guesses from a file's
// imports which packages are test-side.
//
// NAMED EXCLUSIONS, NOT EVERY HIDDEN DIRECTORY. A hidden directory can hold Go
// source that ships, so only what is provably not this tree is skipped: the
// object store, a vendored tree, and another CHECKOUT of this module, which is
// a directory under .claude (where sessions park worktrees) whose go.mod
// declares the same module path. Both conditions, because a nested directory
// anywhere else declaring this module's path is not a checkout, it is a way
// past this walk, and it is walked. A NESTED module with a path of its own is
// walked too: tools/lint is one, and a nested module can reach this module's
// internals through a replace directive, so a shipping tool under one is
// exactly a production importer.
func ProductionImporters(tb testing.TB, importPath string, testSide ...string) []string {
	tb.Helper()

	root := ModuleRoot(tb)
	module := modulePath(tb, filepath.Join(root, "go.mod"))
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

			if name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}

			nested := filepath.Join(path, "go.mod")
			if _, err := os.Stat(nested); err == nil && underClaude(root, path) &&
				modulePath(tb, nested) == module {
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

			if spelled != importPath {
				continue
			}

			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}

			if slices.Contains(testSide, packageOf(module, rel)) {
				continue
			}

			importers = append(importers, rel)
		}

		return nil
	})
	if err != nil {
		tb.Fatalf("walk the module: %v", err)
	}

	return importers
}

// underClaude reports whether a directory sits below the module's .claude
// directory, where sessions park their worktrees.
func underClaude(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return slices.Contains(strings.Split(filepath.ToSlash(rel), "/"), ".claude")
}

// packageOf is the import path of the package a module-relative file belongs
// to, as an importer would spell it.
func packageOf(module, rel string) string {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		return module
	}

	return module + "/" + dir
}

// ModuleRoot finds the directory holding go.mod above the working directory.
func ModuleRoot(tb testing.TB) string {
	tb.Helper()

	dir, err := os.Getwd()
	if err != nil {
		tb.Fatalf("Getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Fatal("no go.mod above this package")
		}

		dir = parent
	}
}

// modulePath reads the module directive out of a go.mod.
func modulePath(tb testing.TB, gomod string) string {
	tb.Helper()

	body, err := os.ReadFile(gomod)
	if err != nil {
		tb.Fatalf("read %s: %v", gomod, err)
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}

	tb.Fatalf("%s declares no module", gomod)

	return ""
}
