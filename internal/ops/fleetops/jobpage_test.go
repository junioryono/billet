package fleetops

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/usage"
)

// pageStart is when the example job's series begins, on the host's clock.
var pageStart = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// pageSecond is a wall time this many seconds after pageStart.
func pageSecond(s float64) time.Time {
	return pageStart.Add(time.Duration(s * float64(time.Second)))
}

// pageStepsJSON is the example job's steps as GitHub's REST API lists them: a
// build that passes the series' gap, a post step the series ends inside, and a
// step that took no time.
const pageStepsJSON = `
	{"number":1,"name":"Set up job","status":"completed","conclusion":"success",
	 "started_at":"2026-10-09T12:00:00Z","completed_at":"2026-10-09T12:00:04Z"},
	{"number":2,"name":"Run actions/checkout@v5","status":"completed","conclusion":"success",
	 "started_at":"2026-10-09T12:00:04Z","completed_at":"2026-10-09T12:00:19Z"},
	{"number":3,"name":"Build","status":"completed","conclusion":"success",
	 "started_at":"2026-10-09T12:00:19Z","completed_at":"2026-10-09T12:01:31Z"},
	{"number":4,"name":"Test","status":"completed","conclusion":"success",
	 "started_at":"2026-10-09T12:01:31Z","completed_at":"2026-10-09T12:02:47Z"},
	{"number":5,"name":"Post Run actions/checkout@v5","status":"completed","conclusion":"success",
	 "started_at":"2026-10-09T12:02:47Z","completed_at":"2026-10-09T12:02:48Z"},
	{"number":6,"name":"Complete job","status":"completed","conclusion":"success",
	 "started_at":"2026-10-09T12:02:48Z","completed_at":"2026-10-09T12:02:48Z"}`

// pagePhase is what the example job does each second of a span: CPU in cores,
// and disk and network in bytes a second.
type pagePhase struct {
	until                         float64
	cores                         float64
	read, written, received, sent float64
	memory                        float64
}

var pagePhases = []pagePhase{
	{until: 4, cores: 0.3, memory: 300 << 20},
	{until: 19, cores: 0.45, written: 30 << 20, received: 22 << 20, sent: 200 << 10, memory: 700 << 20},
	{until: 91, cores: 1.6, read: 5 << 20, written: 40 << 20, received: 1 << 20, sent: 64 << 10, memory: 3300 << 20},
	{until: 167, cores: 1.2, read: 2 << 20, written: 6 << 20, received: 3 << 20, sent: 1 << 20, memory: 2100 << 20},
	{until: math.Inf(1), cores: 0.1, memory: 400 << 20},
}

// exampleSeries is the example job's series: a sample every second to 167s
// and one at 167.5s, the sampler missing the second before 60s, and a level of
// memory that wanders around each phase's.
func exampleSeries() []usage.Point {
	var offsets []int64
	for s := range int64(168) {
		offsets = append(offsets, s*1000)
	}
	offsets = append(offsets, 167_500)

	points := make([]usage.Point, len(offsets))
	var cum usage.Point
	for i, ms := range offsets {
		s := float64(ms) / 1000
		phase := pagePhases[0]
		for _, p := range pagePhases {
			phase = p
			if s < p.until {
				break
			}
		}
		cum.OffsetMillis = ms
		cum.MemoryCurrent = int64(phase.memory * (1 + 0.05*math.Sin(s/7)))
		points[i] = cum
		if i+1 == len(offsets) {
			break
		}
		// WHAT THE NEXT INTERVAL USED lands on the sample that ends it.
		span := float64(offsets[i+1]-ms) / 1000 * (1 + 0.15*math.Sin(s/3))
		cum.CPUUsage += int64(phase.cores * span * 1e6)
		cum.GuestCPU += int64(phase.cores * span * 0.97e6)
		cum.VMMCPU += int64(phase.cores * span * 0.03e6)
		cum.DiskRead += int64(phase.read * span)
		cum.DiskWrite += int64(phase.written * span)
		cum.NetRx += int64(phase.received * span)
		cum.NetTx += int64(phase.sent * span)
		cum.EnergyActive += int64(phase.cores * span * 11e6)
	}
	points[60].AfterGap = true

	return points
}

// exampleUsage is the example job's usage report, its totals the series' last
// point.
func exampleUsage(points []usage.Point) alloc.JobUsage {
	last := points[len(points)-1]
	peak := int64(0)
	for _, p := range points {
		peak = max(peak, p.MemoryCurrent)
	}

	return alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: int64(len(points)), IntervalMillis: 1000, WindowMillis: 167_500,
		CPUUserMicros: last.CPUUsage * 9 / 10, CPUSystemMicros: last.CPUUsage / 10,
		GuestCPUMicros: last.GuestCPU, VMMCPUMicros: last.VMMCPU,
		MemoryPeakBytes: peak, DiskReadBytes: last.DiskRead, DiskWriteBytes: last.DiskWrite,
		NetRxBytes: last.NetRx, NetTxBytes: last.NetTx, NetRxPackets: 310_000, NetTxPackets: 120_000,
		CPUSomeMicros: 2_300_000, MemorySomeMicros: 40_000, IOSomeMicros: 1_900_000, IOFullMicros: 700_000,
		EnergyActiveMicrojoules: last.EnergyActive, EnergyIdleMicrojoules: 1_600_000_000,
		EnergySource: alloc.EnergyRAPL,
		Counters: &alloc.JobCounters{
			Cycles: count(612_000_000_000), Instructions: count(734_400_000_000),
			CacheReferences: count(9_100_000_000), CacheMisses: count(2_350_000_000),
			BranchMisses: count(3_120_000_000), FrontendStallCycles: count(171_000_000_000),
		},
		Destinations: &alloc.JobDestinations{
			Destinations: []alloc.JobDestination{
				{Addr: "140.82.112.4", SentBytes: 3_100_000, ReceivedBytes: 352_000_000, Connections: 4},
				{Addr: "185.199.108.133", SentBytes: 410_000, ReceivedBytes: 61_000_000, Connections: 12},
				{Addr: "10.0.0.2", SentBytes: 24_000, ReceivedBytes: 61_000, Connections: 310},
			},
			Other: alloc.JobDestination{SentBytes: 9_000, ReceivedBytes: 120_000, Connections: 3},
			Tap:   &alloc.TapTotals{SentBytes: 3_560_000, ReceivedBytes: 413_400_000},
		},
	}
}

