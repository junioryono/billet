package fleetops

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/usage"
)

// guestLagMillis is how far the example guest's clock runs behind the host's.
const guestLagMillis = 250

// guestMillis is the guest's stamp for a host time this many seconds into the
// example job.
func guestMillis(s float64) int64 { return pageSecond(s).UnixMilli() - guestLagMillis }

// guestProcess is one of the example guest's processes in a phase: CPU ticks,
// reads and writes over a two-second interval, and its levels.
type guestProcess struct {
	name               string
	procs, ticks       int64
	rss, read, written int64
}

// guestPhases are the example job's processes, phase by phase as pagePhases
// cuts it.
var guestPhases = [][]guestProcess{
	{{"Runner.Worker", 1, 40, 180 << 20, 0, 1 << 20}, {"bash", 1, 10, 4 << 20, 0, 0}},
	{{"git", 2, 70, 300 << 20, 4 << 20, 50 << 20}, {"git-remote-http", 1, 20, 30 << 20, 0, 0},
		{"Runner.Worker", 1, 4, 180 << 20, 0, 0}},
	{{"compile", 4, 280, 2400 << 20, 8 << 20, 70 << 20}, {"go", 1, 30, 400 << 20, 2 << 20, 2 << 20},
		{"link", 1, 10, 600 << 20, 0, 9 << 20}, {"Runner.Worker", 1, 2, 180 << 20, 0, 0}},
	{{"api.test", 3, 200, 1500 << 20, 3 << 20, 10 << 20}, {"go", 1, 25, 400 << 20, 0, 0},
		{"test2json", 1, 8, 20 << 20, 0, 0}, {"Runner.Worker", 1, 2, 180 << 20, 0, 0}},
	{{"Runner.Worker", 1, 6, 180 << 20, 0, 0}},
}

// guestMarks are the example guest's step marks, at the host second each step
// started, as the runner's log names them: the steps GitHub lists but the two
// the runner logs no step line for.
var guestMarks = []struct {
	name string
	at   float64
}{
	{"Run actions/checkout@v5", 4}, {"Build", 19}, {"Test", 91}, {"Post Run actions/checkout@v5", 167},
}

// guestPhaseAt is which of pagePhases a host second is in.
func guestPhaseAt(s float64) int {
	for i, p := range pagePhases {
		if s < p.until {
			return i
		}
	}

	return len(pagePhases) - 1
}

// exampleGuestBatches is what the example job's agent sends: a sample a second
// from 1s to 167s, processes every two seconds, the step marks, and its test
// results in its last batch, ten seconds a batch.
func exampleGuestBatches() []guestreport.Batch {
	const last = 167
	batches := make([]guestreport.Batch, (last+9)/10)
	for i := range batches {
		batches[i] = guestreport.Batch{Seq: uint64(i + 1), AgentVersion: "0.15.1", TicksPerSecond: 100,
			AgentCPUTicks: int64(i+1) * 4}
	}
	sample := guestreport.Sample{CPUBusyTicks: 300, CPUTotalTicks: 600, NetRxBytes: 2 << 20, NetTxBytes: 200 << 10,
		DiskReadBytes: 90 << 20, DiskWriteBytes: 10 << 20}
	for k := 1; k <= last; k++ {
		s := float64(k)
		phase := pagePhases[guestPhaseAt(s)]
		b := &batches[(k-1)/10]
		sample.AtMillis = guestMillis(s)
		sample.CPUBusyTicks += int64(phase.cores * 100)
		sample.CPUTotalTicks += 200
		sample.MemUsedBytes = int64(phase.memory * 0.8)
		sample.MemCachedBytes = 150<<20 + int64(k)*(6<<20)
		sample.MemAvailableBytes = 8<<30 - sample.MemUsedBytes - sample.MemCachedBytes/2
		sample.DiskReadBytes += int64(phase.read * 1.1)
		sample.DiskWriteBytes += int64(phase.written * 0.9)
		sample.NetRxBytes += int64(phase.received)
		sample.NetTxBytes += int64(phase.sent)
		b.Samples = append(b.Samples, sample)

		if k%2 == 0 {
			var rows []guestreport.ProcessRow
			for _, p := range guestPhases[guestPhaseAt(s)] {
				rows = append(rows, guestreport.ProcessRow{Name: p.name, ProcessUsage: guestreport.ProcessUsage{
					Procs: p.procs, CPUTicks: p.ticks, RSSBytes: p.rss, ReadBytes: p.read, WriteBytes: p.written}})
			}
			rows = append(rows, guestreport.ProcessRow{Name: "billet", ProcessUsage: guestreport.ProcessUsage{
				Procs: 1, CPUTicks: 1, RSSBytes: 14 << 20}})
			kept, _, err := guestreport.Fold(rows)
			if err != nil {
				panic(err)
			}
			b.Processes = append(b.Processes, guestreport.ProcessSample{AtMillis: sample.AtMillis, Rows: kept,
				Other: guestreport.ProcessUsage{Procs: 38, CPUTicks: 2, RSSBytes: 210 << 20}, ResidualCPUTicks: 3})
		}
	}
	for _, m := range guestMarks {
		b := &batches[min(int(m.at-1)/10, len(batches)-1)]
		b.Steps = append(b.Steps, guestreport.StepMark{Name: m.name, AtSeconds: guestMillis(m.at) / 1000})
	}
	batches[len(batches)-1].Tests = []guestreport.SuiteResult{
		{Name: "api/internal/store", Tests: 214, Skipped: 3},
		{Name: "api/cmd/server", Tests: 12},
	}

	return batches
}

// guestRow is the row a node sends for batches: each encoded and decoded as the
// node takes it, merged and halved to the ledger's bound, beside the counts in
// row.
func guestRow(t *testing.T, batches []guestreport.Batch, row alloc.GuestReport) *alloc.GuestReport {
	t.Helper()

	var received []guestreport.Batch
	for i := range batches {
		data, err := guestreport.EncodeBatch(batches[i])
		if err != nil {
			t.Fatalf("EncodeBatch %d: %v", batches[i].Seq, err)
		}
		back, err := guestreport.DecodeBatch(data)
		if err != nil {
			t.Fatalf("DecodeBatch %d: %v", batches[i].Seq, err)
		}
		received = append(received, back)
	}
	merged, err := guestreport.Merge(received)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	_, data, err := guestreport.Downsample(merged, alloc.MaxGuestReportBytes)
	if err != nil {
		t.Fatalf("Downsample: %v", err)
	}
	row.Codec, row.Schema, row.Data, row.AgentVersion = alloc.GuestReportCodec, 1, data, merged.AgentVersion

	return &row
}

