package replay

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/jobtrace"
)

// A REPLAYED DEPLOYMENT EXPORTS BACK ITS OWN TRACE AND FLEET: the ledger the
// replay wrote is read as an operator's `billet jobs export` reads one, and
// gives back every job as it arrived, ran and ended, and every host as it
// registered.
func TestTheLedgerExportRoundTrips(t *testing.T) {
	trace := MorningBurst(5, Params{Jobs: 24})

	// FOUR AT ONE INSTANT, each different, so the export's order for jobs that
	// arrived together is the replay's.
	at := trace.Arrivals[3].At
	for i := range 4 {
		trace.Arrivals = append(trace.Arrivals, Arrival{
			At: at, Tier: "billet-2vcpu", Repository: "web", RunID: int64(900 + i),
			WorkflowRef: "acme/web/.github/workflows/ci.yml@refs/heads/main",
			Duration:    Duration(time.Duration(i+1) * time.Minute),
		})
	}

	trace.Normalize()

	for i := range trace.Arrivals {
		trace.Arrivals[i].Job = fmt.Sprintf("build (shard %d)", i%3)
	}

	fleet := twoHosts(config.PlacementPack)

	var (
		exported jobtrace.Trace
		hosts    []jobtrace.Host
		gaps     alloc.ExportGaps
	)

	Run(t, fleet, trace, Options{AfterRun: func(a *alloc.Allocator) {
		ctx := context.WithoutCancel(t.Context())

		var err error
		if exported, gaps, err = a.ExportTrace(ctx, trace.Arrivals[0].At.Add(-time.Hour),
			trace.Arrivals[len(trace.Arrivals)-1].At.Add(24*time.Hour), true); err != nil {
			t.Fatalf("ExportTrace: %v", err)
		}

		if hosts, err = a.ExportHosts(ctx); err != nil {
			t.Fatalf("ExportHosts: %v", err)
		}
	}})

	if gaps.Read != len(trace.Arrivals) || gaps.NeverStarted+gaps.NeverFinished+gaps.NotARuntime+
		gaps.AssignedAsArrival+gaps.Unidentified != 0 || gaps.Partial {
		t.Errorf("the export read %+v; want every job and no gap", gaps)
	}

	if !reflect.DeepEqual(exported, trace) {
		for i := range min(len(exported.Arrivals), len(trace.Arrivals)) {
			if !reflect.DeepEqual(exported.Arrivals[i], trace.Arrivals[i]) {
				t.Errorf("job %d exported as\n%+v\nreplayed as\n%+v", i+1, exported.Arrivals[i], trace.Arrivals[i])
			}
		}

		t.Fatalf("the exported trace of %d jobs differs from the %d replayed", len(exported.Arrivals),
			len(trace.Arrivals))
	}

	if len(hosts) != len(fleet.Hosts) {
		t.Fatalf("exported %d hosts, want %d", len(hosts), len(fleet.Hosts))
	}

	for i, h := range hosts {
		want := fleet.Hosts[i]
		if h.Name != want.Name || h.VCPU != want.VCPU || h.Memory != want.Memory || h.Site != want.Site ||
			h.Provider != config.ProviderSimulated {
			t.Errorf("host %d exported as %+v; want %+v on the simulated backend", i, h, want)
		}
	}
}

// EVERY ADVERSARIAL WORKLOAD KEEPS EVERY PROOF at a small size: every job
// recorded, started and finished, and no host or the deployment overcommitted.
func TestTheAdversarialWorkloadsKeepEveryProof(t *testing.T) {
	small := twoHosts(config.PlacementPack)

	for _, c := range []struct {
		name  string
		fleet Fleet
		trace Trace
	}{
		{"burst", small, Burst(1, Params{Jobs: 30})},
		{"idle-backlog", small, IdleBacklog(2, Params{Jobs: 30})},
		{"quiet-beside-saturated", small, QuietBesideSaturated(3, Params{Jobs: 30})},
		{"wide-shape-mix", WideFleet(), WideShapeMix(4, WideParams{Params: Params{Jobs: 30}, Fleet: WideFleet()})},
	} {
		t.Run(c.name, func(t *testing.T) {
			report := Run(t, c.fleet, c.trace, Options{})
			keep(t, "adversarial-"+c.name, report)

			if len(report.Missing) != 0 || len(report.Unstarted) != 0 || len(report.Unfinished) != 0 {
				t.Errorf("missing %v, never started %v, never finished %v", report.Missing, report.Unstarted,
					report.Unfinished)
			}

			if len(report.Records) != len(c.trace.Arrivals) {
				t.Errorf("recorded %d jobs for a trace of %d", len(report.Records), len(c.trace.Arrivals))
			}

			if len(report.Violations) != 0 {
				t.Errorf("the ledger proves an overcommit: %v", report.Violations)
			}
		})
	}
}

