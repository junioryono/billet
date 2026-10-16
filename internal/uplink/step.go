package uplink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Prober asks every reflector once a round; Pinger is the one a run uses.
type Prober interface {
	Reflectors() []string
	// Gateway is the first hop each round also asks, empty for none.
	Gateway() string
	// Round returns the round trip of every reflector, and of the gateway, that
	// answered within wait; one missing from it went unanswered.
	Round(ctx context.Context, wait time.Duration) (map[string]time.Duration, error)
}

// errStopping is a round cut short because the run is ending.
var errStopping = errors.New("the run is ending")

// stepper is what a run does each Tick: one round of probes, the interface's
// counters, one step of the controller, and the rates it chose applied and
// logged.
type stepper struct {
	log    *slog.Logger
	prober Prober
	ctl    *Controller
	delays Delays
	// gateway is the first hop's own baseline and silence, kept apart from the
	// reflectors' so it never moves their median or their loss.
	gateway Delays
	read    func() (Counters, error)
	set     func(ctx context.Context, upMbit, downMbit float64) error
	now     func() time.Time
	history []reading
	applied [2]float64
	// remember is told every pair of rates CAKE was set to, nil for nothing;
	// unsure is a set that failed partway, after which what CAKE holds is not
	// known and nothing is told.
	remember func(upMbit, downMbit float64)
	unsure   bool
	// blind is when counted reflectors stopped answering, zero while one does;
	// told is whether that has been logged.
	blind time.Time
	told  bool
}

func newStepper(log *slog.Logger, prober Prober, ctl *Controller, read func() (Counters, error),
	set func(ctx context.Context, upMbit, downMbit float64) error, now func() time.Time,
) (*stepper, error) {
	first, err := read()
	if err != nil {
		return nil, err
	}

	// A RUN STARTS UNABLE TO TELL, with no reflector's baseline yet; that is
	// logged only if it lasts.
	start := now()

	return &stepper{
		log: log, prober: prober, ctl: ctl, read: read, set: set, now: now,
		history: []reading{{at: start, counters: first}},
		applied: [2]float64{ctl.Up.Rate, ctl.Down.Rate},
		blind:   start,
	}, nil
}

func (s *stepper) tick(ctx context.Context) error {
	answered, err := s.prober.Round(ctx, Tick*8/10)
	if err != nil {
		if ctx.Err() != nil {
			return errStopping
		}

		return err
	}

	sample := s.delays.Observe(s.prober.Reflectors(), answered)

	// THE GATEWAY'S READING AS IT CAME: the controller confirms a stall from two
	// of them (readGateway), because one ping a round to one device is noisier
	// than the median of three reflectors.
	var gateway Sample
	if g := s.prober.Gateway(); g != "" {
		gateway = s.gateway.Observe([]string{g}, answered)
	}

	now, err := s.read()
	if err != nil {
		return err
	}

	at := s.now()
	s.history = append(s.history, reading{at: at, counters: now})

	// THE OLDEST READING STILL INSIDE THE WINDOW, keeping one at its edge so
	// the span is never shorter than the window once it has filled.
	for len(s.history) > 2 && at.Sub(s.history[1].at) >= rateWindow {
		s.history = s.history[1:]
	}

	oldest := s.history[0]
	interval := at.Sub(oldest.at)
	sent, received := now.Since(oldest.counters)

	s.notice(sample, at)

	ctl := s.ctl
	if !ctl.Step(Observation{At: at, Interval: interval, SentBits: sent, ReceivedBits: received,
		Sample: sample, Gateway: gateway}) {
		return nil
	}

	if !worthApplying(s.applied, ctl) {
		return nil
	}

	if err := s.set(ctx, ctl.Up.Rate, ctl.Down.Rate); err != nil {
		// A SET THAT FAILED MAY HAVE CHANGED ONE DIRECTION (Shaper.Set changes the
		// upload, then the download), so what CAKE holds is no longer known, and
		// nothing more is recorded: the last record, and its time, stand.
		s.unsure = true

		return err
	}

	if ctl.Cause != CauseNone && (ctl.Up.Rate < s.applied[0] || ctl.Down.Rate < s.applied[1]) {
		attrs := []slog.Attr{
			slog.Float64("up_mbit", round(ctl.Up.Rate)), slog.Float64("down_mbit", round(ctl.Down.Rate)),
			slog.Float64("moving_up_mbit", round(float64(sent)/interval.Seconds()/1e6)),
			slog.Float64("moving_down_mbit", round(float64(received)/interval.Seconds()/1e6)),
			slog.String("cause", ctl.Cause.String()), delayAttr(sample),
			slog.String("lost", fmt.Sprintf("%d/%d", sample.Lost(), len(sample.Probes))),
		}
		if s.prober.Gateway() != "" {
			attrs = append(attrs, gatewayAttr(gateway))
			if ctl.cutStall.known {
				attrs = append(attrs, slog.Duration("gateway_stall", ctl.cutStall.delay))
			}
		}

		s.log.LogAttrs(ctx, slog.LevelInfo, cutMessage(ctl.Cause), attrs...)
	}

	s.applied = [2]float64{ctl.Up.Rate, ctl.Down.Rate}

	// ONLY ONCE CAKE HAS THE RATES, so what is remembered is what was applied.
	s.unsure = false
	s.record()

	return nil
}

