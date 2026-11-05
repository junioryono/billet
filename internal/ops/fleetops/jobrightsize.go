package fleetops

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/usage"
)

// The thresholds a recommendation is made by. They are printed in the report and
// stated in the docs, because a recommendation nobody can check is a guess. Each
// fraction is of the job's charged shape, or of the memory its guest saw.
const (
	// rightsizeMinSamples is the fewest measured runs a recommendation is made from,
	// and the fewest runs a memory figure is judged from.
	rightsizeMinSamples = 10
	// A confidence is low from the minimum, medium from rightsizeMediumSamples and
	// high from rightsizeHighSamples.
	rightsizeMediumSamples = 30
	rightsizeHighSamples   = 100
	// rightsizeCoverage is how much of a job's run its host series must cover for
	// the run to be measured; less is a run whose busiest part may be the missing
	// one.
	rightsizeCoverage = 0.90
	// rightsizeMemoryNear is the guest's memory use at p95 that asks for a larger
	// tier: a job this close to its limit is one larger input away from being killed.
	rightsizeMemoryNear = 0.90
	// rightsizeBusyHigh and rightsizePressureHigh are the CPU busy fraction at p90
	// and the share of the lease's life some task stalled for CPU at p90, either of
	// which says the job is CPU-bound.
	rightsizeBusyHigh     = 0.85
	rightsizePressureHigh = 0.25
	// rightsizeBusyIdle and rightsizeMemoryIdle are the fractions under which, both
	// together, a job is idle on its tier.
	rightsizeBusyIdle   = 0.30
	rightsizeMemoryIdle = 0.45
	// rightsizeTarget is how full a smaller tier may be at those percentiles, so a
	// recommendation does not move a job straight back to the edge.
	rightsizeTarget = 0.70
)

// The recommendations, in the words `billet migrate workflows --rightsize` reads.
const (
	rightsizeSmaller = "smaller"
	rightsizeLarger  = "larger"
	rightsizeKeep    = "keep"
	rightsizeNone    = "none"
)

// rightsizeSchema is the version of the JSON report, which another command reads
// back from a file.
const rightsizeSchema = 1

// rightsizeCovers says what the figures are, in the report itself.
//
// THE MEMORY A RECOMMENDATION READS IS THE GUEST'S, measured on the reference
// deployment on 2026-11-04: over 4,262 runs of 43 jobs the host's memory peak at p95
// ran from 50% to 284% of the job's memory and 0 of 7,360 jobs that week were OOM
// killed, because the peak counts the page cache and the VMM, while the guest's own
// highest use ran from 12% to 61%. The host peak only ever proves a job small.
//
// THE CPU IS THE HOST SERIES OVER THE JOB'S OWN WINDOW, because the usage summary's
// window is the lease's whole life, which on a pooled runner ran up to 1,600 times
// the job's duration the same week.
const rightsizeCovers = "Covers the jobs GitHub started on a runner of this deployment in the window, per " +
	"repository, workflow and job, a matrix's legs each its own job. The current tier is the tier of the " +
	"job's newest run, and only runs on it count, since a run on another shape says nothing about this one. " +
	"A canceled run, a run with no result and a run billet's own infrastructure disrupted are left out. A run " +
	"is measured when the host's per-sample series covers at least 90% of it, from GitHub starting it to its " +
	"result, which is kept for server.job_detail_retention. CPU busy is the job's cgroup CPU over that window " +
	"(the guest's vCPUs and its VMM's threads) over the charged vCPUs, so the VMM can take it past 100%. " +
	"Memory is what the job's own VM reported as the most it used, MemTotal less MemAvailable, over the memory " +
	"it saw, read only from a report the node kept whole; the job runs as root there, so it is labelled as the " +
	"VM's and only ever advises. The host's peak counts the page cache and the VMM, so it decides nothing but " +
	"that a job stayed small; an OOM kill is the host's. CPU pressure is over the lease's whole life. A " +
	"candidate tier is one that differs from the current tier only in its name and size: the same target, " +
	"guest OS, architecture, image, providers, trust, runner group, site and node, and a trusted tier must " +
	"allow the workflow. GitHub's runner group settings are not read, and may still refuse a repository."