// exampleGuest is the example job's guest report, whole: every batch arrived,
// the last 850ms after the guest stamped its last sample.
func exampleGuest(t *testing.T) *alloc.GuestReport {
	t.Helper()

	batches := exampleGuestBatches()

	return guestRow(t, batches, alloc.GuestReport{Accepted: int64(len(batches)), Hello: true, FinalSeen: true,
		FirstReceived: pageSecond(10.6), LastReceived: pageSecond(167.6)})
}

// partialGuest lost batch 9 on the way and everything after batch 14.
func partialGuest(t *testing.T) *alloc.GuestReport {
	t.Helper()

	all := exampleGuestBatches()
	kept := append(append([]guestreport.Batch{}, all[:8]...), all[9:14]...)

	return guestRow(t, kept, alloc.GuestReport{Accepted: int64(len(kept)), Hello: true,
		FirstReceived: pageSecond(10.6), LastReceived: pageSecond(140.6)})
}

// restartedGuest is a node that restarted after batch 6: what it held went
// with it, and the hello with it.
func restartedGuest(t *testing.T) *alloc.GuestReport {
	t.Helper()

	kept := exampleGuestBatches()[6:]

	return guestRow(t, kept, alloc.GuestReport{Accepted: int64(len(kept)), FinalSeen: true, NodeRestarted: true,
		FirstReceived: pageSecond(70.6), LastReceived: pageSecond(167.6)})
}

// silentGuest is a guest that never contacted the node.
func silentGuest(*testing.T) *alloc.GuestReport {
	return &alloc.GuestReport{Codec: alloc.GuestReportCodec}
}

// guestGoldens is where each verdict's committed rendering is.
var guestGoldens = filepath.Join("testdata", "jobs-guest")

// showText runs `billet jobs show` as the CLI does and returns the report.
func showText(t *testing.T, cfg, lease string, flags ...string) string {
	t.Helper()

	return capture(t, func() {
		args := append(append([]string{"show", "--config", cfg}, flags...), lease)
		if err := Jobs(t.Context(), processEnv(), args); err != nil {
			t.Errorf("billet jobs show %v: %v", flags, err)
		}
	})
}

// EACH VERDICT IS A COMMITTED RENDERING, read from a ledger seeded through the
// real allocator and from GitHub as the CLI reads them (BILLET_UPDATE_FIXTURES=1
// rewrites them): the text report whole and the page's guest section. What each
// is for is said outright too, so a regenerated golden that lost it fails here.
func TestJobsShowGuestVerdictsAreTheCommittedGoldens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		guest   func(*testing.T) *alloc.GuestReport
		verdict string
		want    []string
		absent  []string
	}{
		{name: "none", verdict: "", want: []string{"guest      no report recorded (the node had " +
			"node.monitoring.guest off, or is older than wire 29"}, absent: []string{guestBanner}},
		{name: "never-contacted", guest: silentGuest, verdict: "never_contacted", want: []string{
			"guest      " + guestNeverContacted, "hello      not seen",
			"batches    0 accepted, 0 refused",
		}, absent: []string{guestBanner, "\nskew ", "0 B (MemTotal"}},
		{name: "partial", guest: partialGuest, verdict: "partial", want: []string{
			"guest      partial: the agent's last batch did not arrive, so the report ends before the job " +
				"did; 1 of the agent's batches between its first and last never arrived",
			"sequence   batches 1 to 14: 13 kept, 0 refused by the merge, 1 never arrived (9)",
			"tests      could not tell: the agent sends its test results in its last batch, and the report does " +
				"not hold it",
			"final      did not arrive", guestBanner,
		}},
		{name: "node-restarted", guest: restartedGuest, verdict: "node_restarted", want: []string{
			"guest      node restarted: the node restarted while the job ran, so the batches it held before " +
				"then were lost and the report starts after the restart",
			"sequence   batches 7 to 17: 11 kept", "hello      not seen", guestBanner,
		}},
		{name: "complete", guest: exampleGuest, verdict: "complete", want: []string{
			"guest      complete: the agent's last batch arrived, the report holds every batch the node " +
				"accepted, and the node did not restart while the job ran",
			guestBanner,
			"skew       the node received the last batch 850ms after the guest stamped its last sample",
			`mark 2     "Build" at 2026-10-09T12:00:18Z on the guest's clock, step 3 "Build", GitHub's ` +
				`start 2026-10-09T12:00:19Z, mark minus GitHub's start -1s`,
			`step 3     "Build", 36 process samples`, `  "compile" x4: cpu 100.8s`,
			`suite      "api/internal/store": 214 tests, 0 failures, 0 errors, 3 skipped`,
			"cpu check  the guest counted", "net check  the guest's eth0 counted",
			"agent cpu  0.7s, 0.3% of the guest's busy CPU since it booted (its own count)",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			cfg := writeJobsConfig(t, stateDir)
			points := exampleSeries()
			measured := exampleUsage(points)
			var row *alloc.GuestReport
			if tc.guest != nil {
				row = tc.guest(t)
			}
			lease := seedPageJob(t, stateDir, exampleJob, &measured, clockedSeriesOf(t, points), row)
			fakeJobsGitHub(t, stepsOn(lease, pageStepsJSON))

			text := showText(t, cfg, lease)
			page := showHTML(t, cfg, lease)
			assertPolicyHolds(t, page)
			guest := section(t, page, "guest")
			got := strings.ReplaceAll("== billet jobs show ==\n"+text+"== billet jobs show --html, the guest section ==\n"+
				guest+"\n", lease, exampleLease)
			compareGolden(t, filepath.Join(guestGoldens, tc.name+".golden"), got)

			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("the report does not say %q:\n%s", want, text)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(text, absent) || strings.Contains(guest, absent) {
					t.Errorf("the report says %q, which it has no report to say:\n%s", absent, text)
				}
			}
			if banner := strings.Contains(guest, `<p class="unverified">`+escapeHTML(guestBanner)+"</p>"); banner !=
				(tc.guest != nil && row.Accepted > 0) {
				t.Errorf("the page's guest section shows the banner = %v", banner)
			}

			assertGuestJSON(t, showText(t, cfg, lease, "--json"), tc.verdict, row)
		})
	}
}

