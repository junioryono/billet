package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A real container, launched and destroyed through real docker.
//
// The stub tests assert what billet SAYS; this asserts that what it says works.
// Uses busybox rather than the runner image because the assertion is about the
// launch path, and a 2GB pull would turn a provider test into a network test.
func TestRealDockerLaunchAndDestroy(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}

	p := New("billet-selftest")

	// A unique name per run. Docker refuses to start a container whose name is
	// taken, so a previous failed run would otherwise block every later one —
	// which is exactly what happened while writing this. Runner names are
	// unique per launch in production for the same reason.
	name := fmt.Sprintf("billet-selftest-%d", os.Getpid())

	// Belt and braces: if an earlier run died between launch and cleanup, take
	// the leftover with us rather than failing on it.
	t.Cleanup(func() {
		//nolint:errcheck // best-effort sweep of a leftover from an earlier crash
		_ = exec.CommandContext(context.WithoutCancel(t.Context()),
			"docker", "rm", "-f", name).Run()
	})

	inst, err := p.Launch(t.Context(), provider.Spec{
		Name:      name,
		Image:     "busybox:latest",
		VCPU:      1,
		Memory:    256 * config.MiB,
		Trust:     provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	})
	if err != nil {
		t.Fatalf("Launch against real docker: %v", err)
	}

	t.Cleanup(func() {
		// WithoutCancel: the test context is already done by the time cleanup
		// runs, and a cleanup that cannot run leaves a container behind.
		if err := p.Destroy(context.WithoutCancel(t.Context()), inst.ID); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	// The credential reached the container, and reached it as an env var rather
	// than as part of a command line.
	out, err := exec.CommandContext(t.Context(),
		"docker", "inspect", "--format", "{{.Config.Env}}", inst.ID).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}

	if !strings.Contains(string(out), jitEnvVar) {
		t.Errorf("the container has no %s; the runner would register nothing:\n%s", jitEnvVar, out)
	}

	// And the label is on it, which is what makes orphan cleanup possible.
	label, err := exec.CommandContext(t.Context(), "docker", "inspect", "--format",
		"{{index .Config.Labels \""+ownerLabel+"\"}}", inst.ID).Output()
	if err != nil {
		t.Fatalf("docker inspect labels: %v", err)
	}

	if strings.TrimSpace(string(label)) != "billet-selftest" {
		t.Errorf("owner label is %q; orphans could not be found after a crash", label)
	}

	if err := p.Destroy(t.Context(), inst.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// Gone, and destroying again is still success.
	if err := p.Destroy(t.Context(), inst.ID); err != nil {
		t.Errorf("second Destroy reported an error: %v", err)
	}
}
