package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/power"
)

// sysfsRoot is where a node reads and writes cpufreq.
const sysfsRoot = "/sys"

// The policy reports through power.Observer, and the runner restores through
// node.PowerRestorer; internal/metrics and internal/power satisfy them without
// importing the packages that declare them.
var (
	_ power.Observer     = (*metrics.PowerObserver)(nil)
	_ node.PowerRestorer = (*power.Controller)(nil)
)

// PowerMetrics is the node's idle power policy as metrics: each cap and
// restore by how it ended, and the latest launch restore's latency, told by
// the policy as they happen. A node with no node.power counts none.
func (n *Node) PowerMetrics() MetricsSource {
	return MetricsSource{
		name: "power",
		attach: func(reg *metrics.Registry) error {
			o, err := reg.PowerObserver(power.TransitionLabels(), power.OutcomeLabels())
			if err != nil {
				return err
			}
			n.powered = o
			// WHAT OPENNODE'S RESTORE DID before there was anything to count it.
			n.leftoverPower.replay(o)

			return nil
		},
	}
}

// heldPowerEvents keeps what a controller reports before the metrics it
// reports to exist, for replay once they do.
type heldPowerEvents struct {
	mu      sync.Mutex
	changes [][2]string
}

func (h *heldPowerEvents) PowerChanged(transition, outcome string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.changes = append(h.changes, [2]string{transition, outcome})
}

// PowerLaunchRestore is never told: nothing launches before OpenNode returns.
func (*heldPowerEvents) PowerLaunchRestore(time.Duration) {}

// replay tells o what was held, once.
func (h *heldPowerEvents) replay(o power.Observer) {
	if h == nil {
		return
	}

	h.mu.Lock()
	changes := h.changes
	h.changes = nil
	h.mu.Unlock()

	for _, c := range changes {
		o.PowerChanged(c[0], c[1])
	}
}

// openNodePower builds this node's idle power policy, having first restored a
// cap an earlier process left, and answers nil when there is nothing for one to
// do: no node.power, and nothing left to restore.
//
// THE LEFTOVER FIRST, WHATEVER THE CONFIGURATION SAYS NOW, and before the host
// is checked: a node whose node.power was removed after a crash, or one refused
// below, must not leave the host capped. A restore that fails here is not a
// refusal; the policy's loop retries it.
func openNodePower(ctx context.Context, cfg *config.Config, root, goos string, observer power.Observer,
) (*power.Controller, error) {
	var settings power.Settings

	if p := cfg.Node.Power; p != nil {
		// CHECKED HERE, NOT IN CONFIG: a config is also validated on machines
		// that will not run it.
		if goos != "linux" {
			return nil, fmt.Errorf("node.power lowers a Linux host's cpufreq ceiling, and this node runs on %s",
				goos)
		}
		idleAfter, err := p.IdleAfterDuration()
		if err != nil {
			return nil, err
		}
		settings = power.Settings{IdleAfter: idleAfter, DisableBoost: p.DisableBoost, MaxFreqKHz: p.IdleMaxFreqKHz}
	} else if goos != "linux" {
		return nil, nil //nolint:nilnil // no policy is a state, not an error
	}

	ctrl, err := power.New(power.Options{SysfsRoot: root, StateDir: cfg.Node.StateDir, Settings: settings,
		Log: slog.Default(), Observer: observer})
	if err != nil {
		return nil, err
	}

	// THE HOST'S CPUFREQ IS CLAIMED FOR THE NODE'S LIFE, by a node that caps: a
	// second node on this host with node.power would fight it, and is refused.
	if err := ctrl.ClaimHost(); err != nil {
		if cfg.Node.Power != nil {
			return nil, fmt.Errorf("node.power: %w; only one node on a host may run node.power", err)
		}
		slog.Default().Warn("another process on this host holds its cpufreq; a cap an earlier process of "+
			"this node left stays recorded until this node can claim it", "error", err)

		return nil, nil //nolint:nilnil // no policy is a state, not an error
	}

	restoreErr := ctrl.Restore(ctx, "start")
	if cfg.Node.Power == nil && restoreErr == nil {
		ctrl.ReleaseHost()

		return nil, nil //nolint:nilnil // no policy is a state, not an error
	}

	if err := ctrl.CheckHost(); err != nil {
		// A REFUSAL STILL OWES A LEFTOVER ITS RESTORE, for as long as a stop
		// would give it.
		if restoreErr != nil {
			closeNodePower(ctx, ctrl)
		} else {
			ctrl.ReleaseHost()
		}

		return nil, fmt.Errorf("node.power: %w", err)
	}

	if cfg.Node.Power != nil {
		slog.Default().Info("lowering the CPU's ceiling when this node is idle", "idle_after", settings.IdleAfter,
			"disable_boost", settings.DisableBoost, "idle_max_freq_khz", settings.MaxFreqKHz)
	}

	return ctrl, nil
}

// runNodePower runs the policy's loop until the returned stop is called, and
// stops capping as soon as ctx ends, since the drain that follows a stop can
// hold the host for hours. Stop waits for the loop, THEN restores through
// closePower: the other order would let the loop cap the host again behind the
// restore.
func runNodePower(ctx context.Context, ctrl *power.Controller, idle func() bool, closePower func()) func() {
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopping := context.AfterFunc(ctx, ctrl.BeginStop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctrl.Run(loopCtx, idle)
	}()

	return func() {
		stopping()
		ctrl.BeginStop()
		cancel()

		// BOUNDED: a loop held inside a sysfs write the kernel never finishes
		// does not keep the node from stopping. It caps nothing more either way.
		timer := time.NewTimer(power.DefaultStopBound)
		select {
		case <-done:
		case <-timer.C:
			slog.Default().Error("the idle power policy's loop did not stop within its bound; restoring anyway",
				"bound", power.DefaultStopBound)
		}
		timer.Stop()

		closePower()
	}
}

// restoreLeftoverPower restores a cap an earlier node process left, on Linux,
// telling observer what it did. A failure is logged, and Node.Run's policy
// retries it.
//
// UNDER THE HOST'S CPUFREQ LOCK, which every live controller holds: taking it
// proves no process on this host has a cap in force, so the record is this
// node's leftover whatever else has not yet been proved about this start. One
// that cannot be taken leaves the record for later.
func restoreLeftoverPower(cfg *config.Config, root, goos string, observer power.Observer) {
	if goos != "linux" {
		return
	}

	ctrl, err := power.New(power.Options{SysfsRoot: root, StateDir: cfg.Node.StateDir, Log: slog.Default(),
		Observer: observer})
	if err == nil {
		err = ctrl.ClaimHost()
		if err == nil {
			defer ctrl.ReleaseHost()
			err = ctrl.RestoreLeftover()
		}
	}
	if errors.Is(err, power.ErrHostHeld) {
		slog.Default().Warn("another process on this host holds its cpufreq; a cap an earlier process of this "+
			"node left stays recorded until this node can claim it", "error", err)

		return
	}
	if err != nil {
		slog.Default().Error("could not restore the CPU ceiling an earlier node process left; this node "+
			"retries once it runs", "error", err)
	}
}

// closeNodePower restores the full ceiling as the node stops, and says so when
// it could not.
func closeNodePower(ctx context.Context, ctrl *power.Controller) {
	if err := ctrl.Close(ctx); err != nil {
		slog.Default().Error("this node stopped with the CPU's ceiling still lowered", "error", err)
	}
}
