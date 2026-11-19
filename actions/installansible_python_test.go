package actions_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakePython writes an interpreter named name that reports version and whose venv's
// ansible either answers --version (works) or fails inside its import, as the
// pinned ansible-core did under Python 3.15 (#792). Each venv it makes is recorded.
func fakePython(t *testing.T, bin, name, version string, works bool, calls string) {
	t.Helper()

	ansibleExit := "0"
	if !works {
		ansibleExit = "1"
	}

	body := fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = -c ]; then echo %[1]s; exit 0; fi
if [ "${1:-}" = -m ] && [ "${2:-}" = venv ]; then
  printf 'venv %[1]s\n' >>%[3]q
  mkdir -p "$3/bin"
  printf '#!/bin/sh\nexit 0\n' >"$3/bin/python"
  printf '#!/bin/sh\necho "ansible [core 2.21.2] python %[1]s"\nexit %[2]s\n' >"$3/bin/ansible"
  printf '#!/bin/sh\nexit 0\n' >"$3/bin/ansible-galaxy"
  chmod +x "$3/bin/python" "$3/bin/ansible" "$3/bin/ansible-galaxy"
  exit 0
fi
exit 2
`, version, ansibleExit, calls)

	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runInstallAnsible runs the installer with bin as the whole of PATH, beside the few
// tools it uses, so no real interpreter on the machine running the test is reached.
func runInstallAnsible(t *testing.T, bin string) (string, string, error) {
	t.Helper()

	tools := filepath.Join(t.TempDir(), "tools")
	if err := os.Mkdir(tools, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tool := range []string{"tr", "head", "mkdir", "rm", "chmod"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s on PATH", tool)
		}
		if err := os.Symlink(path, filepath.Join(tools, tool)); err != nil {
			t.Fatal(err)
		}
	}

	runnerTemp := t.TempDir()
	ghPath := filepath.Join(runnerTemp, "github-path")

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "install-ansible.sh"))
	cmd.Env = []string{
		"PATH=" + bin + ":" + tools,
		"RUNNER_TEMP=" + runnerTemp,
		"GITHUB_PATH=" + ghPath,
		"GITHUB_ACTION_PATH=" + actionDir(t),
	}
	out, err := cmd.CombinedOutput()

	written, readErr := os.ReadFile(ghPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}

	return string(out), string(written), err
}

// A DEFAULT PYTHON THE PIN CANNOT RUN UNDER IS PASSED OVER for a named one it can,
// and an interpreter the default already was is not tried twice.
func TestInstallAnsiblePassesOverAPythonThePinCannotRunUnder(t *testing.T) {
	t.Parallel()

	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	fakePython(t, bin, "python3", "3.15", false, calls)
	fakePython(t, bin, "python3.14", "3.15", false, calls)
	fakePython(t, bin, "python3.13", "3.13", true, calls)

	out, ghPath, err := runInstallAnsible(t, bin)
	if err != nil {
		t.Fatalf("the installer failed with a supported python3.13 present: %v\n%s", err, out)
	}

	if !strings.Contains(out, "under python3.13 (3.13)") {
		t.Errorf("the installer did not say it chose python3.13:\n%s", out)
	}

	if got := string(readRecorded(t, calls)); got != "venv 3.15\nvenv 3.13\n" {
		t.Errorf("the venvs made were %q, want one for 3.15 and then one for 3.13", got)
	}

	if !strings.HasSuffix(ghPath, filepath.Join("billet-ansible", "bin")+"\n") {
		t.Errorf("the venv's bin was not put on GITHUB_PATH: %q", ghPath)
	}
}

// NONE THE PIN RUNS UNDER IS A REFUSAL NAMING WHAT WAS TRIED, and nothing goes on
// GITHUB_PATH for a later step to run.
func TestInstallAnsibleRefusesWhenNoPythonRunsThePin(t *testing.T) {
	t.Parallel()

	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	fakePython(t, bin, "python3", "3.15", false, calls)
	fakePython(t, bin, "python3.12", "3.12", false, calls)

	out, ghPath, err := runInstallAnsible(t, bin)
	if err == nil {
		t.Fatalf("the installer succeeded with no Python the pin runs under:\n%s", out)
	}

	if !strings.Contains(out, "tried: python3 (3.15) python3.12 (3.12)") {
		t.Errorf("the refusal does not name what was tried:\n%s", out)
	}

	if ghPath != "" {
		t.Errorf("a refused install still wrote GITHUB_PATH: %q", ghPath)
	}
}
