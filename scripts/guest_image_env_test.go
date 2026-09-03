package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheImageEnvironmentReachesTheJob executes the agent's own env-reading block
// and inspects the array a job would actually be launched with.
//
// GREPPING THE AGENT PROVES ONLY THAT THE TEXT IS PRESENT, which dead code
// satisfies -- the exact weakness a review found in the checks beside this one.
// The job's environment is built with `env -i`, so what matters is whether a
// variable lands in that array, and the only way to know is to run the code and
// look.
func TestTheImageEnvironmentReachesTheJob(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		file    string
		absent  bool
		wantEnv []string
		denyEnv []string
	}{
		{
			name:    "declared variables are passed through",
			file:    "ImageOS=ubuntu24\nImageVersion=billet\n",
			wantEnv: []string{"ImageOS=ubuntu24", "ImageVersion=billet"},
		},
		{
			// AN IMAGE THAT LOST THE FILE MUST STILL RUN JOBS. They would download
			// their own toolchains, which is slow; refusing to launch is worse.
			name:    "a missing file does not stop the launch",
			absent:  true,
			denyEnv: []string{"ImageOS="},
		},
		{
			name:    "a malformed line is skipped rather than fatal",
			file:    "ImageOS=ubuntu24\nthis is not an assignment\n=novalue\nImageVersion=billet\n",
			wantEnv: []string{"ImageOS=ubuntu24", "ImageVersion=billet"},
			denyEnv: []string{"this is not an assignment", "=novalue"},
		},
		{
			// THE RUNNER'S OWN VARIABLES MUST SURVIVE. A block that assigned to the
			// array instead of appending would drop every one of them, and the job
			// would start with no HOME, no PATH and no registration.
			name:    "the runner's own environment is not replaced",
			file:    "ImageOS=ubuntu24\n",
			wantEnv: []string{"HOME=/home/runner", "RUNNER_TOOL_CACHE=/opt/hostedtoolcache"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			envFile := filepath.Join(dir, "billet-image-env")

			if !tc.absent {
				if err := os.WriteFile(envFile, []byte(tc.file), 0o600); err != nil {
					t.Fatalf("write the env file: %v", err)
				}
			}

			// THE PRELUDE MIRRORS WHAT THE AGENT HAS ALREADY BUILT by the time the
			// block runs: an array holding the runner's own environment. The block
			// under test must ADD to it.
			script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				"runner_env=(\"HOME=/home/runner\" \"RUNNER_TOOL_CACHE=/opt/hostedtoolcache\")\n" +
				"IMAGE_ENV_FILE=" + envFile + "\n" +
				agentEnvBlock(t) + "\n" +
				"printf '%s\\n' \"${runner_env[@]}\"\n"

			path := filepath.Join(dir, "run.sh")
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatalf("write the harness: %v", err)
			}

			out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput()
			if err != nil {
				t.Fatalf("the agent's env block failed: %v\n%s", err, out)
			}

			// COMPARED AS WHOLE LINES. Each element of the array is printed on its
			// own line, and a substring match accepts a LONGER assignment that
			// merely contains the expected text -- "ImageOS=ubuntu24-modified"
			// satisfies a Contains check for "ImageOS=ubuntu24" while being a
			// different value.
			got := map[string]bool{}
			for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
				got[line] = true
			}

			for _, want := range tc.wantEnv {
				if !got[want] {
					t.Errorf("the job would not see %q. The environment is built with env -i, "+
						"so a variable absent from this array does not exist for the "+
						"job:\n%s", want, out)
				}
			}

			for _, deny := range tc.denyEnv {
				for line := range got {
					if strings.Contains(line, deny) {
						t.Errorf("the job would see %q, which is not a valid assignment:\n%s",
							line, out)
					}
				}
			}
		})
	}
}

// TestTheAgentDefaultsToTheRealPath: the seam that makes the block testable must
// not become a block that only works in tests.
func TestTheAgentDefaultsToTheRealPath(t *testing.T) {
	t.Parallel()

	if !strings.Contains(agentEnvBlock(t), `"${IMAGE_ENV_FILE:-/etc/billet-image-env}"`) {
		t.Error("the agent does not default IMAGE_ENV_FILE to /etc/billet-image-env, so the " +
			"variables reach a job only when something sets the path -- which nothing " +
			"does in a real guest")
	}
}

// agentEnvBlock extracts the env-reading block out of the agent that
// build-guest-image.sh bakes into the image.
//
// FROM THE BUILD SCRIPT, NOT A COPY. The agent lives in a quoted heredoc inside
// that script, and a test carrying its own copy of these lines would keep passing
// after the real one changed.
func agentEnvBlock(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatalf("read build-guest-image.sh: %v", err)
	}

	source := string(raw)

	const anchor = `IMAGE_ENV_FILE="${IMAGE_ENV_FILE:-/etc/billet-image-env}"`

	start := strings.Index(source, anchor)
	if start < 0 {
		t.Fatal("the agent no longer reads /etc/billet-image-env; the variables a hosted " +
			"runner exports would reach no job")
	}

	end := strings.Index(source[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("could not find the end of the agent's env block")
	}

	return source[start : start+end+4]
}