// exampleJob is the example job's identity as GitHub named it.
var exampleJob = alloc.HistoryJob{
	JobID: "51000000007", Owner: "acme", Repository: "api", Event: "push",
	WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", Name: "build (ubuntu, go 1.26)",
}

// seedPageJob stages the example job through the real allocator on a clock
// that matches its series: queued and assigned just before the first sample,
// started with it, measured, succeeded and released. It returns the lease.
func seedPageJob(t *testing.T, stateDir string, job alloc.HistoryJob, measured *alloc.JobUsage,
	series *alloc.UsageSeries,
) string {
	t.Helper()

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the ledger: %v", err)
		}
	}()

	now := pageSecond(-3)
	tier := config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier},
		alloc.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err = a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	})
	must("RegisterNode", err)
	lease, err := a.Reserve(t.Context(), tier.Label)
	must("Reserve", err)
	must("Assign", a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, 77))
	now = pageSecond(-2)
	must("Bind", a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"))
	if job.JobID != "" {
		must("RecordJobIdentity", a.RecordJobIdentity(t.Context(), lease.ID, job))
	}
	must("Advance launching", a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseLaunching))
	must("Advance online", a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseOnline))
	now = pageSecond(0)
	must("Advance busy", a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseBusy))
	now = pageSecond(169)
	if measured != nil {
		must("RecordLeaseUsage", a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, *measured, series))
	}
	must("RecordJobResult", a.RecordJobResult(t.Context(), lease.ID, "succeeded", 4242))
	now = pageSecond(171)
	must("Release", a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone))

	return lease.ID
}

// clockedSeriesOf encodes points as the series a node of wire 26 or later
// sends, starting at pageStart.
func clockedSeriesOf(t *testing.T, points []usage.Point) *alloc.UsageSeries {
	t.Helper()

	data, _, err := usage.EncodeSeriesAt(points, pageStart, alloc.MaxUsageSeriesBytes)
	if err != nil {
		t.Fatalf("EncodeSeriesAt: %v", err)
	}

	return &alloc.UsageSeries{Codec: usage.SeriesCodecClocked, Data: data}
}

// showHTML runs `billet jobs show --html` as the CLI does and returns the page.
func showHTML(t *testing.T, cfg, lease string) string {
	t.Helper()

	return capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--html", lease}); err != nil {
			t.Errorf("billet jobs show --html: %v", err)
		}
	})
}

// exampleLease stands in for the example job's random lease id in the
// committed page.
const exampleLease = "6f1c2a9e0b7d4c3e8a5f9d2b1c0e7a64"

// jobPageExample is the committed example page, published with the docs.
var jobPageExample = filepath.Join("..", "..", "..", "docs", "_static", "examples", "jobs-show.html")

// THE PAGE IS THE LEDGER'S AND GITHUB'S RECORD OF ONE JOB, read as the CLI
// reads them, and it is the committed example the docs publish
// (BILLET_UPDATE_FIXTURES=1 rewrites it), so a change to the page is a change
// a reviewer sees.
func TestJobsShowHTMLIsTheCommittedExample(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	points := exampleSeries()
	measured := exampleUsage(points)
	lease := seedPageJob(t, stateDir, exampleJob, &measured, clockedSeriesOf(t, points))
	fakeJobsGitHub(t, stepsOn(lease, pageStepsJSON))

	page := strings.ReplaceAll(showHTML(t, cfg, lease), lease, exampleLease)

	if os.Getenv("BILLET_UPDATE_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(jobPageExample), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jobPageExample, []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(jobPageExample)
	if err != nil {
		t.Fatalf("%v (run with BILLET_UPDATE_FIXTURES=1 to write the example)", err)
	}
	if string(committed) != page {
		t.Errorf("%s differs from what billet jobs show --html prints; run with BILLET_UPDATE_FIXTURES=1",
			jobPageExample)
	}

	// WHAT THE EXAMPLE IS FOR, said outright so a regenerated example that lost it
	// fails here rather than in review.
	for _, want := range []string{
		"<h1>build (ubuntu, go 1.26)</h1>",
		"Every one of the series&#39; 169 samples is drawn",
		`<button type="button" data-from="11343" data-to="54328" data-key="2" data-step="3" aria-pressed="false">`,
		`<th scope="row">3 Build</th><td>2026-10-09T12:00:19Z</td><td>1m12s</td>`,
		"partial: the series covers 1m11s of the step&#39;s 1m12s",
		"partial: the series covers 500ms of the step&#39;s 1s",
		"start and end in the same second, so no usage can be placed in it",
		"1.20 instructions per cycle", "3.2 misses per 1,000 instructions",
		`<rect class="band0" data-key="2" x="11343" y="0" width="42985" height="100"></rect>`,
		"140.82.112.4", "Dashed: tier memory 8.0 GiB.", "Dashed: the lease&#39;s 2 vCPU.",
		"GitHub succeeded; billet done",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	if strings.Contains(page, "not measured") {
		t.Errorf("the example measured every group, and the page says one was not")
	}
}

// policyMeta finds a page's Content-Security-Policy.
var policyMeta = regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]*)">`)