// A HORIZON ENDS THE REPLAY THERE: a job that arrived by it and had not
// started is still queued, with its wait kept out of every percentile, and a
// job arriving after it is outside the window, neither missing nor queued.
func TestAHorizonCountsTheJobsStillQueued(t *testing.T) {
	trace := IdleBacklog(9, Params{Jobs: 24})
	last := trace.Arrivals[len(trace.Arrivals)-1]

	// One more, an hour after the horizon.
	late := last
	late.At = last.At.Add(2 * time.Hour)
	trace.Arrivals = append(trace.Arrivals, late)
	trace.Normalize()

	horizon := last.At.Add(5 * time.Minute)
	report := Run(t, twoHosts(config.PlacementPack), trace, Options{Until: horizon})
	keep(t, "horizon", report)

	if report.LaterArrivals != 1 || len(report.Missing) != 0 {
		t.Errorf("after the horizon: %d later arrivals, missing %v; want the one late job outside the window",
			report.LaterArrivals, report.Missing)
	}

	// Two 16 vCPU hosts hold eight to sixteen of these jobs at a time, each
	// about ten minutes long, so five minutes in the first of them are running
	// and the rest wait.
	if len(report.StillQueued) == 0 || report.LongestQueued <= 0 {
		t.Fatalf("no job is still queued at the horizon (%d started); the backlog cannot have drained",
			len(report.Records)-len(report.Unstarted))
	}

	started := 0
	for i := range report.Records {
		if !report.Records[i].StartedAt.IsZero() {
			started++
		}
	}

	if got := report.QueueWaitBy(func(*Record) string { return "all" })["all"].Count; got != started ||
		started+len(report.StillQueued) != len(trace.Arrivals)-1 {
		t.Errorf("the percentiles cover %d jobs, %d started, %d still queued, of %d in the window", got,
			started, len(report.StillQueued), len(trace.Arrivals)-1)
	}
}

// A PURCHASE BILLET REFUSES IS COUNTED, per tier: a backlog larger than the
// fleet has offers acknowledged unacquired, and a fleet with room for every
// job refuses none.
func TestRefusedPurchasesAreCounted(t *testing.T) {
	crowded := Run(t, twoHosts(config.PlacementPack), IdleBacklog(5, Params{Jobs: 24}), Options{})

	refused := 0
	for _, n := range crowded.Declined {
		refused += n
	}

	if refused == 0 {
		t.Errorf("a backlog of 24 jobs on a fleet holding 8 refused no purchase: %v", crowded.Declined)
	}

	// AND UNDER DIRECT ASSIGNMENT, where a refusal is an assignment billet
	// acknowledged with no runner minted for it.
	direct := Run(t, twoHosts(config.PlacementPack), IdleBacklog(5, Params{Jobs: 24}),
		Options{DirectAssignment: true})

	refused = 0
	for _, n := range direct.Declined {
		refused += n
	}

	if refused == 0 {
		t.Errorf("a backlog of 24 directly assigned jobs on a fleet holding 8 refused no purchase: %v",
			direct.Declined)
	}

	roomy := Run(t, twoHosts(config.PlacementPack), Burst(6, Params{Jobs: 4}), Options{})
	for tier, n := range roomy.Declined {
		if n != 0 {
			t.Errorf("four jobs on a fleet with room for eight refused %d purchases on %s", n, tier)
		}
	}
}

// TWO COMPARISONS OF ONE TRACE GIVE ONE TABLE, so a policy comparison is a
// comparison and not a sample.
func TestAComparisonIsReproducible(t *testing.T) {
	trace := WideShapeMix(12, WideParams{Params: Params{Jobs: 30}, Fleet: WideFleet()})

	opts := Options{DirectAssignment: true}
	first := Compare(t, WideFleet(), trace, DefaultPolicies, opts).Markdown()
	second := Compare(t, WideFleet(), trace, DefaultPolicies, opts).Markdown()

	if first != second {
		t.Fatalf("two comparisons of one trace differ:\n%s\n%s", first, second)
	}

	for _, want := range []string{"| 32 vCPU | fair |", "| 32 vCPU | fill |", "| fair |", "| fill |"} {
		if !strings.Contains(first, want) {
			t.Errorf("the comparison does not have a row %q:\n%s", want, first)
		}
	}
}

