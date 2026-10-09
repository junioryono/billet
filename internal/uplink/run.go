package uplink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	Params     Params
	Log        *slog.Logger
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

	pinger, err := NewPinger(reflectors)
	if err != nil {
		return err
	}
	defer pinger.Close() //nolint:errcheck // a socket closed on the way out has nothing left to say

	speed := Speed(iface)
	if speed == 0 {
		speed = fallbackSpeed
	}

	ctl := &Controller{Params: opts.Params, Up: NewDirection(speed), Down: NewDirection(speed)}

	// WHAT AN EARLIER RUN LEFT IS BILLET'S ONLY IF ITS RECORD SAYS SO, and this
	// run's record is written only once the interface is known to carry nothing
	// of anybody else's: a record written first would make an operator's CAKE
	// look like billet's to the cleanup after a refusal.
	//
	// A RECORD NAMING ANOTHER INTERFACE IS CLEARED FIRST: the default route moved
	// since that run, and overwriting its record would strand its shaping where
	// no cleanup could find it.
	if prior := RecordedInterface(); prior != "" && prior != iface {
		if err := (&Shaper{Iface: prior, Owned: true}).Clear(ctx); err != nil {
			return fmt.Errorf("clear the shaping an earlier run left on %s before shaping %s: %w", prior, iface, err)
		}

		if err := Forget(prior); err != nil {
			return err
		}
	}

	shaper := &Shaper{Iface: iface, Owned: RecordedInterface() == iface}
	if err := shaper.Check(ctx); err != nil {
		return fmt.Errorf("shape %s: %w", iface, err)
	}

	// RECORDED BEFORE ANYTHING IS INSTALLED, so the cleanup after a crash clears
	// this interface even if the default route has moved since.
	if err := recordInterface(iface); err != nil {
		return err
	}

	if err := shaper.Install(ctx, ctl.Up.Rate, ctl.Down.Rate); err != nil {
		return fmt.Errorf("install CAKE on %s: %w", iface, err)
	}

	opts.Log.Info("shaping the uplink; each direction starts at the interface's speed and is cut "+
		"only when its own traffic raises the latency to the reflectors",
		"interface", iface, "start_mbit", speed, "reflectors", reflectors, "bloat", opts.Params.Bloat)

	err = loop(ctx, opts.Log, iface, pinger, ctl, shaper)

	// REMOVED ON EVERY WAY OUT, and a removal that failed is an error: the unit's
	// ExecStopPost tries again from the recorded interface.
	if clearErr := shaper.Clear(context.WithoutCancel(ctx)); clearErr != nil {
		return errors.Join(err, fmt.Errorf("remove the shaping from %s: %w", iface, clearErr))
	}

	return errors.Join(err, Forget(iface))
}

// StateFile records which interface a running shaper shapes. It lives under
// the unit's RuntimeDirectory, which a reboot empties along with everything the
// shaper installed.
var StateFile = "/run/billet-uplink/interface"

// recordInterface publishes the record. One already naming the interface is
// left alone, and a new one replaces the old by rename: truncating the only
// proof that what is on an interface is billet's, and failing before writing
// it again, would leave shaping no cleanup could claim.
func recordInterface(iface string) error {
	if RecordedInterface() == iface {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(StateFile), 0o755); err != nil {
		return fmt.Errorf("record the shaped interface: %w", err)
	}

	next := StateFile + ".next"

	// THE INDEX BESIDE THE NAME, so a cleanup still finds an interface that was
	// renamed after it was shaped.
	line := strings.TrimSpace(iface + " " + ifindex(iface))

	//nolint:gosec // G306: an interface name for root's own cleanup, which any user may read
	if err := os.WriteFile(next, []byte(line+"\n"), 0o644); err != nil {
		return fmt.Errorf("record the shaped interface: %w", err)
	}

	if err := os.Rename(next, StateFile); err != nil {
		return fmt.Errorf("record the shaped interface: %w", err)
	}

	return nil
}