// THE PAGE RUNS ONE SCRIPT, THE ONE ITS POLICY NAMES BY HASH, AND FETCHES
// NOTHING: the policy refuses every source but inline style, the script's
// sha256 is the one the policy allows, and nothing in the page names a URL to
// load.
func TestTheJobPageRunsOnlyItsOwnScriptAndFetchesNothing(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	points := exampleSeries()
	measured := exampleUsage(points)
	lease := seedPageJob(t, stateDir, exampleJob, &measured, clockedSeriesOf(t, points))
	fakeJobsGitHub(t, stepsOn(lease, pageStepsJSON))
	page := showHTML(t, cfg, lease)

	assertPolicyHolds(t, page)
	if !strings.Contains(page, `<svg data-chart=""`) {
		t.Errorf("the page drew no chart, so the check below proves nothing about one")
	}
}

// assertPolicyHolds checks a page's policy, its one script and that it fetches
// nothing.
func assertPolicyHolds(t *testing.T, page string) {
	t.Helper()

	m := policyMeta.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page has no Content-Security-Policy meta tag")
	}
	if !strings.Contains(page, "<head>\n<meta charset=\"utf-8\">\n<meta http-equiv=\"Content-Security-Policy\"") {
		t.Errorf("the policy is not the first thing the head declares after its charset")
	}
	policy := html.UnescapeString(m[1])
	directives := map[string]string{}
	for d := range strings.SplitSeq(policy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	if directives["default-src"] != "'none'" || directives["style-src"] != "'unsafe-inline'" ||
		directives["base-uri"] != "'none'" || directives["form-action"] != "'none'" {
		t.Errorf("policy = %q", policy)
	}

	if n := strings.Count(page, "<script"); n != 1 {
		t.Fatalf("the page has %d script elements, want its one", n)
	}
	open := strings.Index(page, "<script>")
	end := strings.Index(page, "</script>")
	if open < 0 || end < open {
		t.Fatalf("no <script>...</script> in the page")
	}
	body := page[open+len("<script>") : end]
	sum := sha256.Sum256([]byte(body))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; directives["script-src"] != want {
		t.Errorf("script-src = %s, want the inline script's hash %s", directives["script-src"], want)
	}
	if !strings.Contains(body, `addEventListener("click"`) {
		t.Errorf("the inline script is not the page's own:\n%s", body)
	}

	lower := strings.ToLower(page)
	for _, fetch := range []string{"src=", "href", "url(", "@import", "<link", "<img", "<iframe", "<object",
		"<embed", "<base", "srcset", "http:", "https:", "//cdn", "fetch(", "xmlhttprequest", "<form"} {
		if strings.Contains(lower, fetch) {
			t.Errorf("the page contains %q, which can load something", fetch)
		}
	}
}

// hostile is a string a workflow or GitHub could choose that would be markup,
// a script and an attribute break if written raw.
const hostile = `<script>alert("x")</script><img src=x onerror=alert(1)> it's "quoted" & <b>`

// EVERY STRING GITHUB OR A WORKFLOW CHOSE IS TEXT ON THE PAGE, never markup:
// the job's name, repository, workflow and event, a step's name and outcome,
// and GitHub's own error message each land escaped, and the page still has its
// one script.
func TestTheJobPageEscapesWhatGitHubAndTheWorkflowChose(t *testing.T) {
	t.Parallel()

	rec := alloc.JobRecord{
		LeaseID: "l1", Tier: "t", RunID: 4242,
		Job:  alloc.HistoryJob{JobID: hostile, Name: hostile, Event: hostile, WorkflowRef: hostile},
		Repo: hostile, Result: hostile,
	}
	points := exampleSeries()
	tl := usage.Timeline{First: pageStart, Points: points}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(points)}
	measured.Destinations.Destinations[0].Addr = hostile
	steps := jobSteps{runner: "billet-l1", measured: measured, timeline: &tl, job: github.WorkflowJob{
		ID: 1, Name: hostile, RunAttempt: 1, Steps: []github.JobStep{{
			Number: 1, Name: hostile, Conclusion: hostile, StartedAt: pageSecond(1), CompletedAt: pageSecond(9),
		}},
	}}
	var out bytes.Buffer
	if err := renderJobPage(&out, jobPageOf(rec, measured, steps, &tl, "")); err != nil {
		t.Fatalf("renderJobPage: %v", err)
	}
	failed := jobSteps{runner: "billet-l1", err: errors.New("github: HTTP 500: " + hostile)}
	if err := renderJobPage(&out, jobPageOf(rec, measured, failed, &tl, "")); err != nil {
		t.Fatalf("renderJobPage: %v", err)
	}
	page := out.String()

	escaped := `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;&lt;img src=x onerror=alert(1)&gt; it&#39;s &#34;quoted&#34; &amp; &lt;b&gt;`
	// The job's name, its repository, workflow, event, GitHub job id and result,
	// the step's name and outcome, GitHub's name for the job, a destination and
	// GitHub's error.
	if n := strings.Count(page, escaped); n < 10 {
		t.Errorf("the hostile string appears escaped %d times, want every place it is shown:\n%s", n, page)
	}
	for _, raw := range []string{"<script>alert", "<img", "<b>", `"quoted"`, "it's"} {
		if strings.Contains(page, raw) {
			t.Errorf("the page carries %q unescaped", raw)
		}
	}
	if n := strings.Count(page, "<script"); n != 2 {
		t.Errorf("two pages hold %d script elements, want one each", n)
	}
}

