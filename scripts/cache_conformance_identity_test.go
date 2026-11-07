package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// identityStep is the run script of the conformance workflow's candidate-identity
// check, with the image's absolute paths moved to fixtures under root.
func identityStep(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cache-conformance.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "- name: Refuse a non-billet runner or the wrong candidate image\n")
	if !ok {
		t.Fatal("the workflow has no candidate-identity check")
	}
	_, rest, ok = strings.Cut(rest, "        run: |\n")
	if !ok {
		t.Fatal("the check has no run script")
	}
	var lines []string
	for _, line := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(line, "          ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "          "))
	}
	script := strings.Join(lines, "\n")
	for _, path := range []string{"/etc/billet-image", "/usr/local/bin/billet-agent"} {
		if !strings.Contains(script, path) {
			t.Fatalf("the check no longer reads %s", path)
		}
		script = strings.ReplaceAll(script, path, filepath.Join(root, filepath.Base(path)))
	}

	return script
}

// THE CANDIDATE IS AN EC2 IMAGE OR A FIRECRACKER GUEST, AND IS CHECKED AS ONE. An EC2
// image carries its contract in its record and has no agent; a Firecracker guest has
// its agent. Neither passes for lack of the other's file, and the wrong number fails.
// Run, not read: this check is what keeps a conformance run on the image under test.
func TestTheConformanceIdentityCheckKnowsEachImage(t *testing.T) {
	t.Parallel()

	ec2 := t.TempDir()
	writeFixture(t, filepath.Join(ec2, "billet-image"), "RUNNER_VERSION=2.337.0\nAMI_CONTRACT=5\nBUILT_AT=x\n")
	firecracker := t.TempDir()
	writeFixture(t, filepath.Join(firecracker, "billet-image"), "RUNNER_VERSION=2.337.0\nBUILT_AT=x\n")
	writeFixture(t, filepath.Join(firecracker, "billet-agent"), "#!/bin/bash\nWANT_CONTRACT=10\n")

	for _, tc := range []struct {
		name, root, contract string
		pass                 bool
	}{
		{"an EC2 image of the contract named", ec2, "ec2:5", true},
		{"an EC2 image of another contract", ec2, "ec2:4", false},
		{"an EC2 image named as a Firecracker guest", ec2, "5", false},
		{"a Firecracker guest of the contract named", firecracker, "10", true},
		{"a Firecracker guest of another contract", firecracker, "9", false},
		{"a Firecracker guest named as an EC2 image", firecracker, "ec2:10", false},
		{"no contract named", ec2, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "bash", "-c", identityStep(t, tc.root))
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "RUNNER_NAME=billet-runner",
				"EXPECTED_RUNNER_VERSION=2.337.0", "EXPECTED_GUEST_CONTRACT=" + tc.contract,
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("the check answered %v, want pass=%t\n%s", err, tc.pass, output)
			}
		})
	}
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
