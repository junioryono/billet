package scripts_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// THE COMPILER SECTIONS REACH APT, AND BOTH READERS AGREE ON WHAT THEY ARE.
//
// `clang`, `gcc`, `gfortran`, `php` and `postgresql` are declared outside the
// three apt lists, so nothing installed them and nothing checked for them. Two
// readers expand them — Go's AptPackages for the EC2 script, and
// check-guest-image.sh's jq for the guest gate — and a section added to only one
// is either shipped unchecked or checked and never shipped. The guest image
// itself is GitHub's build, which installs them from the same declaration.
func TestTheToolchainSectionsReachEveryReader(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := ts.ToolchainPackages()
	if len(want) == 0 {
		t.Fatal("the declaration expands to no toolchain packages, so this proves nothing")
	}

	// THE NAMES ARE MEASURED, NOT INVENTED. Each of these resolves in ubuntu-24.04
	// main+universe on both architectures; the three shapes the declaration uses
	// are why they cannot be derived by one rule.
	for _, name := range []string{
		"clang-18", "g++-13", "gfortran-13", "php8.3-cli", "postgresql-client-16",
	} {
		if !slices.Contains(want, name) {
			t.Errorf("the expansion does not produce %q, which apt resolves and a job "+
				"expects\ngot: %v", name, want)
		}
	}

	// AND NOT A NAME BUILT BY THE WRONG RULE. gcc and gfortran are already complete
	// package names in the declaration, so prefixing them the way clang's bare
	// majors are prefixed yields something apt has never heard of.
	for _, wrong := range []string{"g++-g++-13", "gfortran-gfortran-13", "clang-clang-18"} {
		if slices.Contains(want, wrong) {
			t.Errorf("the expansion produced %q, which is one shape's rule applied to "+
				"another's declaration", wrong)
		}
	}

	// EVERY READER, RUN. Go's list is the one EC2 interpolates; the gate's is a jq
	// program in shell, and asking Go about it would only prove Go agrees with
	// itself.
	inGo := ts.AptPackages()

	for _, pkg := range want {
		if !slices.Contains(inGo, pkg) {
			t.Errorf("%q is not in the list the EC2 build installs", pkg)
		}
	}

	// AND THE GATE REQUIRES THEM. If the guest gate did not expand these, an image
	// could ship without a compiler and pass its own check.
	missing := gateDeclared(t)

	for _, pkg := range want {
		if pkg == pipxPackage {
			if slices.Contains(missing, pkg) {
				t.Errorf("the guest gate requires the %s package, which GitHub's image installs "+
					"with pip rather than apt", pkg)
			}

			continue
		}

		if !slices.Contains(missing, pkg) {
			t.Errorf("%q is not in the set the guest gate requires; an image without it "+
				"would pass", pkg)
		}
	}
}

// gateDeclared is the set the guest gate requires, read by asking it about an
// empty image and taking the list it reports missing.
func gateDeclared(t *testing.T) []string {
	t.Helper()

	toolset, err := filepath.Abs(toolsetPathForTest)
	if err != nil {
		t.Fatalf("resolve the toolset: %v", err)
	}

	// THE STATUS IS CHECKED EVEN THOUGH THE LIST IS WHAT IS WANTED. An empty image
	// is missing every declared package, so the gate must report exactly that (1).
	// A 2 is the gate failing to look at all, and its partial output would be
	// harvested here as though it were the declared set -- turning a broken gate
	// into a shorter expectation that everything downstream then satisfies.
	out, code := runParityCheck(t, toolset, "")
	if code != 1 {
		t.Fatalf("asking the gate about an empty image exited %d, want 1; the list it "+
			"printed cannot be trusted as the declared set\n%s", code, out)
	}

	return strings.Fields(out)
}

// pipxPackage is the one name the EC2 build installs from apt and the guest gate
// does not require as a package: GitHub's install-python.sh installs pipx with
// pip, so the gate checks that the command runs instead.
const pipxPackage = "pipx"

// gateRequiredPackages is the declaration's package list as the guest gate
// requires it: everything the EC2 build installs, less pipx.
func gateRequiredPackages(ts runnerimages.Toolset) []string {
	return slices.DeleteFunc(ts.AptPackages(), func(pkg string) bool {
		return pkg == pipxPackage
	})
}
