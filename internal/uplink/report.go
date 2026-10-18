package uplink

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// reportEvery is how often a run says where it holds the line. A run logs a
// cut when it takes one and nothing else, so a day with no cut said nothing at
// all: not the rate CAKE held, not what the host moved under it, not whether
// the line showed any queue. A shaper with no ceiling holds a line nobody
// configured, and this is where an operator reads it (2026-10-18: eleven hours
// uncapped on the reference node with no cut, and no way to tell from its
// journal whether the line was never full or was full and unseen).
const reportEvery = 5 * time.Minute

// lineReport is what a run saw since its last report: the rounds, what each
// direction moved, and how many rounds showed a queue, lost a probe, saw the
// gateway stall or measured nothing, with the cuts taken.
type lineReport struct {
	from                                   time.Time
	rounds, delayed, lossy, stalled, blind int
	cuts                                   int
	peak, total                            [2]float64
	delays                                 []time.Duration
}

// observe adds one round: its probes, its gateway reading and what each
// direction moved over the throughput window, up first.
func (r *lineReport) observe(p Params, sample, gateway Sample, moving [2]float64) {
	r.rounds++

	for i, mbit := range moving {
		r.peak[i] = max(r.peak[i], mbit)
		r.total[i] += mbit
	}

	switch {
	case len(sample.Probes) == 0:
		r.blind++
	case sample.Lost() > 0:
		r.lossy++
	}

	if sample.DelayKnown {
		r.delays = append(r.delays, sample.Delay)

		if sample.Delay > p.Bloat {
			r.delayed++
		}
	}

	if gateway.DelayKnown && gateway.Delay > p.Bloat {
		r.stalled++
	}
}

// due is whether a report is owed at at.
func (r *lineReport) due(at time.Time) bool { return at.Sub(r.from) >= reportEvery }

// log says what the rounds since the last report saw, beside the rates CAKE
// holds (applied) and the lines ctl has found, and starts the next report at at.
func (r *lineReport) log(ctx context.Context, log *slog.Logger, at time.Time, applied [2]float64, ctl *Controller,
	gateway bool,
) {
	mean := func(i int) float64 {
		if r.rounds == 0 {
			return 0
		}

		return r.total[i] / float64(r.rounds)
	}

	attrs := []slog.Attr{
		slog.Duration("over", at.Sub(r.from).Round(time.Second)),
		slog.Float64("up_mbit", round(applied[0])), slog.Float64("down_mbit", round(applied[1])),
		slog.Float64("up_limit_mbit", round(ctl.Up.Limit)), slog.Float64("down_limit_mbit", round(ctl.Down.Limit)),
		slog.Float64("up_peak_mbit", round(r.peak[0])), slog.Float64("down_peak_mbit", round(r.peak[1])),
		slog.Float64("up_mean_mbit", round(mean(0))), slog.Float64("down_mean_mbit", round(mean(1))),
		slog.Int("rounds", r.rounds), slog.Int("delayed_rounds", r.delayed), slog.Int("lossy_rounds", r.lossy),
		slog.Int("blind_rounds", r.blind), slog.Int("cuts", r.cuts),
		r.percentile("delay_p50", 50), r.percentile("delay_p95", 95),
	}
	if gateway {
		attrs = append(attrs, slog.Int("gateway_stalled_rounds", r.stalled))
	}

	log.LogAttrs(ctx, slog.LevelInfo, "the line since the last report", attrs...)

	*r = lineReport{from: at}
}

// percentile is the reflectors' delay at the pth percentile of the rounds that
// measured one, or that none did.
func (r *lineReport) percentile(key string, p int) slog.Attr {
	if len(r.delays) == 0 {
		return slog.String(key, "unmeasured")
	}

	sorted := slices.Clone(r.delays)
	slices.Sort(sorted)

	return slog.Duration(key, sorted[min(len(sorted)-1, len(sorted)*p/100)].Round(100*time.Microsecond))
}
