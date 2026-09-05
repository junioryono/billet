package replay

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

	if len(report.Unstarted) != 0 || len(report.Unfinished) != 0 {
		t.Fatalf("the ledger never saw jobs %v start or jobs %v finish", report.Unstarted, report.Unfinished)
	}

	if len(report.Records) != len(trace.Arrivals) {
		t.Fatalf("recorded %d jobs for a trace of %d", len(report.Records), len(trace.Arrivals))
	}

	// THE DISCOVERY SLOTS ARE IN THE PROOF. Each tier holds one escrowed lease
	// nobody is given until shutdown releases it; the ledger charged it, so the
	// capacity sweep must count it, or an overcommit made of escrow alone would
	// read as none.
	if report.EscrowRows != len(twoHosts(config.PlacementPack).Tiers) || len(report.escrows) != report.EscrowRows {
		t.Errorf("counted %d escrow rows and swept %d; want one discovery slot per tier (%d)",
			report.EscrowRows, len(report.escrows), len(twoHosts(config.PlacementPack).Tiers))
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
		// And the charge begins at escrow, before the job was even offered.
		if rec.ChargedFrom.IsZero() || rec.ChargedFrom.After(rec.AssignedAt) {
			t.Errorf("job %d is charged from %s, after or without its escrow (assigned %s)",
				rec.Seq, rec.ChargedFrom, rec.AssignedAt)
		}

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

	for _, policy := range []config.PlacementPolicy{config.PlacementPack, config.PlacementSpread} {
		t.Run(string(policy), func(t *testing.T) {
			fleet := twoHosts(policy)

			first := Run(t, fleet, trace, Options{})
			second := Run(t, fleet, trace, Options{})
			keep(t, "determinism-"+string(policy), first)

			if len(first.Records) != len(trace.Arrivals) || len(second.Records) != len(trace.Arrivals) {
				t.Fatalf("recorded %d and %d jobs for a trace of %d", len(first.Records),
					len(second.Records), len(trace.Arrivals))
			}

			if !reflect.DeepEqual(first.Placements(), second.Placements()) {
				t.Fatalf("two replays of one trace placed jobs differently:\n%v\n%v",
					first.Placements(), second.Placements())
			}

			for i := range first.Records {
				a, b := &first.Records[i], &second.Records[i]
				if !a.ChargedFrom.Equal(b.ChargedFrom) || !a.AssignedAt.Equal(b.AssignedAt) ||
					!a.StartedAt.Equal(b.StartedAt) || !a.FinishedAt.Equal(b.FinishedAt) {
					t.Errorf("job %d was dated differently by two replays: %s/%s/%s/%s and %s/%s/%s/%s",
						a.Seq, a.ChargedFrom, a.AssignedAt, a.StartedAt, a.FinishedAt,
						b.ChargedFrom, b.AssignedAt, b.StartedAt, b.FinishedAt)
				}
			}
		})
	}
}

