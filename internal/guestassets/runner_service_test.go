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
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
				t.Fatalf("make runner bin: %v", err)
			}
			listener := filepath.Join(root, "bin", "Runner.Listener")
			if err := os.WriteFile(listener, []byte("#!/bin/sh\nexit \"$BILLET_TEST_RESULT\"\n"), 0o755); err != nil {
				t.Fatalf("write fake listener: %v", err)
			}
			wrapper := filepath.Join(root, "runner-service")
			if err := os.WriteFile(wrapper, source, 0o755); err != nil {
				t.Fatalf("write runner wrapper: %v", err)
			}

			run := exec.CommandContext(t.Context(), wrapper)
			run.Env = append(os.Environ(), "BILLET_TEST_RESULT="+strconv.Itoa(code))
			err := run.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != code {
				t.Fatalf("hosted result %d became %v", code, err)
			}
		})
	}
}

func TestRunnerServiceMatchesTheStockDeprecatedVersionExitContract(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatalf("make runner bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "Runner.Listener"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatalf("write fake listener: %v", err)
	}
	source, err := os.ReadFile("runner-service.sh")
	if err != nil {
		t.Fatalf("read runner wrapper: %v", err)
	}
	wrapper := filepath.Join(root, "runner-service")
	if err := os.WriteFile(wrapper, source, 0o755); err != nil {
		t.Fatalf("write runner wrapper: %v", err)
	}

	if err := exec.CommandContext(t.Context(), wrapper).Run(); err != nil {
		t.Fatalf("status 7 without opt-in became a service failure: %v", err)
	}
	run := exec.CommandContext(t.Context(), wrapper)
	run.Env = append(os.Environ(), "ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1")
	err = run.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("status 7 with opt-in became %v", err)
	}
}
