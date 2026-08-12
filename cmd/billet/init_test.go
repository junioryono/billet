package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// WHAT `billet init` WRITES HAS TO RUN, and that is the whole claim it makes.
//
// Copying the example did not: it describes the intended Firecracker deployment,
// so the provider had to be changed, every tier's image replaced with something
// pullable, and the state directories pointed somewhere writable — four edits
// before anything started, each silent when wrong. A generated config that also
// needs editing is the same trap with an extra step.
//
// The App ids are the one thing it cannot know, because the App does not exist
// yet. Everything else must be true of THIS machine.
func TestInitWritesAConfigThatLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{"--config", path, "--org", "acme"}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Filled in the way `github-app create --config` fills them, so what is under
	// test is everything init decided rather than the placeholder it leaves.
	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: 1, InstallationID: 2,
		PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
	}); err != nil {
		t.Fatalf("filling in the app: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the config billet generated does not load: %v", err)
	}

	if cfg.Server == nil || cfg.Node == nil {
		t.Fatal("a generated config describes both roles; this machine runs both")
	}

	// SIZED TO THIS MACHINE, and below it. A ceiling at or above what the host
	// has does not fail, it overcommits — so the failure would be a busy machine
	// rather than an error.
	vcpu, memory, err := config.DetectHostCapacity()
	if err != nil {
		t.Skipf("cannot detect this host's capacity: %v", err)
	}

	if cfg.Server.MaxVCPU > vcpu || cfg.Server.MaxMemory > memory {
		t.Errorf("the ceiling is %d vCPU / %s on a machine with %d vCPU / %s",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory, vcpu, memory)
	}

	if cfg.Server.MaxVCPU <= 0 || cfg.Server.MaxMemory <= 0 {
		t.Errorf("the ceiling is %d vCPU / %s, so nothing can ever be escrowed",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	// PULLABLE. The image is handed straight to the backend, so a golden-image
	// name is not a config that runs — it is one where every job fails to launch.
	for i := range cfg.Tiers {
		if !strings.Contains(cfg.Tiers[i].Image, "/") {
			t.Errorf("tier %q has image %q, which is not a pullable reference",
				cfg.Tiers[i].Label, cfg.Tiers[i].Image)
		}
	}

	// The node takes its name from the hostname here, because a single machine
	// has no certificate for the two processes to authenticate with.
	if cfg.Node.Name == "" {
		t.Error("the node has no name and no certificate to take one from")
	}
}

// ON EVERY SIZE OF MACHINE, NOT JUST THIS ONE.
//
// The test above generates for whatever host runs it, so a laptop with plenty of
// cores proves nothing about the small machines billet is most likely to be tried
// on first. The ceiling is detected capacity minus headroom while the tiers are
// billet's own choice, so a generated config is only valid where the tiers happen
// to fit under the ceiling — and "happen to" is not a property, it is a coin toss
// decided by the reviewer's hardware.
func TestInitWritesAConfigThatLoadsOnAnySizeOfMachine(t *testing.T) {
	hosts := []struct {
		vcpu   int
		memory config.ByteSize
	}{
		{1, 2 * config.GiB},     // the smallest thing that can boot
		{2, 4 * config.GiB},     // a small cloud VM
		{4, 16 * config.GiB},    // a GitHub-hosted runner
		{8, 16 * config.GiB},    // cores without the memory to match
		{2, 64 * config.GiB},    // memory without the cores
		{16, 64 * config.GiB},   // a workstation
		{128, 512 * config.GiB}, // the reference host
	}

	for _, h := range hosts {
		t.Run(fmt.Sprintf("%dvcpu-%s", h.vcpu, h.memory), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "billet.yaml")

			body := generatedConfig("acme", config.ProviderDocker, defaultRunnerImage, h.vcpu, h.memory)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			if err := writeGitHubBlock(path, githubBlock{
				Org: "acme", AppID: 1, InstallationID: 2,
				PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
			}); err != nil {
				t.Fatalf("filling in the app: %v", err)
			}

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("a %d vCPU / %s machine gets a config that does not load: %v\n\n%s",
					h.vcpu, h.memory, err, body)
			}

			// A config that loads but advertises nothing is the same failure wearing
			// a different error: the operator starts billet, GitHub queues the job,
			// and nothing ever picks it up.
			if len(cfg.Tiers) == 0 {
				t.Fatal("the generated config has no tiers, so no job can ever be scheduled")
			}

			for i := range cfg.Tiers {
				tier := &cfg.Tiers[i]
				if tier.VCPU > cfg.Server.MaxVCPU || tier.Memory > cfg.Server.MaxMemory {
					t.Errorf("tier %q is %d vCPU / %s under a ceiling of %d vCPU / %s, so it can never be placed",
						tier.Label, tier.VCPU, tier.Memory, cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
				}
			}
		})
	}
}

// IT DOES NOT REPLACE A CONFIG WITHOUT BEING ASKED. The file carries the state
// directory and the App key path, so overwriting one silently is how a working
// deployment loses the only record of where its data is.
func TestInitWillNotClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := os.WriteFile(path, []byte("server:\n  listen: 0.0.0.0:1\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := cmdInit(t.Context(), []string{"--config", path})
	if err == nil {
		t.Fatal("init replaced an existing config")
	}

	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal does not say how to proceed: %v", err)
	}

	kept, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}

	if !strings.Contains(string(kept), "0.0.0.0:1") {
		t.Error("the existing config was modified by a run that refused to proceed")
	}
}

// THE APP BLOCK IS WRITTEN INTO THE FILE, and the operator's own file survives
// it. Printing a block to paste was a step to get wrong, and getting it wrong is
// quiet: an app_id left at 0 surfaces only when `billet check` runs.
func TestWritingTheAppBlockKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	body := `# a comment the operator wrote
server:
  listen: 127.0.0.1:7717
  state_dir: /tmp/billet-server
  max_vcpu: 8
  max_memory: 32GiB

github:
  org: acme
  app_id: 0
  installation_id: 0
  private_key_path: /tmp/key.pem
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: 42, InstallationID: 99, ClientID: "Iv1.abc",
		PrivateKeyPath: "/etc/billet/key.pem",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	for _, want := range []string{"app_id: 42", "installation_id: 99", "client_id: Iv1.abc",
		"private_key_path: /etc/billet/key.pem"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the config does not contain %q after the write:\n%s", want, got)
		}
	}

	// Everything the operator wrote is still theirs.
	for _, keep := range []string{"# a comment the operator wrote", "max_vcpu: 8", "32GiB"} {
		if !strings.Contains(string(got), keep) {
			t.Errorf("writing the app block lost %q:\n%s", keep, got)
		}
	}
}
