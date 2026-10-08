package scripts_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	fuzzFunc  = regexp.MustCompile(`(?m)^func (Fuzz\w+)\(f \*testing\.F\) \{$`)
	fuzzEntry = regexp.MustCompile(`(?m)^\s+- \{ package: (\./\S+), fuzz: (Fuzz\w+) \}$`)
)

// EVERY FUZZ TARGET IS SEARCHED NIGHTLY, AND NOTHING ELSE IS. The seeds of a
// Fuzz function run with every `go test`, but the search past them runs only
// for the targets .github/workflows/fuzz.yml lists, so a target added without a
// row there is one nobody ever fuzzes. And each lives in a file named
// fuzz_test.go, which is where `make fuzz` looks for them.
func TestEveryFuzzTargetIsSearchedNightly(t *testing.T) {
	t.Parallel()

	root := ".."

	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			switch d.Name() {
			case ".git", "tools", "node_modules", "_venv", "testdata":
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, m := range fuzzFunc.FindAllSubmatch(data, -1) {
			if filepath.Base(path) != "fuzz_test.go" {
				t.Errorf("%s declares %s outside a fuzz_test.go, where `make fuzz` does not look", path, m[1])
			}

			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}

			found = append(found, "./"+filepath.ToSlash(rel)+" "+string(m[1]))
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