// rightsizeThresholds words the thresholds for a person.
func rightsizeThresholds() string {
	return fmt.Sprintf("No recommendation under %d measured runs, or for a job not measured. Confidence: low from "+
		"%d runs, medium from %d, high from %d. Larger: any OOM kill (over the memory limit), memory reported by the "+
		"job's own VM at p95 at %.0f%% or more over %d runs or more (near the memory limit), or CPU busy at p90 at "+
		"%.0f%% or more or CPU pressure at p90 at %.0f%% or more (CPU-bound). Smaller: CPU busy at p90 at %.0f%% or "+
		"less and memory at p95 at %.0f%% or less (idle), memory either as the job's own VM reported it or as the "+
		"host's peak, to the smallest candidate these would fill to no more than %.0f%%. Otherwise keep.",
		rightsizeMinSamples, rightsizeMinSamples, rightsizeMediumSamples, rightsizeHighSamples,
		100*rightsizeMemoryNear, rightsizeMinSamples, 100*rightsizeBusyHigh, 100*rightsizePressureHigh,
		100*rightsizeBusyIdle, 100*rightsizeMemoryIdle, 100*rightsizeTarget)
}

// cmdJobsRightsize recommends a tier for each job from what it measurably used.
func cmdJobsRightsize(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs rightsize", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	since := defaultReportWindow
	addSinceFlag(fs, &since)
	asJSON := fs.Bool("json", false, "print the report as one JSON object, which billet migrate workflows "+
		"--rightsize reads")
	if err := cli.Parse(fs, args); err != nil {
		return err
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

	now := time.Now()
	report, err := buildRightsize(ctx, a, cfg.Tiers, now.Add(-since), now)
	if err != nil {
		return err
	}

	if *asJSON {
		return renderRightsizeJSON(env.Stdout, report)
	}
	renderRightsize(env.Stdout, report)

	return nil
}

// rightsizeReport is one report over one window.
type rightsizeReport struct {
	From, Until time.Time
	Partial     bool
	Jobs        []rightsizeJob
}

// rightsizeJob is one job's figures on its current tier and what they recommend.
// A figure with nothing behind it is nil, never zero.
type rightsizeJob struct {
	Repository, Workflow, Job string
	Tier                      string
	// RunsOn is the tier's name on its target, and VCPU and Memory the job's
	// charged shape, from its newest run; RunsOn is empty for a tier this
	// configuration does not hold.
	RunsOn string
	VCPU   int64
	Memory int64
	// Samples is the measured runs on the tier, which every figure but the host's
	// counts is over, and OtherTiers the runs on other tiers. The runs on the tier
	// that were not measured are counted by why: no usage report the host could
	// read CPU from, no series kept (its detail retired, or none sent), or a
	// series that does not cover the run.
	Samples, OtherTiers           int
	NoReport, NoSeries, Uncovered int
	BusyP90                       *float64
	// GuestRuns is how many samples have a report the node kept whole from the
	// job's own VM; GuestP95 is the most memory the VM said it used at p95, over
	// what it saw, and GuestNeedP95 the charged memory that use comes to.
	GuestRuns    int
	GuestP95     *float64
	GuestNeedP95 *int64
	// HostRuns is how many samples the host measured memory for; HostP95 is its
	// peak at p95 over the charged memory, and HostPeakP95 in bytes.
	HostRuns    int
	HostP95     *float64
	HostPeakP95 *int64
	// OOMKills is over the OOMMeasured samples whose host counts them.
	OOMKills    int64
	OOMMeasured int
	// PressureP90 is over the PressureMeasured samples whose host measured it.
	PressureP90      *float64
	PressureMeasured int
	// Recommendation is one of the rightsize words, Reason why, and Confidence the
	// sample count's word, empty with no recommendation.
	Recommendation, Reason, Confidence string
	// Recommended is the tier to move to, nil when none fits or none is asked for.
	Recommended *config.Tier
}

// rightsizeRun is one measured run's figures. A figure the run has not got is
// false in its flag, never zero.
type rightsizeRun struct {
	busy float64
	// guest: the most of its memory the job's own VM said it used, as a fraction
	// and as the charged memory that comes to.
	guest     bool
	guestFrac float64
	guestNeed int64
	host      bool
	hostPeak  int64
	oom       bool
	oomKills  int64
	pressure  bool
	stalled   float64
}

// Why a run on the current tier was not measured.
const (
	runMeasured = iota
	runNoReport
	runNoSeries
	runUncovered
)

// buildRightsize reads the window and recommends for each job in it.
func buildRightsize(ctx context.Context, a *alloc.Allocator, tiers []config.Tier, from, until time.Time,
) (rightsizeReport, error) {
	facts, partial, err := a.JobFacts(ctx, from, until)
	if err != nil {
		return rightsizeReport{}, err
	}

	type jobKey struct{ repo, workflow, job string }

	grouped := map[jobKey][]*alloc.JobFact{}
	for i := range facts {
		f := &facts[i]
		k := jobKey{f.Repo, f.Workflow(), f.JobName}
		grouped[k] = append(grouped[k], f)
	}

	out := rightsizeReport{From: from, Until: until, Partial: partial}
	for k, runs := range grouped {
		job, err := rightsizeJobOf(ctx, a, k.repo, k.workflow, k.job, runs, tiers)
		if err != nil {
			return rightsizeReport{}, err
		}
		out.Jobs = append(out.Jobs, job)
	}
	slices.SortFunc(out.Jobs, func(x, y rightsizeJob) int {
		return cmp.Or(cmp.Compare(x.Repository, y.Repository), cmp.Compare(x.Workflow, y.Workflow),
			cmp.Compare(x.Job, y.Job))
	})

	return out, nil
}

// rightsizeJobOf reads one job's runs and decides. Only the runs on its current tier
// that say something about their shape are read in detail, so a job's history on
// other tiers costs nothing.
func rightsizeJobOf(ctx context.Context, a *alloc.Allocator, repo, workflow, job string, runs []*alloc.JobFact,
	tiers []config.Tier,
) (rightsizeJob, error) {
	newest := slices.MaxFunc(runs, func(x, y *alloc.JobFact) int {
		return cmp.Or(x.StartedAt.Compare(y.StartedAt), cmp.Compare(x.LeaseID, y.LeaseID))
	})
	out := rightsizeJob{Repository: repo, Workflow: workflow, Job: job, Tier: newest.Tier, VCPU: newest.VCPU,
		Memory: newest.Memory}

	var measured []rightsizeRun
	for _, f := range runs {
		if f.Tier != newest.Tier {
			out.OtherTiers++

			continue
		}
		if !rightsizeRunCounts(f) {
			continue
		}
		run, why, err := readRightsizeRun(ctx, a, f)
		if err != nil {
			return rightsizeJob{}, err
		}
		switch why {
		case runNoReport:
			out.NoReport++
		case runNoSeries:
			out.NoSeries++
		case runUncovered:
			out.Uncovered++
		default:
			measured = append(measured, run)
		}
	}

	current := tierNamed(tiers, newest.Tier)
	if current != nil {
		out.RunsOn = current.ScaleSetName()
	}
	summarizeRuns(&out, measured)
	recommend(&out, current, tiers)

	return out, nil
}

// rightsizeRunCounts says whether a run says anything about its shape. A failed run
// does, since being killed for memory is how a job fails; a canceled one was stopped
// by somebody, and one billet disrupted by billet.
func rightsizeRunCounts(f *alloc.JobFact) bool {
	return f.Result != "" && f.Result != statsResultCanceled && !f.Attributed()
}

// readRightsizeRun reads one run's detail: its host series over the job's window, and
// the report from its own VM. A run the host did not measure is never read as one
// that used nothing.
func readRightsizeRun(ctx context.Context, a *alloc.Allocator, f *alloc.JobFact) (rightsizeRun, int, error) {
	u := f.Usage
	d, ok := f.Duration()
	if u == nil || !u.Measured(alloc.UsageCPU) || f.VCPU <= 0 || f.Memory <= 0 || !ok || d <= 0 {
		return rightsizeRun{}, runNoReport, nil
	}

	series, err := a.LeaseUsageSeries(ctx, f.LeaseID)
	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		return rightsizeRun{}, runNoSeries, nil
	case err != nil:
		return rightsizeRun{}, 0, err
	}
	// A SERIES THIS BINARY CANNOT PLACE ON THE HOST'S CLOCK IS NONE: an older node's,
	// or one that does not decode, has no window to integrate, and the run is counted
	// as not measured rather than failing the report of every other job.
	tl, err := usage.TimelineOf(series.Codec, series.Data)
	if err != nil {
		return rightsizeRun{}, runNoSeries, nil //nolint:nilerr // A series that cannot be read is a run not measured, counted as such.
	}
	w := tl.Window(f.StartedAt, f.ResultAt)
	if w.Span <= 0 || float64(w.Covered) < rightsizeCoverage*float64(w.Span) {
		return rightsizeRun{}, runUncovered, nil
	}

	run := rightsizeRun{busy: float64(w.CPUMicros) / (float64(w.Covered.Microseconds()) * float64(f.VCPU))}
	if u.Measured(alloc.UsageMemory) {
		run.host, run.hostPeak = true, u.MemoryPeakBytes
	}
	if u.Measured(alloc.UsageOOM) {
		run.oom, run.oomKills = true, u.OOMKills
	}
	if u.Measured(alloc.UsagePressure) && u.WindowMillis > 0 {
		run.pressure, run.stalled = true, float64(u.CPUSomeMicros)/float64(u.WindowMillis*1000)
	}

	frac, need, ok, err := guestMemoryOf(ctx, a, f)
	if err != nil {
		return rightsizeRun{}, 0, err
	}
	run.guest, run.guestFrac, run.guestNeed = ok, frac, need

	return run, runMeasured, nil
}

