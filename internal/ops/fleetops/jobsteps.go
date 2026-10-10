package fleetops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// stepsReadLimit bounds asking GitHub for a job's steps, the App key's read
// included. Past it the report says the steps could not be read, and the rest
// of it is printed as it would have been. A variable only so a test can
// shorten it.
var stepsReadLimit = 20 * time.Second

// jobSteps is what `billet jobs show` learned about a job's steps.
type jobSteps struct {
	// none says why GitHub was not asked: nothing recorded names a job to ask
	// about.
	none string
	// err is why GitHub's record could not be read.
	err error
	// runner is the name GitHub's record was searched for.
	runner string
	job    github.WorkflowJob
	// timeline is the job's series on the host's clock; without one, unaligned
	// says why no step can be given usage.
	timeline  *usage.Timeline
	unaligned string
	measured  *alloc.RecordedUsage
}

// readJobSteps asks GitHub for the steps of the job a lease ran, and places
// the lease's series on wall time for them. It fails nothing: what it could not
// learn is in what it returns.
func readJobSteps(ctx context.Context, cfgPath string, a *alloc.Allocator, rec alloc.JobRecord,
	measured *alloc.RecordedUsage,
) jobSteps {
	out := jobSteps{measured: measured, runner: provider.InstanceName(rec.LeaseID)}
	owner, repository, ok := config.SplitRepository(rec.Repo)
	switch {
	case rec.Job.JobID == "":
		out.none = "no GitHub job is recorded for this lease, so there are no steps to read"
		return out
	case rec.RunID <= 0:
		out.none = "no workflow run is recorded for this lease's job, so its steps cannot be found"
		return out
	case !ok:
		out.none = "the job's repository is not recorded as owner/name, so its steps cannot be found"
		return out
	}

	out.timeline, out.unaligned = jobTimeline(ctx, a, rec.LeaseID, measured)

	ctx, cancel := context.WithTimeout(ctx, stepsReadLimit)
	defer cancel()
	records, err := app.OpenJobRecords(ctx, cfgPath, rec.Tier, rec.Repo)
	if err != nil {
		out.err = err
		return out
	}
	out.job, out.err = records.RunnerJob(ctx, owner, repository, rec.RunID, out.runner)

	return out
}

// jobTimeline is a lease's stored series on the host's clock, or why there is
// none.
func jobTimeline(ctx context.Context, a *alloc.Allocator, leaseID string, measured *alloc.RecordedUsage,
) (*usage.Timeline, string) {
	if measured == nil {
		return nil, "no usage report was recorded"
	}
	series, err := a.LeaseUsageSeries(ctx, leaseID)
	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		return nil, "the usage report came with no series"
	case err != nil:
		return nil, "the usage series could not be read: " + strconv.Quote(err.Error())
	}
	tl, err := usage.TimelineOf(series.Codec, series.Data)
	switch {
	case errors.Is(err, usage.ErrNoClock):
		return nil, "the series was recorded without its start on the host's clock (by a node, or for a " +
			"control plane, older than wire 26), so no step can be placed on it"
	case err != nil:
		return nil, "the usage series could not be decoded: " + strconv.Quote(err.Error())
	}

	return &tl, ""
}

// renderSteps writes the steps section of a job's report.
//
// EVERY STRING GITHUB OR A WORKFLOW CHOSE IS QUOTED, as in renderJob, and so is
// every error, which can carry GitHub's own message.
//
// COULD-NOT-TELL IS NEVER ZERO: a step GitHub gave no times is listed without
// usage, a step the series does not fully cover says how much it covers, and a
// step with no sample inside it says its share was split by time.
func renderSteps(w io.Writer, s jobSteps) {
	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	fmt.Fprintln(w)
	switch {
	case s.none != "":
		line("steps", "%s", s.none)
		return
	case errors.Is(s.err, github.ErrNoRunnerJob):
		line("steps", "GitHub's record of the run lists no job on runner %s, so there are no steps to show "+
			"(GitHub keeps a job's record about as long as its run, and a deleted run keeps none)", s.runner)
		return
	case s.err != nil:
		line("steps", "could not be read from GitHub: %s%s", strconv.Quote(s.err.Error()), permissionHint(s.err))
		return
	}

	// AN ATTEMPT GITHUB DID NOT GIVE IS SAID TO BE, never printed as attempt 0.
	attempt := "attempt not given"
	if s.job.RunAttempt > 0 {
		attempt = fmt.Sprintf("attempt %d", s.job.RunAttempt)
	}
	line("steps", "github job %d %s, %s, on runner %s, steps listed: %d", s.job.ID,
		strconv.Quote(s.job.Name), attempt, s.runner, len(s.job.Steps))
	if s.timeline == nil {
		line("", "usage per step: could not tell, because %s", s.unaligned)
	} else {
		line("", "aligned to the second on two clocks, GitHub's for the steps and the host's for "+
			"its samples, with no correction for skew between them; within one sample interval, "+
			"usage is split by time")
	}
	for _, step := range s.job.Steps {
		renderStep(line, step, s.timeline, s.measured)
	}
}

