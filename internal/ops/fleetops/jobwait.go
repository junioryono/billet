package fleetops

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

// cmdJobsWait reports how long jobs waited, per tier and shape, from what the
// ledger recorded.
//
// TWO CLOCKS, SAID IN THE OUTPUT. The queue time and the scale-set assignment
// are GitHub's; the purchase and the start are billet's. A part that crosses
// them is only as exact as the two agree, and a negative one is printed as it
// is, never as zero.
func cmdJobsWait(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs wait", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	tier := fs.String("tier", "", "report only this tier")
	since := fs.Duration("since", 24*time.Hour, "report the jobs that started this far back")
	asJSON := fs.Bool("json", false, "print the report as one JSON object")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if *since <= 0 {
		return fmt.Errorf("billet jobs wait: --since must be positive, got %s", *since)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	report, err := buildJobWaits(ctx, a, cfg.Tiers, *tier, time.Now().Add(-*since))
	if err != nil {
		return err
	}

	if *asJSON {
		return renderJobWaitsJSON(env.Stdout, report)
	}

	renderJobWaits(env.Stdout, report)

	return nil
}

// jobWaits is one report: the jobs that started in the window, grouped by tier
// and charged shape, and what each listener last said was still waiting.
type jobWaits struct {
	Since   time.Time
	Partial bool
	Groups  []waitGroup
	Waiting []stillWaiting
}

type waitGroup struct {
	Tier string
	// VCPU is the charged shape, zero for a row that recorded none.
	VCPU    int
	Started int
	// Unknown is how many started with no GitHub queue time on record: before
	// the ledger kept one, or from a message that carried none.
	Unknown  int
	Wait     alloc.WaitSummary
	AtGitHub alloc.WaitSummary
	ForRoom  alloc.WaitSummary
	ToStart  alloc.WaitSummary
}

// stillWaiting is a tier's queue as its listener last reported it. Jobs still
// queued have no start, so they are counted here and kept out of every
// percentile.
type stillWaiting struct {
	Tier   string
	Jobs   int
	Oldest string
}

// waitParts are the rows of a group, in the order they add up: at GitHub, for
// room and to start telescope to the whole wait.
//
// below says what a part under zero means: for room it is usually the room
// bought first, and everywhere else a clock that disagreed.
var waitParts = []struct {
	name  string
	part  func(alloc.JobWait) (time.Duration, bool)
	of    func(*waitGroup) *alloc.WaitSummary
	below string
}{
	{"wait", alloc.JobWait.Wait, func(g *waitGroup) *alloc.WaitSummary { return &g.Wait },
		"GitHub's clock and billet's disagreed by more than the wait"},
	{"at github", alloc.JobWait.AtGitHub, func(g *waitGroup) *alloc.WaitSummary { return &g.AtGitHub },
		"GitHub's own times are out of order"},
	{"for room", alloc.JobWait.ForRoom, func(g *waitGroup) *alloc.WaitSummary { return &g.ForRoom },
		"the room was bought before GitHub handed the job over, or the clocks disagreed"},
	{"to start", alloc.JobWait.ToStart, func(g *waitGroup) *alloc.WaitSummary { return &g.ToStart },
		"billet's own times are out of order"},
}

// buildJobWaits reads the window and groups it. tier, when set, keeps only that
// tier's jobs and queue.
func buildJobWaits(ctx context.Context, a *alloc.Allocator, tiers []config.Tier, tier string,
	since time.Time,
) (jobWaits, error) {
	jobs, partial, err := a.JobWaits(ctx, since)
	if err != nil {
		return jobWaits{}, err
	}

	type key struct {
		tier string
		vcpu int
	}

	grouped := map[key][]alloc.JobWait{}

	for i := range jobs {
		if tier != "" && jobs[i].Tier != tier {
			continue
		}

		k := key{jobs[i].Tier, jobs[i].VCPU}
		grouped[k] = append(grouped[k], jobs[i])
	}

	out := jobWaits{Since: since, Partial: partial}

	for k, members := range grouped {
		g := waitGroup{Tier: k.tier, VCPU: k.vcpu, Started: len(members)}

		for _, p := range waitParts {
			*p.of(&g) = alloc.Summarize(members, p.part)
		}

		g.Unknown = g.Started - g.Wait.Count
		out.Groups = append(out.Groups, g)
	}

	slices.SortFunc(out.Groups, func(x, y waitGroup) int {
		return cmp.Or(cmp.Compare(x.Tier, y.Tier), cmp.Compare(x.VCPU, y.VCPU))
	})

	for i := range tiers {
		label := tiers[i].Label
		if tier != "" && label != tier {
			continue
		}

		report, err := a.CapacityReport(ctx, label)
		if err != nil {
			return jobWaits{}, err
		}

		if report.Listener.Waiting > 0 {
			out.Waiting = append(out.Waiting, stillWaiting{Tier: label, Jobs: report.Listener.Waiting,
				Oldest: report.Listener.WaitingSince})
		}
	}

	return out, nil
}

// renderJobWaits writes the report for a person.
func renderJobWaits(w io.Writer, r jobWaits) {
	fmt.Fprintf(w, "jobs that started since %s\n", r.Since.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "wait: GitHub's queue time to billet's start; at github: queued to this scale set's "+
		"assignment (GitHub's clock); for room: that assignment to billet buying the lease (both clocks; "+
		"negative when the room was bought first); to start: the purchase to the start (billet's clock)")

	if r.Partial {
		fmt.Fprintln(w, "PARTIAL: the window holds more jobs than one report reads; shorten --since")
	}

	if len(r.Groups) == 0 {
		fmt.Fprintln(w, "\nno job started in the window")
	}

	for i := range r.Groups {
		g := &r.Groups[i]

		shape := "shape not recorded"
		if g.VCPU > 0 {
			shape = fmt.Sprintf("%d vCPU", g.VCPU)
		}

		fmt.Fprintf(w, "\n%s, %s: %d started", g.Tier, shape, g.Started)

		if g.Unknown > 0 {
			fmt.Fprintf(w, ", %d with no GitHub queue time on record (unknown, not counted)", g.Unknown)
		}

		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %-10s %6s %9s %9s %9s %9s %9s %9s %9s  %s\n",
			"", "jobs", "mean", "p50", "p75", "p90", "p99", "p99.99", "longest", "lease")

		for _, p := range waitParts {
			s := p.of(g)
			if s.Count == 0 {
				fmt.Fprintf(w, "  %-10s %6d  not recorded\n", p.name, 0)

				continue
			}

			fmt.Fprintf(w, "  %-10s %6d %9s %9s %9s %9s %9s %9s %9s  %s\n", p.name, s.Count,
				waitText(s.Mean), waitText(s.P50), waitText(s.P75), waitText(s.P90), waitText(s.P99),
				waitText(s.P9999), waitText(s.Longest), s.LongestLease)

			if s.Negative > 0 {
				fmt.Fprintf(w, "  %-10s %6s %d below zero, down to %s: %s\n",
					"", "", s.Negative, waitText(s.Shortest), p.below)
			}
		}
	}

	for _, q := range r.Waiting {
		fmt.Fprintf(w, "\nstill waiting: %s, %d job(s) for room, oldest since %s (the listener's last report)\n",
			q.Tier, q.Jobs, orUnknown(q.Oldest))
	}
}