// renderPage renders a page from its parts, as the command does once it has
// read them.
func renderPage(t *testing.T, rec alloc.JobRecord, measured *alloc.RecordedUsage, steps jobSteps,
	tl *usage.Timeline, unaligned string,
) string {
	t.Helper()

	var out bytes.Buffer
	if err := renderJobPage(&out, jobPageOf(rec, measured, steps, tl, unaligned)); err != nil {
		t.Fatalf("renderJobPage: %v", err)
	}

	return out.String()
}

// section is the page's section with this id.
func section(t *testing.T, page, id string) string {
	t.Helper()

	start := strings.Index(page, `<section id="`+id+`">`)
	if start < 0 {
		t.Fatalf("the page has no %s section", id)
	}
	end := strings.Index(page[start:], "</section>")

	return page[start : start+end]
}

// figure is the chart whose caption starts with title.
func figure(t *testing.T, page, title string) string {
	t.Helper()

	start := strings.Index(page, "<figcaption>"+title)
	if start < 0 {
		t.Fatalf("the page has no chart %q", title)
	}
	end := strings.Index(page[start:], "</figure>")

	return page[start : start+end]
}

// A GROUP THE HOST NEVER READ IS "not measured" EVERYWHERE THE PAGE SHOWS IT,
// and only that group: its line of the report, its chart, which draws nothing,
// and its cells of every step.
func TestTheJobPageSaysNotMeasuredForAGroupNeverRead(t *testing.T) {
	t.Parallel()

	charts := map[string]string{
		alloc.UsageCPU: "CPU", alloc.UsageMemory: "Memory", alloc.UsageIO: "Disk", alloc.UsageNet: "Network",
	}
	reportLabels := map[string]string{
		alloc.UsageCPU: "cpu", alloc.UsageThreads: "vmm split", alloc.UsageMemory: "memory",
		alloc.UsageIO: "disk", alloc.UsageNet: "network", alloc.UsagePressure: "stalled",
		alloc.UsageEnergy: "energy",
	}
	// The step table's columns each group fills.
	columns := map[string][]int{
		alloc.UsageCPU: {0}, alloc.UsageMemory: {1}, alloc.UsageIO: {2, 3}, alloc.UsageNet: {4, 5},
		alloc.UsageEnergy: {6},
	}
	points := exampleSeries()
	tl := usage.Timeline{First: pageStart, Points: points}
	steps := []github.JobStep{{Number: 1, Name: "build", Conclusion: "success",
		StartedAt: pageSecond(20), CompletedAt: pageSecond(80)}}

	for group := range reportLabels {
		t.Run(group, func(t *testing.T) {
			t.Parallel()

			measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(points)}
			measured.Unmeasured = []string{group}
			page := renderPage(t, alloc.JobRecord{LeaseID: "l1", VCPU: 2, Memory: 8 << 30}, measured,
				jobSteps{runner: "billet-l1", measured: measured, timeline: &tl,
					job: github.WorkflowJob{ID: 1, Name: "j", Steps: steps}}, &tl, "")

			report := section(t, page, "usage")
			for other, otherLabel := range reportLabels {
				line := "<dt>" + otherLabel + "</dt><dd class=\"unmeasured\">not measured</dd>"
				if got := strings.Contains(report, line); got != (other == group) {
					t.Errorf("the %s line says not measured = %v", otherLabel, got)
				}
			}
			for other, title := range charts {
				chart := figure(t, page, title)
				unmeasured := strings.Contains(chart, `<p class="unmeasured">not measured</p>`)
				drawn := strings.Contains(chart, "<path")
				if unmeasured != (other == group) || drawn == (other == group) {
					t.Errorf("with %s unmeasured, the %s chart says not measured = %v and draws = %v",
						group, title, unmeasured, drawn)
				}
			}
			cells := stepCells(t, section(t, page, "step-usage"))
			for other, cols := range columns {
				for _, col := range cols {
					unmeasured := cells[col] == `<td class="unmeasured">not measured</td>`
					if unmeasured != (other == group) {
						t.Errorf("with %s unmeasured, step column %d reads %s", group, col, cells[col])
					}
				}
			}
		})
	}
}

// stepCells is the usage cells of the first step in the step table.
func stepCells(t *testing.T, table string) []string {
	t.Helper()

	_, row, ok := strings.Cut(table, "<tbody>")
	row, _, ok2 := strings.Cut(row, "</tr>")
	if !ok || !ok2 {
		t.Fatalf("no step row in:\n%s", table)
	}
	cells := regexp.MustCompile(`<td[^>]*>[^<]*</td>`).FindAllString(row, -1)
	if len(cells) != 9 {
		t.Fatalf("the step row has %d cells, want its start, its time and 7 groups:\n%s", len(cells), row)
	}

	return cells[2:]
}

