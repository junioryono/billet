package guestassets

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRunnerServicePreservesHostedJobResultCodes(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("runner-service.sh")
	if err != nil {
		t.Fatalf("read the runner service wrapper: %v", err)
	}
	for _, code := range []int{100, 101, 102, 103, 104, 105} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			linkListener(t, root)

			wrapper := filepath.Join(root, "runner-service")
			if err := os.WriteFile(wrapper, source, 0o755); err != nil {
				t.Fatalf("write runner wrapper: %v", err)
			}

			run := exec.CommandContext(t.Context(), wrapper)
			run.Env = append(os.Environ(), "BILLET_TEST_RESULT="+strconv.Itoa(code))
			err := runRetry(t, run)
			exit, ok := errors.AsType[*exec.ExitError](err)
			if !ok || exit.ExitCode() != code {
				t.Fatalf("hosted result %d became %v", code, err)
			}
		})
	}
}

func TestRunnerServiceMatchesTheStockDeprecatedVersionExitContract(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	linkListener(t, root)

	source, err := os.ReadFile("runner-service.sh")
	if err != nil {
		t.Fatalf("read runner wrapper: %v", err)
	}
	wrapper := filepath.Join(root, "runner-service")
	if err := os.WriteFile(wrapper, source, 0o755); err != nil {
		t.Fatalf("write runner wrapper: %v", err)
	}

	// THE RESULT IS NAMED ON EVERY RUN, never inherited: the shared listener
	// answers what the environment says, and an ambient value would make this
	// case assert a status nothing here chose. The opt-in is named on both
	// runs too, for the same reason.
	without := exec.CommandContext(t.Context(), wrapper)
	without.Env = append(os.Environ(), "BILLET_TEST_RESULT=7",
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=")

	if err := runRetry(t, without); err != nil {
		t.Fatalf("status 7 without opt-in became a service failure: %v", err)
	}

	run := exec.CommandContext(t.Context(), wrapper)
	run.Env = append(os.Environ(), "BILLET_TEST_RESULT=7",
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1")
	err = runRetry(t, run)
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 7 {
		t.Fatalf("status 7 with opt-in became %v", err)
	}
}
