package scripts_test

import (
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

var fuzzEntry = regexp.MustCompile(`(?m)^\s+- \{ package: (\./\S+), fuzz: (Fuzz\w+) \}$`)

// EVERY FUZZ TARGET IS SEARCHED NIGHTLY, AND NOTHING ELSE IS. The seeds of a
// Fuzz function run with every `go test`, but the search past them runs only
// for the targets .github/workflows/fuzz.yml lists, which is also the list
// `make fuzz` reads, so a target added without a row there is one nobody ever
// fuzzes. Found by parsing every test file of this module, whatever the
// testing.F parameter is called.
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

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		testingName := importName(file, "testing")
		if testingName == "" {
			return nil
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Fuzz") || !takesTestingF(fn, testingName) {
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

// importName is the name file refers to the package at path by, or "" when it
// does not import it (or imports it only for effect, or into its own scope).
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) != path {
			continue
		}

		if spec.Name == nil {
			return filepath.Base(path)
		}

		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return ""
		}

		return spec.Name.Name
	}

	return ""
}

// takesTestingF reports whether fn's one parameter is a *testing.F, with the
// testing package imported as testingName.
func takesTestingF(fn *ast.FuncDecl, testingName string) bool {
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}

	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}

	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "F" {
		return false
	}

	pkg, ok := sel.X.(*ast.Ident)

	return ok && pkg.Name == testingName
}