// assertGuestJSON checks `jobs show --json`'s guest member: null with no row,
// and otherwise the verdict, the node's counts and the report the row decodes to.
func assertGuestJSON(t *testing.T, out, verdict string, row *alloc.GuestReport) {
	t.Helper()

	var got struct {
		Guest json.RawMessage `json:"guest"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, out)
	}
	if row == nil {
		if string(got.Guest) != "null" {
			t.Errorf("guest = %s, want null with no row", got.Guest)
		}
		return
	}
	var guest struct {
		Notice  string `json:"notice"`
		Verdict string `json:"verdict"`
		Channel *struct {
			Accepted  int64 `json:"accepted"`
			FinalSeen bool  `json:"final_seen"`
			Hello     bool  `json:"hello"`
		} `json:"channel"`
		Report *guestreport.Report `json:"report"`
	}
	if err := json.Unmarshal(got.Guest, &guest); err != nil {
		t.Fatalf("guest is not an object: %v\n%s", err, got.Guest)
	}
	if guest.Verdict != verdict || guest.Notice != guestBanner || guest.Channel == nil ||
		guest.Channel.Accepted != row.Accepted || guest.Channel.FinalSeen != row.FinalSeen ||
		guest.Channel.Hello != row.Hello {
		t.Errorf("guest = %s", got.Guest)
	}
	if row.Accepted == 0 {
		if guest.Report != nil {
			t.Errorf("a row with nothing accepted has a report: %s", got.Guest)
		}
		return
	}
	want, err := guestreport.Decode(row.Data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if guest.Report == nil || !reflect.DeepEqual(*guest.Report, want) {
		t.Errorf("the JSON's report is not the row's")
	}
}

// compareGolden compares got with the committed file, rewriting it under
// BILLET_UPDATE_FIXTURES=1.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()

	if os.Getenv("BILLET_UPDATE_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with BILLET_UPDATE_FIXTURES=1 to write it)", err)
	}
	if string(committed) != got {
		t.Errorf("%s differs from what billet jobs show prints; run with BILLET_UPDATE_FIXTURES=1:\n%s", path, got)
	}
}

// escapeHTML is s as html/template writes it in text.
func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "'", "&#39;", "<", "&lt;", ">", "&gt;", `"`, "&#34;").Replace(s)
}

// recorded is row as the ledger reads it back.
func recorded(row *alloc.GuestReport) alloc.RecordedGuestReport {
	return alloc.RecordedGuestReport{GuestReport: *row, Node: "epyc-1", RecordedAt: "2026-10-09T12:02:51Z"}
}

// EVERY ROW HAS ONE VERDICT, AND EVERY ONE THAT CANNOT SHOW THE GUEST'S NUMBERS
// SAYS WHY rather than showing zeros: nothing contacted, a hello and nothing
// accepted, batches accepted and not kept, data in another codec or that does
// not decode. A restart outranks a lost last batch and says both; a hello the
// node did not see does not hide batches it accepted. A last batch that arrived
// is complete only when every batch the node accepted is in the report: one the
// node dropped past what it assembles, or one the merge refused, may be the last,
// and then the test results it carried cannot be told either.
func TestEachGuestRowHasItsVerdict(t *testing.T) {
	t.Parallel()

	whole := exampleGuest(t)
	data := whole.Data
	batches := exampleGuestBatches()
	// THE LAST BATCH'S OWN CPU GOES BACK, so the merge refuses it, tests and all.
	batches[len(batches)-1].AgentCPUTicks = 0
	refused := guestRow(t, batches, alloc.GuestReport{Accepted: int64(len(batches)), Hello: true, FinalSeen: true,
		FirstReceived: pageSecond(10.6), LastReceived: pageSecond(167.6)})
	for _, tc := range []struct {
		name      string
		row       alloc.GuestReport
		verdict   guestVerdict
		why       string
		report    bool
		finalKept bool
	}{
		{"final dropped in assembly", alloc.GuestReport{Codec: 1, Hello: true, Accepted: 18, Data: data,
			FinalSeen: true}, guestPartial, "partial: the agent's last batch arrived, but not every batch the " +
			"node accepted is in the report (18 accepted, 17 kept, 0 refused by the merge)", true, false},
		{"final refused by the merge", *refused, guestPartial, "partial: the agent's last batch arrived, but not " +
			"every batch the node accepted is in the report (17 accepted, 16 kept, 1 refused by the merge)", true,
			false},
		{"restarted, final arrived, not all kept", alloc.GuestReport{Codec: 1, Accepted: 18, Data: data,
			FinalSeen: true, NodeRestarted: true}, guestNodeRestarted, "the report starts after the restart; and " +
			"not every batch the node accepted is in the report", true, false},
		{"silent", alloc.GuestReport{Codec: 1}, guestSilent, guestNeverContacted, false, false},
		{"silent with data", alloc.GuestReport{Codec: 1, Data: data}, guestSilent, guestNeverContacted, false,
			false},
		{"hello only", alloc.GuestReport{Codec: 1, Hello: true, Refused: 3}, guestNothingAccepted,
			"the agent asked the node for its limits, and the node accepted none of its batches (3 refused)", false,
			false},
		{"not kept", alloc.GuestReport{Codec: 1, Hello: true, Accepted: 4, DroppedBytes: 4096}, guestNotKept,
			"the node accepted 4 batches and could not keep them (4.0 KiB dropped)", false, false},
		{"other codec", alloc.GuestReport{Codec: 2, Hello: true, Accepted: 4, Data: data}, guestUndecodable,
			"the report is in codec 2, which this billet does not read", false, false},
		{"garbage", alloc.GuestReport{Codec: 1, Hello: true, Accepted: 4, Data: []byte("not deflate")},
			guestUndecodable, "the report could not be decoded (guestreport: ", false, false},
		{"restarted and partial", alloc.GuestReport{Codec: 1, Accepted: 17, Data: data, NodeRestarted: true},
			guestNodeRestarted, "the report starts after the restart; the agent's last batch did not arrive " +
				"either", true, false},
		{"partial", alloc.GuestReport{Codec: 1, Hello: true, Accepted: 17, Data: data}, guestPartial,
			"partial: the agent's last batch did not arrive", true, false},
		{"no hello", alloc.GuestReport{Codec: 1, Accepted: 17, Data: data, FinalSeen: true}, guestComplete,
			"complete: ", true, true},
		{"a batch received twice", alloc.GuestReport{Codec: 1, Accepted: 17, Data: duplicated(t), FinalSeen: true},
			guestComplete, "complete: ", true, true},
		{"complete", *whole, guestComplete, "complete: the agent's last batch arrived, the report holds every " +
			"batch the node accepted, and the node did not restart while the job ran", true, true},
	} {
		g := judgeGuest(recorded(&tc.row))
		if g.verdict != tc.verdict || !strings.Contains(g.why, tc.why) || (g.report != nil) != tc.report ||
			g.finalKept != tc.finalKept {
			t.Errorf("%s: verdict %s, why %q, report %v, final kept %v", tc.name, g.verdict, g.why, g.report != nil,
				g.finalKept)
		}
		if strings.Contains(g.why, "never arrived") {
			t.Errorf("%s: a report with every batch says some never arrived: %q", tc.name, g.why)
		}
		v := guestViewOf(g, nil, jobSteps{none: "none"})
		if v.HasReport != tc.report || (v.Banner != "") != tc.report {
			t.Errorf("%s: the view has a report %v and a banner %q", tc.name, v.HasReport, v.Banner)
		}
		// NO SUITES IN A REPORT WITHOUT ITS LAST BATCH is could-not-tell, never
		// "none found" (the merge-refused case has none).
		if tc.report && !tc.finalKept && len(g.report.Tests) == 0 &&
			!strings.HasPrefix(v.Tests.Why, "could not tell") {
			t.Errorf("%s: the tests of a report without its last batch say %q", tc.name, v.Tests.Why)
		}
	}
}

// duplicated is the example's report with its third batch received twice: 16
// distinct batches and a duplicate, 17 accepted.
func duplicated(t *testing.T) []byte {
	t.Helper()

	b := exampleGuestBatches()[:16]
	b = append(b, b[2])

	return guestRow(t, b, alloc.GuestReport{Accepted: 17}).Data
}

// AN UNDECODABLE ROW READ THROUGH THE LEDGER is its own verdict, never no row and
// never an empty report: the text says why and the JSON has no report.
func TestJobsShowSaysWhenTheGuestReportDoesNotDecode(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	row := &alloc.GuestReport{Codec: 1, Hello: true, Accepted: 2, Data: []byte("not deflate"),
		FirstReceived: pageSecond(10), LastReceived: pageSecond(20)}
	lease := seedPageJob(t, stateDir, exampleJob, nil, nil, row)
	fakeJobsGitHub(t, stepsOn(lease, pageStepsJSON))

	text := showText(t, cfg, lease)
	if !strings.Contains(text, "guest      the report could not be decoded (guestreport: ") ||
		strings.Contains(text, guestBanner) {
		t.Errorf("an undecodable report was not said to be:\n%s", text)
	}
	out := showText(t, cfg, lease, "--json")
	if !strings.Contains(out, `"verdict": "undecodable"`) || !strings.Contains(out, `"report": null`) {
		t.Errorf("--json = %s", out)
	}
}

// hostileGuest is text a job can make the agent report: markup, a script, an
// attribute break and a right-to-left override, within the decoder's bounds.
const hostileGuest = `<script>alert("x")</script><img src=x onerror=1> it's` + "\u202e" + `&`

// EVERY STRING THE GUEST WROTE IS QUOTED TEXT, never markup and never a line of
// its own: a process, a step mark, a suite and a failed test named to break the
// page are escaped there and quoted in the text report, and the override reaches
// neither raw.
func TestGuestStringsAreEscapedAndQuoted(t *testing.T) {
	t.Parallel()

	b := exampleGuestBatches()
	for i := range b {
		for j := range b[i].Processes {
			b[i].Processes[j].Rows[0].Name = hostileGuest
		}
		for j := range b[i].Steps {
			b[i].Steps[j].Name = hostileGuest
		}
	}
	last := &b[len(b)-1]
	last.Tests = []guestreport.SuiteResult{{Name: hostileGuest, Tests: 1, Failures: 1,
		Failed: []string{hostileGuest}}}
	row := guestRow(t, b, alloc.GuestReport{Accepted: int64(len(b)), Hello: true, FinalSeen: true,
		FirstReceived: pageSecond(10.6), LastReceived: pageSecond(167.6)})
	g := judgeGuest(recorded(row))
	if g.report == nil {
		t.Fatalf("the hostile report did not decode: %s", g.why)
	}

	points := exampleSeries()
	tl := usage.Timeline{First: pageStart, Points: points}
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(points)}
	steps := jobSteps{runner: "billet-l1", measured: measured, timeline: &tl, job: github.WorkflowJob{
		ID: 1, Name: "j", Steps: []github.JobStep{{Number: 1, Name: hostileGuest, Conclusion: "success",
			StartedAt: pageSecond(4), CompletedAt: pageSecond(19)}},
	}}
	rec := alloc.JobRecord{LeaseID: "l1", VCPU: 2, Memory: 8 << 30}
	page := jobPageOf(rec, measured, steps, &tl, "")
	page.Guest = guestSection(g, rec, measured, steps, page.Charts)
	var html bytes.Buffer
	if err := renderJobPage(&html, page); err != nil {
		t.Fatalf("renderJobPage: %v", err)
	}
	guest := section(t, html.String(), "guest")

	quoted := escapeHTML(`"<script>alert(\"x\")</script><img src=x onerror=1> it's\u202e&"`)
	// A process row, every step mark and the GitHub step each matched, a process
	// group, the suite and its failed test.
	if n := strings.Count(guest, quoted); n < 8 {
		t.Errorf("the hostile name appears escaped and quoted %d times, want everywhere it is shown:\n%s", n, guest)
	}
	for _, raw := range []string{"<script>alert", "<img", "\u202e", "it's"} {
		if strings.Contains(guest, raw) {
			t.Errorf("the guest section carries %q raw", raw)
		}
	}
	if n := strings.Count(html.String(), "<script"); n != 1 {
		t.Errorf("the page holds %d script elements, want its one", n)
	}

	var text bytes.Buffer
	renderGuest(&text, guestViewOf(g, measured, steps))
	if strings.Contains(text.String(), "\u202e") || !strings.Contains(text.String(),
		`"<script>alert(\"x\")</script><img src=x onerror=1> it's\u202e&"`) {
		t.Errorf("the text report does not quote the guest's strings:\n%s", text.String())
	}
}

