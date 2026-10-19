package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite THIRD_PARTY_NOTICES from the generator")

const root = "../.."

// THE COMMITTED FILE IS WHAT THE GENERATOR WRITES, so a module added to the build,
// or a license file changed in a copy, cannot reach a release without its notice.
func TestTheNoticesFileIsWhatTheGeneratorWrites(t *testing.T) {
	want, err := Generate(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(root, Output)
	if *update {
		if err := os.WriteFile(file, want, 0o600); err != nil {
			t.Fatal(err)
		}

		return
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		gotLines, wantLines := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		line := 0
		for line < len(gotLines) && line < len(wantLines) && gotLines[line] == wantLines[line] {
			line++
		}
		t.Fatalf("%s is not what scripts/mknotices writes (first difference at line %d); "+
			"run `go run ./scripts/mknotices` and commit the result", Output, line+1)
	}
}

// EVERY LICENSE FILE IN THE TREE BELONGS TO A LISTED COPY. A copy is vendored with
// its license beside it (CLAUDE.md), so a license file nobody listed is a copy the
// notices leave out.
func TestEveryLicenseFileInTheTreeIsListed(t *testing.T) {
	copies, err := ReadCopies(root)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, c := range copies {
		for _, f := range c.LicenseFiles {
			listed[f] = true
		}
	}

	// UNTRACKED FILES COUNT. A copy being vendored has not been committed yet, and
	// a check that read only the index would pass the change that forgot its entry.
	out, err := exec.CommandContext(t.Context(), "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	found := 0
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f == "LICENSE" || !IsLicenseName(path.Base(f)) {
			continue
		}
		found++
		if !listed[f] {
			t.Errorf("%s is a license file no entry of scripts/mknotices/copies.json lists", f)
		}
	}
	if found == 0 {
		t.Fatal("found no license file in the tree, so this test proves nothing")
	}
}

// Every path an entry names exists, so a copy that moved cannot keep a notice that
// points at nothing.
func TestEveryPathTheCopiesTableNamesExists(t *testing.T) {
	copies, err := ReadCopies(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		paths := append(append([]string{}, c.LicenseFiles...), c.Copies...)
		if c.NoticeIn != "" {
			paths = append(paths, c.NoticeIn)
		}
		for _, p := range paths {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				t.Errorf("%s names %s: %v", c.Name, p, err)
			}
		}
	}
}

func TestAModuleWithNoLicenseFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "license.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := &writer{b: &bytes.Buffer{}, first: map[string]string{}}
	err := w.modules([]Module{{Path: "example.com/unlicensed", Dir: dir}})
	if err == nil || !strings.Contains(err.Error(), "example.com/unlicensed, linked into billet, has no license file") {
		t.Fatalf("got %v, want the module refused for having no license file", err)
	}
}

func TestAnIdenticalTextIsPrintedOnceAndADifferentOneWhole(t *testing.T) {
	var b bytes.Buffer
	w := &writer{b: &b, first: map[string]string{}}
	w.section("a", nil)
	w.file("LICENSE", []byte("identical body\n"))
	w.section("b", nil)
	w.file("LICENSE", []byte("identical body\n\n"))
	w.section("c", nil)
	w.file("LICENSE", []byte("Copyright c\n"))

	got := b.String()
	if strings.Count(got, "identical body") != 1 {
		t.Errorf("the identical text was printed %d times, want once:\n%s", strings.Count(got, "identical body"), got)
	}
	if !strings.Contains(got, "--- LICENSE: the same text as LICENSE under a above ---") {
		t.Errorf("the second copy does not point at the first:\n%s", got)
	}
	if !strings.Contains(got, "Copyright c") {
		t.Errorf("a different text was folded:\n%s", got)
	}
}

func TestLicenseNames(t *testing.T) {
	for name, want := range map[string]bool{
		"LICENSE":                true,
		"LICENSE.md":             true,
		"license":                true,
		"LICENSE-3RD-PARTY.md":   true,
		"COPYING":                true,
		"NOTICE.txt":             true,
		"COPYRIGHT.txt":          true,
		"PATENTS":                true,
		"container-hook.LICENSE": true,
		"license.go":             false,
		"license_test.go":        false,
		"README.md":              false,
	} {
		if got := IsLicenseName(name); got != want {
			t.Errorf("IsLicenseName(%q) = %v, want %v", name, got, want)
		}
	}
}
