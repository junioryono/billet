package fleetops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/usage"
)

// cmdJobs is the operator's view of what jobs did.
func Jobs(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet jobs show <lease> | billet jobs show --run <run id> | billet jobs wait")
	}
	switch args[0] {
	case "show":
		return cmdJobsShow(ctx, env, args[1:])
	case "wait":
		return cmdJobsWait(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown jobs command %q; try show or wait", args[0])
}

// cmdJobsShow prints one job's history and what the host measured it do, or
// every job of one of GitHub's workflow runs.
func cmdJobsShow(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs show", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	asJSON := fs.Bool("json", false, "print the record as one JSON object (with --run, an array of them)")
	asHTML := fs.Bool("html", false, "print the job as one self-contained HTML page")
	run := fs.Int64("run", 0, "show every job of the GitHub workflow run with this id (the number in the "+
		"run's URL) rather than one lease")
	windowFlag := fs.String("window", "", "with --json, add the usage the host's series shows between FROM and "+
		"TO, given as FROM,TO in RFC 3339")
	if err := cli.ParseWithArgs(fs, args, 1); err != nil {
		return err
	}
	// GIVEN AT ALL, EVEN EMPTY OR ZERO, a flag is a request: an empty --window=
	// or a --run=0 is refused rather than read as none.
	windowGiven, runGiven := false, false
	fs.Visit(func(f *flag.Flag) {
		windowGiven = windowGiven || f.Name == "window"
		runGiven = runGiven || f.Name == "run"
	})
	switch {
	case runGiven && fs.NArg() != 0:
		return errors.New("billet jobs show: --run names the jobs to show; give it or a lease, not both")
	case !runGiven && fs.NArg() != 1:
		return errors.New("usage: billet jobs show [--config PATH] [--json [--window FROM,TO] | --html] " +
			"<lease> | --run <run id>")
	case runGiven && *run <= 0:
		return fmt.Errorf("billet jobs show: --run takes the positive id of a GitHub workflow run, not %d", *run)
	case runGiven && *asHTML:
		return errors.New("billet jobs show: --html prints one lease's page; show a run's jobs as text or " +
			"with --json, and a lease of it with --html")
	case *asJSON && *asHTML:
		return errors.New("billet jobs show: --json and --html each print the whole record; choose one")
	}
	var w *window
	if windowGiven {
		if !*asJSON {
			return errors.New("billet jobs show: --window adds to the --json record; give --json too")
		}
		parsed, err := parseWindow(*windowFlag)
		if err != nil {
			return fmt.Errorf("billet jobs show: %w", err)
		}
		w = &parsed
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	show := func(leaseID string) ([]byte, error) {
		return showJobOnce(ctx, a, *cfgPath, leaseID, *asJSON, *asHTML, w)
	}
	if !runGiven {
		out, err := show(fs.Arg(0))
		if err != nil {
			return err
		}
		if _, err := env.Stdout.Write(out); err != nil {
			return fmt.Errorf("write the job record: %w", err)
		}

		return nil
	}

	return showRun(ctx, env, a, *run, *asJSON, show)
}

// showRun prints every job of one workflow run, oldest assignment first: as
// text, each record under a line naming it, or as one JSON array.
func showRun(ctx context.Context, env cli.Env, a *alloc.Allocator, run int64, asJSON bool,
	show func(leaseID string) ([]byte, error),
) error {
	leases, more, err := a.JobsOfRun(ctx, run)
	if err != nil {
		return err
	}
	if len(leases) == 0 {
		return fmt.Errorf("billet jobs show: no job of run %d is recorded in this deployment's ledger: its "+
			"jobs ran on other runners or another deployment, or it is not a workflow run's id", run)
	}
	if more {
		fmt.Fprintf(env.Stderr, "billet jobs show: run %d has more than %d jobs recorded; the newest are "+
			"left out\n", run, alloc.JobsOfRunLimit)
	}

	var out bytes.Buffer
	records := make([]json.RawMessage, 0, len(leases))
	for i, leaseID := range leases {
		record, err := show(leaseID)
		if err != nil {
			return err
		}
		if asJSON {
			records = append(records, json.RawMessage(record))

			continue
		}
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "== run %d, job %d of %d: lease %s ==\n\n", run, i+1, len(leases), leaseID)
		out.Write(record)
	}
	if asJSON {
		body, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return fmt.Errorf("encode the run's job records: %w", err)
		}
		out.Write(body)
		out.WriteString("\n")
	}
	if _, err := env.Stdout.Write(out.Bytes()); err != nil {
		return fmt.Errorf("write the run's job records: %w", err)
	}

	return nil
}