// guestMemoryOf is the most of its memory the job's own VM said it used, as a
// fraction of what it saw and as the charged memory that comes to, from a report the
// node kept whole; false for any other, since a partial report may have lost the
// peak. The VM's MemTotal is less than what was charged, by what its kernel keeps.
func guestMemoryOf(ctx context.Context, a *alloc.Allocator, f *alloc.JobFact) (float64, int64, bool, error) {
	row, err := a.LeaseGuestReport(ctx, f.LeaseID)
	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		return 0, 0, false, nil
	case err != nil:
		return 0, 0, false, err
	}
	g := judgeGuest(row)
	if g.verdict != guestComplete || g.report == nil {
		return 0, 0, false, nil
	}

	best, need, seen := 0.0, int64(0), false
	for i := range g.report.Samples {
		s := &g.report.Samples[i]
		total := s.MemUsedBytes + s.MemAvailableBytes
		if s.MemUsedBytes < 0 || s.MemAvailableBytes < 0 || total <= 0 {
			continue
		}
		frac := float64(s.MemUsedBytes) / float64(total)
		if !seen || frac > best {
			best, seen = frac, true
			need = int64(math.Ceil(frac * float64(f.Memory)))
		}
	}

	return best, need, seen, nil
}

// summarizeRuns is the job's figures over its measured runs.
func summarizeRuns(job *rightsizeJob, runs []rightsizeRun) {
	job.Samples = len(runs)

	busy := make([]float64, 0, len(runs))
	guest, host, pressure := make([]float64, 0, len(runs)), make([]float64, 0, len(runs)), make([]float64, 0, len(runs))
	guestNeed, hostPeak := make([]int64, 0, len(runs)), make([]int64, 0, len(runs))
	for i := range runs {
		r := &runs[i]
		busy = append(busy, r.busy)
		if r.guest {
			guest = append(guest, r.guestFrac)
			guestNeed = append(guestNeed, r.guestNeed)
		}
		if r.host {
			host = append(host, float64(r.hostPeak)/float64(job.Memory))
			hostPeak = append(hostPeak, r.hostPeak)
		}
		if r.oom {
			job.OOMMeasured++
			job.OOMKills += r.oomKills
		}
		if r.pressure {
			pressure = append(pressure, r.stalled)
		}
	}
	job.GuestRuns, job.HostRuns, job.PressureMeasured = len(guest), len(host), len(pressure)

	job.BusyP90 = percentileOf(busy, 9000)
	job.GuestP95, job.GuestNeedP95 = percentileOf(guest, 9500), percentileOf(guestNeed, 9500)
	job.HostP95, job.HostPeakP95 = percentileOf(host, 9500), percentileOf(hostPeak, 9500)
	job.PressureP90 = percentileOf(pressure, 9000)
}