// A JOB THE HOST COUNTED AND TOTALLED NOTHING FOR SAYS SO: no counters and no
// destinations are "not measured", never zeros or an empty table.
func TestTheJobPageSaysWhenCountersAndDestinationsWereNotMeasured(t *testing.T) {
	t.Parallel()

	points := exampleSeries()
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(points)}
	measured.Counters, measured.Destinations = nil, nil
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{none: "no GitHub job"}, nil,
		"the series was recorded without its start")

	if !strings.Contains(section(t, page, "usage"),
		`<dt>counters</dt><dd class="unmeasured">not measured</dd>`) {
		t.Errorf("missing counters were not said to be unmeasured:\n%s", section(t, page, "usage"))
	}
	dest := section(t, page, "destinations")
	if !strings.Contains(dest, "Destinations not measured (node.monitoring.flows, on a firecracker node).") ||
		strings.Contains(dest, "<table") {
		t.Errorf("missing destinations were not said to be unmeasured:\n%s", dest)
	}
	if !strings.Contains(section(t, page, "charts"),
		"nothing is drawn, because the series was recorded without its start") {
		t.Errorf("a series off the host's clock did not say why nothing is drawn")
	}
}

// A JOB WITH NO USAGE REPORT IS A PAGE THAT SAYS SO in every section that
// would show one, read as the CLI reads it, and its steps are still listed.
func TestTheJobPageOfAJobWithNoUsage(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	lease := seedPageJob(t, stateDir, exampleJob, nil, nil)
	fakeJobsGitHub(t, stepsOn(lease, pageStepsJSON))
	page := showHTML(t, cfg, lease)

	assertPolicyHolds(t, page)
	for id, want := range map[string]string{
		"usage":        noUsageReport,
		"charts":       "no usage report was recorded, so there is nothing to draw",
		"destinations": "No usage report was recorded.",
		"steps":        "Usage per step: could not tell, because no usage report was recorded.",
		"step-usage":   `<td colspan="7" class="none">could not tell</td>`,
	} {
		if s := section(t, page, id); !strings.Contains(s, html.EscapeString(want)) &&
			!strings.Contains(s, want) {
			t.Errorf("the %s section does not say %q:\n%s", id, want, s)
		}
	}
	if !strings.Contains(page, ">Run actions/checkout@v5</span>") {
		t.Errorf("the steps of a job with no usage were not listed")
	}
	if strings.Contains(page, "<svg data-chart") {
		t.Errorf("a job with no usage drew a chart")
	}
}

// A JOB WITH NO STEPS SAYS WHY: GitHub listing none, and a lease that names no
// GitHub job to ask about; its charts are still drawn.
func TestTheJobPageOfAJobWithNoSteps(t *testing.T) {
	for name, tc := range map[string]struct {
		job   alloc.HistoryJob
		steps string
		want  string
	}{
		"github lists none": {exampleJob, "", "GitHub lists no steps for this job."},
		"no job recorded":   {alloc.HistoryJob{}, pageStepsJSON, "no GitHub job is recorded for this lease"},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			cfg := writeJobsConfig(t, stateDir)
			points := exampleSeries()
			measured := exampleUsage(points)
			lease := seedPageJob(t, stateDir, tc.job, &measured, clockedSeriesOf(t, points))
			fakeJobsGitHub(t, stepsOn(lease, tc.steps))
			page := showHTML(t, cfg, lease)

			if !strings.Contains(section(t, page, "step-usage"), tc.want) {
				t.Errorf("the step table does not say %q:\n%s", tc.want, section(t, page, "step-usage"))
			}
			if strings.Contains(page, "data-key=") || strings.Contains(page, "<rect class=\"band") {
				t.Errorf("a job with no steps shows a step")
			}
			if strings.Count(page, "<svg data-chart") != 4 {
				t.Errorf("a job with no steps did not draw its four charts")
			}
		})
	}
}

// A FAILED GITHUB READ IS THE STEPS SECTION'S REASON, NOT A FAILED PAGE: the
// error is shown, the token is not, and the record, the usage and the charts
// render as before.
func TestTheJobPageSaysWhyTheStepsCouldNotBeRead(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	points := exampleSeries()
	measured := exampleUsage(points)
	lease := seedPageJob(t, stateDir, exampleJob, &measured, clockedSeriesOf(t, points))
	fakeJobsGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})
	page := showHTML(t, cfg, lease)

	steps := section(t, page, "steps")
	for _, want := range []string{"The steps could not be read from GitHub:", "HTTP 403",
		"Resource not accessible by integration", "needs the App&#39;s `actions: read`"} {
		if !strings.Contains(steps, want) {
			t.Errorf("the steps section does not say %q:\n%s", want, steps)
		}
	}
	if strings.Contains(page, "installation-secret") {
		t.Errorf("the page carries the installation token")
	}
	if strings.Count(page, "<svg data-chart") != 4 || !strings.Contains(page, "140.82.112.4") ||
		!strings.Contains(page, "<h1>build (ubuntu, go 1.26)</h1>") {
		t.Errorf("a failed GitHub read took the rest of the page with it")
	}
}

