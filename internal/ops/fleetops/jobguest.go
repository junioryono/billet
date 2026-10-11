package fleetops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/guestreport"
)

// guestBanner is said wherever a number the job's guest reported is shown.
const guestBanner = "Reported by the job's own VM. The job runs as root there and can change any " +
	"of it; billet does not act on it."

// guestNeverContacted is the verdict of a row that saw neither the agent's hello
// nor any batch from it.
const guestNeverContacted = "the guest never contacted the node (an image older than the agent, " +
	"or an agent that did not start)"

// noGuestReport is what a job with no guest report row says in place of one.
const noGuestReport = "no report recorded (the node had node.monitoring.guest off, or is older than " +
	"wire 29; a job still running has none yet, since the node sends it as the compute is destroyed)"

// guestTopProcesses is how many process names a step shows; the rest are summed.
const guestTopProcesses = 5

// guestVerdict is what a lease's guest report row says, as one word a program
// reads.
type guestVerdict string

// The verdicts. Every one but the last three is could-not-tell about the guest's
// numbers, each for its own reason.
const (
	guestUnreadable      guestVerdict = "unreadable"
	guestSilent          guestVerdict = "never_contacted"
	guestNothingAccepted guestVerdict = "nothing_accepted"
	guestNotKept         guestVerdict = "not_kept"
	guestUndecodable     guestVerdict = "undecodable"
	guestNodeRestarted   guestVerdict = "node_restarted"
	guestPartial         guestVerdict = "partial"
	guestComplete        guestVerdict = "complete"
)

// jobGuest is a lease's guest report row as read and judged. A lease with no row
// has none (nil).
type jobGuest struct {
	// row is the ledger's row; unset when it could not be read.
	row      alloc.RecordedGuestReport
	readable bool
	verdict  guestVerdict
	why      string
	// report is the guest's decoded report, nil unless the verdict is
	// guestNodeRestarted, guestPartial or guestComplete.
	report *guestreport.Report
	// finalKept says the agent's last batch is in the report, so what only it
	// carries (the test results) can be told.
	finalKept bool
}

// readJobGuest reads and judges what a lease's guest reported. Nil is no row;
// a row the ledger cannot read back is a verdict of its own, never no row.
func readJobGuest(ctx context.Context, a *alloc.Allocator, leaseID string) *jobGuest {
	row, err := a.LeaseGuestReport(ctx, leaseID)
	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		return nil
	case err != nil:
		return &jobGuest{verdict: guestUnreadable,
			why: "the ledger's row could not be read: " + strconv.Quote(err.Error())}
	}

	return judgeGuest(row)
}

// judgeGuest is a readable row's verdict, and its report when it has one that
// decodes.
//
// NOTHING ACCEPTED IS NEVER A REPORT OF ZEROS, and data that does not decode is
// never an empty report: each says why there is nothing to show.
func judgeGuest(row alloc.RecordedGuestReport) *jobGuest {
	g := &jobGuest{row: row, readable: true}
	switch {
	case row.Accepted == 0 && !row.Hello:
		g.verdict, g.why = guestSilent, guestNeverContacted
		return g
	case row.Accepted == 0:
		g.verdict = guestNothingAccepted
		g.why = fmt.Sprintf("the agent asked the node for its limits, and the node accepted none of its "+
			"batches (%d refused), so there is no report to show", row.Refused)
		return g
	case len(row.Data) == 0:
		g.verdict = guestNotKept
		g.why = fmt.Sprintf("the node accepted %d batches and could not keep them (%s dropped), so there "+
			"is no report to show", row.Accepted, cli.HumanBytes(row.DroppedBytes))
		return g
	case row.Codec != guestreport.Codec:
		g.verdict = guestUndecodable
		g.why = fmt.Sprintf("the report is in codec %d, which this billet does not read, so there is "+
			"nothing to show", row.Codec)
		return g
	}
	r, err := guestreport.Decode(row.Data)
	if err != nil {
		// A REFUSAL QUOTES NOTHING THE GUEST WROTE: guestreport builds its text
		// from its own field names.
		g.verdict = guestUndecodable
		g.why = "the report could not be decoded (" + err.Error() + "), so there is nothing to show"
		return g
	}
	g.report = &r

	// THE LAST BATCH ARRIVING IS NOT ITS BEING KEPT: the node counts it final as
	// it accepts it, and then can drop it past what it assembles, or the merge can
	// refuse it. It is in the report only when every batch the node accepted is:
	// the kept and the repeated account for all of them, since a batch the merge
	// refused, a resend with other content and one dropped past the assembly each
	// leave one unaccounted for.
	whole := r.Batches+r.Duplicates == row.Accepted
	g.finalKept = row.FinalSeen && whole
	notKept := fmt.Sprintf("not every batch the node accepted is in the report (%d accepted, %d kept, %d "+
		"refused by the merge), so it may end before the job did", row.Accepted, r.Batches, r.Refused)
	missing := ""
	if r.Missing > 0 {
		missing = fmt.Sprintf("; %d of the agent's batches between its first and last never arrived", r.Missing)
	}
	switch {
	case row.NodeRestarted:
		g.verdict = guestNodeRestarted
		g.why = "node restarted: the node restarted while the job ran, so the batches it held before then " +
			"were lost and the report starts after the restart"
		switch {
		case !row.FinalSeen:
			g.why += "; the agent's last batch did not arrive either, so it ends before the job did"
		case !whole:
			g.why += "; and " + notKept
		}
	case !row.FinalSeen:
		g.verdict = guestPartial
		g.why = "partial: the agent's last batch did not arrive, so the report ends before the job did"
	case !whole:
		g.verdict = guestPartial
		g.why = "partial: the agent's last batch arrived, but " + notKept
	default:
		g.verdict = guestComplete
		g.why = "complete: the agent's last batch arrived, the report holds every batch the node accepted, " +
			"and the node did not restart while the job ran"
	}
	g.why += missing

	return g
}