// THE TEST RESULTS SAY WHICH SILENCE THEY ARE: none found when the last batch,
// which carries them, arrived; could not tell when it did not.
func TestGuestTestResultsSayWhichSilenceTheyAre(t *testing.T) {
	t.Parallel()

	r := &guestreport.Report{}
	if got := guestTests(r, true).Why; !strings.HasPrefix(got, "the agent found no test results") {
		t.Errorf("final, no suites: %q", got)
	}
	if got := guestTests(r, false).Why; !strings.HasPrefix(got, "could not tell") {
		t.Errorf("not final, no suites: %q", got)
	}
	r.Tests = []guestreport.SuiteResult{{Name: "s", Tests: 1}}
	r.Dropped.Suites = 2
	got := guestTests(r, false)
	if got.Why != "" || len(got.Suites) != 1 || got.Dropped == "" {
		t.Errorf("suites that arrived: %+v", got)
	}
}

// THE SKEW IS ONE-SIDED AND SAYS WHICH SIDE: an arrival after the guest's last
// stamp bounds its lag, and one before it proves the guest's clock ahead.
func TestGuestSkewSaysWhichSideItBounds(t *testing.T) {
	t.Parallel()

	r := &guestreport.Report{Samples: []guestreport.Sample{{AtMillis: guestMillis(10)}},
		Processes: []guestreport.ProcessSample{{AtMillis: guestMillis(12)}}}
	for _, tc := range []struct {
		arrived float64
		want    string
	}{
		{13, "received the last batch 1.25s after the guest stamped its last sample: the guest's clock is at most"},
		{11, "received the last batch 750ms before the guest stamped its last sample, so the guest's clock is at " +
			"least that far ahead"},
	} {
		got := guestSkew(alloc.RecordedGuestReport{GuestReport: alloc.GuestReport{LastReceived: pageSecond(tc.arrived)}}, r)
		if !strings.Contains(got, tc.want) {
			t.Errorf("arrived at %vs: %q", tc.arrived, got)
		}
	}
	empty := alloc.RecordedGuestReport{GuestReport: alloc.GuestReport{LastReceived: pageSecond(1)}}
	if got := guestSkew(empty, &guestreport.Report{}); !strings.HasPrefix(got, "could not tell") {
		t.Errorf("no sample: %q", got)
	}
	if got := guestSkew(alloc.RecordedGuestReport{}, r); !strings.HasPrefix(got, "could not tell") {
		t.Errorf("no arrival: %q", got)
	}
	// A STAMP FAR FROM THE NODE'S CLOCK is a span in days, never a wrapped
	// duration.
	far := &guestreport.Report{Samples: []guestreport.Sample{{AtMillis: guestreport.MaxValue}}}
	if got := guestSkew(alloc.RecordedGuestReport{GuestReport: alloc.GuestReport{LastReceived: pageStart}}, far); !strings.Contains(got, " days before") {
		t.Errorf("a far stamp: %q", got)
	}
}