// Lock takes the one claim on this host's shaping, held by a shaper for its
// whole run and by a cleanup while it clears: two of them at once would each
// remove what the other installed. It fails at once, naming the holder's
// claim, rather than wait. The claim ends with the process.
func Lock() (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(StateFile), 0o755); err != nil {
		return nil, fmt.Errorf("claim the uplink's shaping: %w", err)
	}

	path := filepath.Join(filepath.Dir(StateFile), "lock")

	//nolint:gosec // G304: a fixed path under billet's own runtime directory
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("claim the uplink's shaping: %w", err)
	}

	if err := flock(f); err != nil {
		f.Close() //nolint:errcheck // the lock was not taken; its error is the one to report

		return nil, fmt.Errorf("another billet uplink is shaping or clearing this host (%s): %w", path, err)
	}

	return func() { f.Close() }, nil //nolint:errcheck // closing releases the lock; nothing is left to say
}

// RecordedInterface is the interface the last shaper recorded, or empty.
//
// The name as it is now: one that no longer exists is looked up by the index
// recorded beside it, and one found nowhere is returned as recorded, which is an
// interface gone along with every qdisc it carried.
func RecordedInterface() string {
	body, err := os.ReadFile(StateFile)
	if err != nil {
		return ""
	}

	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return ""
	}

	name := fields[0]
	if _, err := os.Stat(filepath.Join(sysNet, name)); err == nil || len(fields) < 2 {
		return name
	}

	if renamed := nameForIndex(fields[1]); renamed != "" {
		return renamed
	}

	return name
}

// Forget removes the record once its interface is clear, and only a record of
// that interface: forgetting another would strand that one's shaping.
func Forget(iface string) error {
	if RecordedInterface() != iface {
		return nil
	}

	if err := os.Remove(StateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("forget the shaped interface: %w", err)
	}

	return nil
}

func loop(ctx context.Context, log *slog.Logger, iface string, pinger *Pinger, ctl *Controller, shaper *Shaper) error {
	var delays Delays

	first, err := ReadCounters(iface)
	if err != nil {
		return err
	}

	history := []reading{{at: time.Now(), counters: first}}
	applied := [2]float64{ctl.Up.Rate, ctl.Down.Rate}
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

		if time.Since(exeCheckedAt) >= time.Minute {
			exeCheckedAt = time.Now()

			if exe.replaced() {
				log.Info("the installed billet was replaced; stopping so the unit restarts onto it")

				return ErrReplaced
			}
		}

		answered, err := pinger.Round(ctx, Tick*8/10)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return err
		}

		delay, known := delays.Observe(pinger.Reflectors(), answered)

		now, err := ReadCounters(iface)
		if err != nil {
			return err
		}

		at := time.Now()
		history = append(history, reading{at: at, counters: now})

		// THE OLDEST READING STILL INSIDE THE WINDOW, keeping one at its edge so
		// the span is never shorter than the window once it has filled.
		for len(history) > 2 && at.Sub(history[1].at) >= rateWindow {
			history = history[1:]
		}

		oldest := history[0]
		interval := at.Sub(oldest.at)
		sent, received := now.Since(oldest.counters)

		if !ctl.Step(Observation{At: at, Interval: interval, SentBits: sent,
			ReceivedBits: received, Delay: delay, DelayKnown: known}) {
			continue
		}

		if !worthApplying(applied, ctl) {
			continue
		}

		if err := shaper.Set(ctx, ctl.Up.Rate, ctl.Down.Rate); err != nil {
			return fmt.Errorf("change CAKE's rate on %s: %w", iface, err)
		}

		if ctl.Up.Rate < applied[0] || ctl.Down.Rate < applied[1] {
			log.Info("the line's queue rose under this host's traffic; cut",
				"up_mbit", round(ctl.Up.Rate), "down_mbit", round(ctl.Down.Rate),
				"moving_up_mbit", round(float64(sent)/interval.Seconds()/1e6),
				"moving_down_mbit", round(float64(received)/interval.Seconds()/1e6), "delay", delay)
		}

		applied = [2]float64{ctl.Up.Rate, ctl.Down.Rate}
	}
}

// reading is an interface's counters at a moment.
type reading struct {
	at       time.Time
	counters Counters
}

// worthApplying is whether the rates moved enough to tell tc: every cut, and a
// rise only once it is two percent, so probing upward is not a command a tick.
func worthApplying(applied [2]float64, ctl *Controller) bool {
	for i, rate := range []float64{ctl.Up.Rate, ctl.Down.Rate} {
		if rate < applied[i] || rate > applied[i]*1.02 {
			return true
		}
	}

	return false
}

func round(mbit float64) float64 { return float64(int64(mbit*10)) / 10 }