// guestView is the guest section, already worded: the text report and the page
// both lay it out and decide nothing. Every string the guest wrote is quoted
// with strconv.Quote, so it is bounded, holds no control or invisible character
// and reads as the guest's.
type guestView struct {
	// Verdict is the one-word verdict, "none" when there is no row.
	Verdict string
	Why     string
	// Banner is set wherever the guest's numbers follow.
	Banner  string
	Channel []pageField
	// The rest is set only with a report.
	HasReport bool
	Account   []pageField
	Skew      string
	Memory    []pageField
	Checks    []pageField
	Agent     []pageField
	Steps     guestStepsView
	Tests     guestTestsView
	Chart     guestChart
}

// guestStepsView is the guest's step marks against GitHub's steps, and its
// processes placed by the marks.
type guestStepsView struct {
	MarksWhy  string
	MarksNote string
	Marks     []guestMarkRow
	ProcsWhy  string
	ProcsNote string
	Procs     []guestStepProcs
}

// guestMarkRow is one step mark and the GitHub step it was matched to.
type guestMarkRow struct {
	Mark, Name, At, Step, GitHub, Offset string
}

// guestStepProcs is the processes of one step, as the marks place them.
type guestStepProcs struct {
	Label, Name, Samples string
	Rows                 []guestProcRow
	Rest, Exited         string
}

// guestProcRow is one process name over a step.
type guestProcRow struct {
	Name, Procs, CPU, RSS, Read, Written string
}

// guestTestsView is the test results the agent found.
type guestTestsView struct {
	Why     string
	Suites  []guestSuiteRow
	Dropped string
}

// guestSuiteRow is one suite's result file.
type guestSuiteRow struct {
	Name, Tests, Failures, Errors, Skipped string
	Failed                                 []string
}

// guestViewOf is the guest section of a job's record: the row's verdict, what
// the node counted of the channel, and, with a report, the guest's own numbers
// beside the host's and GitHub's.
func guestViewOf(g *jobGuest, measured *alloc.RecordedUsage, steps jobSteps) guestView {
	if g == nil {
		return guestView{Verdict: "none", Why: noGuestReport}
	}
	v := guestView{Verdict: string(g.verdict), Why: g.why}
	if !g.readable {
		return v
	}
	v.Channel = guestChannel(g.row)
	if g.report == nil {
		return v
	}
	r := g.report
	v.HasReport, v.Banner = true, guestBanner
	v.Account = guestAccount(r)
	v.Skew = guestSkew(g.row, r)
	v.Memory = guestMemory(r, measured)
	v.Checks = guestChecks(r, measured)
	v.Agent = guestAgent(g.row, r)
	var listed []github.JobStep
	if steps.none == "" && steps.err == nil {
		listed = steps.job.Steps
	}
	v.Steps = guestSteps(r, listed)
	v.Tests = guestTests(r, g.row.FinalSeen, g.finalKept)

	return v
}