// A MARK IS MATCHED BY NAME, IN ORDER, each GitHub step at most once and as the
// agent would have cut its name; a mark GitHub lists no step for is the guest's
// own.
func TestMarksMatchGitHubStepsByNameInOrder(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", guestreport.MaxNameBytes+10)
	steps := []github.JobStep{{Name: "Set up job"}, {Name: "Build"}, {Name: long}, {Name: "Build"}, {Name: "Test"}}
	marks := []guestreport.StepMark{{Name: "Build"}, {Name: guestreport.Clean(long, guestreport.MaxNameBytes)},
		{Name: "Build"}, {Name: "Lint"}, {Name: "Test"}, {Name: "Build"}}
	if got, want := matchMarks(marks, steps), []int{1, 2, 3, -1, 4, -1}; !reflect.DeepEqual(got, want) {
		t.Errorf("matchMarks = %v, want %v", got, want)
	}
	// ONE STEP, ONE MARK: a second mark of the same name matches no step.
	if got := matchMarks([]guestreport.StepMark{{Name: "Build"}, {Name: "Build"}},
		[]github.JobStep{{Name: "Build"}}); !reflect.DeepEqual(got, []int{0, -1}) {
		t.Errorf("two marks of one step = %v", got)
	}
	if got := matchMarks(marks, nil); !reflect.DeepEqual(got, []int{-1, -1, -1, -1, -1, -1}) {
		t.Errorf("with no GitHub steps = %v", got)
	}
}

