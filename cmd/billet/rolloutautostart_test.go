package main

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/rollout"
)

type staticFleet []rollout.Host

// channelManifest is a release the channel names, publishing an artifact this
// machine can run, because the compatibility gate refuses one that does not.
func channelManifest(version string) *releasesource.Manifest {
	return &releasesource.Manifest{
		Version:       version,
		Wire:          releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
		LedgerSchema:  1,
		GuestContract: "10",
		Artifacts: []releasesource.Artifact{{
			Name: "billet_" + strings.TrimPrefix(version, "v") + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz",
			OS:   runtime.GOOS, Arch: runtime.GOARCH, Kind: "archive",
		}},
	}
}

// openRollout is the rollout that is running, or nil when none is.
func openRollout(t *testing.T, store *rollout.Store) *rollout.Rollout {
	t.Helper()

	open, err := store.Open(t.Context())
	if errors.Is(err, rollout.ErrNoRollout) {
		return nil
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return open
}

func (f staticFleet) Hosts(context.Context) ([]rollout.Host, error) { return f, nil }

// autostartHarness is a starter over a real ledger, a fixed fleet and a channel
// that names the given version, at noon UTC unless a test moves the clock.
func autostartHarness(t *testing.T, running string, hosts ...rollout.Host) (*rolloutAutostart, *rollout.Store) {
	t.Helper()

	db := openLedger(t, t.TempDir())
	store := rollout.New(db)
	current := releasesource.Host(running,
		releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version}, 1, "10")

	return &rolloutAutostart{
		release: &config.ReleaseConfig{Channel: releasesource.ChannelStable, Automatic: true},
		store:   store,
		fleet:   staticFleet(hosts),
		running: running,
		current: current,
		resolve: func(context.Context) (*releasesource.Manifest, string, error) {
			return channelManifest("v0.6.0"), strings.Repeat("6", 64), nil
		},
		now: func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
		log: slog.New(slog.DiscardHandler),
	}, store
}

// A CHANNEL THAT NAMES SOMETHING THE FLEET IS NOT ON BECOMES A ROLLOUT, recorded
// as an operator's `rollout start` would record it: every registered host, the
// default policy, and the channel and digest that decided it.
func TestAutomaticRolloutStartsWhenTheChannelIsAheadOfTheFleet(t *testing.T) {
	a, store := autostartHarness(t, "v0.5.0",
		rollout.Host{Name: "n1", Release: "v0.5.0"}, rollout.Host{Name: "n2", Release: "v0.5.0"})

	if err := a.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	open := openRollout(t, store)
	if open == nil {
		t.Fatal("no rollout was recorded")
	}
	if open.TargetVersion != "v0.6.0" || open.TargetDigest != strings.Repeat("6", 64) ||
		open.Channel != releasesource.ChannelStable || open.CreatedBy != "release.automatic" {
		t.Errorf("recorded %+v", open)
	}
	nodes, err := store.Nodes(t.Context(), open.ID)
	if err != nil || len(nodes) != 2 {
		t.Errorf("the rollout covers %d hosts (err=%v), want both", len(nodes), err)
	}

	// And a second look changes nothing: the open rollout is the coordinator's.
	if err := a.Once(t.Context()); err != nil {
		t.Fatalf("second Once: %v", err)
	}
}

// A FLEET ALREADY ON THE CHANNEL'S TARGET RECORDS NOTHING, and so does one behind
// only by a host the channel does not move.
func TestAutomaticRolloutRecordsNothingWhenEverythingIsOnTheTarget(t *testing.T) {
	a, store := autostartHarness(t, "v0.6.0", rollout.Host{Name: "n1", Release: "v0.6.0"})

	if err := a.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if open := openRollout(t, store); open != nil {
		t.Fatalf("a rollout was recorded for a fleet already on the target: %+v", open)
	}

	// One host behind is enough.
	a, store = autostartHarness(t, "v0.6.0", rollout.Host{Name: "n1", Release: "v0.5.0"})
	if err := a.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if openRollout(t, store) == nil {
		t.Fatal("a host behind the channel did not start a rollout")
	}
}

// OUTSIDE THE WINDOW NOTHING BEGINS, and the window bounds only the beginning.
func TestAutomaticRolloutWaitsForTheMaintenanceWindow(t *testing.T) {
	a, store := autostartHarness(t, "v0.5.0", rollout.Host{Name: "n1", Release: "v0.5.0"})
	a.release.MaintenanceWindow = &config.MaintenanceWindow{Start: "02:00", End: "04:00"}

	if err := a.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if open := openRollout(t, store); open != nil {
		t.Fatalf("a rollout began outside the window: %+v", open)
	}

	a.now = func() time.Time { return time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC) }
	if err := a.Once(t.Context()); err != nil {
		t.Fatalf("Once inside the window: %v", err)
	}
	if openRollout(t, store) == nil {
		t.Fatal("no rollout began inside the window")
	}
}

// A TARGET THIS DEPLOYMENT CANNOT SPEAK TO IS NEVER RECORDED, and the refusal is
// reported rather than discovered host by host.
func TestAutomaticRolloutRefusesAnIncompatibleTarget(t *testing.T) {
	a, store := autostartHarness(t, "v0.5.0", rollout.Host{Name: "n1", Release: "v0.5.0"})
	a.resolve = func(context.Context) (*releasesource.Manifest, string, error) {
		m := channelManifest("v0.6.0")
		m.Wire = releasesource.Range{Min: nodeapi.Version + 5, Max: nodeapi.Version + 6}
		return m, strings.Repeat("6", 64), nil
	}

	err := a.Once(t.Context())
	if err == nil {
		t.Fatal("an incompatible target was not refused")
	}
	if open := openRollout(t, store); open != nil {
		t.Fatalf("an incompatible target was recorded: %+v", open)
	}

	// A channel that will not resolve is a fault for the log, not a rollout.
	a.resolve = func(context.Context) (*releasesource.Manifest, string, error) {
		return nil, "", errors.New("the channel statement expired")
	}
	if err := a.Once(t.Context()); err == nil {
		t.Fatal("an unresolvable channel was not reported")
	}
}

// OFF BY DEFAULT: with release.automatic unset the starter is inert.
func TestAutomaticRolloutIsOffUnlessAsked(t *testing.T) {
	a, store := autostartHarness(t, "v0.5.0", rollout.Host{Name: "n1", Release: "v0.5.0"})
	a.release.Automatic = false

	if err := a.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if open := openRollout(t, store); open != nil {
		t.Fatalf("a rollout began with automatic off: %+v", open)
	}
}