// guestChannel is what the node counted of the agent's channel. These are the
// node's counts, not the guest's numbers.
func guestChannel(row alloc.RecordedGuestReport) []pageField {
	agent := "not recorded"
	if row.AgentVersion != "" {
		agent = strconv.Quote(row.AgentVersion) + ", schema " + strconv.Itoa(row.Schema)
	}
	hello := "not seen"
	if row.Hello {
		hello = "seen"
	}
	final := "did not arrive"
	if row.FinalSeen {
		final = "arrived"
	}
	restart := "none while the job ran"
	if row.NodeRestarted {
		restart = "the node restarted while the job ran; what it held before then was lost"
	}
	received := "no batch arrived"
	if !row.FirstReceived.IsZero() {
		received = fmt.Sprintf("first %s, last %s, on the node's clock", stampMillis(row.FirstReceived),
			stampMillis(row.LastReceived))
	}

	return []pageField{
		{Label: "node", Value: orUnknown(row.Node)},
		{Label: "agent", Value: agent},
		{Label: "hello", Value: hello},
		{Label: "batches", Value: fmt.Sprintf("%d accepted, %d refused; %s dropped (the refused batches the "+
			"node read, and any it accepted and could not keep)", row.Accepted, row.Refused,
			cli.HumanBytes(row.DroppedBytes))},
		{Label: "final", Value: final},
		{Label: "restart", Value: restart},
		{Label: "received", Value: received},
		{Label: "encoded", Value: fmt.Sprintf("%s, codec %d", cli.HumanBytes(int64(len(row.Data))), row.Codec)},
	}
}

// guestAccount is the report's account of the agent's batches and its
// resolution.
func guestAccount(r *guestreport.Report) []pageField {
	gaps := ""
	if len(r.Gaps) > 0 {
		var runs []string
		for _, g := range r.Gaps {
			if g.FirstSeq == g.LastSeq {
				runs = append(runs, strconv.FormatUint(g.FirstSeq, 10))
				continue
			}
			runs = append(runs, fmt.Sprintf("%d-%d", g.FirstSeq, g.LastSeq))
		}
		gaps = " (" + strings.Join(runs, ", ")
		if len(r.Gaps) == guestreport.MaxReportGaps {
			gaps += ", and any after"
		}
		gaps += ")"
	}
	out := []pageField{
		{Label: "sequence", Value: fmt.Sprintf("batches %d to %d: %d kept, %d refused by the merge, "+
			"%d never arrived%s", r.FirstSeq, r.LastSeq, r.Batches, r.Refused, r.Missing, gaps)},
		{Label: "repeats", Value: fmt.Sprintf("%d received twice, %d received twice with other content "+
			"(the first received is kept)", r.Duplicates, r.Conflicts)},
		{Label: "resolution", Value: fmt.Sprintf("a sample every %s, processes every %s (%d of the agent's "+
			"samples to each kept)", guestreport.SampleInterval*time.Duration(r.Stride),
			guestreport.ProcessInterval*time.Duration(r.Stride), r.Stride)},
	}
	if d := r.Dropped; d.Steps > 0 || d.Suites > 0 || d.FailedNames > 0 || r.Saturated > 0 {
		out = append(out, pageField{Label: "left out", Value: fmt.Sprintf("%d step marks, %d suites and %d "+
			"failed test names past the report's bounds; %d process sums held at their bound", d.Steps, d.Suites,
			d.FailedNames, r.Saturated)})
	}

	return out
}

// guestSkew is what the node's arrival stamps say about the guest's clock.
//
// ONE-SIDED: the last batch left the guest after its last sample was stamped,
// so the node's arrival minus that stamp is the guest's lag behind the node's
// clock plus the time the batch took. It bounds the lag from above and says
// nothing of how far ahead the guest's clock may be, unless it is negative.
func guestSkew(row alloc.RecordedGuestReport, r *guestreport.Report) string {
	var last int64
	if n := len(r.Samples); n > 0 {
		last = r.Samples[n-1].AtMillis
	}
	if n := len(r.Processes); n > 0 {
		last = max(last, r.Processes[n-1].AtMillis)
	}
	switch {
	case last == 0:
		return "could not tell: the report holds no sample whose time to compare"
	case row.LastReceived.IsZero():
		return "could not tell: the node recorded no arrival time"
	}
	d := row.LastReceived.UnixMilli() - last
	if d >= 0 {
		return fmt.Sprintf("the node received the last batch %s after the guest stamped its last sample: the "+
			"guest's clock is at most that far behind the node's, and how far ahead it may be, nothing here "+
			"says. No time on this page is corrected for it.", spanMillis(d))
	}

	return fmt.Sprintf("the node received the last batch %s before the guest stamped its last sample, so the "+
		"guest's clock is at least that far ahead of the node's. No time on this page is corrected for it.",
		spanMillis(-d))
}

