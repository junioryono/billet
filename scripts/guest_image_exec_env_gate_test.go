package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE PUBLICATION GATE REFUSES AN IMAGE WHOSE AGENT OR HELPER IS NOT THIS TREE'S (#352).
// The agent hands the runner's environment to billet-exec-env on descriptor 3, so
// an image without it starts no runner, and one carrying a different helper can
// launch every job while putting a value in an argument list: only the bytes say
// so. The gate's own block is executed against fixture images.
func TestThePublicationGateRefusesAnAgentOrHelperThatIsNotThisTrees(t *testing.T) {
	t.Parallel()

	gate := readScriptFile(t, "check-guest-image.sh")
	const begin = "# THE LAUNCH READS THE JOB'S ENVIRONMENT FROM DESCRIPTOR 3 (#352)"
	_, after, found := strings.Cut(gate, begin)
	_, after, _ = strings.Cut(after, "\n")
	block, _, closed := strings.Cut(after, "\nfi\n")
	if !found || !closed {
		t.Fatal("check-guest-image.sh no longer has the billet-exec-env block")
	}
	if !strings.Contains(block, `internal/guestassets/exec-env.sh}"`) {
		t.Fatal("the gate does not default EXEC_ENV_SOURCE to internal/guestassets/exec-env.sh")
	}

	source, err := filepath.Abs(filepath.Join("..", "internal", "guestassets", "exec-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// THE INSTALLED AGENT IS THE QUOTED HERE-DOCUMENT'S BODY, byte for byte.
	build := readScriptFile(t, "build-guest-image.sh")
	_, rest, opened := strings.Cut(build, "<<'AGENT'\n")
	agent, _, closed := strings.Cut(rest, "\nAGENT\n")
	if !opened || !closed {
		t.Fatal("build-guest-image.sh no longer installs the agent from a quoted AGENT here-document")
	}
	agent += "\n"
	buildPath, err := filepath.Abs("build-guest-image.sh")
	if err != nil {
		t.Fatal(err)
	}
	// AN AGENT EVERY SUBSTRING CHECK ACCEPTS, whose serialization runs an
	// external printf and so puts every value in that process's argv.
	leaky := strings.Replace(agent, "runner_env_stream=$(printf ", "runner_env_stream=$(/usr/bin/printf ", 1)
	if leaky == agent {
		t.Fatal("the agent no longer serializes the runner's environment with printf")
	}

	for _, tc := range []struct {
		name   string
		agent  string
		helper []byte
		mode   os.FileMode
		pass   bool
	}{
		{name: "this tree's agent and helper", agent: agent, helper: helper, mode: 0o755, pass: true},
		{name: "an agent that serializes with an external printf", agent: leaky, helper: helper, mode: 0o755},
		{name: "a helper with other bytes", agent: agent, helper: append(append([]byte{}, helper...), "\n: other\n"...), mode: 0o755},
		{name: "a helper that is not executable", agent: agent, helper: helper, mode: 0o644},
		{name: "no helper at all", agent: agent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mnt := t.TempDir()
			bin := filepath.Join(mnt, "usr", "local", "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "billet-agent"), []byte(tc.agent), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.helper != nil {
				if err := os.WriteFile(filepath.Join(bin, "billet-exec-env"), tc.helper, tc.mode); err != nil {
					t.Fatal(err)
				}
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				"pass() { echo \"PASS $*\"; }\nfail() { echo \"FAIL $*\"; }\n" +
				"MNT=" + mnt + "\nAGENT=\"$MNT/usr/local/bin/billet-agent\"\n" +
				block + "\nfi\n"
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "EXEC_ENV_SOURCE=" + source,
				"AGENT_SOURCE=" + buildPath}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the gate block did not run: %v\n%s", err, out)
			}
			if got := strings.HasPrefix(string(out), "PASS "); got != tc.pass {
				t.Errorf("the gate said %q, want pass=%v", out, tc.pass)
			}
		})
	}
}