// percentileOf is the nearest-rank percentile of values, which it sorts, nil for none.
func percentileOf[T cmp.Ordered](values []T, perTenThousand int) *T {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	v := alloc.Percentile(values, perTenThousand)

	return &v
}

func tierNamed(tiers []config.Tier, label string) *config.Tier {
	i := slices.IndexFunc(tiers, func(t config.Tier) bool { return t.Label == label })
	if i < 0 {
		return nil
	}

	return &tiers[i]
}

// memoryJudged says whether a memory figure is over enough runs to be judged.
func memoryJudged(runs int, p95 *float64) bool { return p95 != nil && runs >= rightsizeMinSamples }

// recommend decides from the figures, and refuses to where they do not reach.
func recommend(job *rightsizeJob, current *config.Tier, tiers []config.Tier) {
	job.Recommendation = rightsizeNone

	switch {
	case job.Workflow == "" || job.Job == "":
		job.Reason = "GitHub's record does not name the job, so there is nothing a recommendation could be about"

		return
	case job.Samples == 0:
		job.Reason = fmt.Sprintf("not measured: of its runs on this tier, %d had no usage report the host read CPU "+
			"from, %d no series kept (its detail retired, or none sent), and %d a series covering under %.0f%% of "+
			"the run", job.NoReport, job.NoSeries, job.Uncovered, 100*rightsizeCoverage)

		return
	case job.Samples < rightsizeMinSamples:
		job.Reason = fmt.Sprintf("%d of the %d measured runs a recommendation needs", job.Samples, rightsizeMinSamples)

		return
	case current == nil:
		job.Reason = fmt.Sprintf("tier %s is not in this configuration, so there is nothing to compare it with",
			job.Tier)

		return
	}

	switch {
	case job.Samples >= rightsizeHighSamples:
		job.Confidence = "high"
	case job.Samples >= rightsizeMediumSamples:
		job.Confidence = "medium"
	default:
		job.Confidence = "low"
	}

	busy := *job.BusyP90
	guestJudged, hostJudged := memoryJudged(job.GuestRuns, job.GuestP95), memoryJudged(job.HostRuns, job.HostP95)
	overMemory := job.OOMKills > 0
	nearMemory := guestJudged && *job.GuestP95 >= rightsizeMemoryNear
	cpuBound := busy >= rightsizeBusyHigh || (job.PressureP90 != nil && *job.PressureP90 >= rightsizePressureHigh)
	// SMALL IS PROVED BY EITHER SIDE: the guest's own figure, or a host peak low even
	// with the page cache and the VMM in it.
	guestSmall := guestJudged && *job.GuestP95 <= rightsizeMemoryIdle
	hostSmall := hostJudged && *job.HostP95 <= rightsizeMemoryIdle

	switch {
	case overMemory || nearMemory || cpuBound:
		job.Recommendation = rightsizeLarger
		var why []string
		if overMemory {
			why = append(why, fmt.Sprintf("over the memory limit (%d OOM kills)", job.OOMKills))
		} else if nearMemory {
			why = append(why, fmt.Sprintf("near the memory limit (the job's own VM reported using %.0f%% at p95)",
				100**job.GuestP95))
		}
		if cpuBound {
			why = append(why, "CPU-bound"+cpuBoundWhy(job))
		}
		job.Reason = strings.Join(why, " and ")
		job.Recommended = largerTier(job, current, tiers, overMemory || nearMemory, cpuBound)
		if job.Recommended == nil {
			job.Reason += "; no larger tier is configured"
		}
	case busy <= rightsizeBusyIdle && (guestSmall || hostSmall):
		job.Recommendation = rightsizeSmaller
		// THE HOST'S PEAK, WHERE IT PROVES SMALL, IS WHAT A TIER IS SIZED BY: it is an
		// upper bound the job cannot change, where the guest's figure is its word.
		var need int64
		var from string
		if hostSmall {
			need, from = *job.HostPeakP95, fmt.Sprintf("the host's memory peak at p95 %.0f%%", 100**job.HostP95)
		} else {
			need, from = *job.GuestNeedP95, fmt.Sprintf("memory the job's own VM reported at p95 %.0f%%", 100**job.GuestP95)
		}
		job.Reason = fmt.Sprintf("idle (CPU busy at p90 %.0f%%, %s)", 100*busy, from)
		job.Recommended = smallerTier(job, current, tiers, busy, need)
		if job.Recommended == nil {
			job.Reason += "; no smaller tier is configured that would hold it"
		}
	case busy <= rightsizeBusyIdle:
		job.Recommendation = rightsizeKeep
		job.Reason = fmt.Sprintf("CPU busy at p90 %.0f%% is idle, and its memory could not be told: %s", 100*busy,
			memoryUntold(job))
	default:
		job.Recommendation = rightsizeKeep
		job.Reason = fmt.Sprintf("neither idle nor short of room (CPU busy at p90 %.0f%%)", 100*busy)
	}
}