// guestMemory is the guest's memory levels beside the host's.
func guestMemory(r *guestreport.Report, m *alloc.RecordedUsage) []pageField {
	host := "no usage report was recorded"
	if m != nil {
		host = "not measured"
		if m.Measured(alloc.UsageMemory) {
			host = "peak " + cli.HumanBytes(m.MemoryPeakBytes) + " (the cgroup's level, which for a microVM " +
				"counts the VMM and the host's page cache for its disk)"
		}
	}
	if len(r.Samples) == 0 {
		return []pageField{
			{Label: "used", Value: "could not tell: the report holds no sample", Unmeasured: true},
			{Label: "host level", Value: host},
		}
	}
	var used, cached int64
	available := int64(math.MaxInt64)
	for _, s := range r.Samples {
		used, cached = max(used, s.MemUsedBytes), max(cached, s.MemCachedBytes)
		available = min(available, s.MemAvailableBytes)
	}

	return []pageField{
		{Label: "used", Value: "peak " + cli.HumanBytes(used) + " (MemTotal less MemAvailable)"},
		{Label: "page cache", Value: "peak " + cli.HumanBytes(cached) + " (Cached)"},
		{Label: "available", Value: "lowest " + cli.HumanBytes(available)},
		{Label: "host level", Value: host},
	}
}

// guestChecks sets the guest's own counters beside the host's where both
// measured the same thing. Each line shows both and decides nothing.
func guestChecks(r *guestreport.Report, m *alloc.RecordedUsage) []pageField {
	if len(r.Samples) == 0 {
		return []pageField{{Label: "cross-check", Value: "could not tell: the report holds no sample",
			Unmeasured: true}}
	}
	last := r.Samples[len(r.Samples)-1]
	guestCPU := float64(last.CPUBusyTicks) / float64(r.TicksPerSecond)
	cpu := fmt.Sprintf("the guest counted %s busy since it booted, to its last sample", floatSeconds(guestCPU))
	net := fmt.Sprintf("the guest's eth0 counted received %s, sent %s since it booted, to its last sample",
		cli.HumanBytes(last.NetRxBytes), cli.HumanBytes(last.NetTxBytes))
	checks := func(cpu, net string) []pageField {
		return []pageField{{Label: "cpu check", Value: cpu}, {Label: "net check", Value: net}}
	}
	if m == nil {
		const none = "; no usage report was recorded, so there is nothing to check it against"
		return checks(cpu+none, net+none)
	}
	switch {
	case m.Measured(alloc.UsageThreads):
		host := float64(m.GuestCPUMicros) / 1e6
		cpu += fmt.Sprintf("; the host counted %s on its vCPU threads over the whole lease (guest minus "+
			"host %+.1fs)", seconds(m.GuestCPUMicros), guestCPU-host)
	case m.Measured(alloc.UsageCPU):
		cpu += fmt.Sprintf("; the host counted %s for the whole VM, its vCPU split not measured",
			seconds(m.CPUUserMicros+m.CPUSystemMicros))
	default:
		cpu += "; the host's CPU was not measured"
	}
	if m.Measured(alloc.UsageNet) {
		net += fmt.Sprintf("; the host counted received %s, sent %s on the tap over the whole lease "+
			"(guest minus host: received %s, sent %s)", cli.HumanBytes(m.NetRxBytes),
			cli.HumanBytes(m.NetTxBytes),
			signedBytes(new(big.Int).Sub(big.NewInt(last.NetRxBytes), big.NewInt(m.NetRxBytes))),
			signedBytes(new(big.Int).Sub(big.NewInt(last.NetTxBytes), big.NewInt(m.NetTxBytes))))
	} else {
		net += "; the host's network was not measured"
	}

	return checks(cpu, net)
}

// guestAgent is what the agent cost the job, by its own count and the node's.
func guestAgent(row alloc.RecordedGuestReport, r *guestreport.Report) []pageField {
	agentCPU := float64(r.AgentCPUTicks) / float64(r.TicksPerSecond)
	share := "of the guest's busy CPU: could not tell, the report holds no sample"
	if n := len(r.Samples); n > 0 && r.Samples[n-1].CPUBusyTicks > 0 {
		share = fmt.Sprintf("%.1f%% of the guest's busy CPU since it booted",
			float64(r.AgentCPUTicks)/float64(r.Samples[n-1].CPUBusyTicks)*100)
	}

	return []pageField{
		{Label: "agent cpu", Value: fmt.Sprintf("%s, %s (its own count)", floatSeconds(agentCPU), share)},
		{Label: "agent sent", Value: fmt.Sprintf("%d batches accepted, so at most %s of batch bodies on the "+
			"tap, plus HTTP and TCP framing and any batches the node refused (it counts the batches it "+
			"accepted, not their bytes)", row.Accepted,
			bigBytes(new(big.Int).Mul(big.NewInt(row.Accepted), big.NewInt(guestreport.MaxBatchBytes))))},
	}
}

