package scripts_test

import (
	"encoding/json"
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

// AND THE BUILD INSTALLS IT: GitHub's own install-github-cli.sh is a step of the
// plan the guest build runs, and nothing billet carries skips it.
func TestTheGuestBuildInstallsTheGitHubCLI(t *testing.T) {
	t.Parallel()

	planInstalls(t, "install-github-cli.sh")
}

// planInstalls holds that GitHub's plan runs the named installer and that no
// entry in differences.tsv skips it, so the guest image carries what it installs.
func planInstalls(t *testing.T, installer string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(runnerImagesDir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}

	var plan []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}

	if !slices.ContainsFunc(plan, func(step struct {
		ID string `json:"id"`
	}) bool {
		return step.ID == installer
	}) {
		t.Fatalf("GitHub's plan has no %s step, so the guest image does not install what it does", installer)
	}

	differences, err := os.ReadFile(filepath.Join(runnerImagesDir, "differences.tsv"))
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.Lines(string(differences)) {
		if fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t"); len(fields) == 3 &&
			fields[0] == "skip" && fields[1] == installer {
			t.Errorf("differences.tsv skips %s, so the guest image lacks what it installs", installer)
		}
	}
}