// A FLEET DOCUMENT BECOMES A REPLAYABLE FLEET, pins included, and a remote
// backend is refused by name rather than modelled as a host.
func TestAFleetDocumentBecomesAReplayableFleet(t *testing.T) {
	doc := jobtrace.Fleet{
		Hosts: []jobtrace.Host{
			{Name: "ubuntu-01", VCPU: 180, Memory: 480 * config.GiB, Site: "home", Provider: config.ProviderFirecracker},
			{Name: "macos-01", VCPU: 12, Memory: 64 * config.GiB, Provider: config.ProviderTart},
		},
		Tiers: []jobtrace.Tier{
			{Label: "billet-8vcpu", VCPU: 8, Memory: 32 * config.GiB},
			{Label: "billet-macos", VCPU: 6, Memory: 24 * config.GiB, Node: "macos-01"},
		},
		MaxVCPU: 192, MaxMemory: 544 * config.GiB, AdmissionOrder: config.AdmissionFill,
	}

	f, err := FleetFromDocument(doc)
	if err != nil {
		t.Fatalf("FleetFromDocument: %v", err)
	}

	want := Fleet{
		Hosts: []Host{{Name: "ubuntu-01", VCPU: 180, Memory: 480 * config.GiB, Site: "home"},
			{Name: "macos-01", VCPU: 12, Memory: 64 * config.GiB}},
		Tiers: []TierShape{{Label: "billet-8vcpu", VCPU: 8, Memory: 32 * config.GiB},
			{Label: "billet-macos", VCPU: 6, Memory: 24 * config.GiB, Node: "macos-01"}},
		MaxVCPU: 192, MaxMemory: 544 * config.GiB, AdmissionOrder: config.AdmissionFill,
	}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("the fleet is\n%+v\nwant\n%+v", f, want)
	}

	doc.Hosts = append(doc.Hosts, jobtrace.Host{Name: "cloud", VCPU: 64, Memory: 256 * config.GiB,
		Provider: config.ProviderEC2})
	if _, err := FleetFromDocument(doc); err == nil || !strings.Contains(err.Error(), `host "cloud" runs ec2`) {
		t.Errorf("a fleet with an EC2 host gave %v; want it refused naming the host", err)
	}
}

// THE POLICY COMPARISON AT A RECORD'S SIZE, run by hand: BILLET_REPLAY_COMPARE=
// wide replays WideShapeMix at BILLET_REPLAY_JOBS (default 2000) on WideFleet,
// and =file replays the trace and fleet `billet jobs export` wrote, named by
// BILLET_REPLAY_TRACE and BILLET_REPLAY_FLEET. The table goes to the test log
// and, with -replay.out, to a file beside the records.
//
// GITHUB.COM'S PROTOCOL BY DEFAULT, direct assignment, which is the one the
// admission order judges by: under GHES's offers a tier's unmet demand is the
// offers it did not acquire, which no reconciliation counts, so no tier ever
// waits and fair cannot be told from fill (measured 2026-10-25, the first wide
// run). BILLET_REPLAY_PROTOCOL=offer replays the offers anyway.
func TestComparePolicies(t *testing.T) {
	var (
		fleet Fleet
		trace Trace
		opts  Options
	)

	switch os.Getenv("BILLET_REPLAY_COMPARE") {
	case "":
		t.Skip("BILLET_REPLAY_COMPARE is not set; this is the record-size comparison, run by hand")
	case "wide":
		jobs := 2000
		if n := os.Getenv("BILLET_REPLAY_JOBS"); n != "" {
			if _, err := fmt.Sscan(n, &jobs); err != nil || jobs <= 0 {
				t.Fatalf("BILLET_REPLAY_JOBS=%q is not a positive count", n)
			}
		}

		fleet = WideFleet()
		trace = WideShapeMix(1, WideParams{Params: Params{Jobs: jobs}, Fleet: fleet})
	case "file":
		trace, fleet = readExported(t, os.Getenv("BILLET_REPLAY_TRACE"), os.Getenv("BILLET_REPLAY_FLEET"))
		opts.SettleWithin = 5 * time.Minute
	default:
		t.Fatalf("BILLET_REPLAY_COMPARE=%q; want wide or file", os.Getenv("BILLET_REPLAY_COMPARE"))
	}

	switch os.Getenv("BILLET_REPLAY_PROTOCOL") {
	case "", "direct":
		opts.DirectAssignment = true
	case "offer":
	default:
		t.Fatalf("BILLET_REPLAY_PROTOCOL=%q; want direct or offer", os.Getenv("BILLET_REPLAY_PROTOCOL"))
	}

	opts.Log = discardLogger()

	table := Compare(t, fleet, trace, DefaultPolicies, opts).Markdown()
	t.Log("\n" + table)

	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			t.Fatal(err)
		}

		name := "compare-" + os.Getenv("BILLET_REPLAY_COMPARE") + ".md"
		if err := os.WriteFile(filepath.Join(*outDir, name), []byte(table), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readExported reads a trace and its fleet as `billet jobs export` wrote them.
func readExported(t *testing.T, tracePath, fleetPath string) (Trace, Fleet) {
	t.Helper()

	if tracePath == "" || fleetPath == "" {
		t.Fatal("BILLET_REPLAY_COMPARE=file needs BILLET_REPLAY_TRACE and BILLET_REPLAY_FLEET")
	}

	tf, err := os.Open(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	defer tf.Close()

	trace, err := ReadTrace(tf)
	if err != nil {
		t.Fatal(err)
	}

	ff, err := os.Open(fleetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ff.Close()

	doc, err := jobtrace.ReadFleet(ff)
	if err != nil {
		t.Fatal(err)
	}

	fleet, err := FleetFromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}

	return trace, fleet
}

// discardLogger keeps a record-size replay's diagnostics out of the test log,
// which would otherwise hold millions of lines.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
