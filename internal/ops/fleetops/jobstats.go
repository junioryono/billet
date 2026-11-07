package fleetops

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

// The ways `billet jobs stats` groups jobs.
const (
	statsByRepository = "repository"
	statsByWorkflow   = "workflow"
	statsByJob        = "job"
	statsByTier       = "tier"
	statsByNode       = "node"
)

var statsGroupings = []string{statsByRepository, statsByWorkflow, statsByJob, statsByTier, statsByNode}

// GitHub's result words the report names apart. resultSucceeded is alloc's, read
// through JobFact.Succeeded; any other word is printed as it is.
const (
	statsResultFailed   = "failed"
	statsResultCanceled = "canceled"
)

// defaultReportWindow is how far back the two reports over many jobs read.
const defaultReportWindow = 7 * 24 * time.Hour

// maxReportDays bounds a window in days, where one more digit would overflow a
// duration and read as a window that ends before it begins.
const maxReportDays = 36500

// addSinceFlag defines --since, which takes days as well as a Go duration, since
// a week is the window these reports are read over and `168h` is nobody's word
// for it.
func addSinceFlag(fs *flag.FlagSet, since *time.Duration) {
	fs.Func("since", "report the jobs that started this far back, as 7d or 36h (default 7d)",
		func(s string) error {
			d, err := parseReportWindow(s)
			if err != nil {
				return err
			}
			*since = d

			return nil
		})
}

// parseReportWindow reads a window as whole days or a Go duration. Anything not
// positive is refused rather than read as an empty report.
func parseReportWindow(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > maxReportDays {
			return 0, fmt.Errorf("%q is not between 1d and %dd", s, maxReportDays)
		}

		return time.Duration(n) * 24 * time.Hour, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is neither whole days, like 7d, nor a duration, like 36h", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q is not positive", s)
	}

	return d, nil
}