// --json AND --html ARE EACH THE WHOLE RECORD, so asking for both is refused
// before anything is read.
func TestJobsShowRefusesJSONAndHTMLTogether(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "absent.yaml")
	err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--json", "--html", "l1"})
	if err == nil || !strings.Contains(err.Error(), "choose one") {
		t.Errorf("--json --html = %v, want a refusal", err)
	}
}

// maxSeriesPageBytes is the most a page of a series at the ledger's bound and
// no steps may take, and maxStepPageBytes what each step may add to it beyond
// the text GitHub and the workflow chose for it, which the page shows whole.
const (
	maxSeriesPageBytes = 96 << 10
	maxStepPageBytes   = 1536
)

// largestSeries is a series that encodes to near the ledger's bound: as many
// points as the decoder accepts, every column moving by a random amount.
func largestSeries(t *testing.T) *usage.Timeline {
	t.Helper()

	rng := rand.New(rand.NewPCG(1, 2))
	points := make([]usage.Point, 2<<16)
	var cum usage.Point
	for i := range points {
		cum.OffsetMillis = int64(i) * 1000
		cum.MemoryCurrent = rng.Int64N(8 << 30)
		cum.CPUUsage += rng.Int64N(2_000_000)
		cum.DiskRead += rng.Int64N(50 << 20)
		cum.DiskWrite += rng.Int64N(50 << 20)
		cum.NetRx += rng.Int64N(50 << 20)
		cum.NetTx += rng.Int64N(50 << 20)
		cum.EnergyActive += rng.Int64N(30_000_000)
		cum.AfterGap = rng.IntN(500) == 0
		points[i] = cum
	}
	data, stride, err := usage.EncodeSeriesAt(points, pageStart, alloc.MaxUsageSeriesBytes)
	if err != nil {
		t.Fatalf("EncodeSeriesAt: %v", err)
	}
	if len(data) < alloc.MaxUsageSeriesBytes*3/4 {
		t.Fatalf("the series encodes to %d bytes (stride %d), not near the %d bound", len(data), stride,
			alloc.MaxUsageSeriesBytes)
	}
	tl, err := usage.TimelineOf(usage.SeriesCodecClocked, data)
	if err != nil {
		t.Fatalf("TimelineOf: %v", err)
	}

	return &tl
}

// A SERIES AT THE LEDGER'S BOUND RENDERS WITHIN A STATED SIZE: its charts are
// drawn from at most maxChartWindows windows, the page says it was
// downsampled, and each step adds a bounded amount.
func TestAPageOfTheLargestSeriesIsBounded(t *testing.T) {
	t.Parallel()

	tl := largestSeries(t)
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(tl.Points)}
	rec := alloc.JobRecord{LeaseID: "l1", VCPU: 2, Memory: 8 << 30}
	last := tl.First.Add(time.Duration(tl.Points[len(tl.Points)-1].OffsetMillis) * time.Millisecond)
	const stepCount = 1000
	var steps []github.JobStep
	width := last.Sub(tl.First) / stepCount
	for i := range stepCount {
		steps = append(steps, github.JobStep{Number: int64(i + 1), Name: fmt.Sprintf("step %d", i+1),
			Conclusion: "success", StartedAt: tl.First.Add(time.Duration(i) * width).Truncate(time.Second),
			CompletedAt: tl.First.Add(time.Duration(i+1) * width).Truncate(time.Second)})
	}

	bare := renderPage(t, rec, measured, jobSteps{none: "no GitHub job"}, tl, "")
	if len(bare) > maxSeriesPageBytes {
		t.Errorf("a page of a %d-point series is %d bytes, over the %d bound", len(tl.Points), len(bare),
			maxSeriesPageBytes)
	}
	if !strings.Contains(bare, fmt.Sprintf("drawn as %d windows", maxChartWindows)) {
		t.Errorf("the page does not say it was downsampled for drawing")
	}
	full := renderPage(t, rec, measured, jobSteps{runner: "billet-l1", measured: measured, timeline: tl,
		job: github.WorkflowJob{ID: 1, Name: "j", Steps: steps}}, tl, "")
	if limit := maxSeriesPageBytes + stepCount*maxStepPageBytes; len(full) > limit {
		t.Errorf("a page of the series and %d steps is %d bytes, over the %d bound", stepCount, len(full), limit)
	}
	t.Logf("largest series: %d points; page %d bytes, with %d steps %d bytes", len(tl.Points), len(bare),
		stepCount, len(full))
}

// SAMPLES SHARING AN OFFSET DO NOT GROW A CHART: a series of thousands of
// samples at one instant and one a second later, which compresses to almost
// nothing, draws memory as one point an offset and stays within the bound.
func TestRepeatedOffsetsDoNotGrowTheMemoryChart(t *testing.T) {
	t.Parallel()

	tl := usage.Timeline{First: pageStart, Points: make([]usage.Point, 1<<14)}
	tl.Points[len(tl.Points)-1].OffsetMillis = 1000
	tl.Points[100].MemoryCurrent = 5 << 30
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(tl.Points)}
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{none: "no GitHub job"}, &tl, "")

	memory := figure(t, page, "Memory")
	d := regexp.MustCompile(`<path class="line a" d="([^"]*)"`).FindStringSubmatch(memory)
	if d == nil {
		t.Fatalf("the memory chart draws no line:\n%s", memory)
	}
	if n := strings.Count(d[1], "M") + strings.Count(d[1], "L"); n != 2 {
		t.Errorf("the memory line of two offsets draws %d points, want 2: %s", n, d[1])
	}
	// The 5 GiB sample among the zeros at offset 0, under a top 8% above it.
	if !strings.HasPrefix(d[1], "M0 7.41H0") {
		t.Errorf("the level drawn at offset 0 is not its highest: %s", d[1])
	}
}