// waitedLine is one job's wait and its parts, each "unknown" where an end of it
// was not recorded.
func waitedLine(rec alloc.JobRecord) string {
	w := rec.Waited()
	text := func(d time.Duration, ok bool) string {
		if !ok {
			return "unknown"
		}

		return waitText(d)
	}

	return fmt.Sprintf("%s (at github %s, for room %s, to start %s)", text(w.Wait()),
		text(w.AtGitHub()), text(w.ForRoom()), text(w.ToStart()))
}

// waitText is a wait to the second, or to a tenth under ten seconds, signed.
func waitText(d time.Duration) string {
	if d > -10*time.Second && d < 10*time.Second {
		return d.Round(100 * time.Millisecond).String()
	}

	return d.Round(time.Second).String()
}

// The JSON report. Every duration is in seconds, and a part no job could be
// measured for is null rather than a summary of zeros.
type jobWaitsJSON struct {
	Since   string             `json:"since"`
	Clocks  string             `json:"clocks"`
	Partial bool               `json:"partial"`
	Groups  []waitGroupJSON    `json:"groups"`
	Waiting []stillWaitingJSON `json:"still_waiting"`
}

type waitGroupJSON struct {
	Tier     string           `json:"tier"`
	VCPU     int              `json:"vcpu"`
	Started  int              `json:"started"`
	Unknown  int              `json:"no_queue_time"`
	Wait     *waitSummaryJSON `json:"wait"`
	AtGitHub *waitSummaryJSON `json:"at_github"`
	ForRoom  *waitSummaryJSON `json:"for_room"`
	ToStart  *waitSummaryJSON `json:"to_start"`
}

type waitSummaryJSON struct {
	Count        int     `json:"count"`
	Mean         float64 `json:"mean_seconds"`
	P50          float64 `json:"p50_seconds"`
	P75          float64 `json:"p75_seconds"`
	P90          float64 `json:"p90_seconds"`
	P99          float64 `json:"p99_seconds"`
	P9999        float64 `json:"p9999_seconds"`
	Longest      float64 `json:"longest_seconds"`
	LongestLease string  `json:"longest_lease"`
	Shortest     float64 `json:"shortest_seconds"`
	Negative     int     `json:"negative"`
}

type stillWaitingJSON struct {
	Tier   string `json:"tier"`
	Jobs   int    `json:"jobs"`
	Oldest string `json:"oldest_since"`
}

func summaryJSON(s alloc.WaitSummary) *waitSummaryJSON {
	if s.Count == 0 {
		return nil
	}

	return &waitSummaryJSON{Count: s.Count, Mean: s.Mean.Seconds(), P50: s.P50.Seconds(),
		P75: s.P75.Seconds(), P90: s.P90.Seconds(), P99: s.P99.Seconds(), P9999: s.P9999.Seconds(),
		Longest: s.Longest.Seconds(), LongestLease: s.LongestLease, Shortest: s.Shortest.Seconds(),
		Negative: s.Negative}
}

func renderJobWaitsJSON(w io.Writer, r jobWaits) error {
	out := jobWaitsJSON{
		Since:   r.Since.UTC().Format(time.RFC3339Nano),
		Clocks:  "queue time and scale-set assignment are GitHub's clock; purchase and start are billet's",
		Partial: r.Partial, Groups: []waitGroupJSON{}, Waiting: []stillWaitingJSON{},
	}

	for i := range r.Groups {
		g := &r.Groups[i]
		out.Groups = append(out.Groups, waitGroupJSON{Tier: g.Tier, VCPU: g.VCPU, Started: g.Started,
			Unknown: g.Unknown, Wait: summaryJSON(g.Wait), AtGitHub: summaryJSON(g.AtGitHub),
			ForRoom: summaryJSON(g.ForRoom), ToStart: summaryJSON(g.ToStart)})
	}

	for _, q := range r.Waiting {
		out.Waiting = append(out.Waiting, stillWaitingJSON(q))
	}

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the queue-wait report: %w", err)
	}

	if _, err := fmt.Fprintf(w, "%s\n", body); err != nil {
		return fmt.Errorf("write the queue-wait report: %w", err)
	}

	return nil
}
