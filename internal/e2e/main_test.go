package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestMain pulls the test image once, before any test can be waiting on it.
//
// These tests run for real in CI — ubuntu-latest ships a docker daemon, so they
// do not skip — and the provider pulls implicitly on first launch. That pull
// would otherwise happen inside a test's own deadline, on a runner with no layer
// cache and a cold network, which is a flake that only ever appears on someone
// else's machine.
//
// It also keeps a launch failure legible. A pull is the slowest thing `docker
// run` does and the most likely to fail for reasons billet did not cause;
// separating it means a test that fails after this point failed for a reason
// worth reading.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err == nil {
		if err := pullTestImage(); err != nil {
			fmt.Fprintf(os.Stderr, "billet e2e: could not pull %s: %v\n", testImage, err)
			fmt.Fprintln(os.Stderr, "tests that need a container will fail rather than skip, "+
				"because docker IS installed here and a missing image is a real failure")
		}
	}

	os.Exit(m.Run())
}

func pullTestImage() error {
	// Generous, because this is a network operation on a possibly cold runner,
	// and bounded, because a hang here would stall the whole package with no
	// output at all.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "pull", testImage).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}

	return nil
}