// A SERIES OF THE LONGEST OFFSETS THE DECODER ACCEPTS IS NO LARGER A PAGE: the
// charts' coordinates are the series mapped onto a fixed width, so 720
// intervals of months each draw as small as 720 of a second.
func TestLongOffsetsDoNotGrowTheCharts(t *testing.T) {
	t.Parallel()

	tl := usage.Timeline{First: pageStart}
	for i := range int64(maxChartWindows + 1) {
		tl.Points = append(tl.Points, usage.Point{OffsetMillis: i * 12_800_000_000})
	}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(tl.Points)}
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1", VCPU: 2, Memory: 8 << 30}, measured,
		jobSteps{none: "no GitHub job"}, &tl, "")

	if len(page) > maxSeriesPageBytes {
		t.Errorf("a page of %d intervals of %s is %d bytes, over the %d bound", maxChartWindows,
			12_800_000_000*time.Millisecond, len(page), maxSeriesPageBytes)
	}
	if !strings.Contains(page, "Every one of the series&#39; 721 samples is drawn") {
		t.Errorf("the series was not drawn sample by sample, so the case above proves nothing")
	}
}

// WITH NO CHART DRAWN THERE IS NOTHING TO FOCUS: every group unmeasured leaves
// each step's button disabled and no control for the whole series.
func TestNoStepIsFocusableWhenNoChartIsDrawn(t *testing.T) {
	t.Parallel()

	points := exampleSeries()
	tl := usage.Timeline{First: pageStart, Points: points}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(points)}
	measured.Unmeasured = []string{alloc.UsageCPU, alloc.UsageMemory, alloc.UsageIO, alloc.UsageNet}
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{runner: "billet-l1",
		measured: measured, timeline: &tl, job: github.WorkflowJob{ID: 1, Name: "j", Steps: []github.JobStep{
			{Number: 1, Name: "a", StartedAt: pageSecond(1), CompletedAt: pageSecond(5)},
		}}}, &tl, "")

	if strings.Contains(page, "data-key=") || strings.Contains(page, `id="whole"`) ||
		!strings.Contains(page, `<button type="button" disabled>`) {
		t.Errorf("a page with no chart offers a focus:\n%s", section(t, page, "steps"))
	}
	if strings.Count(page, `<p class="unmeasured">not measured</p>`) != 4 {
		t.Errorf("the four charts do not each say not measured")
	}
}

// A MEMORY LEVEL IS NOT DRAWN ACROSS TIME THE SAMPLER DID NOT SEE: the line
// breaks before the sample that ends a gap, as the rate lines do.
func TestTheMemoryLineBreaksAtAGap(t *testing.T) {
	t.Parallel()

	points := exampleSeries()
	tl := usage.Timeline{First: pageStart, Points: points}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(points)}
	charts := buildCharts(&tl, "", alloc.JobRecord{}, measured, nil)
	memory := charts.Charts[1].Lines[0].D
	// 60s of the series' 167.5s on the charts' 100,000 units.
	if !strings.Contains(memory, "M35821 ") || strings.Contains(memory, "L35821 ") {
		t.Errorf("the memory line is not broken before the sample at 60s, which ends a gap:\n%s", memory)
	}
	if strings.Count(memory, "M") != 2 {
		t.Errorf("the memory line breaks %d times, want once:\n%s", strings.Count(memory, "M")-1, memory)
	}
}

// A RATE OR A LEVEL PAST THE LARGEST INT64 IS LABELLED AS ONE, never wrapped
// negative by a conversion.
func TestAChartLabelPastTheLargestInt64(t *testing.T) {
	t.Parallel()

	for f, want := range map[float64]string{1e19: "8.7 EiB", 1024: "1.0 KiB", math.MaxInt64 * 1.08: "8.6 EiB"} {
		if got := floatBytes(f); got != want {
			t.Errorf("floatBytes(%g) = %q, want %q", f, got, want)
		}
	}
	tl := usage.Timeline{First: pageStart, Points: []usage.Point{
		{OffsetMillis: 0}, {OffsetMillis: 1, DiskWrite: 1e16, MemoryCurrent: math.MaxInt64},
	}}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(tl.Points)}
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{none: "no GitHub job"}, &tl, "")
	for title, want := range map[string]string{"Disk": "9.4 EiB/s", "Memory": "8.6 EiB"} {
		if chart := figure(t, page, title); !strings.Contains(chart, `<span class="top">`+want+`</span>`) {
			t.Errorf("the %s chart's top is not %s:\n%s", title, want, chart)
		}
	}
}

