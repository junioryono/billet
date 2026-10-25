package fleetops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

// cmdJobsRuntimes reports how well the history predicts how long jobs run:
// each job that started in the window graded against what a controller
// refreshing hourly would have predicted for it, per tier.
//
// A REPORT, OPENED AS ONE: the ledger is read in the inspect mode, which
// creates, locks, claims and migrates nothing, so it can be run beside a live
// control plane and never moves its ledger.
func cmdJobsRuntimes(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs runtimes", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	tier := fs.String("tier", "", "report only this tier")
	since := fs.Duration("since", alloc.RuntimeWindow, "grade the jobs that started this far back")
	asJSON := fs.Bool("json", false, "print the report as one JSON object")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if *since <= 0 {
		return fmt.Errorf("billet jobs runtimes: --since must be positive, got %s", *since)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if *tier != "" && !slices.ContainsFunc(cfg.Tiers, func(t config.Tier) bool { return t.Label == *tier }) {
		return fmt.Errorf("billet jobs runtimes: --tier %q is not a tier in %s", *tier, *cfgPath)
	}

	a, closeDB, err := app.OpenReport(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	report, err := buildRuntimeReport(ctx, a, *tier, time.Now(), *since)
	if err != nil {
		return err
	}

	if *asJSON {
		return renderRuntimeReportJSON(env.Stdout, report)
	}

	renderRuntimeReport(env.Stdout, report)

	return nil
}

// runtimeReport is one backtest: the window graded, and each tier's grade.
type runtimeReport struct {
	From, To time.Time
	// Partial is true when the history read held more jobs than one read
	// takes, and its oldest were left out.
	Partial bool
	Tiers   []alloc.RuntimeAccuracy
}

// buildRuntimeReport grades the jobs that started in the window before now.
// The read reaches a fortnight before the hour the window begins in, which is
// what that hour's predictions learn from (alloc.BacktestSince).
func buildRuntimeReport(ctx context.Context, a *alloc.Allocator, tier string, now time.Time,
	since time.Duration,
) (runtimeReport, error) {
	from := now.Add(-since)

	samples, partial, err := a.RuntimeSamples(ctx, alloc.BacktestSince(from), now)
	if err != nil {
		return runtimeReport{}, err
	}

	out := runtimeReport{From: from, To: now, Partial: partial}

	for _, acc := range alloc.BacktestRuntimes(samples, from, now) {
		if tier == "" || acc.Tier == tier {
			out.Tiers = append(out.Tiers, acc)
		}
	}

	return out, nil
}

// runtimeLevels are the levels a prediction can come from, most specific
// first, in the order a report lists them.
var runtimeLevels = []alloc.RuntimeLevel{alloc.RuntimeByJob, alloc.RuntimeByName, alloc.RuntimeByTier,
	alloc.RuntimeByDefault}

func renderRuntimeReport(w io.Writer, r runtimeReport) {
	fmt.Fprintf(w, "runtime predictions for the jobs that started from %s to %s, each predicted as a "+
		"controller refreshing hourly would have, from the %s before the hour it started in\n",
		r.From.UTC().Format(time.RFC3339), r.To.UTC().Format(time.RFC3339), alloc.RuntimeWindow)

	if r.Partial {
		fmt.Fprintln(w, "partial: the history held more jobs than one read takes, and its oldest were left out")
	}

	if len(r.Tiers) == 0 {
		fmt.Fprintln(w, "no job started and finished in the window")

		return
	}

	for _, t := range r.Tiers {
		fmt.Fprintf(w, "\n%s: %d jobs, median error %s, %.0f%% within 2x of the prediction\n",
			t.Tier, t.Count, WaitText(t.MedianAbsError), 100*t.Within2xShare())

		for _, level := range runtimeLevels {
			if n := t.Levels[level]; n > 0 {
				fmt.Fprintf(w, "  predicted from %s: %d\n", levelText(level), n)
			}
		}
	}
}

// levelText says where a prediction came from.
func levelText(level alloc.RuntimeLevel) string {
	switch level {
	case alloc.RuntimeByJob:
		return "the job's own history"
	case alloc.RuntimeByName:
		return "the job's history across its matrix"
	case alloc.RuntimeByTier:
		return "its tier's history"
	default:
		return fmt.Sprintf("nothing learned (the %s default)", alloc.DefaultRuntime)
	}
}

type runtimeReportJSON struct {
	From    time.Time         `json:"from"`
	To      time.Time         `json:"to"`
	Partial bool              `json:"partial"`
	Tiers   []runtimeTierJSON `json:"tiers"`
}

type runtimeTierJSON struct {
	Tier                  string         `json:"tier"`
	Jobs                  int            `json:"jobs"`
	MedianAbsErrorSeconds float64        `json:"median_abs_error_seconds"`
	Within2x              int            `json:"within_2x"`
	Within2xShare         float64        `json:"within_2x_share"`
	Levels                map[string]int `json:"levels"`
}

func renderRuntimeReportJSON(w io.Writer, r runtimeReport) error {
	out := runtimeReportJSON{From: r.From.UTC(), To: r.To.UTC(), Partial: r.Partial,
		Tiers: make([]runtimeTierJSON, 0, len(r.Tiers))}

	for _, t := range r.Tiers {
		levels := make(map[string]int, len(runtimeLevels))
		for _, level := range runtimeLevels {
			levels[string(level)] = t.Levels[level]
		}

		out.Tiers = append(out.Tiers, runtimeTierJSON{
			Tier: t.Tier, Jobs: t.Count, MedianAbsErrorSeconds: t.MedianAbsError.Seconds(),
			Within2x: t.Within2x, Within2xShare: t.Within2xShare(), Levels: levels,
		})
	}

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the runtime report: %w", err)
	}

	if _, err := fmt.Fprintf(w, "%s\n", body); err != nil {
		return fmt.Errorf("write the runtime report: %w", err)
	}

	return nil
}

// jobPriors answers the snapshot as of an hour from the ledger, once per hour
// asked: a run's jobs mostly started within a few hours of each other.
func jobPriors(ctx context.Context, a *alloc.Allocator) func(time.Time) (*alloc.RuntimePriors, error) {
	learned := map[time.Time]*alloc.RuntimePriors{}

	return func(asOf time.Time) (*alloc.RuntimePriors, error) {
		if p, ok := learned[asOf]; ok {
			return p, nil
		}

		p, err := a.RuntimePriorsAsOf(ctx, asOf)
		if err != nil {
			return nil, err
		}

		learned[asOf] = p

		return p, nil
	}
}

// runtimeLine is a job's runtime beside what was predicted for it when it
// started.
func runtimeLine(rec alloc.JobRecord) string {
	if rec.StartedAt == "" {
		return "not started"
	}

	ran := "still running"
	if d, ok := rec.Runtime(); ok {
		ran = "ran " + WaitText(d)
	}

	p := rec.Predicted
	if p == nil {
		if rec.NotPredicted != "" {
			return ran + "; not predicted: " + rec.NotPredicted
		}

		return ran + "; not predicted"
	}

	line := fmt.Sprintf("%s; predicted %s (p90 %s) from %s", ran, WaitText(p.Median), WaitText(p.P90),
		samplesText(*p))
	if p.Level == alloc.RuntimeByDefault {
		line = fmt.Sprintf("%s; predicted %s, the default, with nothing learned", ran, WaitText(p.Median))
	}

	if d, ok := rec.Runtime(); ok {
		switch off := d - p.Median; {
		case off > 0:
			line += ", and it ran " + WaitText(off) + " longer"
		case off < 0:
			line += ", and it ran " + WaitText(-off) + " shorter"
		default:
			line += ", and it ran exactly that"
		}
	}

	return line
}

// samplesText says what a prediction rests on.
func samplesText(p alloc.RuntimeEstimate) string {
	switch p.Level {
	case alloc.RuntimeByJob:
		return fmt.Sprintf("%d runs of this job", p.Samples)
	case alloc.RuntimeByName:
		return fmt.Sprintf("%d runs of this job across its matrix", p.Samples)
	default:
		return fmt.Sprintf("%d jobs of its tier", p.Samples)
	}
}

// predictionJSON is a job's prediction in `billet jobs show --json`.
// RuntimeSeconds and ErrorSeconds are null while the job has no runtime.
type predictionJSON struct {
	MedianSeconds  float64  `json:"median_seconds"`
	P90Seconds     float64  `json:"p90_seconds"`
	Samples        int      `json:"samples"`
	Level          string   `json:"level"`
	RuntimeSeconds *float64 `json:"runtime_seconds"`
	ErrorSeconds   *float64 `json:"error_seconds"`
}

func predictionJSONOf(rec alloc.JobRecord) *predictionJSON {
	p := rec.Predicted
	if p == nil {
		return nil
	}

	out := &predictionJSON{MedianSeconds: p.Median.Seconds(), P90Seconds: p.P90.Seconds(),
		Samples: p.Samples, Level: string(p.Level)}

	if d, ok := rec.Runtime(); ok {
		runtime, off := d.Seconds(), (d - p.Median).Seconds()
		out.RuntimeSeconds, out.ErrorSeconds = &runtime, &off
	}

	return out
}
