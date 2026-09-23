package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

func joinArgs(path string, extra ...string) []string {
	return append([]string{
		"--config", path, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}, extra...)
}

// AN EMPTY --join IS REFUSED BY PRESENCE. `--join "$ADDR"` with an unset
// variable otherwise generated a control plane on the machine meant to join one.
func TestInitRefusesAnEmptyJoin(t *testing.T) {
	ownHome(t)

	for _, addr := range []string{"", "  "} {
		path := filepath.Join(t.TempDir(), "billet.yaml")

		err := cmdInit(t.Context(), joinArgs(path, "--join", addr))
		if err == nil || !strings.Contains(err.Error(), "--join") {
			t.Errorf("--join %q was not refused by name: %v", addr, err)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("a refused --join %q wrote %s (stat: %v)", addr, path, statErr)
		}
	}
}

// A JOIN OVER A SINGLE-MACHINE CONFIG DROPS ITS APP IDENTITY, and a relative
// --config still names the bundle absolutely, which node.tls requires.
func TestInitJoinWritesANodeWithoutTheAppIdentity(t *testing.T) {
	ownHome(t)
	dir := t.TempDir()
	t.Chdir(dir)

	if err := cmdInit(t.Context(), joinArgs("billet.yaml")); err != nil {
		t.Fatalf("first init: %v", err)
	}
	keyPath, err := defaultKeyPath(filepath.Join(dir, "billet.yaml"))
	if err != nil {
		t.Fatalf("resolve the key path: %v", err)
	}
	if err := writeGitHubBlock("billet.yaml", githubBlock{
		Org: "acme", AppID: 7, InstallationID: 42, ClientID: "Iv1.abc",
		PrivateKeyPath: keyPath,
	}); err != nil {
		t.Fatalf("simulate github-app create: %v", err)
	}

	out := capture(t, func() {
		if err := cmdInit(t.Context(), joinArgs("billet.yaml", "--force",
			"--join", "controller.example:7717")); err != nil {
			t.Fatalf("join: %v", err)
		}
	})

	cfg, err := config.Load("billet.yaml")
	if err != nil {
		t.Fatalf("the joined config does not load: %v", err)
	}
	if cfg.Server != nil || cfg.GitHub != nil {
		t.Errorf("the joined config still carries a control plane: server %v, github %v",
			cfg.Server != nil, cfg.GitHub != nil)
	}
	// The working directory filepath.Abs resolves against, which on macOS is
	// the /private spelling of the temporary directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if want := filepath.Join(wd, "tls", "node.crt"); cfg.Node == nil || cfg.Node.TLS == nil ||
		cfg.Node.TLS.CertPath != want {
		t.Errorf("node.tls does not name %s: %+v", want, cfg.Node)
	}
	for _, want := range []string{"On the control plane", "Raise server.max_vcpu", "billet node --config"} {
		if !strings.Contains(out, want) {
			t.Errorf("the join's guidance does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "create the App") {
		t.Errorf("a joined node was told to create an App:\n%s", out)
	}
}

// A JOIN NEVER REPLACES A LIVE CONTROL PLANE. The node-only file has no
// server, so replacing a config whose state holds a deployment identity
// orphans every container and lease under it, even when the generation names
// the same state directory.
func TestInitJoinRefusesReplacingALiveControlPlane(t *testing.T) {
	ownHome(t)
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), joinArgs(path)); err != nil {
		t.Fatalf("first init: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stateDir, ok := initconfig.ExistingServerStateDir(before)
	if !ok || stateDir == "" {
		t.Fatalf("the generated config names no server state directory")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "deployment-id"),
		[]byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("mint identity: %v", err)
	}

	joinErr := cmdInit(t.Context(), joinArgs(path, "--force", "--join", "controller.example:7717"))
	if joinErr == nil {
		t.Fatal("--join --force replaced a live control plane's config")
	}
	if !strings.Contains(joinErr.Error(), "decommission") {
		t.Errorf("the refusal does not name the retirement: %v", joinErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Errorf("the refused join changed %s (err %v)", path, err)
	}
}