// memoryUntold says why a job's memory could not be judged small.
func memoryUntold(job *rightsizeJob) string {
	guest := fmt.Sprintf("%d of %d runs have a whole report from the job's own VM", job.GuestRuns, job.Samples)
	if memoryJudged(job.GuestRuns, job.GuestP95) {
		guest = fmt.Sprintf("the job's own VM reported %.0f%% at p95", 100**job.GuestP95)
	}
	host := "the host measured no peak"
	if job.HostP95 != nil {
		host = fmt.Sprintf("the host's peak, which counts the page cache and the VMM, is %.0f%% at p95", 100**job.HostP95)
	}

	return guest + ", and " + host
}

// cpuBoundWhy says which figure called the job CPU-bound.
func cpuBoundWhy(job *rightsizeJob) string {
	if *job.BusyP90 >= rightsizeBusyHigh {
		return fmt.Sprintf(" (CPU busy at p90 %.0f%%)", 100**job.BusyP90)
	}

	return fmt.Sprintf(" (CPU pressure at p90 %.0f%%)", 100**job.PressureP90)
}

// sameFamily says a tier differs from current only in its name and size, so a move
// between them changes how much room a job has and nothing about what it runs on,
// where, or under whose authority. A trusted tier must also allow the workflow,
// or the runner group would refuse the job the recommendation moved there.
func sameFamily(current, other *config.Tier, workflow string) bool {
	if other.Label == current.Label || other.Target != current.Target || other.GuestOS != current.GuestOS ||
		other.Arch != current.Arch || other.RunnerGroup != current.RunnerGroup || other.Site != current.Site ||
		other.Node != current.Node || other.Trust.Effective() != current.Trust.Effective() {
		return false
	}

	providers := current.AcceptableProviders()
	if !slices.Equal(providers, other.AcceptableProviders()) {
		return false
	}
	for _, p := range providers {
		if current.ImageFor(p) != other.ImageFor(p) {
			return false
		}
	}

	if other.Trust.Effective() != config.WorkloadTrusted {
		return true
	}

	return slices.ContainsFunc(other.Workflows, func(allowed string) bool {
		path, _, _ := strings.Cut(allowed, "@")

		return path == workflow
	})
}

