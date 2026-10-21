package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// joinFixture is a control plane (writeAdminConfig's: an 8 vCPU, 32GiB
// ceiling) with one registered host contributing all of it.
type joinFixture struct {
	stateDir, configPath string
}

func newJoinFixture(t *testing.T) joinFixture {
	t.Helper()

	stateDir := ledgertest.Dir(t)
	f := joinFixture{stateDir: stateDir, configPath: writeAdminConfig(t, stateDir)}

	// THE IDENTITY A CONTROL PLANE MINTS AT ITS FIRST START, which the
	// template ledger does not carry.
	if _, err := state.DeploymentID(stateDir); err != nil {
		t.Fatalf("mint the deployment id: %v", err)
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "linux-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return f
}

// proposal is a joining node's proposal, deployment named or not.
func (f joinFixture) proposal(t *testing.T, deployment string) string {
	t.Helper()

	doc := "schema: 1\nnode: mac-1\nprovider: docker\nmax_vcpu: 10\nmax_memory: 28GiB\n" +
		"tiers:\n  - label: billet-4vcpu\n    provider: docker\n    vcpu: 4\n    memory: 8GiB\n    image: ubuntu:24.04\n"
	if deployment != "" {
		doc += "deployment: " + deployment + "\n"
	}

	p, err := initconfig.ParseJoinProposal(doc)
	if err != nil {
		t.Fatalf("ParseJoinProposal: %v", err)
	}
	token, err := p.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	return token
}

// deployment is the fixture's own deployment id.
func (f joinFixture) deployment(t *testing.T) string {
	t.Helper()

	id, ok, err := state.PeekDeploymentID(f.stateDir)
	if err != nil || !ok {
		t.Fatalf("the fixture's deployment id (found %v): %v", ok, err)
	}

	return id
}

func runJoin(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Stdin: strings.NewReader(stdin), Getenv: os.Getenv}
	err := Join(t.Context(), env, args)

	return out.String(), err
}

// A JOIN ACCEPTED MERGES THE NODE'S HALF INTO THE CONTROL PLANE'S CONFIG
// (#183): the tier added, the ceiling raised to the registered host's 8 plus
// the node's 10, the file's mode kept, and the restart that takes it named.
func TestJoinAcceptMergesTheNodesHalfIntoTheControlPlane(t *testing.T) {
	f := newJoinFixture(t)
	if err := os.Chmod(f.configPath, 0o640); err != nil {
		t.Fatal(err)
	}

	out, err := runJoin(t, "", "accept", "--config", f.configPath, f.proposal(t, f.deployment(t)))
	if err != nil {
		t.Fatalf("join accept: %v\n%s", err, out)
	}

	cfg, err := config.Load(f.configPath)
	if err != nil {
		t.Fatalf("the accepted config does not load: %v", err)
	}
	if cfg.Server.MaxVCPU != 18 || cfg.Server.MaxMemory != 60*config.GiB {
		t.Errorf("the ceiling is %d and %s, want 18 and 60GiB (the registered host's and the node's)",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}
	found := false
	for i := range cfg.Tiers {
		found = found || cfg.Tiers[i].Label == "billet-4vcpu"
	}
	if !found {
		t.Errorf("the proposed tier was not merged")
	}
	if info, err := os.Stat(f.configPath); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("the config's mode is %v (%v), want it kept at 0640", info.Mode().Perm(), err)
	}
	for _, want := range []string{"add tier      billet-4vcpu", "raise ceiling 8 vCPU, 32GiB -> 18 vCPU, 60GiB",
		serverRestartCommand(), "billet nodes approve mac-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the accept does not say %q:\n%s", want, out)
		}
	}

	// ACCEPTED AGAIN, from a file and from stdin: nothing more to change.
	path := filepath.Join(t.TempDir(), "proposal")
	if err := os.WriteFile(path, []byte(f.proposal(t, f.deployment(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f.configPath)
	for _, arg := range []string{"@" + path, "-"} {
		out, err := runJoin(t, f.proposal(t, f.deployment(t)), "accept", "--config", f.configPath, arg)
		if err != nil || !strings.Contains(out, "nothing to change") {
			t.Errorf("accepting %s again: %v\n%s", arg, err, out)
		}
	}
	if after, _ := os.ReadFile(f.configPath); !bytes.Equal(before, after) {
		t.Errorf("accepting again rewrote the config")
	}
}

// A DRY RUN SAYS WHAT WOULD CHANGE AND WRITES NOTHING; A CONFIG THE INVENTORY
// RENDERS IS NOT EDITED, AND THE SAME HALF IS GIVEN IN THE INVENTORY'S TERMS;
// AND A PROPOSAL FROM ANOTHER DEPLOYMENT IS REFUSED.
func TestJoinAcceptWritesNothingItShouldNot(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		f := newJoinFixture(t)
		before, _ := os.ReadFile(f.configPath)

		out, err := runJoin(t, "", "accept", "--config", f.configPath, "--dry-run", f.proposal(t, f.deployment(t)))
		if err != nil || !strings.Contains(out, "raise ceiling") || !strings.Contains(out, "--dry-run") {
			t.Fatalf("dry run: %v\n%s", err, out)
		}
		if after, _ := os.ReadFile(f.configPath); !bytes.Equal(before, after) {
			t.Errorf("a dry run changed the config")
		}
	})

	t.Run("the inventory's", func(t *testing.T) {
		f := newJoinFixture(t)
		raw, err := os.ReadFile(f.configPath)
		if err != nil {
			t.Fatal(err)
		}
		rendered := initconfig.RoleRenderedMarker + "\n" + string(raw)
		if err := os.WriteFile(f.configPath, []byte(rendered), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := runJoin(t, "", "accept", "--config", f.configPath, f.proposal(t, f.deployment(t)))
		if err == nil || !strings.Contains(err.Error(), "the inventory's") {
			t.Fatalf("a rendered config was edited: %v\n%s", err, out)
		}
		for _, want := range []string{"billet_config.tiers", "label: billet-4vcpu", "max_vcpu to 18"} {
			if !strings.Contains(out, want) {
				t.Errorf("the inventory form does not say %q:\n%s", want, out)
			}
		}
		if after, _ := os.ReadFile(f.configPath); string(after) != rendered {
			t.Errorf("a rendered config was changed")
		}
	})

	t.Run("another deployment", func(t *testing.T) {
		f := newJoinFixture(t)
		before, _ := os.ReadFile(f.configPath)

		other := "fedcba9876543210fedcba9876543210"
		if other == f.deployment(t) {
			t.Fatal("the fixture's deployment is the one this case calls another")
		}
		_, err := runJoin(t, "", "accept", "--config", f.configPath, f.proposal(t, other))
		if err == nil || !strings.Contains(err.Error(), other) {
			t.Fatalf("another deployment's node was accepted: %v", err)
		}
		if after, _ := os.ReadFile(f.configPath); !bytes.Equal(before, after) {
			t.Errorf("a refused accept changed the config")
		}
	})
}
