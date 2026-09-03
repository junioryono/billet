package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/rollout"
)

// rolloutAutostart is what `release.automatic: true` means: a control plane that
// resolves its channel on a slow tick and records a rollout when the fleet is
// not on what the channel names, inside the maintenance window, exactly as
// `billet rollout start` would have.
//
// IT STARTS AND NEVER DRIVES. The coordinator converges whatever is open, on its
// own tick, with its own rules; this only writes the one durable decision an
// operator would otherwise write. Everything it refuses to do it refuses
// quietly and says so in the log: a channel that will not resolve, an open
// rollout, a closed window, a target that is not compatible with what this
// deployment runs. None of those is a reason to stop a control plane that is
// taking work, and none becomes a rollout.
type rolloutAutostart struct {
	release *config.ReleaseConfig
	store   *rollout.Store
	fleet   rollout.Fleet
	// running is the version this control plane runs; the target has to differ
	// from it or from some host's for a rollout to be worth recording.
	running string
	// current is what the fleet speaks, for the compatibility check a hand-run
	// `rollout start` makes before it records anything.
	current releasesource.Current
	resolve func(ctx context.Context) (*releasesource.Manifest, string, error)
	now     func() time.Time
	log     *slog.Logger
}

// autostartEvery is how often the channel is asked. Thirty seconds is the
// coordinator's pace and would be an unkind pace at which to fetch a signed
// statement from a CDN; a channel that advanced is picked up within this.
const autostartEvery = 10 * time.Minute

// Run asks Once on every tick until the context ends.
func (a *rolloutAutostart) Run(ctx context.Context) {
	ticker := time.NewTicker(autostartEvery)
	defer ticker.Stop()

	for {
		if err := a.Once(ctx); err != nil {
			a.log.Warn("automatic rollout: could not decide this tick", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once records a rollout if one is due, and returns why nothing was recorded
// only when that is a fault rather than a decision.
func (a *rolloutAutostart) Once(ctx context.Context) error {
	if a.release == nil || !a.release.Automatic {
		return nil
	}

	switch _, err := a.store.Open(ctx); {
	case err == nil:
		// The coordinator's, until it finishes.
		return nil
	case !errors.Is(err, rollout.ErrNoRollout):
		return err
	}

	if !a.release.OpenAt(a.now()) {
		a.log.Debug("automatic rollout: outside the maintenance window")

		return nil
	}

	target, digest, err := a.resolve(ctx)
	if err != nil {
		return err
	}

	hosts, err := a.fleet.Hosts(ctx)
	if err != nil {
		return err
	}

	behind := a.running != target.Version
	names := make([]string, 0, len(hosts))

	for i := range hosts {
		names = append(names, hosts[i].Name)

		if hosts[i].Release != target.Version {
			behind = true
		}
	}

	if !behind {
		return nil
	}

	// THE SAME GATE A PERSON'S `rollout start` PASSES. A target this deployment
	// cannot speak to is not something to record and let the coordinator discover
	// host by host.
	warnings, err := releasesource.Compatibility(target, a.current)
	for _, w := range warnings {
		a.log.Warn("automatic rollout: note", "note", w)
	}

	if err != nil {
		return err
	}

	recorded, err := a.store.Start(ctx, rollout.StartRequest{
		Channel:       a.release.Channel,
		TargetVersion: target.Version,
		TargetDigest:  digest,
		PriorVersion:  a.running,
		Policy:        rollout.DefaultPolicy(),
		CreatedBy:     "release.automatic",
		Nodes:         names,
	})
	if err != nil {
		if errors.Is(err, rollout.ErrOpen) {
			return nil
		}

		return err
	}

	a.log.Info("automatic rollout started; the control plane converges the fleet",
		"rollout", recorded.ID, "from", recorded.PriorVersion, "to", recorded.TargetVersion,
		"hosts", len(names))

	return nil
}
