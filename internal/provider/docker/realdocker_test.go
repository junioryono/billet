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

// Find and List against real docker, because both rest on assumptions about
// how the CLI behaves that no stub can check.
//
// The one that matters: `--filter name=X` is a SUBSTRING match, not an exact
// one. Measured, not assumed — a lookup for `billet-abc` really does return
// `billet-abcdef` too, which is why Find compares names exactly afterwards. Get
// that wrong and reconciliation destroys a container belonging to another lease
// whose id happens to start with the same characters.
func TestRealDockerFindAndList(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}

	owner := fmt.Sprintf("billet-findtest-%d", os.Getpid())
	p := New(owner)

	// Two names where one is a prefix of the other. This is the whole point.
	short := fmt.Sprintf("billet-%dabc", os.Getpid())
	long := short + "def"

	for _, name := range []string{short, long} {
		t.Cleanup(func() {
			//nolint:errcheck // best-effort
			_ = exec.CommandContext(context.WithoutCancel(t.Context()),
				"docker", "rm", "-f", name).Run()
		})

		if _, err := p.Launch(t.Context(), provider.Spec{
			Name:      name,
			Image:     "busybox:latest",
			VCPU:      1,
			Memory:    256 * config.MiB,
			Trust:     provider.TrustTrusted,
			JITConfig: "not-a-real-registration",
		}); err != nil {
			t.Fatalf("Launch %s: %v", name, err)
		}
	}

	inst, found, err := p.Find(t.Context(), short)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if !found {
		t.Fatal("Find missed a container that is running")
	}

	if inst.Name != short {
		t.Errorf("Find(%q) returned %q — the substring filter was not narrowed to an exact match",
			short, inst.Name)
	}

	// A name nothing carries. Must be a clean miss rather than an error or a
	// near-match: the caller's next move on a hit is to destroy.
	if _, found, err := p.Find(t.Context(), short+"-nonexistent"); err != nil {
		t.Fatalf("Find for an absent name: %v", err)
	} else if found {
		t.Error("Find claimed to have found a container that does not exist")
	}

	// A DECOY, carrying a DIFFERENT owner label. Without one this assertion
	// passes on an otherwise-empty daemon even if the owner filter is deleted
	// entirely — which is the difference between testing the filter and testing
	// that the developer had no other containers running.
	decoy := fmt.Sprintf("billet-decoy-%d", os.Getpid())
	other := New(owner + "-someone-else")

	t.Cleanup(func() {
		//nolint:errcheck // best-effort
		_ = exec.CommandContext(context.WithoutCancel(t.Context()),
			"docker", "rm", "-f", decoy).Run()
	})

	if _, err := other.Launch(t.Context(), provider.Spec{
		Name:      decoy,
		Image:     "busybox:latest",
		VCPU:      1,
		Memory:    256 * config.MiB,
		Trust:     provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}); err != nil {
		t.Fatalf("Launch the decoy: %v", err)
	}

	// Find must not see it either, even though its name starts with billet-.
	if _, found, err := p.Find(t.Context(), decoy); err != nil {
		t.Fatalf("Find the decoy: %v", err)
	} else if found {
		t.Error("Find returned a container belonging to another billet deployment")
	}

	// List sees both of ours, and only ours.
	all, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 2 {
		t.Fatalf("List returned %d containers, want the 2 this deployment started "+
			"(a third exists under a different owner label): %v", len(all), all)
	}

	for _, got := range all {
		if got.Name != short && got.Name != long {
			t.Errorf("List returned %q, which this test did not start", got.Name)
		}

		if got.ID == "" {
			t.Errorf("List returned %q with no id, so nothing can be destroyed by it", got.Name)
		}
	}
}

// A stopped container still holds its name and its disk, and still blocks a
// relaunch under that name — so reconciliation has to be able to see it. `docker
// ps` without --all does not, which is the mistake this guards.
func TestRealDockerListSeesStoppedContainers(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}

	owner := fmt.Sprintf("billet-stoptest-%d", os.Getpid())
	p := New(owner)
	name := fmt.Sprintf("billet-stopped-%d", os.Getpid())

	t.Cleanup(func() {
		//nolint:errcheck // best-effort
		_ = exec.CommandContext(context.WithoutCancel(t.Context()),
			"docker", "rm", "-f", name).Run()
	})

	if _, err := p.Launch(t.Context(), provider.Spec{
		Name:      name,
		Image:     "busybox:latest",
		VCPU:      1,
		Memory:    256 * config.MiB,
		Trust:     provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := exec.CommandContext(t.Context(), "docker", "stop", "-t", "0", name).Run(); err != nil {
		t.Fatalf("stop the container: %v", err)
	}

	all, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 1 {
		t.Fatalf("List returned %d containers, want the 1 stopped one: %v", len(all), all)
	}
}
