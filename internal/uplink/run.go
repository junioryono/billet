package uplink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// DefaultReflectors answer ICMP from anywhere and are run by three different
// operators, so the median of their delays is the line's and not one network's.
var DefaultReflectors = []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}

// Tick is how often the loop measures and adjusts.
const Tick = 500 * time.Millisecond

// rateWindow is how far back what an interface moved is measured. Drivers
// update their byte counters on their own schedule, and on the reference node a
// half-second difference read 1478 Mbit/s on a 1 Gbit/s link: two updates' worth
// in one tick. Over two seconds the lumps average out.
const rateWindow = 2 * time.Second

// fallbackSpeed is the starting rate for an interface whose speed the kernel
// does not report, in Mbit/s: high enough to hold nothing back on any line.
const fallbackSpeed = 10000

// Options are what Run shapes and how.
type Options struct {
	// Iface is the interface to shape; empty means the default route's.
	Iface      string
	Reflectors []string
	// MaxUp and MaxDown are the most each direction is ever let through, in
	// Mbit/s; zero is the interface's speed. Learning the line from latency
	// alone left a gateway that drops before it queues unprotected: from
	// 02:05 to 05:15 UTC on 2026-10-14 the reference node cut nothing while it
	// sent up to 888 Mbit/s and a laptop behind the same gateway lost its
	// connections (shared-uplink record).
	MaxUp, MaxDown float64
	// Gateway is the first hop asked each round beside the reflectors; empty
	// asks the default route's, unless NoGateway. A host beside the reference
	// node saw the gateway stall while the reflectors saw little (2026-10-14).
	Gateway   string
	NoGateway bool
	Params    Params
	Log       *slog.Logger
}

// Ceiling is the rate a direction starts at and never exceeds: the configured
// maximum when there is one below the interface's speed, else that speed.
func Ceiling(speed, configured float64) float64 {
	if configured > 0 && configured < speed {
		return configured
	}

	return speed
}

// newRunController is the controller a run starts with, each direction at its
// ceiling.
func newRunController(speed float64, opts Options) *Controller {
	return &Controller{Params: opts.Params, Up: NewDirection(Ceiling(speed, opts.MaxUp)),
		Down: NewDirection(Ceiling(speed, opts.MaxDown))}
}

