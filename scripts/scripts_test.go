package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryKeyVerifierRequiresTheExactPrimaryKeySet(t *testing.T) {
	t.Parallel()

	pinned := strings.Repeat("A", 40)
	attacker := strings.Repeat("B", 40)
	verifier, err := filepath.Abs(filepath.Join("..", "ansible_collections", "junioryono", "billet", "roles", "development_host", "files", "verify-repository-key.sh"))
	if err != nil {
		t.Fatalf("absolute verifier path: %v", err)
	}
	for _, tc := range []struct {
		name         string
		fingerprints []string
		wantSuccess  bool
	}{
		{name: "one pinned primary", fingerprints: []string{pinned}, wantSuccess: true},
		{name: "pinned plus an extra primary", fingerprints: []string{pinned, attacker}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tools := t.TempDir()
			var output strings.Builder
			output.WriteString("#!/bin/sh\n")
			for _, fingerprint := range tc.fingerprints {
				output.WriteString("printf '%s\\n' 'pub:::::::::' 'fpr:::::::::")
				output.WriteString(fingerprint)
				output.WriteString(":'\n")
			}
			if err := os.WriteFile(filepath.Join(tools, "gpg"), []byte(output.String()), 0o755); err != nil {
				t.Fatalf("write fake gpg: %v", err)
			}
			cmd := exec.CommandContext(t.Context(), verifier, filepath.Join(tools, "bundle.asc"), pinned)
			cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))
			err := cmd.Run()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("verification error = %v; want success %t", err, tc.wantSuccess)
			}
		})
	}
}

func TestProductionSurfacesUseTheTestedSecurityHelpers(t *testing.T) {
	t.Parallel()

	assertContains(t,
		filepath.Join("..", "ansible_collections", "junioryono", "billet", "roles", "development_host", "tasks", "packages-linux.yml"),
		`- "{{ billet_development_apt_stage }}/verify-repository-key.sh"`)
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		"run: scripts/plan-release.sh")
}

func TestReleasePlannerOrdersNewSeriesWithoutBlockingMaintainedHotfixes(t *testing.T) {
	t.Parallel()

	planner, err := filepath.Abs("plan-release.sh")
	if err != nil {
		t.Fatalf("absolute planner path: %v", err)
	}
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.name", "Billet Test")
	runGit(t, repository, "config", "user.email", "billet@example.invalid")
	runGit(t, repository, "commit", "--allow-empty", "--quiet", "-m", "fixture")
	runGit(t, repository, "tag", "v0.4.2")
	runGit(t, repository, "tag", "v0.5.0")

	for _, tc := range []struct {
		name        string
		requested   string
		wantOutput  string
		wantSuccess bool
	}{
		{name: "backward new series", requested: "v0.3.0"},
		{name: "forward new series", requested: "v0.6.0", wantOutput: "tag=v0.6.0\nbranch=release/v0.6\n", wantSuccess: true},
		{name: "maintained older series hotfix", requested: "v0.4.3", wantOutput: "tag=v0.4.3\nbranch=release/v0.4\n", wantSuccess: true},
		{name: "backward older series patch", requested: "v0.4.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output := filepath.Join(t.TempDir(), "github-output")
			cmd := exec.CommandContext(t.Context(), planner)
			cmd.Dir = repository
			cmd.Env = append(os.Environ(), "REQUESTED="+tc.requested, "GITHUB_OUTPUT="+output)
			err := cmd.Run()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("plan error = %v; want success %t", err, tc.wantSuccess)
			}
			if !tc.wantSuccess {
				return
			}
			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatalf("read workflow output: %v", err)
			}
			if string(body) != tc.wantOutput {
				t.Fatalf("workflow output = %q; want %q", body, tc.wantOutput)
			}
		})
	}
}

func TestInstallerCanSelectARemoteTargetPlatform(t *testing.T) {
	t.Parallel()

	installer, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("absolute installer path: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; detect_platform`, "sh", installer)
	cmd.Env = append(os.Environ(),
		"BILLET_INSTALL_SH_TEST=1",
		"BILLET_OS=linux",
		"BILLET_ARCH=amd64",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detect target platform: %v\n%s", err, output)
	}
	if got := string(output); got != "linux_amd64\n" {
		t.Fatalf("target platform = %q; want linux_amd64", got)
	}
}

func TestInstallerRequiresCompleteTargetPlatform(t *testing.T) {
	t.Parallel()

	installer, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("absolute installer path: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; detect_platform`, "sh", installer)
	cmd.Env = append(os.Environ(),
		"BILLET_INSTALL_SH_TEST=1",
		"BILLET_OS=linux",
		"BILLET_ARCH=",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("incomplete target platform succeeded: %s", output)
	}
	if !strings.Contains(string(output), "BILLET_OS and BILLET_ARCH must be set together") {
		t.Fatalf("error = %q; want paired-variable guidance", output)
	}
}

func TestInstallerDoesNotExecuteACrossTargetBinary(t *testing.T) {
	t.Parallel()

	installer, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("absolute installer path: %v", err)
	}
	tools := t.TempDir()
	uname := `#!/bin/sh
case "$1" in
    -s) echo Darwin ;;
    -m) echo arm64 ;;
    *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "uname"), []byte(uname), 0o755); err != nil {
		t.Fatalf("write fake uname: %v", err)
	}
	foreign := filepath.Join(tools, "billet")
	if err := os.WriteFile(foreign, []byte("not executable on the control host\n"), 0o755); err != nil {
		t.Fatalf("write foreign binary: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; report_install "$2" linux_amd64`, "sh", installer, foreign)
	cmd.Env = append(os.Environ(),
		"BILLET_INSTALL_SH_TEST=1",
		"PATH="+tools+":"+os.Getenv("PATH"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report cross-target install: %v\n%s", err, output)
	}
	want := "Installed linux_amd64 billet to " + foreign + "\n"
	if got := string(output); got != want {
		t.Fatalf("report = %q; want %q", got, want)
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), want) {
		t.Fatalf("%s is not wired to tested helper %q", path, want)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = directory
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