// smallestOf is the smallest of the family that fits, by vCPU, then memory, then
// name, so the same rows name the same tier on every run.
func smallestOf(current *config.Tier, tiers []config.Tier, workflow string, fits func(*config.Tier) bool) *config.Tier {
	var best *config.Tier
	for i := range tiers {
		t := &tiers[i]
		if !sameFamily(current, t, workflow) || !fits(t) {
			continue
		}
		if best == nil || cmp.Or(cmp.Compare(t.VCPU, best.VCPU), cmp.Compare(t.Memory, best.Memory),
			cmp.Compare(t.Label, best.Label)) < 0 {
			best = t
		}
	}

	return best
}

// largerTier is the smallest tier with more of what the job is short of and no
// less of the other.
func largerTier(job *rightsizeJob, current *config.Tier, tiers []config.Tier, moreMemory, moreCPU bool) *config.Tier {
	return smallestOf(current, tiers, job.Workflow, func(t *config.Tier) bool {
		if t.VCPU < current.VCPU || t.Memory < current.Memory {
			return false
		}

		return (!moreMemory || t.Memory > current.Memory) && (!moreCPU || t.VCPU > current.VCPU)
	})
}

// smallerTier is the smallest tier smaller than the current one that the job, busy
// at busy and needing need bytes of charged memory, would fill to no more than
// rightsizeTarget.
func smallerTier(job *rightsizeJob, current *config.Tier, tiers []config.Tier, busy float64, need int64) *config.Tier {
	needCPU := int(math.Ceil(busy * float64(current.VCPU) / rightsizeTarget))
	needMemory := config.ByteSize(math.Ceil(float64(need) / rightsizeTarget))

	return smallestOf(current, tiers, job.Workflow, func(t *config.Tier) bool {
		smaller := t.VCPU < current.VCPU && t.Memory <= current.Memory ||
			t.Memory < current.Memory && t.VCPU <= current.VCPU

		return smaller && t.VCPU >= needCPU && t.Memory >= needMemory
	})
}

