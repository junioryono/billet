package app

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func poolBootConfig() *config.Config {
	return &config.Config{
		GitHub: &config.GitHubConfig{Org: "acme"},
		Server: &config.ServerConfig{Pool: &config.PoolConfig{
			JoinServer: "10.77.0.10:7717", BootstrapAddr: "10.77.0.10:7718",
		}},
	}
}

// AN ORDERED HOST'S BOOT IS THE PRELUDE, THEN THE JOIN, with its order's own token,
// the deployment's authority and a node name drawn from the order; every argument is
// quoted, so a token is one word to the shell whatever it holds.
func TestAPoolHostsBootRunsThePreludeThenJoins(t *testing.T) {
	t.Parallel()
	boot, err := PoolBoot(poolBootConfig(), "apt-get install -y billet\n", "SHA256:ab", "p01", "tok'en $(x)")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	s := string(boot)
	prelude := strings.Index(s, "apt-get install -y billet")
	join := strings.Index(s, "'billet' 'join'")
	if !strings.HasPrefix(s, "#!/bin/bash\n") || prelude < 0 || join < prelude {
		t.Fatalf("boot =\n%s", s)
	}
	if !strings.Contains(s, "set -euo pipefail") {
		t.Fatalf("the boot does not stop on a failed prelude:\n%s", s)
	}
	// THE SHELL'S OWN READING, not a pattern: the join line, run through bash's word
	// splitting, is exactly these arguments, the token one word and unexpanded.
	line := strings.TrimSpace(s[join:])
	out, err := exec.CommandContext(t.Context(), "bash", "-c",
		`set -- `+line+`; for a; do printf '%s\n' "$a"; done`).Output()
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	want := []string{"billet", "join", "--server", "10.77.0.10:7717", "--ca-fingerprint", "SHA256:ab",
		"--token", "tok'en $(x)", "--node-name", "pool-p01", "--provider", "firecracker",
		"--bootstrap-addr", "10.77.0.10:7718", "--org", "acme"}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("join reads as %q, want %q", got, want)
	}
}

func TestAPoolBootPastTheUserDataLimitIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := PoolBoot(poolBootConfig(), strings.Repeat("x", maxBootPayload), "fp", "p01", "tok"); err == nil ||
		!strings.Contains(err.Error(), "boot_prelude") {
		t.Fatalf("an oversized boot answered %v", err)
	}
}

// PHOENIXNAP'S CREDENTIALS ARE READ FROM THE ENVIRONMENT, and an empty one stops the
// assembly naming the variables, rather than a pool that fails every order.
func TestAPoolWithAnEmptyCredentialVariableIsRefused(t *testing.T) {
	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	cfg := poolBootConfig()
	cfg.Server.Pool.BootPrelude = prelude
	cfg.Server.Pool.PhoenixNAP = &config.PoolPhoenixNAP{
		ClientIDEnv: "BILLET_TEST_POOL_ID", ClientSecretEnv: "BILLET_TEST_POOL_SECRET",
		Shapes: []config.PoolShape{{Type: "s2.c2.small", Arch: config.ArchX64}},
	}
	t.Setenv("BILLET_TEST_POOL_ID", "client")
	t.Setenv("BILLET_TEST_POOL_SECRET", "")
	if _, _, err := NewPool(cfg, nil, "dep", nil); err == nil || !strings.Contains(err.Error(), "BILLET_TEST_POOL_SECRET") {
		t.Fatalf("an empty secret variable answered %v", err)
	}
}
