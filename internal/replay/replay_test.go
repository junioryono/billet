package replay

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// outDir keeps a replay's records and summary beside the test log when set, so a
// run worth reading can be read after the test binary has gone.
var outDir = flag.String("replay.out", "", "directory to write each replay's records and summary into")

// twoHosts is the fleet every scenario below runs against unless it says
// otherwise: two identical machines, two tiers, a ceiling the machines add up to.
func twoHosts(policy config.PlacementPolicy) Fleet {
	return Fleet{
		Hosts: []Host{
			{Name: "host-a", VCPU: 16, Memory: 64 * config.GiB},
			{Name: "host-b", VCPU: 16, Memory: 64 * config.GiB},
		},
		Tiers: []TierShape{
			{Label: "billet-2vcpu", VCPU: 2, Memory: 4 * config.GiB},
			{Label: "billet-4vcpu", VCPU: 4, Memory: 8 * config.GiB},
		},
		MaxVCPU:   32,
		MaxMemory: 128 * config.GiB,
		Placement: policy,
	}
}

// keep writes a report where -replay.out points, and logs its summary either way.
func keep(t *testing.T, name string, report *Report) {
	t.Helper()

	t.Log(report.Summary())

	if *outDir == "" {
		return
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", *outDir, err)
	}

	var records bytes.Buffer
	if err := report.WriteJSONL(&records); err != nil {
		t.Fatal(err)
	}

	for file, body := range map[string][]byte{
		name + ".jsonl":       records.Bytes(),
		name + ".summary.txt": []byte(report.Summary()),
	} {
		if err := os.WriteFile(filepath.Join(*outDir, file), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
}

// EVERY JOB THE TRACE OFFERED HAS A ROW THE LEDGER WROTE, with a start and a
// finish, and the report is built from those rows rather than from anything the
// harness counted itself.
func TestEveryTraceJobHasALedgerRow(t *testing.T) {
	trace := MorningBurst(7, Params{Jobs: 24})

	report := Run(t, twoHosts(config.PlacementPack), trace, Options{})
	keep(t, "ledger-rows", report)

	if len(report.Missing) != 0 {
		t.Fatalf("the ledger has no row for jobs %v", report.Missing)
	}

	if len(report.Unstarted) != 0 {
		t.Fatalf("the ledger never saw jobs %v start", report.Unstarted)
	}

	if len(report.Records) != len(trace.Arrivals) {
		t.Fatalf("recorded %d jobs for a trace of %d", len(report.Records), len(trace.Arrivals))
	}

	for _, rec := range report.Records {
		if rec.Node == "" || rec.FinishedAt.IsZero() || rec.Conclusion == "" {
			t.Errorf("job %d is recorded without a node, a finish or a conclusion: %+v", rec.Seq, rec)
		}

		if rec.Result != ResultSucceeded {
			t.Errorf("job %d recorded result %q, want GitHub's %q", rec.Seq, rec.Result, ResultSucceeded)
		}

		// THE CHARGED SHAPE AND THE BACKEND ARE THE ROW'S. A host-backed lease is
		// charged the tier request, on the simulated backend, with no price and
		// no cache observation; the record carries exactly that and nothing the
		// harness invented in its place.
		shape, _ := twoHosts(config.PlacementPack).shape(rec.Tier)
		if rec.Provider != string(config.ProviderSimulated) || rec.VCPU != shape.VCPU ||
			rec.Memory != shape.Memory || rec.InstanceType != "" || rec.PriceMicrosPerHour != 0 {
			t.Errorf("job %d is recorded as %s/%q charged %d vCPU %s at %d micros, want simulated, "+
				"the tier's %d vCPU %s and no price", rec.Seq, rec.Provider, rec.InstanceType,
				rec.VCPU, rec.Memory, rec.PriceMicrosPerHour, shape.VCPU, shape.Memory)
		}

		if rec.ImageCache != "" || rec.ActionsCache != "" || rec.CostSource != "" {
			t.Errorf("job %d carries a cache observation or a cost (%q, %q, %q) that nothing observed",
				rec.Seq, rec.ImageCache, rec.ActionsCache, rec.CostSource)
		}

		// THE LEDGER'S TIMES ARE THE CLOCK'S, not the wall's: a start recorded at
		// wall time would be dated in September and read as a queue wait of months.
		if rec.StartedAt.Before(rec.Arrival) || rec.FinishedAt.Before(rec.StartedAt) {
			t.Errorf("job %d is recorded out of order: arrived %s, started %s, finished %s",
				rec.Seq, rec.Arrival, rec.StartedAt, rec.FinishedAt)
		}
	}
}

// THE SAME TRACE REPLAYS TO THE SAME PLACEMENTS AND THE SAME TIMES, so a
// comparison between two policies is a comparison and not a sample.
//
// Two whole stacks, two ledgers, two sets of goroutines; what is compared is
// where every job landed and when the ledger says it started and finished.
func TestTheSameTraceReplaysToTheSamePlacements(t *testing.T) {
	trace := MonorepoFanOut(11, Params{Jobs: 60})
	fleet := twoHosts(config.PlacementSpread)

	first := Run(t, fleet, trace, Options{})
	second := Run(t, fleet, trace, Options{})
	keep(t, "determinism", first)

	if len(first.Records) != len(trace.Arrivals) || len(second.Records) != len(trace.Arrivals) {
		t.Fatalf("recorded %d and %d jobs for a trace of %d", len(first.Records), len(second.Records),
			len(trace.Arrivals))
	}

	if !reflect.DeepEqual(first.Placements(), second.Placements()) {
		t.Fatalf("two replays of one trace placed jobs differently:\n%v\n%v",
			first.Placements(), second.Placements())
	}

	for i := range first.Records {
		a, b := first.Records[i], second.Records[i]
		if !a.AssignedAt.Equal(b.AssignedAt) || !a.StartedAt.Equal(b.StartedAt) || !a.FinishedAt.Equal(b.FinishedAt) {
			t.Errorf("job %d was dated differently by two replays: %s/%s/%s and %s/%s/%s", a.Seq,
				a.AssignedAt, a.StartedAt, a.FinishedAt, b.AssignedAt, b.StartedAt, b.FinishedAt)
		}
	}
}

// PACK AND SPREAD ARE DISTINGUISHABLE IN THE REPORT. Pack fills a host before
// starting the next, spread keeps them even; on one trace the two must leave a
// different footprint, or the harness cannot see the policy it exists to measure.
func TestPackAndSpreadPlaceDifferently(t *testing.T) {
	trace := MorningBurst(3, Params{Jobs: 40})

	pack := Run(t, twoHosts(config.PlacementPack), trace, Options{})
	spread := Run(t, twoHosts(config.PlacementSpread), trace, Options{})
	keep(t, "pack", pack)
	keep(t, "spread", spread)

	if reflect.DeepEqual(pack.Placements(), spread.Placements()) {
		t.Fatalf("pack and spread produced identical placements on %d jobs; the report cannot see the policy",
			len(trace.Arrivals))
	}

	// Spread's busiest host carries no more than pack's, and the gap between its
	// two hosts is smaller: the shape each policy is named for.
	packPeaks, spreadPeaks := pack.PeakVCPUByNode(), spread.PeakVCPUByNode()

	if maxOf(spreadPeaks) > maxOf(packPeaks) {
		t.Errorf("spread's busiest host peaked at %d vCPU and pack's at %d; pack should fill a host first",
			maxOf(spreadPeaks), maxOf(packPeaks))
	}

	if gap(spreadPeaks) >= gap(packPeaks) {
		t.Errorf("spread left a gap of %d vCPU between its hosts and pack %d; spread should keep them even",
			gap(spreadPeaks), gap(packPeaks))
	}
}

func maxOf(peaks map[string]int) int {
	out := 0
	for _, v := range peaks {
		out = max(out, v)
	}

	return out
}

func gap(peaks map[string]int) int {
	lo, hi := -1, 0
	for _, v := range peaks {
		hi = max(hi, v)
		if lo < 0 || v < lo {
			lo = v
		}
	}

	return hi - max(lo, 0)
}

// NO PLACEMENT EXCEEDS A HOST OR THE DEPLOYMENT, under a burst that saturates
// the fleet and queues jobs behind it.
//
// THE HARNESS'S OWN MUTATION TEST. The fleet here is small enough that the burst
// cannot fit at once, so a placer that ignored a host's room or the ceiling
// would land jobs past it and the ledger's own timestamps would show the
// overlap. Queueing is asserted too: a run in which nothing waited did not
// saturate anything, and would prove nothing about the ceiling.
func TestNoPlacementExceedsAHostOrTheDeployment(t *testing.T) {
	fleet := Fleet{
		Hosts: []Host{
			{Name: "small-a", VCPU: 8, Memory: 32 * config.GiB},
			{Name: "small-b", VCPU: 8, Memory: 32 * config.GiB},
		},
		Tiers: []TierShape{
			{Label: "billet-2vcpu", VCPU: 2, Memory: 4 * config.GiB},
			{Label: "billet-4vcpu", VCPU: 4, Memory: 8 * config.GiB},
		},
		MaxVCPU:   12,
		MaxMemory: 64 * config.GiB,
		Placement: config.PlacementPack,
	}

	trace := MonorepoFanOut(5, Params{Jobs: 80})

	report := Run(t, fleet, trace, Options{})
	keep(t, "saturation", report)

	if len(report.Missing) != 0 || len(report.Unstarted) != 0 {
		t.Fatalf("missing %v, unstarted %v", report.Missing, report.Unstarted)
	}

	if len(report.Violations) != 0 {
		t.Fatalf("the ledger records an overcommit: %v", report.Violations)
	}

	queued := 0

	for _, rec := range report.Records {
		if time.Duration(rec.QueueWait) > fleet.boot() {
			queued++
		}
	}

	if queued == 0 {
		t.Fatal("no job waited beyond the boot model, so the burst never saturated the fleet and the " +
			"ceiling was never tested")
	}

	// AND THE CEILING, NOT ONLY THE HOSTS, DID SOME OF THE WORK: 16 vCPU of hosts
	// under a 12 vCPU deployment means the peak sits at the ceiling.
	total := 0
	for _, peak := range report.PeakVCPUByNode() {
		total += peak
	}

	if total < fleet.MaxVCPU-4 {
		t.Errorf("the hosts peaked at %d vCPU between them under a %d vCPU ceiling; the burst did not reach it",
			total, fleet.MaxVCPU)
	}
}

// A FEW THOUSAND JOBS REPLAY IN MINUTES AND PRODUCE A PER-LEASE RECORD SET. The
// acceptance-size run; its wall time is logged and belongs in the record of the
// change that moved it.
func TestAFewThousandJobsReplayInMinutes(t *testing.T) {
	const jobs = 2000

	fleet := Fleet{
		Hosts: []Host{
			{Name: "rack-1", VCPU: 32, Memory: 128 * config.GiB},
			{Name: "rack-2", VCPU: 32, Memory: 128 * config.GiB},
			{Name: "rack-3", VCPU: 32, Memory: 128 * config.GiB},
			{Name: "mac-1", VCPU: 8, Memory: 32 * config.GiB},
		},
		Tiers: []TierShape{
			{Label: "billet-2vcpu", VCPU: 2, Memory: 4 * config.GiB},
			{Label: "billet-4vcpu", VCPU: 4, Memory: 8 * config.GiB},
			{Label: "billet-8vcpu", VCPU: 8, Memory: 16 * config.GiB},
		},
		MaxVCPU:   104,
		MaxMemory: 416 * config.GiB,
		Placement: config.PlacementPack,
	}

	trace := MorningBurst(2026, Params{
		Jobs:         jobs,
		Tiers:        []string{"billet-2vcpu", "billet-4vcpu", "billet-8vcpu"},
		Repositories: []string{"web", "api", "infra", "mobile", "docs"},
	})

	started := time.Now()
	report := Run(t, fleet, trace, Options{})
	t.Logf("replayed %d jobs in %s", jobs, time.Since(started).Round(time.Second))
	keep(t, "morning-burst-2000", report)

	if len(report.Records) != jobs || len(report.Missing) != 0 || len(report.Unstarted) != 0 {
		t.Fatalf("recorded %d of %d jobs (missing %v, unstarted %v)", len(report.Records), jobs,
			report.Missing, report.Unstarted)
	}

	if len(report.Violations) != 0 {
		t.Fatalf("the ledger records an overcommit: %v", report.Violations)
	}
}