// PROCESSES ARE PLACED BY THE STEP THEIR SAMPLE ENDED IN: the busiest names of
// a step shown, the rest and the other row summed, the exited CPU apart; with
// no marks, the whole report is one group.
func TestGuestProcessesArePlacedInTheStepTheyEndedIn(t *testing.T) {
	t.Parallel()

	sample := func(at int64, names ...string) guestreport.ProcessSample {
		p := guestreport.ProcessSample{AtMillis: at, Other: guestreport.ProcessUsage{Procs: 1, CPUTicks: 7},
			ResidualCPUTicks: 30}
		for i, n := range names {
			p.Rows = append(p.Rows, guestreport.ProcessRow{Name: n, ProcessUsage: guestreport.ProcessUsage{
				Procs: 1, CPUTicks: int64(100 - i), RSSBytes: 1 << 20}})
		}
		return p
	}
	r := &guestreport.Report{TicksPerSecond: 100, Stride: 1,
		Steps: []guestreport.StepMark{{Name: "a", AtSeconds: 10}, {Name: "b", AtSeconds: 20}},
		Processes: []guestreport.ProcessSample{
			sample(9_000, "early"), sample(10_000, "p1", "p2", "p3", "p4", "p5", "p6", "p7"),
			sample(19_999, "p1"), sample(20_000, "late"),
		}}
	got := guestSteps(r, []github.JobStep{{Number: 4, Name: "a"}})
	if len(got.Procs) != 3 {
		t.Fatalf("groups = %+v", got.Procs)
	}
	first, a, b := got.Procs[0], got.Procs[1], got.Procs[2]
	if first.Label != "before" || first.Name != "the first mark" || len(first.Rows) != 1 || first.Rows[0].Name != `"early"` {
		t.Errorf("before the first mark: %+v", first)
	}
	if a.Label != "step 4" || a.Name != `"a"` || len(a.Rows) != guestTopProcesses || a.Rows[0].Name != `"p1"` ||
		a.Rows[0].CPU != "2.0s" || !strings.Contains(a.Samples, "2 process samples") {
		t.Errorf("step 4: %+v", a)
	}
	// p6 and p7 (95 and 94 ticks) and the other rows (14 ticks) are the rest;
	// each sample's residual (30 ticks, 60 in all) is the exited.
	if !strings.Contains(a.Rest, "2 more names") || !strings.Contains(a.Rest, "cpu 2.0s") ||
		!strings.HasSuffix(a.Exited, ": 0.6s") {
		t.Errorf("step 4's rest %q, exited %q", a.Rest, a.Exited)
	}
	if b.Label != "mark 2" || !strings.HasPrefix(b.Name, `"b" (the guest's name`) || b.Rows[0].Name != `"late"` {
		t.Errorf("mark 2: %+v", b)
	}

	// MARKS DROPPED PAST THE BOUND leave the last group's end open: its processes
	// may belong to the steps those marks began.
	r.Dropped.Steps = 3
	if last := guestSteps(r, nil).Procs[2]; !strings.HasSuffix(last.Name, ", and whatever came after it: 3 "+
		"later marks were left out past the report's bound") {
		t.Errorf("the last group after dropped marks is named %q", last.Name)
	}
	if first := guestSteps(r, nil).Procs[1]; strings.Contains(first.Name, "whatever came after") {
		t.Errorf("a group before the last is named %q", first.Name)
	}
	r.Dropped.Steps = 0

	r.Steps = nil
	whole := guestSteps(r, nil)
	if whole.MarksWhy == "" || len(whole.Procs) != 1 || whole.Procs[0].Label != "whole" {
		t.Errorf("with no marks: %+v", whole)
	}
	r.Steps = []guestreport.StepMark{{Name: "a", AtSeconds: 10}, {Name: "b", AtSeconds: 10}}
	r.Processes = nil
	if got := guestSteps(r, nil); got.ProcsWhy == "" || len(got.Procs) != 0 {
		t.Errorf("with no process samples: %+v", got)
	}
}

// A MARK WITH NO PROCESS SAMPLE INSIDE IT SAYS SO rather than listing nothing.
func TestAMarkWithNoProcessSampleSaysSo(t *testing.T) {
	t.Parallel()

	r := &guestreport.Report{TicksPerSecond: 100, Stride: 1,
		Steps:     []guestreport.StepMark{{Name: "a", AtSeconds: 10}, {Name: "b", AtSeconds: 10}},
		Processes: []guestreport.ProcessSample{{AtMillis: 11_000}}}
	got := guestSteps(r, nil).Procs
	if len(got) != 2 || got[0].Samples != "no process sample ended inside it" || got[0].Rest != "" {
		t.Errorf("groups = %+v", got)
	}
}

// THE CROSS-CHECKS SET THE GUEST BESIDE THE HOST, AND SAY WHAT THE HOST DID NOT
// MEASURE: the vCPU threads, else the whole VM's CPU, else not measured; the
// tap, else not measured; no report, nothing to check against.
func TestGuestCrossChecksSayWhatTheHostMeasured(t *testing.T) {
	t.Parallel()

	r := &guestreport.Report{TicksPerSecond: 100, Samples: []guestreport.Sample{{AtMillis: 1, CPUBusyTicks: 25_000,
		CPUTotalTicks: 50_000, NetRxBytes: 3 << 20, NetTxBytes: 1 << 20}}}
	u := alloc.JobUsage{GuestCPUMicros: 240_000_000, CPUUserMicros: 245_000_000, CPUSystemMicros: 5_000_000,
		NetRxBytes: 2 << 20, NetTxBytes: 1 << 20}
	check := func(unmeasured ...string) (string, string) {
		m := &alloc.RecordedUsage{JobUsage: u}
		m.Unmeasured = unmeasured
		f := guestChecks(r, m)
		return f[0].Value, f[1].Value
	}
	cpu, net := check()
	if !strings.Contains(cpu, "the guest counted 250.0s busy") ||
		!strings.Contains(cpu, "the host counted 240.0s on its vCPU threads over the whole lease (guest minus host +10.0s)") {
		t.Errorf("cpu = %q", cpu)
	}
	if !strings.Contains(net, "received 3.0 MiB, sent 1.0 MiB since it booted") ||
		!strings.Contains(net, "(guest minus host: received +1.0 MiB, sent +0 B)") {
		t.Errorf("net = %q", net)
	}
	if cpu, net := check(alloc.UsageThreads, alloc.UsageNet); !strings.Contains(cpu, "250.0s for the whole VM") ||
		!strings.HasSuffix(net, "the host's network was not measured") {
		t.Errorf("no vCPU split, no network: %q, %q", cpu, net)
	}
	if cpu, _ := check(alloc.UsageThreads, alloc.UsageCPU); !strings.HasSuffix(cpu, "the host's CPU was not measured") {
		t.Errorf("no CPU: %q", cpu)
	}
	f := guestChecks(r, nil)
	if !strings.HasSuffix(f[0].Value, "nothing to check it against") || !strings.HasSuffix(f[1].Value, "nothing to check it against") {
		t.Errorf("no usage report: %+v", f)
	}
	if f := guestChecks(&guestreport.Report{TicksPerSecond: 100}, nil); !f[0].Unmeasured {
		t.Errorf("no sample: %+v", f)
	}

	// THE AGENT'S SHARE needs a busy count to be a share of.
	idle := &guestreport.Report{TicksPerSecond: 100, AgentCPUTicks: 50, Samples: []guestreport.Sample{{AtMillis: 1}}}
	if got := guestAgent(alloc.RecordedGuestReport{}, idle)[0].Value; !strings.Contains(got, "could not tell") {
		t.Errorf("no busy CPU: %q", got)
	}
	if got := guestAgent(alloc.RecordedGuestReport{}, r)[0].Value; got != "0.0s, 0.0% of the guest's busy CPU "+
		"since it booted (its own count)" {
		t.Errorf("agent cpu = %q", got)
	}
}