// Run shapes the uplink until ctx ends, and removes the shaping as it returns.
func Run(ctx context.Context, opts Options) error {
	// THE CLAIM IS HELD FOR THE WHOLE RUN, cleanup included: a second shaper
	// would clear and reinstall this one's qdiscs, and either one stopping would
	// remove the other's.
	release, err := Lock()
	if err != nil {
		return err
	}
	defer release()

	iface := opts.Iface
	if iface == "" {
		found, err := DefaultInterface()
		if err != nil {
			return err
		}

		iface = found
	}

	reflectors := opts.Reflectors
	if len(reflectors) == 0 {
		reflectors = DefaultReflectors
	}

	gateway := runGateway(opts, iface)

	pinger, err := NewPinger(ctx, reflectors, gateway)
	if err != nil {
		return err
	}
	defer pinger.Close()

	speed := Speed(iface)
	if speed == 0 {
		speed = fallbackSpeed
	}

	ctl := newRunController(speed, opts)

	// WHAT AN EARLIER RUN LEFT IS CLEARED FIRST, wherever its record says it is
	// now, so every run starts from an interface carrying nothing of billet's: a
	// crash's leftovers, and those on an interface the default route has since
	// left, which overwriting the record would strand.
	if _, _, err := clearRecorded(ctx, nil); err != nil {
		return fmt.Errorf("clear what an earlier run left before shaping %s: %w", iface, err)
	}

	// THIS RUN'S RECORD IS WRITTEN ONLY ONCE THE INTERFACE IS KNOWN TO CARRY
	// NOTHING OF ANYBODY ELSE'S: a record written first would make an operator's
	// CAKE look like billet's to the cleanup after a refusal. And BEFORE
	// anything is installed, so the cleanup after a crash finds it.
	shaper := &Shaper{Iface: iface}
	if err := shaper.Check(ctx); err != nil {
		return fmt.Errorf("shape %s: %w", iface, err)
	}

	at := thisNIC(iface)
	if err := writeRecord(Record{Iface: iface, Index: at.index, IFB: shaper.IFB()}); err != nil {
		return err
	}

	// WHAT AN EARLIER RUN LEARNED, BEFORE ANYTHING IS INSTALLED, so the line is
	// held where it was from the first packet rather than relearned from the
	// ceiling after every restart.
	resumeLearned(opts.Log, ctl, at, time.Now())

	if err := shaper.Install(ctx, ctl.Up.Rate, ctl.Down.Rate); err != nil {
		return fmt.Errorf("install CAKE on %s: %w", iface, err)
	}

	remember := rememberer(opts.Log, ctl, at)
	remember(ctl.Up.Rate, ctl.Down.Rate)

	opts.Log.Info("shaping the uplink; each direction starts at its ceiling, or at the rate an earlier run "+
		"learned, and is cut only when its own traffic raises the latency to the reflectors or the gateway, "+
		"or makes the line drop the reflectors' probes",
		"interface", iface, "interface_mbit", speed, "up_ceiling_mbit", ctl.Up.Max,
		"down_ceiling_mbit", ctl.Down.Max, "up_mbit", round(ctl.Up.Rate), "down_mbit", round(ctl.Down.Rate),
		"reflectors", pinger.Reflectors(), "gateway", gatewayLabel(pinger.Gateway()), "bloat", opts.Params.Bloat)

	// THE DEFAULT ROUTE'S GATEWAY IS FOLLOWED, one the operator named is not: a
	// route that moves to another gateway on the same interface would otherwise
	// have the shaper ask the old one, whose stalls are no longer this line's.
	var follow func() (string, error)
	if !opts.NoGateway && opts.Gateway == "" {
		follow = func() (string, error) { return DefaultGateway(iface) }
	}

	err = loop(ctx, opts.Log, iface, at.index, pinger, ctl, shaper, remember, follow)

	// REMOVED ON EVERY WAY OUT, by the record, so an interface renamed while it
	// ran is still cleared; a removal that failed is an error, and the unit's
	// ExecStopPost tries again. SAID IN THE LOG, because the ExecStopPost that
	// follows finds no record, and without this line nothing says the shaping
	// was there and is gone.
	r, cleared, clearErr := clearRecorded(context.WithoutCancel(ctx), nil)
	if clearErr == nil && cleared {
		opts.Log.Info("removed the shaping this run installed, as it stopped", "interface", r.Iface, "ifb", r.IFB)
	}

	return errors.Join(err, clearErr)
}

// runGateway is the first hop a run asks: the one named, none when NoGateway,
// else the default route's by iface. A lookup that fails is logged and asks
// none, because the gateway adds a signal and the shaping does without it.
func runGateway(opts Options, iface string) string {
	if opts.NoGateway {
		return ""
	}

	if opts.Gateway != "" {
		return opts.Gateway
	}

	gateway, err := DefaultGateway(iface)
	if err != nil {
		opts.Log.Warn("not asking the gateway: its address could not be read", "interface", iface, "error", err)

		return ""
	}

	return gateway
}

// gatewaySetter is what followGateway moves: the Pinger.
type gatewaySetter interface {
	Gateway() string
	setGateway(gateway string) (bool, error)
}

// followGateway asks the gateway lookup names now, when it has changed, and
// starts its baseline and every cut's evidence of it afresh: a new device's
// round trip is no measure against the old one's. A lookup that could not tell
// keeps the gateway asked now.
func followGateway(log *slog.Logger, lookup func() (string, error), pinger gatewaySetter, st *stepper,
	ctl *Controller,
) {
	gateway, err := lookup()
	if err != nil {
		return
	}

	changed, err := pinger.setGateway(gateway)
	if err != nil {
		log.Warn("the default route's gateway changed to one that cannot be asked; asking the old one",
			"gateway", gateway, "error", err)

		return
	}

	if !changed {
		return
	}

	st.gateway = Delays{}
	ctl.forgetGateway()

	log.Info("the default route's gateway changed; asking the new one", "gateway", gatewayLabel(pinger.Gateway()))
}

// gatewayLabel is the gateway as logged.
func gatewayLabel(gateway string) string {
	if gateway == "" {
		return "none"
	}

	return gateway
}