func percentOrNotMeasured(v *float64) string {
	if v == nil {
		return "not measured"
	}

	return fmt.Sprintf("%.1f%%", 100**v)
}

// renderRightsize writes the report for a person. EVERY STRING GITHUB OR A
// WORKFLOW CHOSE IS QUOTED, so a name holding a newline cannot forge a line of it.
func renderRightsize(w io.Writer, r rightsizeReport) {
	fmt.Fprintf(w, "jobs that started from %s to %s\n", statsTime(r.From), statsTime(r.Until))
	fmt.Fprintln(w, rightsizeCovers)
	fmt.Fprintln(w, rightsizeThresholds())
	if r.Partial {
		fmt.Fprintln(w, "PARTIAL: the window holds more jobs than one report reads, and its oldest were left out; "+
			"shorten --since")
	}
	if len(r.Jobs) == 0 {
		fmt.Fprintln(w, "\nno job started in the window")
	}

	for i := range r.Jobs {
		job := &r.Jobs[i]
		fmt.Fprintf(w, "\n%s %s %s\n", strconv.Quote(job.Repository), strconv.Quote(job.Workflow),
			strconv.Quote(job.Job))
		line := func(label, format string, args ...any) {
			fmt.Fprintf(w, "  %-13s%s\n", label, fmt.Sprintf(format, args...))
		}

		shape := "shape not recorded"
		if job.VCPU > 0 {
			shape = fmt.Sprintf("%d vCPU, %s", job.VCPU, cli.HumanBytes(job.Memory))
		}
		runsOn := "not in this configuration"
		if job.RunsOn != "" {
			runsOn = "runs-on " + job.RunsOn
		}
		line("tier", "%s (%s), %s", job.Tier, runsOn, shape)
		line("runs", "%d measured on this tier; not measured: %d with no usage report, %d with no series kept, %d "+
			"with a series short of the run; %d on other tiers", job.Samples, job.NoReport, job.NoSeries,
			job.Uncovered, job.OtherTiers)
		line("cpu busy", "%s at p90", percentOrNotMeasured(job.BusyP90))
		if job.GuestP95 == nil {
			line("vm memory", "not reported (no measured run has a whole report from the job's own VM)")
		} else {
			line("vm memory", "%s at p95 over %d runs, as the job's own VM reported it: %s",
				percentOrNotMeasured(job.GuestP95), job.GuestRuns, guestBanner)
		}
		if job.HostPeakP95 == nil {
			line("host memory", "not measured")
		} else {
			line("host memory", "peak %s at p95 over %d runs, %s of the job's charged memory, with the page cache "+
				"and the VMM in it", cli.HumanBytes(*job.HostPeakP95), job.HostRuns, percentOrNotMeasured(job.HostP95))
		}
		if job.OOMMeasured == 0 {
			line("oom kills", "not measured")
		} else {
			line("oom kills", "%d over %d runs", job.OOMKills, job.OOMMeasured)
		}
		if job.PressureP90 == nil {
			line("cpu pressure", "not measured")
		} else {
			line("cpu pressure", "%s at p90 over %d runs, over the lease's life", percentOrNotMeasured(job.PressureP90),
				job.PressureMeasured)
		}
		line("recommend", "%s", recommendationText(job))
	}
}

// recommendationText is the line a person acts on.
func recommendationText(job *rightsizeJob) string {
	if job.Recommendation == rightsizeNone {
		return "no recommendation: " + job.Reason
	}

	text := job.Recommendation
	if job.Recommended != nil {
		text += fmt.Sprintf(": %s (runs-on %s, %d vCPU, %s)", job.Recommended.Label, job.Recommended.ScaleSetName(),
			job.Recommended.VCPU, cli.HumanBytes(int64(job.Recommended.Memory)))
	}

	return fmt.Sprintf("%s; %s; confidence %s (%d runs)", text, job.Reason, job.Confidence, job.Samples)
}