// guestSteps is the guest's step marks matched to GitHub's steps, and its
// processes placed in the steps the marks begin.
func guestSteps(r *guestreport.Report, listed []github.JobStep) guestStepsView {
	var out guestStepsView
	matched := matchMarks(r.Steps, listed)

	if len(r.Steps) == 0 {
		out.MarksWhy = "the guest marked no steps (the runner's log gave it none), so its processes are not " +
			"placed in steps"
	} else {
		out.MarksNote = "Each mark is the runner's log line for a step's start, stamped by the guest's clock " +
			"to the second, matched by name, in order, to GitHub's steps, whose numbers and names are the " +
			"authority."
		if listed == nil {
			out.MarksNote += " GitHub's steps are not here (see Steps), so no mark is matched to one."
		}
		for i, mark := range r.Steps {
			row := guestMarkRow{Mark: strconv.Itoa(i + 1), Name: strconv.Quote(mark.Name),
				At: stamp(time.Unix(mark.AtSeconds, 0)), Step: "matched to no GitHub step"}
			if j := matched[i]; j >= 0 {
				// BY NUMBER ALONE: the step's name, as the agent cut it, is the
				// mark's, and GitHub's whole name is the Steps section's to show.
				step := listed[j]
				row.Step = fmt.Sprintf("step %d", step.Number)
				row.GitHub = "not given"
				if !step.StartedAt.IsZero() {
					row.GitHub = stamp(step.StartedAt)
					row.Offset = signedSpan((mark.AtSeconds - step.StartedAt.Unix()) * 1000)
				}
			}
			out.Marks = append(out.Marks, row)
		}
	}

	if len(r.Processes) == 0 {
		out.ProcsWhy = "the report holds no process sample"
		return out
	}
	interval := guestreport.ProcessInterval * time.Duration(r.Stride)
	out.ProcsNote = fmt.Sprintf("Each process sample covers the %s before it and is placed in the step it "+
		"ended in, so a step's edges blur by that much. A process is named by its comm, never its arguments; "+
		"the busiest %d names of each are shown and the rest summed.", interval, guestTopProcesses)
	// A MARK IS LOST WITH ITS BATCH, and the processes after it then fall to the
	// mark before: no group can then be said to hold one step's alone.
	if len(r.Steps) > 0 && (r.Missing > 0 || r.Refused > 0) {
		out.ProcsNote += fmt.Sprintf(" Not every batch between the agent's first and last is in the report "+
			"(%d never arrived, %d refused by the merge), and a step mark in one of them is lost with it, so a "+
			"group may also hold processes of steps that came after its own.", r.Missing, r.Refused)
	}

	buckets := make([][]guestreport.ProcessSample, len(r.Steps)+1)
	for _, p := range r.Processes {
		k := sort.Search(len(r.Steps), func(i int) bool { return r.Steps[i].AtSeconds*1000 > p.AtMillis })
		buckets[k] = append(buckets[k], p)
	}
	for k, samples := range buckets {
		var group guestStepProcs
		switch {
		case len(r.Steps) == 0:
			group.Label, group.Name = "whole", "report (the guest marked no steps)"
		case k == 0:
			if len(samples) == 0 {
				continue
			}
			group.Label, group.Name = "before", "the first mark"
		case matched[k-1] >= 0:
			// GITHUB'S NAME AS THE AGENT CUT IT, which is what matched.
			step := listed[matched[k-1]]
			group.Label = "step " + strconv.FormatInt(step.Number, 10)
			group.Name = strconv.Quote(guestreport.Clean(step.Name, guestreport.MaxNameBytes))
		default:
			group.Label = "mark " + strconv.Itoa(k)
			group.Name = strconv.Quote(r.Steps[k-1].Name) + " (the guest's name, matched to no GitHub step)"
		}
		// MARKS PAST THE REPORT'S BOUND WERE DROPPED, so the processes after the
		// last mark kept may belong to the steps those marks began.
		if k == len(r.Steps) && k > 0 && r.Dropped.Steps > 0 {
			group.Name += fmt.Sprintf(", and whatever came after it: %d later marks were left out past the "+
				"report's bound", r.Dropped.Steps)
		}
		fillProcs(&group, samples, r.TicksPerSecond)
		out.Procs = append(out.Procs, group)
	}

	return out
}

