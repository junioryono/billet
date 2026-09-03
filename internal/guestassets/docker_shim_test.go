package guestassets_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shimHarness stands the shim in front of a fake docker client on PATH, exactly
// as the guest image does with the real one, and returns a runner for it. The
// fake prints its arguments and the two variables the shim may set, so a test
// can see what the client process would have been handed.
func shimHarness(t *testing.T) func(env []string, args ...string) (string, error) {
	t.Helper()

	shim, err := filepath.Abs("docker-shim.sh")
	if err != nil {
		t.Fatal(err)
	}

	front := filepath.Join(t.TempDir(), "front")
	behind := filepath.Join(t.TempDir(), "behind")
	for _, dir := range []string{front, behind} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A copy rather than a symlink into the tree, because the shim resolves its
	// own directory through symlinks to find the client behind it and the test
	// must not depend on where the checkout lives.
	body, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(front, "docker"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nprintf 'argv=%s\\n' \"$*\"\n" +
		"printf 'results=%s\\n' \"${ACTIONS_RESULTS_URL:-unset}\"\n" +
		"printf 'v2=%s\\n' \"${ACTIONS_CACHE_SERVICE_V2:-unset}\"\n"
	if err := os.WriteFile(filepath.Join(behind, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	return func(env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), filepath.Join(front, "docker"), args...)
		cmd.Env = append([]string{"PATH=" + front + ":" + behind + ":/usr/bin:/bin"}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

// A build is pointed at the adapter with the version that makes buildx read the
// URL at all; the runner's own value for the results URL is what would otherwise
// send a container builder to a certificate it cannot verify.
func TestTheDockerShimPointsABuildAtTheAdapter(t *testing.T) {
	t.Parallel()
	run := shimHarness(t)
	env := []string{
		"BILLET_ACTIONS_CACHE_URL=http://172.17.0.1:41321/",
		"ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/",
	}

	for _, args := range [][]string{
		{"build", "-t", "x", "."},
		{"buildx", "build", "--cache-to", "type=gha", "."},
		{"buildx", "bake"},
		{"bake", "app"},
		{"compose", "build"},
		{"compose", "up", "--build", "-d"},
	} {
		out, err := run(env, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if !strings.Contains(out, "results=http://172.17.0.1:41321/\n") ||
			!strings.Contains(out, "v2=true\n") ||
			!strings.Contains(out, "argv="+strings.Join(args, " ")+"\n") {
			t.Errorf("%v: the client was not pointed at the adapter with its arguments intact:\n%s", args, out)
		}
	}
}

// ANYTHING THAT IS NOT A BUILD IS UNTOUCHED. The results origin also carries
// artifacts and logs, which the adapter does not serve, and `docker run -e
// ACTIONS_RESULTS_URL` forwards this process's value into a container.
func TestTheDockerShimLeavesEverythingElseAlone(t *testing.T) {
	t.Parallel()
	run := shimHarness(t)
	env := []string{
		"BILLET_ACTIONS_CACHE_URL=http://172.17.0.1:41321/",
		"ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/",
	}

	for _, args := range [][]string{
		{"run", "--rm", "-e", "ACTIONS_RESULTS_URL", "alpine"},
		{"compose", "up", "-d"},
		{"ps"},
		{},
	} {
		out, err := run(env, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if !strings.Contains(out, "results=https://results-receiver.actions.githubusercontent.com/\n") ||
			!strings.Contains(out, "v2=unset\n") {
			t.Errorf("%v: a non-build invocation was rewritten:\n%s", args, out)
		}
	}
}

// WITHOUT THE ADAPTER THERE IS NOTHING TO POINT AT, and a build is passed
// through exactly as it arrived: on a tier without interception the real
// results URL is the right one.
func TestTheDockerShimDoesNothingWhenTheAdapterIsNotServing(t *testing.T) {
	t.Parallel()
	run := shimHarness(t)

	out, err := run([]string{"ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/"},
		"buildx", "build", ".")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "results=https://results-receiver.actions.githubusercontent.com/\n") ||
		!strings.Contains(out, "v2=unset\n") {
		t.Errorf("a build was rewritten with no adapter URL published:\n%s", out)
	}
}

// A PATH WITH NO CLIENT BEHIND THE SHIM IS A LOUD FAILURE, not a shim that
// exec's itself until the process table fills.
func TestTheDockerShimRefusesToExecItself(t *testing.T) {
	t.Parallel()

	shim, err := filepath.Abs("docker-shim.sh")
	if err != nil {
		t.Fatal(err)
	}
	front := t.TempDir()
	body, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(front, "docker"), body, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), filepath.Join(front, "docker"), "ps")
	cmd.Env = []string{"PATH=" + front + ":" + front + ":/nonexistent"}
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 127 || !strings.Contains(string(out), "no docker client on PATH") {
		t.Fatalf("want exit 127 naming the missing client, got err=%v\n%s", err, out)
	}
}
