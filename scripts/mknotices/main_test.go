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

// A LICENSE BESIDE COPIED-IN CODE COVERS WHAT IS LINKED FROM BELOW IT, so a module's
// notices are its root's and every one between the root and a linked package; one
// beside code billet does not link is not billet's to reproduce.
func TestALicenseAboveALinkedPackageIsIncludedAndOneBesideUnlinkedCodeIsNot(t *testing.T) {
	dir := t.TempDir()
	for p, body := range map[string]string{
		"LICENSE":                          "root license\n",
		"internal/upstream/LICENSE":        "upstream license\n",
		"internal/upstream/pkg/pkg.go":     "package pkg\n",
		"internal/upstream/pkg/NOTICE":     "package notice\n",
		"internal/unlinked/LICENSE":        "unlinked license\n",
		"internal/unlinked/other/other.go": "package other\n",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var b bytes.Buffer
	w := &writer{b: &b, first: map[string]string{}}
	err := w.modules([]Module{{Path: "example.com/m", Dir: dir, Packages: []string{dir, filepath.Join(dir, "internal", "upstream", "pkg")}}})
	if err != nil {
		t.Fatal(err)
	}

	got := b.String()
	for _, want := range []string{
		"--- LICENSE ---\n\nroot license\n",
		"--- internal/upstream/LICENSE ---\n\nupstream license\n",
		"--- internal/upstream/pkg/NOTICE ---\n\npackage notice\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the notices lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unlinked license") {
		t.Errorf("the notices carry a license beside code billet does not link:\n%s", got)
	}

	if _, err := NestedLicenseFiles(dir, []string{filepath.Dir(dir)}); err == nil ||
		!strings.Contains(err.Error(), "is not under its module root") {
		t.Errorf("a package outside its module root gave %v, want it refused", err)
	}
}

// A module with no root license is reproduced when a nested license covers every
// package billet links from it, and refused, naming the package, when one is not.
func TestAModuleWithOnlyANestedLicenseIsJudgedByWhatItLinks(t *testing.T) {
	dir := t.TempDir()
	for p, body := range map[string]string{
		"covered/LICENSE":    "covered license\n",
		"covered/pkg.go":     "package covered\n",
		"covered/sub/sub.go": "package sub\n",
		"bare/bare.go":       "package bare\n",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	covered := []string{filepath.Join(dir, "covered"), filepath.Join(dir, "covered", "sub")}

	var b bytes.Buffer
	w := &writer{b: &b, first: map[string]string{}}
	if err := w.modules([]Module{{Path: "example.com/m", Dir: dir, Packages: covered}}); err != nil {
		t.Fatalf("a module whose linked packages a nested license covers was refused: %v", err)
	}
	if !strings.Contains(b.String(), "--- covered/LICENSE ---\n\ncovered license\n") {
		t.Fatalf("the nested license was not reproduced:\n%s", b.String())
	}

	bare := filepath.Join(dir, "bare")
	w = &writer{b: &bytes.Buffer{}, first: map[string]string{}}
	err := w.modules([]Module{{Path: "example.com/m", Dir: dir, Packages: append(covered, bare)}})
	if err == nil || !strings.Contains(err.Error(), "has no license file covering "+bare) {
		t.Fatalf("got %v, want the uncovered package %s named", err, bare)
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

func TestALicenseWithCRLFEndingsIsWrittenWithLF(t *testing.T) {
	var b bytes.Buffer
	w := &writer{b: &b, first: map[string]string{}}
	w.section("a", nil)
	w.file("LICENSE", []byte("line one\r\nline two\r\n"))

	if strings.Contains(b.String(), "\r") {
		t.Fatalf("the notices carry a carriage return, which git's normalization would strip: %q", b.String())
	}
	if !strings.Contains(b.String(), "line one\nline two\n") {
		t.Fatalf("the license text was not kept: %q", b.String())
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
