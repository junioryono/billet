package fleetops

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/usage"
)

// The job page's template, style sheet and script, embedded so the page is one
// file that fetches nothing.
var (
	//go:embed jobpage/page.html.tmpl
	jobPageSource string
	//go:embed jobpage/page.css
	jobPageStyle string
	//go:embed jobpage/page.js
	jobPageScriptFile string

	jobPageScript   = inlineScript(jobPageScriptFile)
	jobPageTemplate = template.Must(template.New("page").Parse(jobPageSource))
)

// inlineScript is a script as a browser hashes it once inline: the HTML parser
// turns every CRLF and lone CR into LF before the policy sees the text, so a
// checkout with CRLF line ends would otherwise name a hash no browser computes.
func inlineScript(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// jobPagePolicy is the page's Content-Security-Policy: nothing may be fetched,
// inline style is allowed, and the one script runs only because its hash is
// named here, so no string a workflow chose can become a script even if it
// escaped its element.
func jobPagePolicy() string {
	sum := sha256.Sum256([]byte(jobPageScript))

	return "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-" +
		base64.StdEncoding.EncodeToString(sum[:]) + "'; base-uri 'none'; form-action 'none'"
}

// jobPage is what `billet jobs show --html` shows, already worded: the
// template lays it out and decides nothing. It is built from what the text
// report reads and nothing more, so a served page can render the same model.
type jobPage struct {
	Title, Heading string
	// Result is GitHub's word for how the job ended, and ResultClass the colour
	// it is shown in.
	Result, ResultClass string
	Identity            []pageField
	Usage               pageUsage
	Steps               pageSteps
	Charts              pageCharts
	Destinations        pageDestinations
	Guest               guestView
}

// pageField is one labelled value. Unmeasured marks a value that is "not
// measured", which the page sets apart from a measurement.
type pageField struct {
	Label, Value string
	Unmeasured   bool
}

// pageUsage is the usage section: who measured the job, then each group and
// each counter as the text report words them. Report alone is set when no
// report was recorded.
type pageUsage struct {
	Report   string
	Groups   []pageField
	Counters []pageField
	// Series, SeriesGaps and SeriesMore are the series' account of its own
	// sampling, worded as the text report's series section is.
	Series     string
	SeriesGaps []string
	SeriesMore string
}

// pageSteps is the steps section: why there are none, or GitHub's job and
// each of its steps.
type pageSteps struct {
	Why       string
	Head      string
	Alignment string
	// Aligned says each step's usage was read from the series; without it,
	// Alignment says why none could be.
	Aligned bool
	Rows    []pageStep
}

// pageStep is one step as GitHub records it, where it sits among the job's
// steps on GitHub's clock, and what the series says about its window.
type pageStep struct {
	// Key is the step's place in GitHub's list, the same as its band's on the
	// charts; Number is GitHub's, which two steps can share.
	Key                  int
	Number               int64
	Name, Outcome, Class string
	Started, Took        string
	// Note says why the step has no usage, or qualifies the usage it has.
	Note string
	// Usage is the step's window, one cell a group; nil when it has none.
	Usage []pageField
	// Bar places the step among the steps (thousandths of their span); HasBar
	// is false for a step with no length.
	HasBar     bool
	BarX, BarW int64
	// Focus is the step's window on the charts' axis, when it has one there.
	Focus    bool
	From, To int64
}

// pageDestinations is where the job's traffic went.
type pageDestinations struct {
	Why     string
	Summary string
	Rows    []pageDestination
	More    string
	Other   string
	// Complete answers whether the totals are complete.
	Complete string
	Tap      string
	TapNote  string
}

// pageDestination is one destination's row.
type pageDestination struct {
	Addr, Sent, Received, Connections string
}

// buildJobPage is the page for a job: its record, its usage report and the
// steps readJobSteps learned. The series is the one the steps were placed on;
// when no step was asked about, it is read here for the charts.
func buildJobPage(ctx context.Context, a *alloc.Allocator, rec alloc.JobRecord, measured *alloc.RecordedUsage,
	steps jobSteps, guest *jobGuest,
) jobPage {
	tl, unaligned := steps.timeline, steps.unaligned
	if steps.none != "" {
		tl, unaligned = jobTimeline(ctx, a, rec, measured)
	}
	page := jobPageOf(rec, measured, steps, tl, unaligned)
	page.Guest = guestSection(guest, rec, measured, steps, page.Charts)

	return page
}

// guestSection is the page's guest section: the text report's, and the guest's
// memory drawn on the host's charts' axis.
func guestSection(guest *jobGuest, rec alloc.JobRecord, measured *alloc.RecordedUsage, steps jobSteps,
	charts pageCharts,
) guestView {
	v := retiredGuest(guestViewOf(guest, measured, steps), guest, rec)
	if v.HasReport {
		v.Chart = guestMemoryChart(charts, rec, measured, guest.report)
	}

	return v
}

// jobPageOf is buildJobPage once the series is read.
func jobPageOf(rec alloc.JobRecord, measured *alloc.RecordedUsage, steps jobSteps, tl *usage.Timeline,
	unaligned string,
) jobPage {
	page := jobPage{
		Title:   "billet job " + rec.LeaseID,
		Heading: rec.Job.Name,
		Result:  orNotRecorded(rec.Result),
	}
	if page.Heading == "" {
		page.Heading = "lease " + rec.LeaseID
	}
	switch rec.Result {
	case "succeeded":
		page.ResultClass = "ok"
	case "failed":
		page.ResultClass = "bad"
	default:
		page.ResultClass = "other"
	}
	page.Identity = identityFields(rec)

	page.Usage = usageSection(measured)
	if v, ok := seriesViewOf(tl, unaligned, measured); ok {
		page.Usage.Series, page.Usage.SeriesGaps, page.Usage.SeriesMore = v.head, v.gaps, v.more
	}
	var listed []github.JobStep
	if steps.none == "" && steps.err == nil {
		listed = steps.job.Steps
	}
	page.Charts = buildCharts(tl, unaligned, rec, measured, listed)
	page.Steps = stepsSection(steps, tl, unaligned, measured, page.Charts)
	page.Destinations = destinationsSection(rec, measured)
	page.Guest = guestViewOf(nil, measured, steps)

	return page
}

// identityFields is the job's record: what GitHub named it, how it ended, its
// times, and where it ran.
func identityFields(rec alloc.JobRecord) []pageField {
	shape := "not recorded"
	if rec.VCPU > 0 {
		shape = fmt.Sprintf("%d vCPU, %s", rec.VCPU, cli.HumanBytes(rec.Memory))
	}
	request := "none (a pooled lease's request is billet's own, not GitHub's)"
	if rec.RequestID > 0 {
		request = strconv.FormatInt(rec.RequestID, 10)
	}
	run := "not recorded"
	if rec.RunID > 0 {
		run = strconv.FormatInt(rec.RunID, 10)
	}

	return []pageField{
		{Label: "repository", Value: orNotRecorded(rec.Repo)},
		{Label: "workflow", Value: orNotRecorded(rec.Job.WorkflowRef)},
		{Label: "job", Value: orNotRecorded(rec.Job.Name)},
		{Label: "event", Value: orNotRecorded(rec.Job.Event)},
		{Label: "github job", Value: orNotRecorded(rec.Job.JobID)},
		{Label: "run", Value: run},
		{Label: "request", Value: request},
		{Label: "result", Value: "GitHub " + orNotRecorded(rec.Result) + "; billet " + orNotRecorded(rec.Conclusion)},
		{Label: "queued", Value: orNotRecorded(rec.QueuedAt)},
		{Label: "assigned", Value: orNotRecorded(rec.AssignedAt)},
		{Label: "started", Value: orNotRecorded(rec.StartedAt)},
		{Label: "finished", Value: orNotRecorded(rec.FinishedAt)},
		{Label: "github times", Value: githubTimes(rec, orNotRecorded)},
		{Label: "waited", Value: waitedLine(rec)},
		{Label: "detail", Value: orKept(rec)},
		{Label: "tier", Value: orNotRecorded(rec.Tier)},
		{Label: "node", Value: orNotRecorded(rec.Node)},
		{Label: "provider", Value: orNotRecorded(rec.ChosenProvider)},
		{Label: "shape", Value: shape},
		{Label: "lease", Value: rec.LeaseID},
	}
}

func orNotRecorded(s string) string {
	if s == "" {
		return "not recorded"
	}

	return s
}

// orKept is whether a job's detail is still kept, or when it was retired.
func orKept(rec alloc.JobRecord) string {
	if rec.DetailRetiredAt == "" {
		return "kept"
	}

	return detailRetired(rec)
}

// collect is a line function, as renderJob's sections take, that keeps each
// line as a field: the page words a group exactly as the text report does.
func collect(into *[]pageField) func(label, format string, args ...any) {
	return func(label, format string, args ...any) {
		value := fmt.Sprintf(format, args...)
		*into = append(*into, pageField{Label: label, Value: value, Unmeasured: value == "not measured"})
	}
}

// usageSection is the usage report's groups and counters.
func usageSection(u *alloc.RecordedUsage) pageUsage {
	if u == nil {
		return pageUsage{Report: noUsageReport}
	}
	out := pageUsage{Report: measuredBy(u)}
	renderUsageGroups(collect(&out.Groups), u)
	renderCounters(collect(&out.Counters), u.Counters)

	return out
}

// stepsSection is the steps GitHub listed and what the series says about
// each, worded as renderSteps words them.
func stepsSection(s jobSteps, tl *usage.Timeline, unaligned string, measured *alloc.RecordedUsage,
	charts pageCharts,
) pageSteps {
	switch {
	case s.none != "":
		return pageSteps{Why: s.none}
	case errors.Is(s.err, github.ErrNoRunnerJob):
		return pageSteps{Why: "GitHub's record of the run lists no job on runner " + s.runner + ", so there are " +
			"no steps to show (GitHub keeps a job's record about as long as its run, and a deleted run keeps none)"}
	case s.err != nil:
		return pageSteps{Why: "The steps could not be read from GitHub: " + s.err.Error() + permissionHint(s.err)}
	}

	attempt := "attempt not given"
	if s.job.RunAttempt > 0 {
		attempt = fmt.Sprintf("attempt %d", s.job.RunAttempt)
	}
	out := pageSteps{
		Head: fmt.Sprintf("GitHub job %d %s, %s, on runner %s, steps listed: %d", s.job.ID,
			strconv.Quote(s.job.Name), attempt, s.runner, len(s.job.Steps)),
		Aligned: tl != nil && measured != nil,
	}
	if out.Aligned {
		out.Alignment = "Aligned to the second on two clocks, GitHub's for the steps and the host's for its " +
			"samples, with no correction for skew between them; within one sample interval, usage is split by time."
	} else {
		out.Alignment = "Usage per step: could not tell, because " + unaligned + "."
	}

	var lo, hi time.Time
	for _, step := range s.job.Steps {
		if step.StartedAt.IsZero() || step.CompletedAt.IsZero() || !step.CompletedAt.After(step.StartedAt) {
			continue
		}
		if lo.IsZero() || step.StartedAt.Before(lo) {
			lo = step.StartedAt
		}
		if hi.IsZero() || step.CompletedAt.After(hi) {
			hi = step.CompletedAt
		}
	}
	for i, step := range s.job.Steps {
		row := stepRow(step, tl, measured)
		row.Key = i
		if !lo.IsZero() && step.CompletedAt.After(step.StartedAt) && !step.StartedAt.IsZero() {
			whole := float64(hi.Sub(lo))
			row.HasBar = true
			row.BarX = int64(float64(step.StartedAt.Sub(lo)) / whole * 1000)
			row.BarW = max(int64(float64(step.CompletedAt.Sub(step.StartedAt))/whole*1000), 3)
		}
		if out.Aligned {
			row.From, row.To, row.Focus = charts.stepWindow(step, tl)
		}
		out.Rows = append(out.Rows, row)
	}

	return out
}

// stepRow is one step: GitHub's account of it, then its window's usage, with
// the same verdicts renderStep prints.
func stepRow(step github.JobStep, tl *usage.Timeline, measured *alloc.RecordedUsage) pageStep {
	row := pageStep{Number: step.Number, Name: step.Name, Outcome: step.Conclusion}
	if row.Outcome == "" {
		row.Outcome = step.Status
	}
	switch row.Outcome {
	case "success":
		row.Class = "ok"
	case "failure", "cancelled", "timed_out":
		row.Class = "bad"
	default:
		row.Class = "other"
	}
	if !step.StartedAt.IsZero() {
		row.Started = stamp(step.StartedAt)
	}

	switch {
	case step.StartedAt.IsZero() && step.CompletedAt.IsZero():
		row.Note = "no start or end time from GitHub, so no usage"
		return row
	case step.CompletedAt.IsZero():
		row.Note = "no end time from GitHub, so no duration or usage"
		return row
	case step.StartedAt.IsZero():
		row.Note = "ended " + stamp(step.CompletedAt) + ", no start time from GitHub, so no duration or usage"
		return row
	}
	took := step.CompletedAt.Sub(step.StartedAt)
	switch {
	case took < 0:
		row.Note = fmt.Sprintf("GitHub's end is %s before its start, so no usage", -took)
		return row
	case took == 0:
		row.Took = "0s"
		row.Note = "start and end in the same second, so no usage can be placed in it"
		return row
	}
	row.Took = took.String()
	if tl == nil || measured == nil {
		row.Note = "could not tell"
		return row
	}

	used := windowOf(*tl, step.StartedAt, step.CompletedAt)
	switch {
	case used.Covered == 0:
		row.Note = "the series covers none of this step, so no usage"
		return row
	case !used.Complete():
		// THE PEAK IS NOT THE COVERED PART'S: it is the highest level sampled
		// anywhere inside the step, which a sample next to unseen time can set.
		row.Note = fmt.Sprintf("partial: the series covers %s of the step's %s; CPU, disk, network and "+
			"energy are over that part, and the peak memory is the highest level sampled anywhere in the step",
			used.Covered, used.Span)
	case used.Samples == 0:
		row.Note = "no sample inside the step: its share of the interval around it, split by time"
	}
	row.Usage = windowCells(used, measured)

	return row
}

// windowCells is what a window used, one cell a group, "not measured" for a
// group the host never read, as windowText words it.
func windowCells(u usage.WindowUsage, m *alloc.RecordedUsage) []pageField {
	cell := func(group, measured string) pageField {
		if !m.Measured(group) {
			return pageField{Value: "not measured", Unmeasured: true}
		}
		return pageField{Value: measured}
	}
	memory := "not sampled inside the step"
	if u.Samples > 0 {
		memory = cli.HumanBytes(u.MemoryPeak)
	}
	energy := joules(u.EnergyActive)
	if m.EnergySource == alloc.EnergyRAPL {
		energy += " active"
	}

	return []pageField{
		cell(alloc.UsageCPU, seconds(u.CPUMicros)),
		cell(alloc.UsageMemory, memory),
		cell(alloc.UsageIO, cli.HumanBytes(u.DiskRead)),
		cell(alloc.UsageIO, cli.HumanBytes(u.DiskWrite)),
		cell(alloc.UsageNet, cli.HumanBytes(u.NetRx)),
		cell(alloc.UsageNet, cli.HumanBytes(u.NetTx)),
		cell(alloc.UsageEnergy, energy),
	}
}

// destinationsSection is where the job's traffic went, worded as
// renderDestinations words it.
func destinationsSection(rec alloc.JobRecord, m *alloc.RecordedUsage) pageDestinations {
	switch {
	case m == nil:
		return pageDestinations{Why: "No usage report was recorded."}
	case m.Destinations == nil && rec.DetailRetiredAt != "":
		return pageDestinations{Why: "Destinations " + detailRetired(rec) + "."}
	case m.Destinations == nil:
		return pageDestinations{Why: "Destinations not measured (node.monitoring.flows, on a firecracker node)."}
	}
	d := m.Destinations
	sorted := slices.Clone(d.Destinations)
	slices.SortStableFunc(sorted, func(x, y alloc.JobDestination) int {
		return exact(y.SentBytes, y.ReceivedBytes).Cmp(exact(x.SentBytes, x.ReceivedBytes))
	})
	shown := sorted[:min(len(sorted), shownDestinations)]

	out := pageDestinations{
		Summary: fmt.Sprintf("%d destinations by total bytes, from the host's connection tracker "+
			"(attributed by the guest's source address)", len(sorted)),
		Other: fmt.Sprintf("%s (beyond the %d destinations the node keeps by name)", sumOf(d.Other),
			alloc.MaxJobDestinations),
		Complete: "yes",
	}
	for _, dest := range shown {
		out.Rows = append(out.Rows, pageDestination{Addr: dest.Addr, Sent: cli.HumanBytes(dest.SentBytes),
			Received: cli.HumanBytes(dest.ReceivedBytes), Connections: groupDigits(dest.Connections)})
	}
	if rest := sorted[len(shown):]; len(rest) > 0 {
		out.More = fmt.Sprintf("and %d more: %s", len(rest), sumOf(rest...))
	}
	if d.Incomplete {
		out.Complete = "no: flows may have been missed, so every total here is a lower bound"
	}
	if d.Tap == nil {
		out.Tap = "not read, so there is nothing to compare the destinations with"
		return out
	}
	attributed := sumOf(append(slices.Clone(d.Destinations), d.Other)...)
	out.Tap = fmt.Sprintf("sent %s, received %s; tap minus attributed: sent %s, received %s",
		cli.HumanBytes(d.Tap.SentBytes), cli.HumanBytes(d.Tap.ReceivedBytes),
		signedBytes(new(big.Int).Sub(big.NewInt(d.Tap.SentBytes), attributed.sent)),
		signedBytes(new(big.Int).Sub(big.NewInt(d.Tap.ReceivedBytes), attributed.received)))
	out.TapNote = "A comparison of two counters read a moment apart, not a measurement of what was missed."

	return out
}

// jobPageDocument is what the template renders: the page and its assets.
type jobPageDocument struct {
	Page   jobPage
	Policy string
	Style  template.CSS
	Script template.JS
}

// renderJobPage writes the page as one HTML document. Every string in it is
// escaped by html/template for where it lands; the style sheet and the script
// are the embedded ones, inserted as written so the script's hash holds.
func renderJobPage(w io.Writer, page jobPage) error {
	if err := jobPageTemplate.Execute(w, jobPageDocument{
		Page: page, Policy: jobPagePolicy(),
		Style:  template.CSS(jobPageStyle),
		Script: template.JS(jobPageScript),
	}); err != nil {
		return fmt.Errorf("write the job page: %w", err)
	}

	return nil
}
