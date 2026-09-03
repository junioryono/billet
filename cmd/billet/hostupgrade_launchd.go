package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/lifeops/launchd"
	"github.com/junioryono/billet/internal/provenance"
)

// agentManager is what the launchd host asks of launchd.
//
// AN INTERFACE SO THE HOST IS TESTABLE, for the reason hostupgrade.Host is one:
// every method here boots an agent out or in on a real Mac, and what a test can
// assert is which labels were asked, in which order, and which files were kept.
type agentManager interface {
	Services() (string, string)
	AgentPath(label string) string
	StopAndProve(ctx context.Context, label string) (lifeops.StopResult, error)
	StartAndProve(ctx context.Context, label string) (string, error)
	ProveStable(ctx context.Context, label string) error
}

// launchdHost is the Mac.
//
// THE SAME TRANSACTION, THE SAME ORDER, A DIFFERENT VOCABULARY. Both agents run
// in the operator's GUI session as the operator, so this runs as the operator
// too — started by the node agent it is about to boot out, and outliving it the
// way a `tart run` guest outlives the node: in its own session. What differs
// from Linux is only what a stop and a start are (a bootout with every pid
// waited for; a bootstrap with the same pid proved to survive a settle window),
// which files a rollback puts back (the two plists in ~/Library/LaunchAgents
// instead of two units), and where the binary lives.
type launchdHost struct {
	ledgerHost

	staged string
	agents agentManager
	// tartImages is what PrepareImages runs on a tart node: a pull of
	// every configured image that is absent. A seam so the test does not reach
	// for tart.
	tartImages func(ctx context.Context) error
}

func newLaunchdHost(cfg *config.Config, cfgPath, staged string,
	journal *hostupgrade.Journal,
) *launchdHost {
	return &launchdHost{
		ledgerHost: newLedgerHost(cfg, cfgPath, journal),
		staged:     staged,
		agents:     launchd.New(),
		tartImages: func(ctx context.Context) error { return pullTartImages(ctx, cfg, "") },
	}
}

// StopNode boots the node agent out and waits for its process, which drains.
func (h *launchdHost) StopNode(ctx context.Context) error {
	_, node := h.agents.Services()

	return h.stop(ctx, node)
}

// StopServer boots the server agent out, after the node's custody has settled.
func (h *launchdHost) StopServer(ctx context.Context) error {
	server, _ := h.agents.Services()

	return h.stop(ctx, server)
}

func (h *launchdHost) stop(ctx context.Context, label string) error {
	if !h.wanted(label) {
		return nil
	}

	result, err := h.agents.StopAndProve(ctx, label)
	if err != nil {
		return fmt.Errorf("stopping %s: %w", label, err)
	}

	// PROVED GONE, OR NOT STOPPED. A bootout returns while the process is still
	// draining and StopAndProve waits it out; anything short of a proved absence
	// is a node that may still hold guests, and installing over it is the
	// failure this whole transaction exists to refuse.
	if result.Gone != lifeops.Yes {
		return fmt.Errorf("stopping %s: it %s, which is not proved stopped", label, result.How)
	}

	fmt.Printf("  stopped %s (%s)\n", label, result.How)

	return nil
}

// PrepareImages pulls every configured tart image that is absent.
//
// PULL IF ABSENT, NOTHING MORE, for the reason `images refresh` gives on a Mac:
// a tart tier names an OCI image by tag, and re-pulling one on every upgrade is
// a decision a person makes. A candidate whose tiers name an image the store
// does not hold is a node that refuses every job on that tier; this is what
// stops that being discovered after the swap.
func (h *launchdHost) PrepareImages(ctx context.Context) error {
	if h.cfg.Node == nil || h.cfg.Node.Provider != config.ProviderTart {
		return nil
	}

	return h.tartImages(ctx)
}

// PreserveCurrent copies what a rollback puts back into the recovery directory.
func (h *launchdHost) PreserveCurrent(_ context.Context, dir string) error {
	preserved := filepath.Join(dir, "preserved")
	if err := os.MkdirAll(preserved, 0o700); err != nil {
		return fmt.Errorf("prepare the preserved directory: %w", err)
	}

	for _, path := range h.preservedPaths() {
		if err := copyIfPresent(path, filepath.Join(preserved, filepath.Base(path))); err != nil {
			return err
		}
	}

	return nil
}

// preservedPaths is what a rollback puts back on a Mac.
//
// ONE LIST FOR BOTH HALVES, for the reason the systemd host keeps one: a path
// preserved and never restored, or restored from a copy nobody made, fails only
// on the rollback that needs it. The plists are the launchd analogue of the
// units, read from the agents directory launchd scans — resolved from the
// account database rather than $HOME, which the converger already does.
func (h *launchdHost) preservedPaths() []string {
	server, node := h.agents.Services()

	return []string{
		installedBinary,
		h.agents.AgentPath(server),
		h.agents.AgentPath(node),
		h.configPath(),
		provenance.Path,
	}
}

// HideBinary removes the installed executable.
//
// REMOVING THE FILE THIS PROCESS IS RUNNING FROM IS FINE ON macOS: the inode
// lives until the last process holding it exits, which is this updater, after
// it has put a replacement under the same name.
func (h *launchdHost) HideBinary(_ context.Context) error {
	if err := os.Remove(installedBinary); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("hide %s: %w", installedBinary, err)
	}

	return nil
}

// InstallCandidate puts the staged binary in place.
func (h *launchdHost) InstallCandidate(_ context.Context) error {
	return copyFile(h.staged, installedBinary)
}

// StartServices bootstraps the agents again, server first, and proves each.
//
// BOOTSTRAPPED, NOT ENABLED. The agents were booted out, which removes them
// from the domain and touches no override; their plists are still installed and
// still enabled, so a bootstrap is all a start is. What each proves is
// launchd's weaker sentence — the same pid survived the settle window — and it
// is reported in those words.
func (h *launchdHost) StartServices(ctx context.Context) error {
	server, node := h.agents.Services()

	for _, label := range []string{server, node} {
		if !h.wanted(label) {
			continue
		}

		proof, err := h.agents.StartAndProve(ctx, label)
		if err != nil {
			return fmt.Errorf("starting %s: %w", label, err)
		}

		fmt.Printf("  started %s (%s)\n", label, proof)
	}

	return nil
}

// ProveStable asks again, after a pause, whether each agent held its process.
func (h *launchdHost) ProveStable(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(stabilityWait):
	}

	server, node := h.agents.Services()

	for _, label := range []string{server, node} {
		if !h.wanted(label) {
			continue
		}

		if err := h.agents.ProveStable(ctx, label); err != nil {
			return fmt.Errorf("%s did not stay up: %w", label, err)
		}
	}

	return nil
}

// RestorePreserved puts back what PreserveCurrent copied.
func (h *launchdHost) RestorePreserved(_ context.Context, dir string) error {
	preserved := filepath.Join(dir, "preserved")

	for _, target := range h.preservedPaths() {
		source := filepath.Join(preserved, filepath.Base(target))
		if err := copyIfPresent(source, target); err != nil {
			return err
		}
	}

	return nil
}

// wanted reports whether this deployment runs an agent at all.
func (h *launchdHost) wanted(label string) bool {
	server, node := h.agents.Services()

	switch label {
	case server:
		return h.cfg.Server != nil
	case node:
		return h.cfg.Node != nil
	}

	return false
}