// THE GUEST'S MEMORY IS DRAWN ON THE HOST'S AXIS, with the host's level when it
// was measured, and a sample outside the host's series is counted, not drawn;
// with no host chart there is nothing to draw it on, and it says so.
func TestTheGuestMemoryChart(t *testing.T) {
	t.Parallel()

	points := exampleSeries()
	tl := usage.Timeline{First: pageStart, Points: points}
	measured := &alloc.RecordedUsage{JobUsage: exampleUsage(points)}
	rec := alloc.JobRecord{LeaseID: "l1", VCPU: 2, Memory: 8 << 30}
	charts := buildCharts(&tl, "", rec, measured, nil)
	r := &guestreport.Report{Samples: []guestreport.Sample{
		{AtMillis: guestMillis(-30), MemUsedBytes: 1 << 30},
		{AtMillis: guestMillis(10), MemUsedBytes: 1 << 30, MemCachedBytes: 1 << 29},
		{AtMillis: guestMillis(11), MemUsedBytes: 1 << 30, MemCachedBytes: 1 << 29},
	}}

	got := guestMemoryChart(charts, rec, measured, r)
	if got.Why != "" || len(got.Chart.Lines) != 3 || got.Chart.Lines[2].Class != "c" || got.Chart.Limit == nil ||
		!strings.Contains(got.Chart.Note, "1 of the guest's 3 samples fall outside the host's series") {
		t.Errorf("chart = %+v", got)
	}
	for _, line := range got.Chart.Lines {
		if line.D == "" {
			t.Errorf("the %s line draws nothing", line.Label)
		}
	}
	measured.Unmeasured = []string{alloc.UsageMemory}
	if got := guestMemoryChart(charts, rec, measured, r); len(got.Chart.Lines) != 2 ||
		!strings.Contains(got.Chart.Note, "The host's level is not drawn") {
		t.Errorf("host memory unmeasured: %+v", got)
	}
	if got := guestMemoryChart(pageCharts{Why: "no usage report was recorded"}, rec, nil, r); !strings.HasSuffix(got.Why,
		"none is drawn: no usage report was recorded") {
		t.Errorf("no host chart: %q", got.Why)
	}
	r.Samples = r.Samples[:1]
	if got := guestMemoryChart(charts, rec, measured, r); !strings.HasPrefix(got.Why, "none of the guest's samples") {
		t.Errorf("every sample outside: %q", got.Why)
	}
	if got := guestMemoryChart(charts, rec, measured, &guestreport.Report{}); !strings.HasPrefix(got.Why,
		"the report holds no sample") {
		t.Errorf("no sample: %q", got.Why)
	}
}

// maxGuestPageBytes is the most a guest report the ledger can keep adds to a page
// beside the same page without it. renderLargestGuest holds every section the page
// shows at its bound, each name at its bound in the character that grows most once
// quoted and escaped (a double quote: six bytes) and each number at its widest.
const maxGuestPageBytes = 3 << 20

// incompressibleGuest is a report whose text is random, so it encodes to near
// the ledger's bound with its process samples halved to fit: 512 step marks, one
// process sample with twenty names between each two of them, and the suites and
// failed names at their bounds.
func incompressibleGuest(t *testing.T) []byte {
	t.Helper()

	rng := rand.New(rand.NewPCG(3, 4))
	const alphabet = `abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789<>&"'_-./`
	text := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		return string(b)
	}
	base := pageStart.Unix()
	r := guestreport.Report{AgentVersion: "0.15.1", TicksPerSecond: 100, FirstSeq: 1, LastSeq: 1, Batches: 1,
		Stride: 1}
	for i := range guestreport.MaxReportSteps {
		at := base + int64(i)*2
		r.Steps = append(r.Steps, guestreport.StepMark{Name: text(guestreport.MaxNameBytes), AtSeconds: at})
		var rows []guestreport.ProcessRow
		for range guestreport.KeepProcessesByName {
			rows = append(rows, guestreport.ProcessRow{Name: text(guestreport.MaxNameBytes),
				ProcessUsage: guestreport.ProcessUsage{Procs: 1, CPUTicks: rng.Int64N(1000),
					RSSBytes: rng.Int64N(1 << 32)}})
		}
		r.Processes = append(r.Processes, guestreport.ProcessSample{AtMillis: (at + 1) * 1000, Rows: rows})
		r.Samples = append(r.Samples, guestreport.Sample{AtMillis: (at + 1) * 1000, CPUBusyTicks: int64(i),
			CPUTotalTicks: int64(i) * 2, MemUsedBytes: rng.Int64N(8 << 30)})
	}
	names := 0
	for range guestreport.MaxReportSuites {
		s := guestreport.SuiteResult{Name: text(guestreport.MaxNameBytes), Tests: 9, Failures: 2}
		for range min(2, guestreport.MaxReportFailedNames-names) {
			s.Failed = append(s.Failed, text(guestreport.MaxFailedNameBytes))
			names++
		}
		r.Tests = append(r.Tests, s)
	}
	_, data, err := guestreport.Downsample(r, alloc.MaxGuestReportBytes)
	if err != nil {
		t.Fatalf("Downsample: %v", err)
	}
	if len(data) < alloc.MaxGuestReportBytes*3/4 {
		t.Fatalf("the report encodes to %d bytes, not near the %d bound", len(data), alloc.MaxGuestReportBytes)
	}

	return data
}

