package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/provenance"
)

// fakeAgents records what the launchd host asked of launchd, in order.
type fakeAgents struct {
	asked     []string
	agentsDir string
	// unproved makes every stop come back not proved gone.
	unproved bool
}

func (f *fakeAgents) Services() (string, string) {
	return deploy.ServerAgentLabel, deploy.NodeAgentLabel
}

func (f *fakeAgents) AgentPath(label string) string {
	return filepath.Join(f.agentsDir, label+".plist")
}

func (f *fakeAgents) StopAndProve(_ context.Context, label string) (lifeops.StopResult, error) {
	f.asked = append(f.asked, "stop "+label)

	if f.unproved {
		return lifeops.StopResult{Gone: lifeops.Unknown, How: "is still draining"}, nil
	}

	return lifeops.StopResult{Gone: lifeops.Yes, How: "is out of its domain"}, nil
}

func (f *fakeAgents) StartAndProve(_ context.Context, label string) (string, error) {
	f.asked = append(f.asked, "start "+label)

	return "the same pid survived", nil
}

func (f *fakeAgents) ProveStable(_ context.Context, label string) error {
	f.asked = append(f.asked, "prove "+label)

	return nil
}

func macHost(t *testing.T, cfg *config.Config) (*launchdHost, *fakeAgents) {
	t.Helper()

	agents := &fakeAgents{agentsDir: t.TempDir()}

	h := &launchdHost{
		ledgerHost: newLedgerHost(cfg, "/usr/local/etc/billet/billet.yaml", nil),
		staged:     filepath.Join(t.TempDir(), "billet"),
		agents:     agents,
		tartImages: func(context.Context) error { return errors.New("tart was reached") },
	}

	return h, agents
}

// THE NODE IS BOOTED OUT BEFORE THE SERVER AND BOOTSTRAPPED AFTER IT, on the
// labels billet ships, and only the labels the config wants. The same order the
// systemd host keeps, for the same reason: compute drains while the control
// plane is still there to record it, and registers against a control plane that
// is already up.
func TestTheLaunchdHostStopsAndStartsTheAgentsInOrder(t *testing.T) {
	t.Parallel()

	both := &config.Config{Server: &config.ServerConfig{}, Node: &config.NodeConfig{}}

	h, agents := macHost(t, both)

	for _, step := range []func(context.Context) error{
		h.StopNode, h.StopServer, h.StartServices,
	} {
		if err := step(t.Context()); err != nil {
			t.Fatalf("step: %v", err)
		}
	}

	want := []string{
		"stop " + deploy.NodeAgentLabel,
		"stop " + deploy.ServerAgentLabel,
		"start " + deploy.ServerAgentLabel,
		"start " + deploy.NodeAgentLabel,
	}

	if !slices.Equal(agents.asked, want) {
		t.Errorf("launchd was asked\n  %v\nwant\n  %v", agents.asked, want)
	}

	// A NODE-ONLY MAC NEVER TOUCHES THE SERVER AGENT, and fences and snapshots
	// no ledger, because it has none.
	nodeOnly := &config.Config{Node: &config.NodeConfig{}}

	h, agents = macHost(t, nodeOnly)

	for _, step := range []func(context.Context) error{
		h.StopNode, h.StopServer, h.StartServices,
		func(ctx context.Context) error { return h.Fence(ctx, "test") },
		func(ctx context.Context) error { return h.SnapshotLedger(ctx, "/nowhere") },
		func(ctx context.Context) error { return h.RestoreLedger(ctx, "/nowhere") },
	} {
		if err := step(t.Context()); err != nil {
			t.Fatalf("node-only step: %v", err)
		}
	}

	if want := []string{"stop " + deploy.NodeAgentLabel, "start " + deploy.NodeAgentLabel}; !slices.Equal(
		agents.asked, want) {
		t.Errorf("a node-only Mac asked launchd\n  %v\nwant\n  %v", agents.asked, want)
	}
}

// A STOP THAT IS NOT PROVED IS NOT A STOP. A bootout returns while the process is
// still draining; installing over a node whose guests may still be running is
// the failure the transaction exists to refuse.
func TestTheLaunchdHostRefusesAnUnprovedStop(t *testing.T) {
	t.Parallel()

	h, agents := macHost(t, &config.Config{Node: &config.NodeConfig{}})
	agents.unproved = true

	err := h.StopNode(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not proved stopped") {
		t.Fatalf("an unproved stop was accepted: %v", err)
	}
}

// WHAT A ROLLBACK PUTS BACK ON A MAC: the binary the agents run, both plists
// from the directory launchd scans, the config and the provenance record. One
// list for both halves, read from the converger's own idea of where an agent
// lives rather than from $HOME.
func TestTheLaunchdHostPreservesTheMacsPaths(t *testing.T) {
	t.Parallel()

	h, agents := macHost(t, &config.Config{Server: &config.ServerConfig{}, Node: &config.NodeConfig{}})

	got := h.preservedPaths()

	want := []string{
		installedBinary,
		filepath.Join(agents.agentsDir, deploy.ServerAgentLabel+".plist"),
		filepath.Join(agents.agentsDir, deploy.NodeAgentLabel+".plist"),
		"/usr/local/etc/billet/billet.yaml",
		provenance.Path,
	}

	if !slices.Equal(got, want) {
		t.Errorf("the launchd host preserves\n  %v\nwant\n  %v", got, want)
	}
}

// A TART NODE PULLS WHAT IS ABSENT; ANY OTHER MAC PULLS NOTHING.
func TestTheLaunchdHostRefreshesImagesOnlyForTart(t *testing.T) {
	t.Parallel()

	tart := &config.Config{Node: &config.NodeConfig{Provider: config.ProviderTart}}

	h, _ := macHost(t, tart)
	if err := h.RefreshGuestImages(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "tart was reached") {
		t.Errorf("a tart node did not reach the pull: %v", err)
	}

	docker := &config.Config{Node: &config.NodeConfig{Provider: config.ProviderDocker}}

	h, _ = macHost(t, docker)
	if err := h.RefreshGuestImages(t.Context()); err != nil {
		t.Errorf("a docker node reached the tart pull: %v", err)
	}
}

// THE PLATFORM SWITCH PICKS THE MAC'S HOST ON darwin AND REFUSES ANYTHING ELSE
// BEFORE A CLAIM. A dispatching node then reports the refusal, and the rollout
// backs off rather than cordoning a machine nothing could move.
func TestNewHostForPicksTheServiceManagerByPlatform(t *testing.T) {
	restore := hostOS
	t.Cleanup(func() { hostOS = restore })

	cfg := &config.Config{Node: &config.NodeConfig{}}
	journal := &hostupgrade.Journal{ToVersion: "v0.5.0"}

	hostOS = "darwin"

	if h, err := newHostFor(cfg, "", "/staged", journal); err != nil {
		t.Fatalf("darwin: %v", err)
	} else if _, ok := h.(*launchdHost); !ok {
		t.Fatalf("darwin built %T, want the launchd host", h)
	}

	hostOS = "linux"

	if h, err := newHostFor(cfg, "", "/staged", journal); err != nil {
		t.Fatalf("linux: %v", err)
	} else if _, ok := h.(*systemdHost); !ok {
		t.Fatalf("linux built %T, want the systemd host", h)
	}

	hostOS = "freebsd"

	if _, err := newHostFor(cfg, "", "/staged", journal); err == nil {
		t.Fatal("a platform with no host implementation was accepted")
	}
}
