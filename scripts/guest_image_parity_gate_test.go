package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// TestTheParityGateNamesWhatIsMissing drives the gate's comparison directly.
//
// THE GATE ITSELF NEEDS ROOT AND A MOUNTED IMAGE, which is why none of it has
// unit tests. This comparison is the part most able to be silently wrong, and
// every case below is a way a package check has actually been wrong before:
// a substring match reporting a package present that is not, an empty install
// list read as "everything is fine", and a declaration that parsed to nothing
// making the whole gate vacuous.
func TestTheParityGateNamesWhatIsMissing(t *testing.T) {
	t.Parallel()

	realToolset, err := filepath.Abs(filepath.Join("..", "internal", "runnerimages",
		"toolset-2404.json"))
	if err != nil {
		t.Fatalf("resolve the toolset: %v", err)
	}

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	everything := strings.Join(ts.AptPackages(), "\n")

	// THE LITERAL-MATCH CASE BELOW PROVES NOTHING WITHOUT ONE OF THESE. It relies
	// on the declaration containing a package whose name is a regex metacharacter
	// sequence -- today that is `g++`, and it is the only one. If upstream drops it
	// the case silently becomes a duplicate of "everything is installed", so the
	// absence is reported here rather than passing quietly.
	if !slices.ContainsFunc(ts.AptPackages(), func(p string) bool {
		return strings.ContainsAny(p, `+*?.[]()|^$`)
	}) {
		t.Error("no declared package name carries a regex metacharacter, so the " +
			"literal-match case below no longer distinguishes grep -F from grep -E")
	}

	for _, tc := range []struct {
		name      string
		toolset   string
		installed string
		wantExit  int
		wantOut   []string
		denyOut   []string
	}{
		{
			name:      "everything declared is installed",
			toolset:   realToolset,
			installed: everything,
			wantExit:  0,
			wantOut:   []string{strconv.Itoa(len(ts.AptPackages()))},
		},
		{
			name:      "a missing package is named",
			toolset:   realToolset,
			installed: strings.ReplaceAll(everything, "shellcheck", "somethingelse"),
			wantExit:  1,
			wantOut:   []string{"shellcheck"},
		},
		{
			name:      "several missing packages are all named",
			toolset:   realToolset,
			installed: "curl\ntar",
			wantExit:  1,
			wantOut:   []string{"shellcheck", "openssh-client", "tzdata"},
			// THE ONES THAT ARE PRESENT MUST NOT BE REPORTED, or an operator
			// reading the list cannot tell what to fix.
			denyOut: []string{" curl", " tar"},
		},
		{
			// A SUBSTRING MATCH WOULD PASS THIS. "ssh" is a line in the installed
			// list only as part of "sshpass", and a grep without -x reports it
			// present -- so the gate would say an absent package is there.
			name:      "a package is not satisfied by a longer name containing it",
			toolset:   realToolset,
			installed: strings.ReplaceAll(everything, "\nssh\n", "\n"),
			wantExit:  1,
			wantOut:   []string{"ssh"},
		},
		{
			// A PATTERN MATCH WOULD FAIL THIS, AND ONLY FOR ONE PACKAGE.
			//
			// `g++` is the only name in the whole declaration carrying a regex
			// metacharacter, and as an ERE it means "one or more g, one or more
			// times" -- which does not match the literal string `g++`. So a grep
			// that lost its -F reports exactly one package missing, from an
			// installed list that contains it, and every other package still
			// passes.
			//
			// THAT IS THE SYMPTOM THAT WAS SEEN: a CI run of this test failed naming
			// g++ and nothing else, and would not reproduce. Whether a dropped -F
			// explains it is not established -- the flag is present in the source
			// at that commit -- but the class was uncovered either way, and the
			// existing case above covers -x while nothing covered -F.
			name:      "a package whose name is a regex is matched literally",
			toolset:   realToolset,
			installed: everything,
			wantExit:  0,
			wantOut:   []string{strconv.Itoa(len(ts.AptPackages()))},
		},
		{
			name:      "nothing installed reports everything",
			toolset:   realToolset,
			installed: "",
			wantExit:  1,
			wantOut:   []string{"curl", "tar", "shellcheck"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A CASE WITHOUT A DECLARATION EXERCISES A DIFFERENT FUNCTION.
			//
			// An empty path makes jq read nothing, which the gate answers with its
			// "the declaration parsed to nothing" refusal -- exit 2, no output, and
			// nothing to do with the comparison the case is named for. An edit
			// dropped this field from one case and it failed as `exit 2, want 1`,
			// which reads as a broken gate rather than a broken fixture.
			if tc.toolset == "" {
				t.Fatal("this case names no declaration, so it would exercise the " +
					"empty-declaration refusal rather than the comparison it describes")
			}

			out, code := runParityCheck(t, tc.toolset, tc.installed)

			if code != tc.wantExit {
				t.Fatalf("exit %d, want %d\n%s", code, tc.wantExit, out)
			}

			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("the result does not mention %q:\n%s", want, out)
				}
			}

			for _, deny := range tc.denyOut {
				if strings.Contains(out, deny) {
					t.Errorf("the result reports %q as missing when it is installed:\n%s",
						deny, out)
				}
			}
		})
	}
}