// record tells remember the rates CAKE holds now, read when it runs: the run
// calls it after every change, once a minute and as it ends, so the next run
// counts only the time the shaper was down. Nothing when a failed set left
// what CAKE holds unknown.
func (s *stepper) record() {
	if s.remember == nil || s.unsure {
		return
	}

	s.remember(s.applied[0], s.applied[1])
}

// notice logs the rounds going blind, once no counted reflector has answered
// for as long as a reflector is given and the silent ones have stopped
// counting, so both rates are held, and their seeing again: a could-not-tell is
// said as such, never as a measurement. Answers are what count for the time,
// not probes: the lost probes of reflectors still being given their time
// measure loss, not the line's queue, and a first answer only sets a baseline.
// And the warning waits for the round that counts nobody, not only for the
// time, which a stalled process can pass in two rounds whose losses still cut.
func (s *stepper) notice(sample Sample, at time.Time) {
	answered := len(sample.Probes) > sample.Lost()

	switch {
	case answered && !s.blind.IsZero():
		if s.told {
			s.log.Info("the reflectors answer again; measuring the line",
				"could_not_tell_for", at.Sub(s.blind).Round(time.Second))
		}

		s.blind, s.told = time.Time{}, false
	case !answered && s.blind.IsZero():
		s.blind = at
	case !answered && !s.told && len(sample.Probes) == 0 && at.Sub(s.blind) >= silentAfter*Tick:
		s.told = true
		s.log.Warn("no reflector answers, so the line's queue cannot be told; holding both rates until one does",
			"reflectors", s.prober.Reflectors(), "for", at.Sub(s.blind).Round(time.Second))
	}
}

// cutMessage says what a cut was taken on.
func cutMessage(cause Cause) string {
	switch cause {
	case CauseLoss:
		return "the line dropped the reflectors' probes under this host's traffic; cut"
	case CauseDelayAndLoss:
		return "the line's queue rose and dropped the reflectors' probes under this host's traffic; cut"
	}

	return "the line's queue rose under this host's traffic; cut"
}

// delayAttr is the round's delay as logged: the measurement, or that there was
// none.
func delayAttr(sample Sample) slog.Attr {
	if !sample.DelayKnown {
		return slog.String("delay", "unmeasured")
	}

	return slog.Duration("delay", sample.Delay)
}

// gatewayAttr is the gateway's rise as logged: the measurement, or that there
// was none this round.
func gatewayAttr(gateway Sample) slog.Attr {
	if !gateway.DelayKnown {
		return slog.String("gateway_delay", "unmeasured")
	}

	return slog.Duration("gateway_delay", gateway.Delay)
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