// showJobOnce builds one job's report in the form asked for.
//
// A RETIREMENT THAT COMMITS WHILE THE REPORT IS READ takes the detail and
// writes its marker in one transaction, and the reads below are separate, so
// the report is built first and the marker read again after: if it was written
// meanwhile, the report is built once more, now with the marker, which is never
// unset. A report is therefore never missing detail it does not say was
// retired.
func showJobOnce(ctx context.Context, a *alloc.Allocator, cfgPath, leaseID string, asJSON, asHTML bool,
	w *window,
) ([]byte, error) {
	show := func(out io.Writer) (string, error) {
		return showJob(ctx, out, a, cfgPath, leaseID, asJSON, asHTML, w)
	}
	// A BUILD THAT FAILED is rebuilt too when the job is now retired: on
	// PostgreSQL a usage summary and its destinations are two reads at read
	// committed, and a retirement between them reads as a summary claiming rows
	// it has not, which a rebuild over the retired job does not.
	var out bytes.Buffer
	retired, err := show(&out)
	if afterJobShown != nil {
		afterJobShown(a)
	}
	if retired == "" {
		again, readErr := a.Job(ctx, leaseID)
		switch {
		case readErr != nil && err != nil:
			return nil, err
		case readErr != nil:
			return nil, readErr
		case again.DetailRetiredAt != "":
			out.Reset()
			_, err = show(&out)
		}
	}
	if err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

// afterJobShown runs between a report's first build and the read of its
// retirement marker, with the command's allocator; nil outside a test that
// commits a retirement there.
var afterJobShown func(a *alloc.Allocator)

// showJob writes one job's report to out in the form asked for, and says the
// retirement marker it was built with.
func showJob(ctx context.Context, out io.Writer, a *alloc.Allocator, cfgPath, leaseID string,
	asJSON, asHTML bool, w *window,
) (string, error) {
	rec, err := a.Job(ctx, leaseID)
	if err != nil {
		return "", err
	}
	var measured *alloc.RecordedUsage
	switch u, err := a.LeaseUsage(ctx, leaseID); {
	case err == nil:
		measured = &u
	case !errors.Is(err, alloc.ErrLeaseNotFound):
		return "", err
	}

	guest := readJobGuest(ctx, a, leaseID)

	// THE JSON RECORD IS THE LEDGER'S ALONE, and asks GitHub nothing.
	if asJSON {
		tl, unaligned := jobTimeline(ctx, a, rec, measured)
		var wj *windowJSON
		if w != nil {
			wj = windowJSONOf(tl, unaligned, *w)
		}

		return rec.DetailRetiredAt, renderJobJSON(out, rec, measured, tl, unaligned, guest, wj)
	}
	steps := readJobSteps(ctx, cfgPath, a, rec, measured)
	if asHTML {
		return rec.DetailRetiredAt, renderJobPage(out, buildJobPage(ctx, a, rec, measured, steps, guest))
	}
	// THE SERIES THE STEPS WERE PLACED ON, read here when no step was asked about.
	tl, unaligned := steps.timeline, steps.unaligned
	if steps.none != "" {
		tl, unaligned = jobTimeline(ctx, a, rec, measured)
	}
	renderJob(out, rec, measured)
	renderSeries(out, tl, unaligned, measured)
	renderSteps(out, steps)
	renderGuest(out, retiredGuest(guestViewOf(guest, measured, steps), guest, rec))

	return rec.DetailRetiredAt, nil
}

// jobJSON is `billet jobs show --json`: the record renderJob prints, for a
// program. An empty string is a field the ledger has no value for.
type jobJSON struct {
	Lease       string `json:"lease"`
	Tier        string `json:"tier"`
	Node        string `json:"node"`
	Provider    string `json:"provider"`
	VCPU        int64  `json:"vcpu"`
	MemoryBytes int64  `json:"memory_bytes"`
	GitHubJobID string `json:"github_job_id"`
	RunID       int64  `json:"run_id"`
	// GitHubRunID is the run of the job that started, which for a pooled runner
	// may differ from RunID, the run the lease was assigned; zero is not
	// recorded.
	GitHubRunID int64 `json:"github_run_id"`
	// RequestID is GitHub's runner request id, zero for a pooled lease, whose
	// negative id is billet's own scheduler identity and nothing GitHub shows.
	RequestID   int64  `json:"request_id"`
	JobName     string `json:"job_name"`
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflow_ref"`
	Event       string `json:"event"`
	Result      string `json:"result"`
	Conclusion  string `json:"conclusion"`
	QueuedAt    string `json:"queued_at"`
	AssignedAt  string `json:"assigned_at"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	// GitHubQueuedAt, GitHubScaleSetAssignedAt and GitHubRunnerAssignedAt are
	// GitHub's clock; BoughtAt is when billet bought the lease, on billet's.
	GitHubQueuedAt           string `json:"github_queued_at"`
	GitHubScaleSetAssignedAt string `json:"github_scale_set_assigned_at"`
	GitHubRunnerAssignedAt   string `json:"github_runner_assigned_at"`
	BoughtAt                 string `json:"bought_at"`
	// LaunchedAt, RegisteredAt, CustodyAt, TeardownAt and QuarantinedAt are
	// when the lease first entered each part of its life, on billet's clock; an
	// empty one was never entered or not recorded.
	LaunchedAt    string `json:"launched_at"`
	RegisteredAt  string `json:"registered_at"`
	CustodyAt     string `json:"custody_at"`
	TeardownAt    string `json:"teardown_at"`
	QuarantinedAt string `json:"quarantined_at"`
	// RunnerID is GitHub's id for the runner registered, zero when none was
	// recorded; LaunchIncarnation and HolderIncarnation are the node processes
	// that launched the job and held its compute when its lease ended.
	RunnerID          int64  `json:"runner_id"`
	LaunchIncarnation string `json:"launch_incarnation"`
	HolderIncarnation string `json:"holder_incarnation"`
	// FailureReason is why billet failed the lease, and Disruption what its own
	// infrastructure did to the job, with when; empty when none was recorded.
	FailureReason string `json:"failure_reason"`
	Disruption    string `json:"disruption"`
	DisruptedAt   string `json:"disrupted_at"`
	// Cache is what each cache did for the job, as the node observed it; an
	// empty token was not observed.
	Cache cacheJSON `json:"cache"`
	// DetailRetiredAt is when the job's series, destinations and guest report
	// were deleted past the deployment's retention, empty while they are kept.
	DetailRetiredAt string     `json:"detail_retired_at"`
	Usage           *usageJSON `json:"usage"`
	// Guest is what the job's own VM reported, null when no row was recorded.
	Guest *guestJSON `json:"guest"`
	// Window is present only when --window asked for one.
	Window *windowJSON `json:"window,omitempty"`
}

// cacheJSON is each cache's token for one job, verbatim.
type cacheJSON struct {
	Image           string `json:"image"`
	ImageGeneration string `json:"image_generation"`
	Actions         string `json:"actions"`
	Sticky          string `json:"sticky"`
	Git             string `json:"git"`
	Bazel           string `json:"bazel"`
	Go              string `json:"go"`
}

// usageJSON is what the host measured. Groups answers for every group, with
// the OOM count unmeasured whenever memory is, so a reader never re-derives
// which zeros were read. Series is the per-sample series' account of its own
// sampling, null when none is on the host's clock, and SeriesUnavailable then
// says why: none was recorded, it has no clock, or it could not be read.
type usageJSON struct {
	Node              string          `json:"node"`
	RecordedAt        string          `json:"recorded_at"`
	Groups            map[string]bool `json:"measured"`
	Series            *seriesJSON     `json:"series"`
	SeriesUnavailable string          `json:"series_unavailable,omitempty"`
	alloc.JobUsage
}

// usageGroups is every group a usage report answers for.
var usageGroups = []string{alloc.UsageCPU, alloc.UsageThreads, alloc.UsageMemory, alloc.UsageOOM,
	alloc.UsageIO, alloc.UsageNet, alloc.UsagePressure, alloc.UsageEnergy}

// renderJobJSON writes one job's record as an indented JSON object. A job with
// no usage report has a null usage, and one with no guest report a null guest,
// never a report of zeros.
func renderJobJSON(w io.Writer, rec alloc.JobRecord, u *alloc.RecordedUsage, tl *usage.Timeline, unaligned string,
	g *jobGuest,
	win *windowJSON,
) error {
	out := jobJSON{
		Lease: rec.LeaseID, Tier: rec.Tier, Node: rec.Node, Provider: rec.ChosenProvider,
		VCPU: rec.VCPU, MemoryBytes: rec.Memory, GitHubJobID: rec.Job.JobID, RunID: rec.RunID,
		RequestID: max(rec.RequestID, 0), JobName: rec.Job.Name, Repository: rec.Repo,
		WorkflowRef: rec.Job.WorkflowRef, Event: rec.Job.Event, Result: rec.Result,
		Conclusion: rec.Conclusion, QueuedAt: rec.QueuedAt, AssignedAt: rec.AssignedAt,
		StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt, Guest: guestJSONOf(g), Window: win,
		GitHubQueuedAt: rec.GitHubQueuedAt, GitHubScaleSetAssignedAt: rec.GitHubScaleSetAssignedAt,
		GitHubRunnerAssignedAt: rec.GitHubRunnerAssignedAt, BoughtAt: rec.BoughtAt,
		DetailRetiredAt: rec.DetailRetiredAt,
		GitHubRunID:     rec.GitHubRunID, LaunchedAt: rec.LaunchedAt, RegisteredAt: rec.RegisteredAt,
		CustodyAt: rec.CustodyAt, TeardownAt: rec.TeardownAt, QuarantinedAt: rec.QuarantinedAt,
		RunnerID: rec.RunnerID, LaunchIncarnation: rec.LaunchIncarnation,
		HolderIncarnation: rec.HolderIncarnation, FailureReason: rec.FailureReason,
		Disruption: rec.Disruption, DisruptedAt: rec.DisruptedAt,
		Cache: cacheJSON{
			Image: string(rec.Caches.ImageCache), ImageGeneration: rec.Caches.CacheGeneration,
			Actions: string(rec.Caches.ActionsCache), Sticky: string(rec.Caches.Sticky),
			Git: string(rec.Caches.Git), Bazel: string(rec.Caches.Bazel), Go: string(rec.Caches.Go),
		},
	}
	if u != nil {
		groups := make(map[string]bool, len(usageGroups))
		for _, g := range usageGroups {
			groups[g] = u.Measured(g)
		}
		out.Usage = &usageJSON{Node: u.Node, RecordedAt: u.RecordedAt, Groups: groups, Series: seriesJSONOf(tl),
			SeriesUnavailable: seriesUnavailable(tl, unaligned),
			JobUsage:          u.JobUsage}
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the job record: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", body); err != nil {
		return fmt.Errorf("write the job record: %w", err)
	}

	return nil
}

// renderJob writes one job's record.
//
// EVERY STRING GITHUB OR A WORKFLOW CHOSE IS QUOTED: a job name is whatever the
// workflow file says, and an unquoted newline in it would forge a line of this
// report.
func renderJob(w io.Writer, rec alloc.JobRecord, u *alloc.RecordedUsage) {
	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	quoted := func(s string) string {
		if s == "" {
			return "not recorded"
		}
		return strconv.Quote(s)
	}

	line("lease", "%s", rec.LeaseID)
	shape := ""
	if rec.VCPU > 0 {
		shape = fmt.Sprintf(", %d vCPU, %s", rec.VCPU, cli.HumanBytes(rec.Memory))
	}
	line("tier", "%s on %s (%s%s)", rec.Tier, orUnknown(rec.Node), orUnknown(rec.ChosenProvider), shape)
	// NO REQUEST ID FOR A POOLED LEASE, for `billet leases failures`' reason:
	// its negative id is billet's scheduler identity, not anything GitHub shows.
	request := ""
	if rec.RequestID > 0 {
		request = fmt.Sprintf(", request %d", rec.RequestID)
	}
	line("job", "%s (github job %s, run %s%s)", quoted(rec.Job.Name),
		quoted(rec.Job.JobID), identifier(rec.RunID), request)
	line("repository", "%s", quoted(rec.Repo))
	line("workflow", "%s", quoted(rec.Job.WorkflowRef))
	line("event", "%s", quoted(rec.Job.Event))
	line("result", "github %s; billet %s", quoted(rec.Result), orUnknown(rec.Conclusion))
	line("times", "queued %s, assigned %s, started %s, finished %s", orUnknown(rec.QueuedAt),
		orUnknown(rec.AssignedAt), orUnknown(rec.StartedAt), orUnknown(rec.FinishedAt))
	line("github", "%s", githubTimes(rec, orUnknown))
	line("waited", "%s", waitedLine(rec))
	if rec.DetailRetiredAt != "" {
		line("detail", "%s", detailRetired(rec))
	}
	renderLifecycle(line, rec)

	fmt.Fprintln(w)
	if u == nil {
		// NOT A REASON: a job still running has no report either, since the node
		// sends one as its compute is destroyed.
		line("usage", "%s", noUsageReport)
		return
	}
	line("usage", "%s", measuredBy(u))
	renderUsageGroups(line, u)
	renderCounters(line, u.Counters)
	if u.Destinations == nil && rec.DetailRetiredAt != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "destinations "+detailRetired(rec))
		return
	}
	renderDestinations(w, u.Destinations)
}

// detailRetired says that a job's per-sample series, destinations and guest
// report were deleted past the deployment's retention, and when.
func detailRetired(rec alloc.JobRecord) string {
	return "retired " + rec.DetailRetiredAt + ", past server.job_detail_retention: the per-sample series, " +
		"the destinations and the guest report are gone, and the usage summary is kept"
}

// noUsageReport is what a job with no usage report says in place of one.
const noUsageReport = "no report recorded (one arrives as the job's compute is destroyed, " +
	"from a node with node.monitoring)"

// measuredBy says which host measured a job, and how often over how long.
func measuredBy(u *alloc.RecordedUsage) string {
	return fmt.Sprintf("measured by the host %s, %d samples every %s over %s",
		orUnknown(u.Node), u.Samples, time.Duration(u.IntervalMillis)*time.Millisecond,
		time.Duration(u.WindowMillis)*time.Millisecond)
}

// renderUsageGroups writes one line for each group a usage report answers for,
// "not measured" for a group the host could not read. The text report and the
// job page both word a group through here.
func renderUsageGroups(line func(label, format string, args ...any), u *alloc.RecordedUsage) {
	group := func(name, label string, render func() string) {
		if !u.Measured(name) {
			line(label, "not measured")
			return
		}
		line(label, "%s", render())
	}
	group(alloc.UsageCPU, "cpu", func() string {
		return fmt.Sprintf("user %s, system %s", seconds(u.CPUUserMicros), seconds(u.CPUSystemMicros))
	})
	group(alloc.UsageThreads, "vmm split", func() string {
		return fmt.Sprintf("guest vCPUs %s, VMM %s", seconds(u.GuestCPUMicros), seconds(u.VMMCPUMicros))
	})
	group(alloc.UsageMemory, "memory", func() string {
		if !u.Measured(alloc.UsageOOM) {
			return fmt.Sprintf("peak %s, oom kills not measured", cli.HumanBytes(u.MemoryPeakBytes))
		}
		return fmt.Sprintf("peak %s, oom kills %d", cli.HumanBytes(u.MemoryPeakBytes), u.OOMKills)
	})
	group(alloc.UsageIO, "disk", func() string {
		return fmt.Sprintf("read %s, written %s (host io, not the guest's page cache)",
			cli.HumanBytes(u.DiskReadBytes), cli.HumanBytes(u.DiskWriteBytes))
	})
	group(alloc.UsageNet, "network", func() string {
		return fmt.Sprintf("received %s, sent %s (the guest's view)",
			cli.HumanBytes(u.NetRxBytes), cli.HumanBytes(u.NetTxBytes))
	})
	group(alloc.UsagePressure, "stalled", func() string {
		return fmt.Sprintf("cpu %s, memory %s (full %s), io %s (full %s)", seconds(u.CPUSomeMicros),
			seconds(u.MemorySomeMicros), seconds(u.MemoryFullMicros), seconds(u.IOSomeMicros),
			seconds(u.IOFullMicros))
	})
	group(alloc.UsageEnergy, "energy", func() string {
		switch u.EnergySource {
		case alloc.EnergyRAPL:
			return fmt.Sprintf("%s active, %s idle (package RAPL, a model rather than a meter)",
				joules(u.EnergyActiveMicrojoules), joules(u.EnergyIdleMicrojoules))
		case alloc.EnergyProcess:
			return fmt.Sprintf("%s (macOS's own estimate for the VM's process; the machine's idle draw is not in it)",
				joules(u.EnergyActiveMicrojoules))
		}
		return fmt.Sprintf("%s (package RAPL with no idle baseline, so idle is inside it; source %s)",
			joules(u.EnergyActiveMicrojoules), strconv.Quote(u.EnergySource))
	})
}

// renderCounters writes what the hardware counters saw and the ratios a reader
// wants from them. An event that was not counted says so, and so does every
// ratio that needs it: a ratio of a count and a missing count is not a number.
func renderCounters(line func(label, format string, args ...any), c *alloc.JobCounters) {
	if c == nil {
		line("counters", "not measured")
		return
	}
	count := func(v *int64) string {
		if v == nil {
			return "not measured"
		}
		return groupDigits(*v)
	}
	line("counters", "cycles %s, instructions %s (the vCPU threads, guest and host mode)",
		count(c.Cycles), count(c.Instructions))
	line("", "cache references %s, cache misses %s", count(c.CacheReferences), count(c.CacheMisses))
	line("", "branch misses %s, frontend stall cycles %s", count(c.BranchMisses),
		count(c.FrontendStallCycles))

	ratio := func(num, den *int64, scale float64, format, per string) string {
		switch {
		case num == nil || den == nil:
			return "not measured"
		case *den == 0:
			return "undefined (no " + per + " counted)"
		}
		return fmt.Sprintf(format, float64(*num)*scale/float64(*den))
	}
	line("ipc", "%s", ratio(c.Instructions, c.Cycles, 1, "%.2f instructions per cycle", "cycles"))
	line("cache", "%s", ratio(c.CacheMisses, c.Instructions, 1000,
		"%.1f misses per 1,000 instructions", "instructions"))
	line("branches", "%s", ratio(c.BranchMisses, c.Instructions, 1000,
		"%.1f mispredicted per 1,000 instructions", "instructions"))
	line("frontend", "%s", ratio(c.FrontendStallCycles, c.Cycles, 100,
		"stalled %.1f%% of cycles", "cycles"))
}

// groupDigits writes a count with its thousands separated, as perf stat does.
func groupDigits(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if n < 0 {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}

	return sign + b.String()
}

func seconds(micros int64) string { return fmt.Sprintf("%.1fs", float64(micros)/1e6) }

func joules(microjoules int64) string {
	j := float64(microjoules) / 1e6
	if j >= 1000 {
		return fmt.Sprintf("%.1f kJ", j/1000)
	}

	return fmt.Sprintf("%.1f J", j)
}