// matchMarks is, for each mark, the index of the GitHub step it names, or -1:
// matched by name, in order, each step at most once. A step's name is compared
// as the agent would have cut it.
func matchMarks(marks []guestreport.StepMark, listed []github.JobStep) []int {
	out := make([]int, len(marks))
	next := 0
	for i, mark := range marks {
		out[i] = -1
		for j := next; j < len(listed); j++ {
			if guestreport.Clean(listed[j].Name, guestreport.MaxNameBytes) == mark.Name {
				out[i], next = j, j+1
				break
			}
		}
	}

	return out
}

// procTotal is a process name over several samples: what each added, summed as
// floats so no sum of the guest's figures can wrap, and its levels' highest.
type procTotal struct {
	name               string
	procs, rss         int64
	cpu, read, written float64
}

func (t *procTotal) add(u guestreport.ProcessUsage) {
	t.procs, t.rss = max(t.procs, u.Procs), max(t.rss, u.RSSBytes)
	t.cpu += float64(u.CPUTicks)
	t.read += float64(u.ReadBytes)
	t.written += float64(u.WriteBytes)
}

// fillProcs sums a step's process samples by name and words the busiest.
func fillProcs(group *guestStepProcs, samples []guestreport.ProcessSample, ticks int64) {
	if len(samples) == 0 {
		group.Samples = "no process sample ended inside it"
		return
	}
	plural := "s"
	if len(samples) == 1 {
		plural = ""
	}
	group.Samples = fmt.Sprintf("%d process sample%s, ending %s to %s on the guest's clock", len(samples), plural,
		stampMillis(time.UnixMilli(samples[0].AtMillis)), stampMillis(time.UnixMilli(samples[len(samples)-1].AtMillis)))

	index := map[string]int{}
	var totals []procTotal
	var other procTotal
	var exited float64
	for _, p := range samples {
		for _, row := range p.Rows {
			i, ok := index[row.Name]
			if !ok {
				i = len(totals)
				index[row.Name] = i
				totals = append(totals, procTotal{name: row.Name})
			}
			totals[i].add(row.ProcessUsage)
		}
		other.add(p.Other)
		exited += float64(p.ResidualCPUTicks)
	}
	slices.SortFunc(totals, func(a, b procTotal) int {
		return cmp.Or(cmp.Compare(b.cpu, a.cpu), cmp.Compare(a.name, b.name))
	})
	shown := totals[:min(len(totals), guestTopProcesses)]
	for _, t := range totals[len(shown):] {
		other.cpu += t.cpu
		other.read += t.read
		other.written += t.written
	}
	cpu := func(v float64) string { return floatSeconds(v / float64(ticks)) }
	for _, t := range shown {
		group.Rows = append(group.Rows, guestProcRow{Name: strconv.Quote(t.name), Procs: groupDigits(t.procs),
			CPU: cpu(t.cpu), RSS: cli.HumanBytes(t.rss), Read: floatBytes(t.read), Written: floatBytes(t.written)})
	}
	group.Rest = fmt.Sprintf("every other process (%d more names, and those past the agent's own cut): cpu %s, "+
		"read %s, written %s", len(totals)-len(shown), cpu(other.cpu), floatBytes(other.read),
		floatBytes(other.written))
	group.Exited = "cpu no live process was charged with (processes that exited inside an interval): " +
		cpu(exited)
}

// guestTests is the test results the agent found. They come in its last batch:
// finalSeen says the node saw it arrive, and finalKept that the report is shown
// to hold it.
func guestTests(r *guestreport.Report, finalSeen, finalKept bool) guestTestsView {
	var out guestTestsView
	switch {
	case len(r.Tests) == 0 && !finalSeen:
		out.Why = "could not tell: the agent sends its test results in its last batch, which did not arrive"
	case len(r.Tests) == 0 && !finalKept:
		out.Why = "could not tell: the agent sends its test results in its last batch, which arrived, and the " +
			"report cannot show that it holds that batch"
	case len(r.Tests) == 0:
		out.Why = "the agent found no test results (it reads the JUnit XML files it finds when the job ends)"
	}
	for _, s := range r.Tests {
		row := guestSuiteRow{Name: strconv.Quote(s.Name), Tests: groupDigits(s.Tests),
			Failures: groupDigits(s.Failures), Errors: groupDigits(s.Errors), Skipped: groupDigits(s.Skipped)}
		for _, name := range s.Failed {
			row.Failed = append(row.Failed, strconv.Quote(name))
		}
		out.Suites = append(out.Suites, row)
	}
	if d := r.Dropped; d.Suites > 0 || d.FailedNames > 0 {
		out.Dropped = fmt.Sprintf("%d suites and %d failed test names past the report's bounds are left out",
			d.Suites, d.FailedNames)
	}

	return out
}