// renderLargestGuest is the report that renders largest, and GitHub's steps for it:
// every mark matched to a step of the same name, a process group before the
// first mark and after each, each with twenty names of which the page shows five,
// 64 gaps in the account, the suites and failed names at their bounds, every
// name made of double quotes and every number at its widest, at one tick a
// second. It compresses to well under the ledger's bound, so nothing is halved.
func renderLargestGuest(t *testing.T) ([]byte, []github.JobStep) {
	t.Helper()

	const top = guestreport.MaxValue
	quotes := func(n int) string { return strings.Repeat(`"`, n) }
	widest := guestreport.ProcessUsage{Procs: top, CPUTicks: top, RSSBytes: top, ReadBytes: top, WriteBytes: top}
	base := pageStart.Unix()
	r := guestreport.Report{AgentVersion: quotes(guestreport.MaxNameBytes), TicksPerSecond: 1,
		AgentCPUTicks: top, FirstSeq: top - 1000, LastSeq: top, Batches: 1001 - guestreport.MaxReportGaps,
		Missing: guestreport.MaxReportGaps, Duplicates: top, Conflicts: top, Stride: 1, Saturated: top,
		Dropped: guestreport.Dropped{Steps: top, Suites: top, FailedNames: top}}
	for i := range guestreport.MaxReportGaps {
		seq := uint64(top - 999 + 2*i)
		r.Gaps = append(r.Gaps, guestreport.Gap{FirstSeq: seq, LastSeq: seq})
	}
	process := func(atMillis int64) guestreport.ProcessSample {
		p := guestreport.ProcessSample{AtMillis: atMillis, Other: widest, ResidualCPUTicks: top}
		for k := range guestreport.KeepProcessesByName {
			p.Rows = append(p.Rows, guestreport.ProcessRow{Name: quotes(guestreport.MaxNameBytes-1) +
				string(rune('a'+k)), ProcessUsage: widest})
		}
		return p
	}
	var steps []github.JobStep
	r.Processes = append(r.Processes, process((base-1)*1000))
	for i := range guestreport.MaxReportSteps {
		at := base + int64(i)*2
		name := quotes(guestreport.MaxNameBytes)
		r.Steps = append(r.Steps, guestreport.StepMark{Name: name, AtSeconds: at})
		steps = append(steps, github.JobStep{Number: math.MaxInt64, Name: name, Conclusion: "success",
			StartedAt: time.Unix(at, 0), CompletedAt: time.Unix(at+2, 0)})
		r.Processes = append(r.Processes, process((at+1)*1000))
		r.Samples = append(r.Samples, guestreport.Sample{AtMillis: (at + 1) * 1000, CPUBusyTicks: top,
			CPUTotalTicks: top, MemUsedBytes: top, MemAvailableBytes: top, MemCachedBytes: top,
			DiskReadBytes: top, DiskWriteBytes: top, NetRxBytes: top, NetTxBytes: top})
	}
	names := 0
	for range guestreport.MaxReportSuites {
		s := guestreport.SuiteResult{Name: quotes(guestreport.MaxNameBytes), Tests: top, Failures: top,
			Errors: top, Skipped: top}
		for range min(2, guestreport.MaxReportFailedNames-names) {
			s.Failed = append(s.Failed, quotes(guestreport.MaxFailedNameBytes))
			names++
		}
		r.Tests = append(r.Tests, s)
	}
	data, err := guestreport.Encode(r)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	return data, steps
}

// A GUEST REPORT THE LEDGER CAN KEEP ADDS A STATED SIZE TO A PAGE, measured
// against the same page without it beside the largest series: one whose text
// does not compress, and the one that renders largest. The page keeps its policy.
func TestAPageWithTheLargestGuestReportIsBounded(t *testing.T) {
	t.Parallel()

	tl := largestSeries(t)
	hostile, hostileSteps := renderLargestGuest(t)
	for _, tc := range []struct {
		name   string
		data   []byte
		listed []github.JobStep
	}{
		{"incompressible", incompressibleGuest(t), nil},
		{"hostile", hostile, hostileSteps},
	} {
		g := judgeGuest(alloc.RecordedGuestReport{GuestReport: alloc.GuestReport{Codec: 1, Data: tc.data,
			Accepted: math.MaxInt64, Refused: math.MaxInt64, DroppedBytes: math.MaxInt64, Hello: true,
			FinalSeen: true, FirstReceived: pageSecond(1), LastReceived: pageSecond(2000)}})
		if g.report == nil {
			t.Fatalf("%s: the report did not decode: %s", tc.name, g.why)
		}
		measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(tl.Points)}
		rec := alloc.JobRecord{LeaseID: "l1", VCPU: 2, Memory: 8 << 30}
		steps := jobSteps{none: "no GitHub job"}
		if tc.listed != nil {
			steps = jobSteps{runner: "billet-l1", measured: measured, timeline: tl,
				job: github.WorkflowJob{ID: 1, Name: "j", Steps: tc.listed}}
		}
		page := jobPageOf(rec, measured, steps, tl, "")
		var without bytes.Buffer
		if err := renderJobPage(&without, page); err != nil {
			t.Fatalf("renderJobPage: %v", err)
		}
		page.Guest = guestSection(g, rec, measured, steps, page.Charts)
		var with bytes.Buffer
		if err := renderJobPage(&with, page); err != nil {
			t.Fatalf("renderJobPage: %v", err)
		}
		if added := with.Len() - without.Len(); added > maxGuestPageBytes {
			t.Errorf("%s: the guest report adds %d bytes to the page, over the %d bound", tc.name, added,
				maxGuestPageBytes)
		}
		assertPolicyHolds(t, with.String())
		if !strings.Contains(with.String(), "<figcaption>Guest memory") {
			t.Errorf("%s: the guest's memory was not drawn, so the bound proves nothing about its chart", tc.name)
		}
		if tc.listed != nil && strings.Count(with.String(), "matched to no GitHub step") != 0 {
			t.Errorf("%s: a mark was not matched, so the bound misses the GitHub step it would show", tc.name)
		}
		t.Logf("%s guest report: %d bytes encoded, %d marks, %d process samples; it adds %d bytes to a page "+
			"of %d", tc.name, len(tc.data), len(g.report.Steps), len(g.report.Processes), with.Len()-without.Len(),
			with.Len())
	}
}