// cmdJobsStats reports what jobs did over a window, per group, against the window
// before it of the same length.
func cmdJobsStats(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs stats", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	since := defaultReportWindow
	addSinceFlag(fs, &since)
	by := fs.String("by", statsByRepository, "group by "+strings.Join(statsGroupings, ", "))
	repo := fs.String("repo", "", "report only this repository, as owner/name")
	asJSON := fs.Bool("json", false, "print the report as one JSON object")
	asHTML := fs.Bool("html", false, "print the report as one self-contained HTML page")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	switch {
	case *asJSON && *asHTML:
		return errors.New("billet jobs stats: --json and --html each print the whole report; choose one")
	case !slices.Contains(statsGroupings, *by):
		return fmt.Errorf("billet jobs stats: --by takes one of %s, not %q", strings.Join(statsGroupings, ", "), *by)
	}
	if *repo != "" {
		// REFUSED RATHER THAN READ AS A REPOSITORY THAT RAN NOTHING, so a bare name
		// is not an empty report.
		if _, _, ok := config.SplitRepository(*repo); !ok {
			return fmt.Errorf("billet jobs stats: --repo takes owner/name, not %q", *repo)
		}
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	report, err := buildJobStats(ctx, a, statsQuery{By: *by, Repo: *repo, Window: since, Now: time.Now()})
	if err != nil {
		return err
	}

	switch {
	case *asJSON:
		return renderJobStatsJSON(env.Stdout, report)
	case *asHTML:
		return renderJobStatsPage(env.Stdout, report)
	}
	renderJobStats(env.Stdout, report)

	return nil
}

// statsQuery is what one report is asked for.
type statsQuery struct {
	By, Repo string
	Window   time.Duration
	Now      time.Time
}

// jobStats is one report: every group that started a job in either window.
type jobStats struct {
	By, Repo                  string
	From, Until, PreviousFrom time.Time
	Partial, PreviousPartial  bool
	Groups                    []statsGroup
}

// statsGroup is one group's figures in the window and the one before it. Workflow
// and Job are set when it is grouped by job, and Key alone otherwise.
type statsGroup struct {
	Key, Workflow, Job string
	Current, Previous  statsFigures
}

// statsFigures is what one group did in one window.
//
// EVERY JOB IS IN EXACTLY ONE OF Succeeded, Attributed, Failed, Canceled, Other
// and NoResult, so they add up to Jobs. Rates are over the jobs with an outcome,
// which is all of them but NoResult.
type statsFigures struct {
	Jobs      int
	Succeeded int
	// Attributed did not succeed while billet's own infrastructure was disrupted
	// or its lease failed: two facts, not a verdict. Failed is the rest of the
	// failures, the job's own.
	Attributed int
	Failed     int
	Canceled   int
	// Other is a word GitHub used that this binary does not know, by word.
	Other    map[string]int
	NoResult int
	// WithAttempt is how many jobs have their run attempt recorded. Judged is how
	// many of a job's runs could be told flaky or not, and Flaky how many were.
	WithAttempt int
	Judged      int
	Flaky       int
	// Duration is over the jobs with both ends recorded; NoDuration counts the rest.
	Duration   alloc.DurationSummary
	NoDuration int
	// Wait is the queue wait over the jobs with a GitHub queue time; NoQueueTime
	// counts the rest.
	Wait        alloc.WaitSummary
	NoQueueTime int
	// VCPUHours is the charged vCPUs over the job's duration, for VCPUJobs jobs.
	VCPUHours float64
	VCPUJobs  int
	// Energy is per source, since a package's model and a wall meter measure
	// different things and a sum of them is neither. EnergyUnmeasured counts the
	// jobs with no energy measured.
	Energy           []statsEnergy
	EnergyUnmeasured int
	// CostUSD is the charged price over the duration of the CostJobs jobs whose
	// lease bought a shape at a recorded price. Bought counts the jobs whose lease
	// bought a shape at all, so the rest of them have a cost nobody recorded.
	CostUSD  float64
	CostJobs int
	Bought   int
}

// statsEnergy is what one source measured for a group's jobs.
type statsEnergy struct {
	Source            string
	ActiveMicrojoules int64
	IdleMicrojoules   int64
	Jobs              int
}

// outcomes is the jobs with an outcome, which the rates are over.
func (f *statsFigures) outcomes() int { return f.Jobs - f.NoResult }

// buildJobStats reads both windows and groups them. A job belongs to the window
// its start falls in.
func buildJobStats(ctx context.Context, a *alloc.Allocator, q statsQuery) (jobStats, error) {
	from := q.Now.Add(-q.Window)
	previousFrom := from.Add(-q.Window)

	current, partial, err := a.JobFacts(ctx, from, q.Now)
	if err != nil {
		return jobStats{}, err
	}
	previous, previousPartial, err := a.JobFacts(ctx, previousFrom, from)
	if err != nil {
		return jobStats{}, err
	}

	out := jobStats{By: q.By, Repo: q.Repo, From: from, Until: q.Now, PreviousFrom: previousFrom,
		Partial: partial, PreviousPartial: previousPartial}
	current, previous = factsOfRepo(current, q.Repo), factsOfRepo(previous, q.Repo)
	out.Groups = statsGroupsOf(q.By, from, current, previous)

	return out, nil
}

func factsOfRepo(facts []alloc.JobFact, repo string) []alloc.JobFact {
	if repo == "" {
		return facts
	}

	return slices.DeleteFunc(facts, func(f alloc.JobFact) bool { return !strings.EqualFold(f.Repo, repo) })
}

// statsKey is the group one job belongs to. A job name is the workflow's to choose
// and may hold anything, so a job's key joins its two parts on a byte no name
// holds.
func statsKey(by string, f *alloc.JobFact) string {
	switch by {
	case statsByWorkflow:
		return f.Workflow()
	case statsByJob:
		return f.Workflow() + "\x00" + f.JobName
	case statsByTier:
		return f.Tier
	case statsByNode:
		return f.Node
	default:
		return f.Repo
	}
}

// statsGroupsOf groups two windows' jobs and sorts the groups by the vCPU-hours of
// the window read, the largest first, because which group uses the fleet is the
// first question and a name is no order for it.
func statsGroupsOf(by string, from time.Time, current, previous []alloc.JobFact) []statsGroup {
	type facts struct{ current, previous []alloc.JobFact }

	grouped := map[string]*facts{}
	of := func(key string) *facts {
		if grouped[key] == nil {
			grouped[key] = &facts{}
		}

		return grouped[key]
	}
	for i := range current {
		g := of(statsKey(by, &current[i]))
		g.current = append(g.current, current[i])
	}
	for i := range previous {
		g := of(statsKey(by, &previous[i]))
		g.previous = append(g.previous, previous[i])
	}

	flakes := judgeFlakes(by, from, current, previous)

	out := make([]statsGroup, 0, len(grouped))
	for key, g := range grouped {
		group := statsGroup{Key: key}
		if by == statsByJob {
			group.Workflow, group.Job, _ = strings.Cut(key, "\x00")
		}
		group.Current = figuresOf(g.current)
		group.Previous = figuresOf(g.previous)
		group.Current.Judged, group.Current.Flaky = flakes[flakeTally{key, false}].judged, flakes[flakeTally{key, false}].flaky
		group.Previous.Judged, group.Previous.Flaky = flakes[flakeTally{key, true}].judged, flakes[flakeTally{key, true}].flaky
		out = append(out, group)
	}
	slices.SortFunc(out, func(x, y statsGroup) int {
		return cmp.Or(cmp.Compare(y.Current.VCPUHours, x.Current.VCPUHours),
			cmp.Compare(y.Previous.VCPUHours, x.Previous.VCPUHours), cmp.Compare(x.Key, y.Key))
	})

	return out
}

// figuresOf is one group's figures in one window, its flakes apart.
func figuresOf(facts []alloc.JobFact) statsFigures {
	out := statsFigures{Jobs: len(facts)}
	durations := make([]alloc.Exemplar, 0, len(facts))
	waits := make([]alloc.JobWait, 0, len(facts))
	energy := map[string]*statsEnergy{}

	for i := range facts {
		f := &facts[i]

		switch {
		case f.Succeeded():
			out.Succeeded++
		case f.Attributed():
			out.Attributed++
		case f.Result == statsResultFailed:
			out.Failed++
		case f.Result == statsResultCanceled:
			out.Canceled++
		case f.Result == "":
			out.NoResult++
		default:
			if out.Other == nil {
				out.Other = map[string]int{}
			}
			out.Other[f.Result]++
		}

		if f.RunAttempt > 0 {
			out.WithAttempt++
		}

		waits = append(waits, f.Wait())

		d, ok := f.Duration()
		if !ok {
			out.NoDuration++
		} else {
			durations = append(durations, alloc.Exemplar{Value: d, Lease: f.LeaseID})
			if f.VCPU > 0 {
				out.VCPUHours += float64(f.VCPU) * d.Hours()
				out.VCPUJobs++
			}
			// A ZERO PRICE IS NOT A PRICE: a host-backed lease buys nothing, and a
			// remote row from before the column has a shape and a zero.
			if f.PriceMicrosPerHour > 0 {
				out.CostUSD += float64(f.PriceMicrosPerHour) / 1e6 * d.Hours()
				out.CostJobs++
			}
		}
		if f.InstanceType != "" {
			out.Bought++
		}

		if f.Usage == nil || !f.Usage.Measured(alloc.UsageEnergy) {
			out.EnergyUnmeasured++

			continue
		}
		e := energy[f.Usage.EnergySource]
		if e == nil {
			e = &statsEnergy{Source: f.Usage.EnergySource}
			energy[f.Usage.EnergySource] = e
		}
		e.ActiveMicrojoules += f.Usage.EnergyActiveMicrojoules
		e.IdleMicrojoules += f.Usage.EnergyIdleMicrojoules
		e.Jobs++
	}

	out.Duration = alloc.SummarizeDurations(durations)
	out.Wait = alloc.Summarize(waits, alloc.JobWait.Wait)
	out.NoQueueTime = out.Jobs - out.Wait.Count
	for _, e := range energy {
		out.Energy = append(out.Energy, *e)
	}
	slices.SortFunc(out.Energy, func(x, y statsEnergy) int { return cmp.Compare(x.Source, y.Source) })

	return out
}

// flakeTally names one group in one window.
type flakeTally struct {
	key      string
	previous bool
}

// flakeCount is how many of one group's job runs could be told, and how many were
// flaky.
type flakeCount struct{ judged, flaky int }

// flakeRun is one job of one workflow run, across its attempts.
type flakeRun struct {
	repo, workflow, job string
	run                 int64
}

// judgeFlakes counts each group's flaky job runs, reading the two windows as one,
// because a job that failed on the last day of one and passed on the first of the
// next is one flake. A job run belongs to the group and window of its newest
// attempt, the one that says how it ended.
//
// FLAKY IS A JOB THAT FAILED AND THEN PASSED ON A RE-RUN OF THE SAME RUN, which is
// the same commit. Each of the following cannot be told, and is left out of what
// was judged rather than counted as passing or as flaky:
//   - an attempt is not recorded;
//   - two jobs of one attempt share a name, so neither is known to be the other's
//     re-run;
//   - the job has no name, no run, or no workflow.
//
// A FAILURE BILLET'S OWN LIFECYCLE HAS SOMETHING TO SAY ABOUT DOES NOT COUNT: a
// job whose host was lost and which then passed is not the job's flake.
func judgeFlakes(by string, from time.Time, current, previous []alloc.JobFact) map[flakeTally]flakeCount {
	runs := map[flakeRun][]*alloc.JobFact{}
	add := func(facts []alloc.JobFact) {
		for i := range facts {
			f := &facts[i]
			if f.RunID <= 0 || f.JobName == "" || f.Workflow() == "" {
				continue
			}
			k := flakeRun{f.Repo, f.Workflow(), f.JobName, f.RunID}
			runs[k] = append(runs[k], f)
		}
	}
	add(current)
	add(previous)

	out := map[flakeTally]flakeCount{}
	for _, attempts := range runs {
		verdict, judged := flakyAcross(attempts)
		if !judged {
			continue
		}

		newest := slices.MaxFunc(attempts, func(x, y *alloc.JobFact) int {
			return cmp.Compare(x.RunAttempt, y.RunAttempt)
		})
		tally := flakeTally{statsKey(by, newest), newest.StartedAt.Before(from)}
		count := out[tally]
		count.judged++
		if verdict {
			count.flaky++
		}
		out[tally] = count
	}

	return out
}

// flakyAcross says whether a job's attempts failed and then passed, and whether
// they could be told at all.
func flakyAcross(attempts []*alloc.JobFact) (bool, bool) {
	sorted := slices.Clone(attempts)
	slices.SortFunc(sorted, func(x, y *alloc.JobFact) int { return cmp.Compare(x.RunAttempt, y.RunAttempt) })

	// THE FIRST ATTEMPT MUST BE ON RECORD: one that ran elsewhere, or before both
	// windows, may be the failure a later pass makes a flake.
	if sorted[0].RunAttempt != 1 {
		return false, false
	}
	for i, f := range sorted {
		if f.RunAttempt <= 0 || (i > 0 && sorted[i-1].RunAttempt == f.RunAttempt) {
			return false, false
		}
	}

	// AN ATTEMPT WITH NO RESULT YET DECIDES NOTHING, and the run is judged only once a
	// flake is already there or every attempt has ended: a failure followed by a
	// re-run still going is not a run that stayed failed.
	failed := false
	for _, f := range sorted {
		switch {
		case f.Result == "":
			return false, false
		case f.Result == statsResultFailed && !f.Attributed():
			failed = true
		case f.Succeeded() && failed:
			return true, true
		}
	}

	return false, true
}

// statsCovers says what every figure covers, in the report itself, so a number
// read away from the docs still says what it is.
const statsCovers = "Covers jobs GitHub started on a runner of this deployment: a job it never started " +
	"has no row here. A job belongs to the window its start falls in. Rates are over the jobs with an " +
	"outcome. Duration runs from billet's record of the start to its record of GitHub's result, without " +
	"the lease's teardown; each percentile is nearest rank and names the lease that holds it, for " +
	"`billet jobs show`. Queue wait runs from GitHub's queue time (GitHub's clock) to billet's start. " +
	"billet's counts the jobs that did not succeed while billet's own infrastructure was disrupted or " +
	"their lease failed: two facts, not a verdict. Flaky is a job that failed and then passed on a re-run " +
	"of the same run, which is the same commit, counted over the job runs whose attempts are all " +
	"recorded; both attempts must have run on this deployment. vCPU-hours is the charged vCPUs over the " +
	"duration. Energy is what the host measured, per source, never summed across sources. Cloud cost is " +
	"the price the lease recorded when it bought a shape, over the duration."

// statsRow is one figure as every form of the report words it, in this window
// and the one before. Unmeasured marks a figure there is no number for.
type statsRow struct {
	Label, Current, Previous string
	Unmeasured               bool
}

// statsRows words one group's figures. The text report and the page both print
// these, so a figure cannot read one way in one and another way in the other.
func statsRows(g *statsGroup) []statsRow {
	c, p := &g.Current, &g.Previous
	jobs := statsRow{Label: "jobs", Current: strconv.Itoa(c.Jobs), Previous: strconv.Itoa(p.Jobs)}
	if c.Jobs > 0 && p.Jobs > 0 {
		jobs.Previous += " (" + percentChange(float64(c.Jobs), float64(p.Jobs)) + ")"
	}

	rows := []statsRow{jobs,
		outcomeRow("succeeded", c.Succeeded, p.Succeeded, c, p),
		outcomeRow("failed", c.Failed, p.Failed, c, p),
		outcomeRow("canceled", c.Canceled, p.Canceled, c, p),
		outcomeRow("billet's", c.Attributed, p.Attributed, c, p),
	}
	if len(c.Other) > 0 || len(p.Other) > 0 {
		rows = append(rows, statsRow{Label: "other", Current: otherWords(c.Other), Previous: otherWords(p.Other)})
	}
	if c.NoResult > 0 || p.NoResult > 0 {
		rows = append(rows, statsRow{Label: "no result", Current: strconv.Itoa(c.NoResult),
			Previous: strconv.Itoa(p.NoResult)})
	}

	return append(rows,
		statsRow{Label: "flaky", Current: flakyText(c), Previous: flakyText(p), Unmeasured: c.Judged == 0},
		statsRow{Label: "duration", Current: durationText(c), Previous: durationText(p), Unmeasured: c.Duration.Count == 0},
		statsRow{Label: "queue wait", Current: waitText(c), Previous: waitText(p), Unmeasured: c.Wait.Count == 0},
		statsRow{Label: "vCPU-hours", Current: vcpuHoursText(c), Previous: vcpuHoursText(p), Unmeasured: c.VCPUJobs == 0},
		statsRow{Label: "energy", Current: energyText(c), Previous: energyText(p), Unmeasured: len(c.Energy) == 0},
		statsRow{Label: "cloud cost", Current: costText(c), Previous: costText(p), Unmeasured: c.CostJobs == 0},
	)
}

// outcomeRow is one outcome's share of the jobs with an outcome, and how far the
// share moved in points.
func outcomeRow(label string, current, previous int, c, p *statsFigures) statsRow {
	row := statsRow{Label: label, Current: "no job has an outcome", Previous: "no job has an outcome"}
	if c.outcomes() > 0 {
		row.Current = fmt.Sprintf("%.1f%% (%d)", share(current, c.outcomes()), current)
	}
	if p.outcomes() > 0 {
		row.Previous = fmt.Sprintf("%.1f%% (%d)", share(previous, p.outcomes()), previous)
	}
	if c.outcomes() > 0 && p.outcomes() > 0 {
		row.Previous += fmt.Sprintf(" (%+.1f points)", share(current, c.outcomes())-share(previous, p.outcomes()))
	}

	return row
}

func share(part, whole int) float64 { return 100 * float64(part) / float64(whole) }

// percentChange says how far current moved from previous.
func percentChange(current, previous float64) string {
	return fmt.Sprintf("%+.1f%%", 100*(current-previous)/previous)
}

// otherWords lists the result words this binary does not know, quoted, since
// GitHub chose them.
func otherWords(words map[string]int) string {
	if len(words) == 0 {
		return "none"
	}

	names := make([]string, 0, len(words))
	for word := range words {
		names = append(names, word)
	}
	slices.Sort(names)

	parts := make([]string, 0, len(names))
	for _, word := range names {
		parts = append(parts, fmt.Sprintf("%s (%d)", strconv.Quote(word), words[word]))
	}

	return strings.Join(parts, ", ")
}

func flakyText(f *statsFigures) string {
	if f.Judged == 0 {
		return fmt.Sprintf("not recorded (attempt recorded for %d of %d jobs)", f.WithAttempt, f.Jobs)
	}

	return fmt.Sprintf("%.1f%% (%d of %d job runs; attempt recorded for %d of %d jobs)",
		share(f.Flaky, f.Judged), f.Flaky, f.Judged, f.WithAttempt, f.Jobs)
}

func durationText(f *statsFigures) string {
	if f.Duration.Count == 0 {
		return fmt.Sprintf("not recorded for any of %d jobs", f.Jobs)
	}

	text := fmt.Sprintf("p50 %s on %s, p90 %s on %s, p99 %s on %s (%d jobs", WaitText(f.Duration.P50.Value),
		f.Duration.P50.Lease, WaitText(f.Duration.P90.Value), f.Duration.P90.Lease,
		WaitText(f.Duration.P99.Value), f.Duration.P99.Lease, f.Duration.Count)
	if f.NoDuration > 0 {
		text += fmt.Sprintf("; %d more not recorded", f.NoDuration)
	}

	return text + ")"
}

func waitText(f *statsFigures) string {
	if f.Wait.Count == 0 {
		return fmt.Sprintf("not recorded for any of %d jobs", f.Jobs)
	}

	text := fmt.Sprintf("p99 %s (%d jobs", WaitText(f.Wait.P99), f.Wait.Count)
	if f.NoQueueTime > 0 {
		text += fmt.Sprintf("; %d with no GitHub queue time", f.NoQueueTime)
	}

	return text + ")"
}

func vcpuHoursText(f *statsFigures) string {
	if f.VCPUJobs == 0 {
		return fmt.Sprintf("not recorded for any of %d jobs", f.Jobs)
	}

	return fmt.Sprintf("%.1f over %d of %d jobs", f.VCPUHours, f.VCPUJobs, f.Jobs)
}

func energyText(f *statsFigures) string {
	if len(f.Energy) == 0 {
		return "not measured"
	}

	parts := make([]string, 0, len(f.Energy))
	for i := range f.Energy {
		e := &f.Energy[i]
		parts = append(parts, fmt.Sprintf("%s over %d jobs", energyOfSource(e), e.Jobs))
	}
	text := strings.Join(parts, "; ")
	if f.EnergyUnmeasured > 0 {
		text += fmt.Sprintf("; not measured for %d jobs", f.EnergyUnmeasured)
	}

	return text
}

// energyOfSource words one source's energy as `billet jobs show` words a job's: only
// a source that splits it says what was idle.
func energyOfSource(e *statsEnergy) string {
	switch e.Source {
	case alloc.EnergyRAPL:
		return fmt.Sprintf("rapl: %s active, %s idle (package RAPL, a model rather than a meter)",
			joules(e.ActiveMicrojoules), joules(e.IdleMicrojoules))
	case alloc.EnergyMeter:
		return fmt.Sprintf("meter: %s active, %s idle (the whole host at the wall)", joules(e.ActiveMicrojoules),
			joules(e.IdleMicrojoules))
	case alloc.EnergyProcess:
		return fmt.Sprintf("process: %s (macOS's own estimate for the VM's process; the machine's idle draw is not in it)",
			joules(e.ActiveMicrojoules))
	case alloc.EnergyRAPLUnsplit:
		return fmt.Sprintf("rapl-unsplit: %s (package RAPL with no idle baseline, so idle is inside it)",
			joules(e.ActiveMicrojoules))
	case "":
		return fmt.Sprintf("source not recorded: %s", joules(e.ActiveMicrojoules))
	}

	return fmt.Sprintf("%s: %s, split by a source this binary does not know", strconv.Quote(e.Source),
		joules(e.ActiveMicrojoules))
}

// energySplit says whether a source's idle figure is one: the others report a total
// with idle in it or none at all.
func energySplit(source string) bool {
	return source == alloc.EnergyRAPL || source == alloc.EnergyMeter
}

// costText says what the bought shapes cost. NOTHING BOUGHT IS SAID ONLY WHEN NO LEASE
// BOUGHT A SHAPE: a bought one whose price or duration was not recorded has a cost
// nobody can say, never none.
func costText(f *statsFigures) string {
	switch {
	case f.Bought == 0 && f.CostJobs == 0:
		return "no bought shape"
	case f.CostJobs == 0:
		return fmt.Sprintf("not recorded for the %d jobs on a bought shape (no price or no duration)", f.Bought)
	}

	text := fmt.Sprintf("$%.2f over %d of %d jobs", f.CostUSD, f.CostJobs, f.Jobs)
	if missing := f.Bought - f.CostJobs; missing > 0 {
		text += fmt.Sprintf("; not recorded for %d more on a bought shape", missing)
	}

	return text
}

// groupHeading names a group. EVERY STRING GITHUB OR A WORKFLOW CHOSE IS QUOTED,
// so a name holding a newline cannot forge a line of the report.
func groupHeading(by string, g *statsGroup) string {
	quoted := func(s string) string {
		if s == "" {
			return "not recorded"
		}

		return strconv.Quote(s)
	}
	if by == statsByJob {
		return quoted(g.Workflow) + " " + quoted(g.Job)
	}

	return quoted(g.Key)
}

func statsScope(r *jobStats) string {
	scope := "by " + r.By
	if r.Repo != "" {
		scope += " in " + strconv.Quote(r.Repo)
	}

	return scope
}

// statsTime is a window's end as the report prints it.
func statsTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// statsPartial says which windows left jobs out, empty when neither did.
func statsPartial(r *jobStats) string {
	switch {
	case r.Partial && r.PreviousPartial:
		return "PARTIAL: both windows hold more jobs than one report reads, and their oldest were left out; shorten --since"
	case r.Partial:
		return "PARTIAL: this window holds more jobs than one report reads, and its oldest were left out; shorten --since"
	case r.PreviousPartial:
		return "PARTIAL: the previous window holds more jobs than one report reads, and its oldest were left out, " +
			"so what it is compared with is short; shorten --since"
	}

	return ""
}

// renderJobStats writes the report for a person.
func renderJobStats(w io.Writer, r jobStats) {
	fmt.Fprintf(w, "jobs that started from %s to %s, %s, against %s to %s\n", statsTime(r.From), statsTime(r.Until),
		statsScope(&r), statsTime(r.PreviousFrom), statsTime(r.From))
	fmt.Fprintln(w, statsCovers)
	if partial := statsPartial(&r); partial != "" {
		fmt.Fprintln(w, partial)
	}
	if len(r.Groups) == 0 {
		fmt.Fprintln(w, "\nno job started in either window")
	}

	for i := range r.Groups {
		g := &r.Groups[i]
		fmt.Fprintf(w, "\n%s\n", groupHeading(r.By, g))
		for _, row := range statsRows(g) {
			fmt.Fprintf(w, "  %-11s%s\n", row.Label, row.Current)
			fmt.Fprintf(w, "  %-11sprevious: %s\n", "", row.Previous)
		}
	}
}

// The job statistics page's template and style sheet, embedded so the page is one
// file that fetches nothing.
var (
	//go:embed jobstats/page.html.tmpl
	jobStatsPageSource string
	//go:embed jobstats/page.css
	jobStatsPageStyle string

	jobStatsPageTemplate = template.Must(template.New("page").Parse(jobStatsPageSource))
)

// jobStatsPagePolicy is the page's Content-Security-Policy: nothing may be fetched
// and no script runs at all, so no string a workflow chose can become one.
const jobStatsPagePolicy = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

type statsPageGroup struct {
	Heading string
	Rows    []statsRow
}

// statsPageDocument is what the template renders, already worded.
type statsPageDocument struct {
	Policy          string
	Style           template.CSS
	Heading, Covers string
	Partial         string
	Groups          []statsPageGroup
}

// renderJobStatsPage writes the report as one HTML document. Every string in it is
// escaped by the template, and the style is the embedded one.
func renderJobStatsPage(w io.Writer, r jobStats) error {
	doc := statsPageDocument{
		Policy: jobStatsPagePolicy,
		Style:  template.CSS(jobStatsPageStyle),
		Heading: fmt.Sprintf("Jobs that started from %s to %s, %s, against %s to %s", statsTime(r.From),
			statsTime(r.Until), statsScope(&r), statsTime(r.PreviousFrom), statsTime(r.From)),
		Covers: statsCovers, Partial: statsPartial(&r),
	}
	for i := range r.Groups {
		g := &r.Groups[i]
		doc.Groups = append(doc.Groups, statsPageGroup{Heading: groupHeading(r.By, g), Rows: statsRows(g)})
	}
	if err := jobStatsPageTemplate.Execute(w, doc); err != nil {
		return fmt.Errorf("render the job statistics page: %w", err)
	}

	return nil
}

// The JSON report. Every duration is in seconds, and a figure with nothing behind
// it is null, never zero.
type jobStatsJSON struct {
	From            string           `json:"from"`
	Until           string           `json:"until"`
	PreviousFrom    string           `json:"previous_from"`
	By              string           `json:"by"`
	Repo            string           `json:"repo,omitempty"`
	Covers          string           `json:"covers"`
	Partial         bool             `json:"partial"`
	PreviousPartial bool             `json:"previous_partial"`
	Groups          []statsGroupJSON `json:"groups"`
}

type statsGroupJSON struct {
	Key      string           `json:"key"`
	Workflow string           `json:"workflow,omitempty"`
	Job      string           `json:"job,omitempty"`
	Current  statsFiguresJSON `json:"current"`
	Previous statsFiguresJSON `json:"previous"`
}

type statsFiguresJSON struct {
	Jobs             int               `json:"jobs"`
	Succeeded        int               `json:"succeeded"`
	Failed           int               `json:"failed"`
	Canceled         int               `json:"canceled"`
	BilletAttributed int               `json:"billet_attributed"`
	Other            map[string]int    `json:"other"`
	NoResult         int               `json:"no_result"`
	SuccessRate      *float64          `json:"success_rate"`
	FailureRate      *float64          `json:"failure_rate"`
	CanceledRate     *float64          `json:"canceled_rate"`
	WithAttempt      int               `json:"jobs_with_attempt"`
	JudgedRuns       int               `json:"job_runs_judged"`
	FlakyRuns        int               `json:"job_runs_flaky"`
	FlakyRate        *float64          `json:"flaky_rate"`
	Duration         *statsDurations   `json:"duration"`
	NoDuration       int               `json:"no_duration"`
	QueueWaitP99     *float64          `json:"queue_wait_p99_seconds"`
	QueueWaitJobs    int               `json:"queue_wait_jobs"`
	NoQueueTime      int               `json:"no_queue_time"`
	VCPUHours        *float64          `json:"vcpu_hours"`
	VCPUHoursJobs    int               `json:"vcpu_hours_jobs"`
	Energy           []statsEnergyJSON `json:"energy"`
	EnergyUnmeasured int               `json:"energy_not_measured"`
	CostUSD          *float64          `json:"cloud_cost_usd"`
	CostJobs         int               `json:"cloud_cost_jobs"`
	BoughtJobs       int               `json:"bought_shape_jobs"`
}

type statsDurations struct {
	Count    int     `json:"count"`
	P50      float64 `json:"p50_seconds"`
	P50Lease string  `json:"p50_lease"`
	P90      float64 `json:"p90_seconds"`
	P90Lease string  `json:"p90_lease"`
	P99      float64 `json:"p99_seconds"`
	P99Lease string  `json:"p99_lease"`
}

// statsEnergyJSON is one source's energy. IdleJoules is null for a source that does
// not split idle out, and IdleIncluded says whether the active figure has the idle in
// it, null for a source this binary does not know.
type statsEnergyJSON struct {
	Source       string   `json:"source"`
	ActiveJoules float64  `json:"active_joules"`
	IdleJoules   *float64 `json:"idle_joules"`
	IdleIncluded *bool    `json:"idle_included"`
	Jobs         int      `json:"jobs"`
}

// idleIncluded says whether a source's one figure has the machine's idle draw in it,
// nil for a source this binary does not know.
func idleIncluded(source string) *bool {
	var included bool
	switch source {
	case alloc.EnergyRAPLUnsplit:
		included = true
	case alloc.EnergyRAPL, alloc.EnergyMeter, alloc.EnergyProcess:
	default:
		return nil
	}

	return &included
}

// idleJoules is a source's idle energy, nil for one that reports none of its own.
func idleJoules(e *statsEnergy) *float64 {
	if !energySplit(e.Source) {
		return nil
	}
	idle := float64(e.IdleMicrojoules) / 1e6

	return &idle
}

// rateOf is part over whole, null when whole is zero.
func rateOf(part, whole int) *float64 {
	if whole == 0 {
		return nil
	}

	rate := float64(part) / float64(whole)

	return &rate
}

func statsFiguresJSONOf(f *statsFigures) statsFiguresJSON {
	out := statsFiguresJSON{
		Jobs: f.Jobs, Succeeded: f.Succeeded, Failed: f.Failed, Canceled: f.Canceled,
		BilletAttributed: f.Attributed, Other: f.Other, NoResult: f.NoResult,
		SuccessRate: rateOf(f.Succeeded, f.outcomes()), FailureRate: rateOf(f.Failed, f.outcomes()),
		CanceledRate: rateOf(f.Canceled, f.outcomes()), WithAttempt: f.WithAttempt, JudgedRuns: f.Judged,
		FlakyRuns: f.Flaky, FlakyRate: rateOf(f.Flaky, f.Judged), NoDuration: f.NoDuration,
		QueueWaitJobs: f.Wait.Count, NoQueueTime: f.NoQueueTime, VCPUHoursJobs: f.VCPUJobs,
		Energy: []statsEnergyJSON{}, EnergyUnmeasured: f.EnergyUnmeasured, CostJobs: f.CostJobs,
		BoughtJobs: f.Bought,
	}
	if out.Other == nil {
		out.Other = map[string]int{}
	}
	if d := &f.Duration; d.Count > 0 {
		out.Duration = &statsDurations{Count: d.Count, P50: d.P50.Value.Seconds(), P50Lease: d.P50.Lease,
			P90: d.P90.Value.Seconds(), P90Lease: d.P90.Lease, P99: d.P99.Value.Seconds(), P99Lease: d.P99.Lease}
	}
	if f.Wait.Count > 0 {
		seconds := f.Wait.P99.Seconds()
		out.QueueWaitP99 = &seconds
	}
	if f.VCPUJobs > 0 {
		hours := f.VCPUHours
		out.VCPUHours = &hours
	}
	if f.CostJobs > 0 {
		cost := f.CostUSD
		out.CostUSD = &cost
	}
	for i := range f.Energy {
		e := &f.Energy[i]
		out.Energy = append(out.Energy, statsEnergyJSON{Source: e.Source,
			ActiveJoules: float64(e.ActiveMicrojoules) / 1e6, IdleJoules: idleJoules(e),
			IdleIncluded: idleIncluded(e.Source),
			Jobs:         e.Jobs})
	}

	return out
}

func renderJobStatsJSON(w io.Writer, r jobStats) error {
	out := jobStatsJSON{
		From: r.From.UTC().Format(time.RFC3339Nano), Until: r.Until.UTC().Format(time.RFC3339Nano),
		PreviousFrom: r.PreviousFrom.UTC().Format(time.RFC3339Nano), By: r.By, Repo: r.Repo, Covers: statsCovers,
		Partial: r.Partial, PreviousPartial: r.PreviousPartial, Groups: []statsGroupJSON{},
	}
	for i := range r.Groups {
		g := &r.Groups[i]
		key := g.Key
		if r.By == statsByJob {
			key = g.Workflow + " " + g.Job
		}
		out.Groups = append(out.Groups, statsGroupJSON{Key: key, Workflow: g.Workflow, Job: g.Job,
			Current: statsFiguresJSONOf(&g.Current), Previous: statsFiguresJSONOf(&g.Previous)})
	}

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the job statistics: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", body); err != nil {
		return fmt.Errorf("write the job statistics: %w", err)
	}

	return nil
}