// TWO STEPS GITHUB GIVES ONE NUMBER ARE STILL TWO STEPS to choose between:
// each has its own key, and so does its band.
func TestStepsSharingANumberAreChosenApart(t *testing.T) {
	t.Parallel()

	points := exampleSeries()
	tl := usage.Timeline{First: pageStart, Points: points}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(points)}
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{runner: "billet-l1",
		measured: measured, timeline: &tl, job: github.WorkflowJob{ID: 1, Name: "j", Steps: []github.JobStep{
			{Number: 1, Name: "a", StartedAt: pageSecond(1), CompletedAt: pageSecond(5)},
			{Number: 1, Name: "b", StartedAt: pageSecond(5), CompletedAt: pageSecond(9)},
		}}}, &tl, "")

	for _, want := range []string{`data-key="0" data-step="1"`, `data-key="1" data-step="1"`,
		`<rect class="band0" data-key="0"`, `<rect class="band1" data-key="1"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not carry %q", want)
		}
	}
}

// windowOfCases is how many random series and windows TestWindowOfIsWindow
// compares.
const windowOfCases = 20_000

// WINDOWOF IS WINDOW: on random series with repeated offsets, gaps and
// counters that fall, the window read from the points it depends on is the
// window read from the whole series, for windows inside, across and outside
// it.
func TestWindowOfIsWindow(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(3, 4))
	for c := range windowOfCases {
		tl := usage.Timeline{First: pageStart}
		var cum usage.Point
		for range 1 + rng.IntN(40) {
			if rng.IntN(4) != 0 {
				cum.OffsetMillis += int64(rng.IntN(3000))
			}
			step := func(v int64) int64 {
				if rng.IntN(12) == 0 {
					return max(v-int64(rng.IntN(5000)), 0)
				}
				return v + int64(rng.IntN(5000))
			}
			cum.CPUUsage, cum.DiskRead, cum.DiskWrite = step(cum.CPUUsage), step(cum.DiskRead), step(cum.DiskWrite)
			cum.NetRx, cum.NetTx, cum.EnergyActive = step(cum.NetRx), step(cum.NetTx), step(cum.EnergyActive)
			cum.MemoryCurrent = int64(rng.IntN(1 << 20))
			cum.AfterGap = rng.IntN(8) == 0
			tl.Points = append(tl.Points, cum)
		}
		span := cum.OffsetMillis
		from := pageStart.Add(time.Duration(rng.Int64N(span+4000)-2000) * time.Millisecond)
		to := from.Add(time.Duration(rng.Int64N(span+2000)) * time.Millisecond)
		if got, want := windowOf(tl, from, to), tl.Window(from, to); got != want {
			t.Fatalf("case %d: windowOf = %+v, Window = %+v\nwindow %s to %s over %+v", c, got, want,
				from.Sub(pageStart), to.Sub(pageStart), tl.Points)
		}
	}
}

// A SERIES WITH NO INTERVAL IN IT DRAWS NOTHING AND SAYS SO, and no step is
// offered a focus on charts that are not there: one sample, and none.
func TestTheJobPageOfASeriesWithNoInterval(t *testing.T) {
	t.Parallel()

	for name, points := range map[string][]usage.Point{
		"one sample": {{OffsetMillis: 0, CPUUsage: 1}},
		"no sample":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tl := usage.Timeline{First: pageStart, Points: points}
			measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(exampleSeries())}
			page := renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{runner: "billet-l1",
				measured: measured, timeline: &tl, job: github.WorkflowJob{ID: 1, Name: "j", Steps: []github.JobStep{
					{Number: 1, Name: "s", StartedAt: pageSecond(-1), CompletedAt: pageSecond(2)},
				}}}, &tl, "")

			if !strings.Contains(section(t, page, "charts"), "the series holds no interval to draw") {
				t.Errorf("a series with no interval did not say why nothing is drawn")
			}
			if strings.Contains(page, "data-key=") || strings.Contains(page, "id=\"whole\"") || strings.Contains(page, "<svg data-chart") {
				t.Errorf("a series with no interval drew a chart or offered a focus")
			}
		})
	}
}

// A STEP SHORTER THAN ONE UNIT OF A LONG SERIES FOCUSES ON ONE UNIT INSIDE THE
// CHARTS, at either end: never a window that starts at the last unit and runs
// past it.
func TestAShortStepFocusesInsideTheCharts(t *testing.T) {
	t.Parallel()

	days := 5 * 24 * time.Hour
	tl := usage.Timeline{First: pageStart, Points: []usage.Point{
		{OffsetMillis: 0}, {OffsetMillis: days.Milliseconds(), CPUUsage: 1},
	}}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(tl.Points)}
	charts := buildCharts(&tl, "", alloc.JobRecord{}, measured, nil)

	for name, tc := range map[string]struct {
		step     github.JobStep
		from, to int64
	}{
		"last second":  {github.JobStep{StartedAt: pageStart.Add(days - time.Second), CompletedAt: pageStart.Add(days)}, chartWidth - 1, chartWidth},
		"first second": {github.JobStep{StartedAt: pageStart, CompletedAt: pageSecond(1)}, 0, 1},
	} {
		from, to, ok := charts.stepWindow(tc.step, &tl)
		if !ok || from != tc.from || to != tc.to {
			t.Errorf("%s: focus %d to %d (%v), want %d to %d", name, from, to, ok, tc.from, tc.to)
		}
	}
}

// THE SCRIPT IS HASHED AS A BROWSER HASHES IT: the HTML parser makes every line
// end LF before the policy is checked, so a checkout with CRLF line ends must
// still name the hash a browser computes.
func TestTheInlineScriptHasTheLineEndsABrowserHashes(t *testing.T) {
	t.Parallel()

	if got := inlineScript("a\r\nb\rc\nd"); got != "a\nb\nc\nd" {
		t.Errorf("inlineScript = %q, want every line end LF", got)
	}
	if strings.Contains(jobPageScript, "\r") {
		t.Errorf("the page's script carries a CR")
	}
}