// renderGuest writes the guest section of a job's report.
//
// EVERY STRING THE GUEST WROTE IS ALREADY QUOTED in the view, so none can forge
// a line.
func renderGuest(w io.Writer, v guestView) {
	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	fields := func(fs []pageField) {
		for _, f := range fs {
			line(f.Label, "%s", f.Value)
		}
	}
	fmt.Fprintln(w)
	line("guest", "%s", v.Why)
	if v.Banner != "" {
		line("", "%s", v.Banner)
	}
	fields(v.Channel)
	if !v.HasReport {
		return
	}
	fields(v.Account)
	line("skew", "%s", v.Skew)
	fields(v.Memory)
	fields(v.Checks)
	fields(v.Agent)

	s := v.Steps
	if s.MarksWhy != "" {
		line("marks", "%s", s.MarksWhy)
	} else {
		line("marks", "%s", s.MarksNote)
	}
	for _, m := range s.Marks {
		text := fmt.Sprintf("%s at %s on the guest's clock, %s", m.Name, m.At, m.Step)
		if m.GitHub != "" {
			text += ", GitHub's start " + m.GitHub
		}
		if m.Offset != "" {
			text += ", mark minus GitHub's start " + m.Offset
		}
		line("mark "+m.Mark, "%s", text)
	}
	if s.ProcsWhy != "" {
		line("processes", "%s", s.ProcsWhy)
	} else {
		line("processes", "%s", s.ProcsNote)
	}
	for _, p := range s.Procs {
		head := p.Samples
		if p.Name != "" {
			head = p.Name + ", " + head
		}
		line(p.Label, "%s", head)
		for _, row := range p.Rows {
			line("", "  %s x%s: cpu %s, peak rss %s, read %s, written %s", row.Name, row.Procs, row.CPU, row.RSS,
				row.Read, row.Written)
		}
		if p.Rest != "" {
			line("", "  %s", p.Rest)
			line("", "  %s", p.Exited)
		}
	}

	t := v.Tests
	if t.Why != "" {
		line("tests", "%s", t.Why)
	}
	for _, suite := range t.Suites {
		line("suite", "%s: %s tests, %s failures, %s errors, %s skipped", suite.Name, suite.Tests, suite.Failures,
			suite.Errors, suite.Skipped)
		if len(suite.Failed) > 0 {
			line("", "  failed: %s", strings.Join(suite.Failed, ", "))
		}
	}
	if t.Dropped != "" {
		line("", "%s", t.Dropped)
	}
}

// guestJSON is the guest member of `billet jobs show --json`: null when no row
// was recorded.
type guestJSON struct {
	Notice  string `json:"notice"`
	Verdict string `json:"verdict"`
	Why     string `json:"why"`
	// Channel is what the node counted, null when the row could not be read.
	Channel *guestChannelJSON `json:"channel"`
	// Report is the guest's decoded report, null when there is none to show.
	Report *guestreport.Report `json:"report"`
}

// guestChannelJSON is the row's own fields. An empty time is one the node did
// not record.
type guestChannelJSON struct {
	Node          string `json:"node"`
	RecordedAt    string `json:"recorded_at"`
	AgentVersion  string `json:"agent_version"`
	Schema        int    `json:"schema"`
	Codec         int    `json:"codec"`
	EncodedBytes  int    `json:"encoded_bytes"`
	Accepted      int64  `json:"accepted"`
	Refused       int64  `json:"refused"`
	DroppedBytes  int64  `json:"dropped_bytes"`
	Hello         bool   `json:"hello"`
	FinalSeen     bool   `json:"final_seen"`
	NodeRestarted bool   `json:"node_restarted"`
	FirstReceived string `json:"first_received"`
	LastReceived  string `json:"last_received"`
}

// guestJSONOf is a lease's guest member: nil with no row.
func guestJSONOf(g *jobGuest) *guestJSON {
	if g == nil {
		return nil
	}
	out := &guestJSON{Notice: guestBanner, Verdict: string(g.verdict), Why: g.why, Report: g.report}
	if g.readable {
		row := g.row
		at := func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.UTC().Format(time.RFC3339Nano)
		}
		out.Channel = &guestChannelJSON{Node: row.Node, RecordedAt: row.RecordedAt, AgentVersion: row.AgentVersion,
			Schema: row.Schema, Codec: row.Codec, EncodedBytes: len(row.Data), Accepted: row.Accepted,
			Refused: row.Refused, DroppedBytes: row.DroppedBytes, Hello: row.Hello, FinalSeen: row.FinalSeen,
			NodeRestarted: row.NodeRestarted, FirstReceived: at(row.FirstReceived), LastReceived: at(row.LastReceived)}
	}

	return out
}