// renderStep writes one step: what GitHub says about it, then what the series
// says about its window.
func renderStep(line func(label, format string, args ...any), step github.JobStep,
	tl *usage.Timeline, measured *alloc.RecordedUsage,
) {
	label := fmt.Sprintf("step %d", step.Number)
	outcome := step.Conclusion
	if outcome == "" {
		outcome = step.Status
	}
	head := strconv.Quote(step.Name)
	if outcome != "" {
		head += " " + strconv.Quote(outcome)
	}

	switch {
	case step.StartedAt.IsZero() && step.CompletedAt.IsZero():
		line(label, "%s, no start or end time from GitHub, so no usage", head)
		return
	case step.CompletedAt.IsZero():
		line(label, "%s, started %s, no end time from GitHub, so no duration or usage", head, stamp(step.StartedAt))
		return
	case step.StartedAt.IsZero():
		line(label, "%s, ended %s, no start time from GitHub, so no duration or usage", head, stamp(step.CompletedAt))
		return
	}
	took := step.CompletedAt.Sub(step.StartedAt)
	switch {
	case took < 0:
		line(label, "%s, started %s, GitHub's end is %s before its start, so no usage", head,
			stamp(step.StartedAt), -took)
		return
	case took == 0:
		line(label, "%s, started %s, took 0s (start and end in the same second), so no usage can be placed in it",
			head, stamp(step.StartedAt))
		return
	}
	line(label, "%s, started %s, took %s", head, stamp(step.StartedAt), took)
	if tl == nil || measured == nil {
		return
	}

	used := tl.Window(step.StartedAt, step.CompletedAt)
	text := windowText(used, measured)
	switch {
	case used.Covered == 0:
		line("", "the series covers none of this step, so no usage")
	case !used.Complete():
		line("", "partial: the series covers %s of the step's %s, and over that part: %s",
			used.Covered, used.Span, text)
	case used.Samples == 0:
		line("", "%s (no sample inside the step: its share of the interval around it, split by time)", text)
	default:
		line("", "%s", text)
	}
}

// windowText renders what a window used, group by group, saying "not
// measured" for a group the host never read rather than its zero.
func windowText(u usage.WindowUsage, m *alloc.RecordedUsage) string {
	text := func(group, measured string) string {
		if !m.Measured(group) {
			return "not measured"
		}
		return measured
	}
	memory := "not sampled inside the step"
	if u.Samples > 0 {
		memory = cli.HumanBytes(u.MemoryPeak)
	}
	energy := "energy " + text(alloc.UsageEnergy, joules(u.EnergyActive))
	if m.EnergySource == alloc.EnergyRAPL && m.Measured(alloc.UsageEnergy) {
		energy += " active"
	}

	return fmt.Sprintf("cpu %s, peak memory %s, disk %s, network %s, %s",
		text(alloc.UsageCPU, seconds(u.CPUMicros)),
		text(alloc.UsageMemory, memory),
		text(alloc.UsageIO, fmt.Sprintf("read %s, written %s", cli.HumanBytes(u.DiskRead), cli.HumanBytes(u.DiskWrite))),
		text(alloc.UsageNet, fmt.Sprintf("received %s, sent %s", cli.HumanBytes(u.NetRx), cli.HumanBytes(u.NetTx))),
		energy)
}

// permissionHint names the App's missing permission when GitHub's refusal
// reads as one, which is the one cause an operator fixes rather than waits out.
func permissionHint(err error) string {
	api, ok := errors.AsType[*github.APIError](err)
	if !ok || api.Status != http.StatusForbidden || api.RateLimited {
		return ""
	}

	return " (reading steps needs the App's `actions: read`, which `billet github-app create` requests " +
		"unless --actions-read=false)"
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