// rightsizeJSON is the report for a program, and what `billet migrate workflows
// --rightsize` reads back. A figure with nothing behind it is null, never zero.
type rightsizeJSON struct {
	Schema     int                `json:"schema"`
	From       string             `json:"from"`
	Until      string             `json:"until"`
	Partial    bool               `json:"partial"`
	Covers     string             `json:"covers"`
	Thresholds string             `json:"thresholds"`
	Jobs       []rightsizeJobJSON `json:"jobs"`
}

type rightsizeJobJSON struct {
	Repository       string `json:"repository"`
	Workflow         string `json:"workflow"`
	Job              string `json:"job"`
	CurrentTier      string `json:"current_tier"`
	CurrentRunsOn    string `json:"current_runs_on"`
	VCPU             int64  `json:"vcpu"`
	MemoryBytes      int64  `json:"memory_bytes"`
	Runs             int    `json:"runs"`
	RunsNoReport     int    `json:"runs_no_usage_report"`
	RunsNoSeries     int    `json:"runs_no_series"`
	RunsUncovered    int    `json:"runs_series_short"`
	RunsOnOtherTiers int    `json:"runs_on_other_tiers"`
	// CPUBusyP90 is the cgroup's CPU over the job's window and the charged vCPUs.
	CPUBusyP90 *float64 `json:"cpu_busy_p90"`
	// GuestMemoryP95 is what the job's own VM reported, which it could have changed.
	GuestMemoryP95    *float64 `json:"guest_reported_memory_p95_fraction"`
	GuestRuns         int      `json:"guest_reported_runs"`
	HostMemoryPeakP95 *int64   `json:"host_memory_peak_p95_bytes"`
	HostMemoryP95     *float64 `json:"host_memory_peak_p95_fraction"`
	HostMemoryRuns    int      `json:"host_memory_runs"`
	OOMKills          *int64   `json:"oom_kills"`
	OOMMeasuredRuns   int      `json:"oom_measured_runs"`
	CPUPressureP90    *float64 `json:"cpu_pressure_p90"`
	PressureMeasured  int      `json:"pressure_measured_runs"`
	Recommendation    string   `json:"recommendation"`
	Reason            string   `json:"reason"`
	Confidence        string   `json:"confidence"`
	RecommendedTier   string   `json:"recommended_tier"`
	RecommendedRunsOn string   `json:"recommended_runs_on"`
}

func renderRightsizeJSON(w io.Writer, r rightsizeReport) error {
	out := rightsizeJSON{
		Schema: rightsizeSchema, From: r.From.UTC().Format(time.RFC3339Nano),
		Until: r.Until.UTC().Format(time.RFC3339Nano), Partial: r.Partial, Covers: rightsizeCovers,
		Thresholds: rightsizeThresholds(), Jobs: []rightsizeJobJSON{},
	}
	for i := range r.Jobs {
		job := &r.Jobs[i]
		row := rightsizeJobJSON{
			Repository: job.Repository, Workflow: job.Workflow, Job: job.Job, CurrentTier: job.Tier,
			CurrentRunsOn: job.RunsOn, VCPU: job.VCPU, MemoryBytes: job.Memory, Runs: job.Samples,
			RunsNoReport: job.NoReport, RunsNoSeries: job.NoSeries, RunsUncovered: job.Uncovered,
			RunsOnOtherTiers: job.OtherTiers, CPUBusyP90: job.BusyP90, GuestMemoryP95: job.GuestP95,
			GuestRuns: job.GuestRuns, HostMemoryPeakP95: job.HostPeakP95, HostMemoryP95: job.HostP95,
			HostMemoryRuns: job.HostRuns, OOMMeasuredRuns: job.OOMMeasured, CPUPressureP90: job.PressureP90,
			PressureMeasured: job.PressureMeasured, Recommendation: job.Recommendation, Reason: job.Reason,
			Confidence: job.Confidence,
		}
		if job.OOMMeasured > 0 {
			kills := job.OOMKills
			row.OOMKills = &kills
		}
		if job.Recommended != nil {
			row.RecommendedTier, row.RecommendedRunsOn = job.Recommended.Label, job.Recommended.ScaleSetName()
		}
		out.Jobs = append(out.Jobs, row)
	}

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the tier recommendations: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", body); err != nil {
		return fmt.Errorf("write the tier recommendations: %w", err)
	}

	return nil
}