// stampMillis is a time to the millisecond, in UTC.
func stampMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// floatSeconds writes a CPU time held as a float.
func floatSeconds(s float64) string { return strconv.FormatFloat(s, 'f', 1, 64) + "s" }

// spanMillis writes a span of milliseconds the guest's clock chose, which can
// pass what a time.Duration holds.
func spanMillis(ms int64) string {
	const durationBound = math.MaxInt64 / int64(time.Millisecond)
	if ms > durationBound || ms < -durationBound {
		return fmt.Sprintf("%d days", ms/(24*3600*1000))
	}

	return (time.Duration(ms) * time.Millisecond).String()
}

// signedSpan writes a span of milliseconds with its sign.
func signedSpan(ms int64) string {
	if ms < 0 {
		return "-" + spanMillis(-ms)
	}

	return "+" + spanMillis(ms)
}

// guestChart is the guest's memory drawn on the host's series, or why it is not.
type guestChart struct {
	Why   string
	Chart pageChart
}

// guestMemoryChart draws the guest's used memory and page cache on the host's
// charts' axis, with the host's own level and the lease's bound, each window the
// highest level the guest sampled in it and a break where it sampled none.
//
// PLACED BY THE GUEST'S OWN CLOCK, with no correction for skew, as the steps are
// placed by GitHub's: the page says so beside the chart.
func guestMemoryChart(c pageCharts, rec alloc.JobRecord, m *alloc.RecordedUsage, r *guestreport.Report,
) guestChart {
	switch {
	case len(r.Samples) == 0:
		return guestChart{Why: "the report holds no sample of the guest's memory, so nothing is drawn"}
	case c.tl == nil: // buildCharts keeps the series only when it drew it
		return guestChart{Why: "the guest's memory is drawn on the host's series, and none is drawn: " + c.Why}
	}
	n := len(c.windows)
	used, cached := make([]int64, n), make([]int64, n)
	sampled := make([]bool, n)
	outside := 0
	highest := float64(max(rec.Memory, 0))
	for _, s := range r.Samples {
		at := time.UnixMilli(s.AtMillis)
		i := sort.Search(n, func(i int) bool { return !c.windows[i].to.Before(at) })
		if i == n || at.Before(c.windows[0].from) {
			outside++
			continue
		}
		used[i], cached[i] = max(used[i], s.MemUsedBytes), max(cached[i], s.MemCachedBytes)
		sampled[i] = true
		highest = max(highest, float64(s.MemUsedBytes), float64(s.MemCachedBytes))
	}
	if outside == len(r.Samples) {
		return guestChart{Why: "none of the guest's samples falls inside the host's series on the two " +
			"clocks, so nothing is drawn"}
	}
	hostMeasured := m != nil && m.Measured(alloc.UsageMemory)
	if hostMeasured {
		highest = max(highest, float64(peakLevel(*c.tl)))
	}
	top := chartTop(highest)
	level := func(class, label string, of []int64) chartLine {
		var p svgPath
		for i, w := range c.windows {
			if !sampled[i] {
				p.lift()
				continue
			}
			p.level(c.x(w.from), c.x(w.to), chartY(float64(of[i]), top))
		}
		return chartLine{Class: class, Label: label, D: p.String()}
	}
	chart := pageChart{Title: "Guest memory (the guest's own view)", Top: floatBytes(top),
		Lines: []chartLine{level("a", "guest used", used), level("b", "guest page cache", cached)}}
	if hostMeasured {
		host, _ := memoryChart(*c.tl, c.windows, c.used, c.x, c.sampled, rec.Memory, top)
		host[0].Class, host[0].Label = "c", "the host's level"
		chart.Lines = append(chart.Lines, host[0])
	}
	if rec.Memory > 0 {
		chart.Limit = &chartLimit{Y: chartY(float64(rec.Memory), top),
			Label: "tier memory " + cli.HumanBytes(rec.Memory)}
	}
	chart.Note = "Placed on the host's series by the guest's own clock, with no correction for skew. Used is " +
		"MemTotal less MemAvailable; page cache is Cached, which the host's level also counts for a " +
		"microVM's disk."
	if !hostMeasured {
		chart.Note += " The host's level is not drawn: it was not measured."
	}
	if outside > 0 {
		chart.Note += fmt.Sprintf(" %d of the guest's %d samples fall outside the host's series on the two "+
			"clocks and are not drawn.", outside, len(r.Samples))
	}

	return guestChart{Chart: chart}
}
