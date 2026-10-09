package scripts_test

import (
	"go/ast"
	gobuild "go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

var fuzzEntry = regexp.MustCompile(`(?m)^\s+- \{ package: (\./\S+), fuzz: (Fuzz\w+) \}$`)

// EVERY FUZZ TARGET IS SEARCHED NIGHTLY, AND NOTHING ELSE IS. The seeds of a
// Fuzz function run with every `go test`, but the search past them runs only
// for the targets .github/workflows/fuzz.yml lists, which is also the list
// `make fuzz` reads, so a target added without a row there is one nobody ever
// fuzzes. Found by parsing every test file of this module the nightly
// platform builds, by Go's own rule for what a fuzz target is, whatever the
// testing package is imported as.
func TestEveryFuzzTargetIsSearchedNightly(t *testing.T) {
	t.Parallel()

	const root = ".."

	fset := token.NewFileSet()

	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// ANOTHER MODULE'S TESTS ARE ITS OWN, and Go never reads testdata
			// or a hidden or underscored directory as a package.
			if path != root {
				name := d.Name()
				if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					return filepath.SkipDir
				}

				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}

			return nil
		}

		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		// WHAT THE NIGHTLY RUN BUILDS: a file its build constraints exclude
		// there holds no target that run can search.
		built, err := nightly.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return err
		}

		if !built {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		names, dot := importNames(file, "testing")
		if len(names) == 0 && !dot {
			return nil
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isFuzzName(fn.Name.Name) || fn.Type.TypeParams != nil ||
				fn.Type.Results.NumFields() > 0 || !takesTestingF(fn, names, dot) {
				continue
			}

			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}

			found = append(found, "./"+filepath.ToSlash(rel)+" "+fn.Name.Name)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "fuzz.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var listed []string

	for _, m := range fuzzEntry.FindAllSubmatch(workflow, -1) {
		listed = append(listed, string(m[1])+" "+string(m[2]))
	}

	slices.Sort(found)
	slices.Sort(listed)

	if len(found) == 0 {
		t.Fatal("found no fuzz targets at all, so this test is reading the wrong tree")
	}

	if !slices.Equal(found, listed) {
		t.Errorf("the fuzz targets in the tree and the nightly workflow's matrix differ:\ntree:     %q\nworkflow: %q",
			found, listed)
	}
}

// nightly is the build the nightly fuzz workflow makes: linux/amd64 at the
// default GOAMD64 level, without cgo (billet builds with CGO_ENABLED=0
// everywhere). The release tags stay this toolchain's, which go.mod pins for
// both.
var nightly = func() gobuild.Context {
	ctx := gobuild.Default
	ctx.GOOS, ctx.GOARCH = "linux", "amd64"
	ctx.ToolTags = []string{"amd64.v1"}
	ctx.CgoEnabled = false

	return ctx
}()

// isFuzzName is go test's rule for a fuzz target's name: Fuzz, then nothing or
// anything that does not begin with a lower-case letter.
func isFuzzName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Fuzz")
	if !ok {
		return false
	}

	r, _ := utf8.DecodeRuneInString(rest)

	return rest == "" || !unicode.IsLower(r)
}

// importNames is every name file refers to the package at path by, and
// whether it is also dot-imported, so its names are in the file's own scope.
func importNames(file *ast.File, path string) ([]string, bool) {
	var (
		names []string
		dot   bool
	)

	for _, spec := range file.Imports {
		// A PATH IS A STRING LITERAL, raw or escaped, so it is read as one.
		if imported, err := strconv.Unquote(spec.Path.Value); err != nil || imported != path {
			continue
		}

		switch {
		case spec.Name == nil:
			names = append(names, filepath.Base(path))
		case spec.Name.Name == ".":
			dot = true
		case spec.Name.Name != "_":
			names = append(names, spec.Name.Name)
		}
	}

	return names, dot
}

// takesTestingF reports whether fn's one parameter is a *testing.F: a selector
// on one of the names testing is imported under, or F itself where testing is
// dot-imported.
func takesTestingF(fn *ast.FuncDecl, names []string, dot bool) bool {
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}

	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}

	switch typ := star.X.(type) {
	case *ast.Ident:
		return dot && typ.Name == "F"
	case *ast.SelectorExpr:
		pkg, ok := typ.X.(*ast.Ident)

		return ok && typ.Sel.Name == "F" && slices.Contains(names, pkg.Name)
	default:
		return false
	}
}
