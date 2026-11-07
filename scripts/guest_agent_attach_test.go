package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// attachAgentRun runs the agent's terminal-server block and the block that names its hook, as the
// image carries them, against a metadata stand-in, a recording systemctl and a hook that exists,
// and answers what systemctl was asked and the runner's environment at the end.
func attachAgentRun(t *testing.T, attach, hold string, startFails bool, runnerEnv ...string) (string, []string) {
	t.Helper()

	dir := t.TempDir()

	calls := filepath.Join(dir, "systemctl.calls")

	status := "0"
	if startFails {
		status = "1"
	}

	stub := "#!/bin/sh\necho \"$*\" >>\"" + calls + "\"\nexit " + status + "\n"
	if err := forkSafeWriteFile(filepath.Join(dir, "systemctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	hook := filepath.Join(dir, "job-started-hook.sh")
	if err := forkSafeWriteFile(hook, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	billet := filepath.Join(dir, "billet")
	if err := forkSafeWriteFile(billet, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var preset strings.Builder

	preset.WriteString("runner_env=()\n")

	for _, entry := range runnerEnv {
		preset.WriteString("runner_env+=('" + entry + "')\n")
	}

	script := `set -euo pipefail
log() { :; }
fetch() {
	case "$1" in
	guest-attach) [ -n "$TEST_ATTACH" ] && printf '%s' "$TEST_ATTACH" ;;
	guest-attach-hold) [ -n "$TEST_HOLD" ] && printf '%s' "$TEST_HOLD" ;;
	*) return 1 ;;
	esac
}
GUEST_BILLET="$TEST_BILLET"
GUEST_ATTACH_HOOK="$TEST_HOOK"
` + preset.String() + agentBlock(t, "BILLET_GUEST_ATTACH_START") + "\n" + agentBlock(t, "BILLET_GUEST_ATTACH_HOOK") + `
printf '%s\n' "${runner_env[@]+"${runner_env[@]}"}"
`

	cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "TEST_ATTACH=" + attach, "TEST_HOLD=" + hold,
		"TEST_BILLET=" + billet, "TEST_HOOK=" + hook}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the agent's blocks failed: %v\n%s", err, out)
	}

	recorded, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	var env []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			env = append(env, strings.ReplaceAll(line, hook, "HOOK"))
		}
	}

	return string(recorded), env
}

// THE TERMINAL SERVER STARTS ONLY WHEN THE NODE ASKS, AS A UNIT OF ITS OWN, and the job's log is
// told only then. The word is the node's: an older node says nothing, and the job goes on.
func TestTheTerminalServerStartsOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	calls, env := attachAgentRun(t, "", "", false)
	if calls != "" || len(env) != 0 {
		t.Errorf("not asked: systemctl %q, environment %q", calls, env)
	}

	calls, env = attachAgentRun(t, "0", "", false)
	if calls != "" || len(env) != 0 {
		t.Errorf("asked for no terminal: systemctl %q, environment %q", calls, env)
	}

	calls, env = attachAgentRun(t, "1", "", false)
	if strings.TrimSpace(calls) != "start --no-block billet-guest-attach.service" {
		t.Errorf("asked: systemctl %q", calls)
	}
	if !slices.Equal(env, []string{"ACTIONS_RUNNER_HOOK_JOB_STARTED=HOOK"}) {
		t.Errorf("asked: environment %q", env)
	}
}

// A SERVER THAT DID NOT START IS A JOB NOBODY CAN ATTACH TO, and the log must not say otherwise. The
// job goes on: set -e is on in these blocks.
func TestATerminalServerThatDidNotStartIsNotOffered(t *testing.T) {
	t.Parallel()

	calls, env := attachAgentRun(t, "1", "5m0s", true)
	if calls == "" {
		t.Error("nothing tried to start it")
	}
	if len(env) != 0 {
		t.Errorf("the job's log would offer a terminal: environment %q", env)
	}
}

// THE HOLD REACHES THE HOOK, and A HOOK ALREADY NAMED STAYS BEHIND IT: the runner runs one, and the
// Actions cache's is what makes the job's trust bundle.
func TestTheHookStandsInFrontOfTheOneNamed(t *testing.T) {
	t.Parallel()

	_, env := attachAgentRun(t, "1", "5m0s", false, "A=1", "ACTIONS_RUNNER_HOOK_JOB_STARTED=/the/cache/hook.sh", "B=2")

	want := []string{"A=1", "BILLET_JOB_STARTED_NEXT=/the/cache/hook.sh", "B=2",
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=HOOK", "BILLET_ATTACH_HOLD=5m0s"}
	if !slices.Equal(env, want) {
		t.Errorf("environment %q, want %q", env, want)
	}
}

// AN IMAGE'S UNIT, HOOK AND CHECK ARE THIS TREE'S: the script installs what the check insists on.
func TestTheImageInstallsTheTerminalServer(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"ExecStart=/opt/billet/bin/billet guest attach-serve",
		"/etc/systemd/system/billet-guest-attach.service",
		"internal/guestassets/job-started-hook.sh",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("build-guest-image.sh does not hold %q", want)
		}
	}

	// NOT ENABLED: a guest whose tier does not say has nothing listening on its vsock.
	if strings.Contains(string(raw), "systemctl enable billet-guest-attach") ||
		strings.Contains(string(raw), "billet-agent billet-guest-attach") {
		t.Error("the terminal server is enabled in every guest")
	}
}
