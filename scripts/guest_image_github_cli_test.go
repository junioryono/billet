package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE GUEST CARRIES THE GITHUB CLI, AND THE GATE REFUSES ONE THAT DOES NOT.
//
// GitHub's image carries `gh` and its toolset declaration does not name it, so
// the declared-package check passes an image without it. A fleet image without it
// failed a workflow's first `gh` call with 127 (2026-09-22), on a job that runs
// only after a merge, so no pull request's run could have found it.
func TestTheGateRefusesAnImageWithoutTheGitHubCLI(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		gh         os.FileMode // 0 for no file at all
		wantFailed string
	}{
		{"installed", 0o755, "0"},
		{"absent", 0, "1"},
		{"present but not executable", 0o644, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
				t.Fatal(err)
			}

			if tc.gh != 0 {
				if err := os.WriteFile(filepath.Join(root, "usr", "bin", "gh"), []byte("#!/bin/sh\n"), tc.gh); err != nil {
					t.Fatal(err)
				}
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\n" +
				"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
				checkImageFunction(t, "check_github_cli") + "\n" +
				"check_github_cli \"$1\"\necho \"FAILED=$FAILED\"\n"

			path := filepath.Join(t.TempDir(), "gate.sh")
			if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			out, err := exec.CommandContext(t.Context(), "bash", path, root).CombinedOutput()
			if err != nil {
				t.Fatalf("the check did not run to its verdict: %v\n%s", err, out)
			}

			if !strings.Contains(string(out), "FAILED="+tc.wantFailed+"\n") {
				t.Errorf("want FAILED=%s:\n%s", tc.wantFailed, out)
			}
		})
	}
}

// AND THE GATE ASKS IT: a check that is defined and never called passes every
// image, which is the vacuous shape this directory keeps finding.
func TestTheGateAsksForTheGitHubCLI(t *testing.T) {
	t.Parallel()

	if !hasExactLine(readScriptFile(t, "check-guest-image.sh"), `check_github_cli "$MNT"`) {
		t.Error(`check-guest-image.sh never runs check_github_cli "$MNT", so an image without gh passes`)
	}
}

// AND THE BUILD INSTALLS IT, in its own package list, as a whole word: a
// substring test would be satisfied by "github".
func TestTheGuestBuildInstallsTheGitHubCLI(t *testing.T) {
	t.Parallel()

	source := readScriptFile(t, "build-guest-image.sh")

	start := strings.Index(source, "local billet_packages=(")
	if start < 0 {
		t.Fatal("build-guest-image.sh has no billet_packages list, so this test is reading the wrong script")
	}

	end := strings.Index(source[start:], "\n\t)")
	if end < 0 {
		t.Fatal("the billet_packages list is not closed where this test expects")
	}

	var packages []string

	// A `#` STARTS A COMMENT WHEREVER IT IS, and bash hands nothing after it to
	// apt: `python3-apt # gh` installs no gh. No package name contains one.
	for line := range strings.SplitSeq(source[start+len("local billet_packages=("):start+end], "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}

		packages = append(packages, strings.Fields(line)...)
	}

	if !slices.Contains(packages, "gh") {
		t.Errorf("the guest build installs %v, without gh; GitHub's image carries the GitHub CLI and its "+
			"declaration does not name it, so nothing else puts it on this one", packages)
	}
}