// EVENTS AT ONE TRACE INSTANT ARE DATED AT DISTINCT INSTANTS, IN DELIVERY
// ORDER. Four jobs arrive at once; each is escrowed and assigned after the one
// before it, and the ledger's timestamps have to say so, or a sweep over the
// ledger cannot tell which charge saw which release. A clock that only moved
// forward to the trace's instant dated all four identically and passed every
// other test here.
func TestJobsArrivingAtOneInstantAreDatedInOrder(t *testing.T) {
	at := DefaultStart

	var trace Trace

	for range 4 {
		trace.Arrivals = append(trace.Arrivals, Arrival{
			At: at, Tier: "billet-2vcpu", Repository: "web",
			WorkflowRef: "acme/web/.github/workflows/ci.yml@refs/heads/main",
			Duration:    Duration(3 * time.Minute),
		})
	}

	trace.Normalize()

	report := Run(t, twoHosts(config.PlacementPack), trace, Options{})

	if len(report.Records) != 4 || len(report.Missing) != 0 {
		t.Fatalf("recorded %d of 4 jobs (missing %v)", len(report.Records), report.Missing)
	}

	for i := 1; i < len(report.Records); i++ {
		prev, next := &report.Records[i-1], &report.Records[i]

		if !next.AssignedAt.After(prev.AssignedAt) {
			t.Errorf("jobs %d and %d arrived together and are assigned at %s and %s; the second must be "+
				"dated after the first", prev.Seq, next.Seq, prev.AssignedAt, next.AssignedAt)
		}

		if !next.ChargedFrom.After(prev.ChargedFrom) {
			t.Errorf("jobs %d and %d arrived together and are charged from %s and %s; the second must be "+
				"dated after the first", prev.Seq, next.Seq, prev.ChargedFrom, next.ChargedFrom)
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

	// Spread's busiest host carries no more at its peak than pack's, and pack
	// sends a larger share of the jobs to one host: the shape each policy is
	// named for. Peaks alone cannot separate them once a burst fills both hosts,
	// which is why the share of jobs is the discriminator.
	packPeaks, spreadPeaks := pack.PeakVCPUByNode(), spread.PeakVCPUByNode()

	if maxOf(spreadPeaks) > maxOf(packPeaks) {
		t.Errorf("spread's busiest host peaked at %d vCPU and pack's at %d; pack should fill a host first",
			maxOf(spreadPeaks), maxOf(packPeaks))
	}

	if busiestShare(spread) >= busiestShare(pack) {
		t.Errorf("spread sent %.0f%% of the jobs to its busiest host and pack %.0f%%; pack should "+
			"concentrate and spread should even out", 100*busiestShare(spread), 100*busiestShare(pack))
	}
}

func maxOf(peaks map[string]int) int {
	out := 0
	for _, v := range peaks {
		out = max(out, v)
	}

	return out
}

// busiestShare is the share of a report's jobs that landed on its busiest host.
func busiestShare(r *Report) float64 {
	counts := map[string]int{}
	for _, node := range r.Placements() {
		counts[node]++
	}

	return float64(maxOf(counts)) / float64(len(r.Records))
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
	// under a 12 vCPU deployment means the fleet's peak, escrow included and
	// measured at one instant, sits at the ceiling.
	if peak := report.PeakDeploymentVCPU(); peak != fleet.MaxVCPU {
		t.Errorf("the deployment peaked at %d vCPU under a %d vCPU ceiling; a saturating burst reaches it exactly",
			peak, fleet.MaxVCPU)
	}

	if len(report.Unfinished) != 0 {
		t.Fatalf("leases %v were never archived, so the capacity verdict is provisional", report.Unfinished)
	}
}

// A FEW THOUSAND JOBS REPLAY IN MINUTES AND PRODUCE A PER-LEASE RECORD SET. The
// acceptance-size run; its wall time is logged and belongs in the record of the
// change that moved it.
//
// SIZED BY WHERE IT RUNS. Two thousand jobs take about a minute and a half on a
// laptop without the race detector and did not finish inside CI's shared,
// race-instrumented test job before that job's cap; so the suite replays three
// hundred by default, which every gate carries, and BILLET_REPLAY_JOBS names
// the size a dedicated run wants. CI's replay job sets it to two thousand.
func TestAFewThousandJobsReplayInMinutes(t *testing.T) {
	jobs := 300

	if want := os.Getenv("BILLET_REPLAY_JOBS"); want != "" {
		n, err := strconv.Atoi(want)
		if err != nil || n <= 0 {
			t.Fatalf("BILLET_REPLAY_JOBS=%q is not a positive count", want)
		}

		jobs = n
	}

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
	keep(t, "morning-burst", report)

	if len(report.Records) != jobs || len(report.Missing) != 0 || len(report.Unstarted) != 0 {
		t.Fatalf("recorded %d of %d jobs (missing %v, unstarted %v)", len(report.Records), jobs,
			report.Missing, report.Unstarted)
	}

	if len(report.Violations) != 0 {
		t.Fatalf("the ledger records an overcommit: %v", report.Violations)
	}
}