// TestAnEmptyDeclarationIsNotAPass is the vacuous-gate case, and it gets its own
// exit code because it is a different failure from a missing package.
//
// A TOOLSET THAT PARSED TO NOTHING would make the comparison find nothing absent
// and report success over an image containing no packages at all. That is the
// exact shape of check this project has twice found passing against the bug it
// was written for.
func TestAnEmptyDeclarationIsNotAPass(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, body string
	}{
		{"empty apt lists", `{"apt":{"vital_packages":[],"common_packages":[],"cmd_packages":[]}}`},
		{"no apt key at all", `{"toolcache":[]}`},
		{"not json", `this is not a toolset`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "toolset.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write the toolset: %v", err)
			}

			// A FULL INSTALL LIST, so the only reason to fail is the declaration.
			out, code := runParityCheck(t, path, "curl\ntar\nshellcheck")

			if code != 2 {
				t.Errorf("exit %d, want 2. A declaration that parses to nothing must be its "+
					"own failure: reporting success here would pass an image carrying no "+
					"packages at all\n%s", code, out)
			}
		})
	}
}

// TestTheAliasedPackageIsCheckedUnderTheNameThatIsInstalled keeps a mapping from
// quietly becoming an exemption.
//
// `netcat` is a pure virtual package on noble with two providers, so apt refuses
// it and the build installs netcat-openbsd instead. If this gate went on looking
// for `netcat` it would report a missing package on EVERY correct image — and the
// obvious fix for that noise is to delete the check, which is how a mapping
// quietly becomes an exemption.
//
// BOTH DIRECTIONS. The installed name must satisfy the declaration, and the
// declared name must NOT, because nothing installs a package called `netcat`.
func TestTheAliasedPackageIsCheckedUnderTheNameThatIsInstalled(t *testing.T) {
	t.Parallel()

	realToolset, err := filepath.Abs(filepath.Join("..", "internal", "runnerimages",
		"toolset-2404.json"))
	if err != nil {
		t.Fatalf("resolve the toolset: %v", err)
	}

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// AptPackages() ALREADY APPLIES THE ALIAS, so it is what the build installs.
	out, code := runParityCheck(t, realToolset, strings.Join(ts.AptPackages(), "\n"))
	if code != 0 {
		t.Errorf("an image carrying the packages the build installs was reported incomplete "+
			"(exit %d): %s", code, out)
	}

	if !slices.Contains(ts.AptPackages(), "netcat-openbsd") {
		t.Fatal("AptPackages does not apply the netcat alias, so this test's positive case " +
			"is not exercising the mapping at all")
	}

	// AND THE RAW DECLARED NAMES ARE NOT ENOUGH. Built from the groups rather than
	// from AptPackages(), which would apply the alias and make this case identical
	// to the one above — a negative assertion satisfied by the positive fixture.
	var unmapped []string

	for _, group := range [][]string{
		ts.Apt.VitalPackages, ts.Apt.CommonPackages, ts.Apt.CmdPackages,
	} {
		unmapped = append(unmapped, group...)
	}

	out, code = runParityCheck(t, realToolset, strings.Join(unmapped, "\n"))
	if code == 0 {
		t.Error("an image with a literal `netcat` and no netcat-openbsd was accepted; " +
			"nothing installs a package called netcat on noble, so the gate is checking a " +
			"name that can never be present")
	}

	if !strings.Contains(out, "netcat-openbsd") {
		t.Errorf("the gate does not name netcat-openbsd as what is missing: %s", out)
	}
}

func runParityCheck(t *testing.T, toolset, installed string) (string, int) {
	t.Helper()

	aliases, err := filepath.Abs(filepath.Join("..", "internal", "runnerimages",
		"apt-aliases.json"))
	if err != nil {
		t.Fatalf("resolve the aliases: %v", err)
	}

	script := "#!/usr/bin/env bash\nset -uo pipefail\nAPT_ALIASES=" + aliases + "\n" +
		checkImageFunction(t, "declared_packages_missing_from") + "\n" +
		"declared_packages_missing_from \"$1\" \"$2\"\nexit $?\n"

	path := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "bash", path, toolset, installed).CombinedOutput()

	if err == nil {
		return string(out), 0
	}

	// THE EXIT CODE IS THE ANSWER, so an error that is not an exit is a broken
	// harness rather than a failing check, and must not be reported as one.
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("running the parity check: %v\n%s", err, out)
	}

	return string(out), exit.ExitCode()
}

func checkImageFunction(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile("check-guest-image.sh")
	if err != nil {
		t.Fatalf("read check-guest-image.sh: %v", err)
	}

	source := string(raw)

	start := strings.Index(source, name+"() {")
	if start < 0 {
		t.Fatalf("check-guest-image.sh has no %s function", name)
	}

	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s", name)
	}

	return source[start : start+end+2]
}