// rememberer records the rates CAKE was set to, for the next run, or the next
// boot's, to resume; a write that fails is said once and changes nothing about
// the shaping. THE ADDRESS IS READ AT EACH WRITE, not once at the start, so a
// MAC set while the shaper runs is the one the next boot compares; but only
// while the name still has the run's index, so a device that took the name, or
// none, never puts its address, or nothing, beside this device's rates; a run
// that could not read its index at the start keeps the address it started
// with, having nothing to tell its own device by.
func rememberer(log *slog.Logger, ctl *Controller, at nic) func(upMbit, downMbit float64) {
	told, addr := false, at.addr

	return func(upMbit, downMbit float64) {
		if now := ifaddr(at.iface); now != "" && at.index != "" && ifindex(at.iface) == at.index {
			addr = now
		}

		err := writeLearned(learned{Iface: at.iface, Index: at.index, Addr: addr, Boot: at.boot,
			At:   time.Now(),
			Up:   learnedRate{Max: ctl.Up.Max, Rate: upMbit},
			Down: learnedRate{Max: ctl.Down.Max, Rate: downMbit}})
		if err != nil && !told {
			told = true
			log.Warn("could not record the rates for the next run to resume; it will start at the ceiling",
				"error", err)
		}
	}
}

// Lock takes the one claim on this host's shaping, held by a shaper for its
// whole run and by a cleanup while it clears: two of them at once would each
// remove what the other installed. It fails at once, naming the holder's
// claim, rather than wait. The claim ends with the process.
func Lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(StateFile), 0o755); err != nil {
		return nil, fmt.Errorf("claim the uplink's shaping: %w", err)
	}

	path := filepath.Join(filepath.Dir(StateFile), "lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("claim the uplink's shaping: %w", err)
	}

	if err := flock(f); err != nil {
		f.Close()

		return nil, fmt.Errorf("another billet uplink is shaping or clearing this host (%s): %w", path, err)
	}

	return func() { f.Close() }, nil
}

func loop(ctx context.Context, log *slog.Logger, iface, index string, pinger *Pinger, ctl *Controller,
	shaper *Shaper, remember func(upMbit, downMbit float64), follow func() (string, error),
) error {
	read := func() (Counters, error) { return ReadCounters(iface) }
	set := func(ctx context.Context, upMbit, downMbit float64) error {
		if err := shaper.Set(ctx, upMbit, downMbit); err != nil {
			return fmt.Errorf("change CAKE's rate on %s: %w", iface, err)
		}

		return nil
	}

	st, err := newStepper(log, pinger, ctl, read, set, time.Now)
	if err != nil {
		return err
	}
	st.remember = remember

	// AND THE RATES IN FORCE ARE RECORDED AGAIN EVERY MINUTE AND AS THE LOOP
	// ENDS, though they have not changed: the next run returns them toward the
	// ceiling for the time since they were recorded, which must be the time the
	// shaper was down, not the time since the line was last cut. A rate held for
	// hours resumed after an upgrade's restart would otherwise come back near the
	// ceiling. What CAKE was given (applied), never a rise too small to apply.
	defer st.record()

	exe := currentExecutable()
	exeCheckedAt := time.Now()
	ticker := time.NewTicker(Tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		// THE NAME STILL MEANS THE INTERFACE THIS RUN SHAPED, checked before every
		// read and every change: renamed, with another device under the old name,
		// the next `tc qdisc change` would retune somebody else's CAKE. Stopping
		// leaves the cleanup to the record, which follows the index.
		if index != "" && ifindex(iface) != index {
			return fmt.Errorf("%s no longer has the index %s it was shaped under; stopping, and clearing "+
				"by the record", iface, index)
		}

		if time.Since(exeCheckedAt) >= time.Minute {
			exeCheckedAt = time.Now()
			st.record()

			if follow != nil {
				followGateway(log, follow, pinger, st, ctl)
			}

			if exe.replaced() {
				log.Info("the installed billet was replaced; stopping so the unit restarts onto it")

				return ErrReplaced
			}
		}

		if err := st.tick(ctx); err != nil {
			if errors.Is(err, errStopping) {
				return nil
			}

			return err
		}
	}
}
