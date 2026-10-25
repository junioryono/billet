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

// A REPLAYED DEPLOYMENT EXPORTS BACK ITS OWN TRACE AND FLEET, under either
// protocol: the ledger the replay wrote is read as an operator's `billet jobs
// export` reads one, and gives back every job as it arrived, ran and ended,
// its name or its lack of one, and every host as it registered.
func TestTheLedgerExportRoundTrips(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "offers", true: "direct-assignment"}[direct], func(t *testing.T) {
			ledgerRoundTrip(t, direct)
		})
	}
}

// ledgerRoundTrip replays a trace under one protocol and exports it back.
func ledgerRoundTrip(t *testing.T, direct bool) {
	t.Helper()

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

	// A BACKLOG LAUNCHED TOGETHER: eight 4 vCPU jobs fill both hosts, then four
	// 2 vCPU jobs arrive at one instant and wait; each 4 vCPU completion frees
	// room for two of them, which one reconciliation launches in one step, so
	// billet dates both identically and the export orders them by the identities
	// it allocated them, downwards under direct assignment.
	full := trace.Arrivals[len(trace.Arrivals)-1].At.Add(time.Hour)
	for i := range 8 {
		trace.Arrivals = append(trace.Arrivals, Arrival{
			At: full, Tier: "billet-4vcpu", Repository: "api", RunID: int64(950 + i),
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
			Duration:    Duration(time.Duration(10+i) * time.Minute),
		})
	}

	for i := range 4 {
		trace.Arrivals = append(trace.Arrivals, Arrival{
			At: full.Add(time.Second), Tier: "billet-2vcpu", Repository: "api", RunID: int64(970 + i),
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
			Duration:    Duration(time.Duration(1+i) * time.Minute),
		})
	}

	trace.Normalize()

	// EVERY FIFTH JOB HAS NO NAME, which must come back as none.
	for i := range trace.Arrivals {
		if i%5 != 0 {
			trace.Arrivals[i].Job = fmt.Sprintf("build (shard %d)", i%3)
		}
	}

	fleet := twoHosts(config.PlacementPack)

	var (
		exported jobtrace.Trace
		hosts    []jobtrace.Host
		gaps     alloc.ExportGaps
	)

	Run(t, fleet, trace, Options{DirectAssignment: direct, AfterRun: func(a *alloc.Allocator) {
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

	if gaps.Read != len(trace.Arrivals) || gaps.NeverStarted+gaps.NeverCompleted+gaps.NotARuntime+
		gaps.UndeclaredTier+gaps.AssignedAsArrival+gaps.Unidentified != 0 || gaps.Partial {
		t.Errorf("the export read %+v; want every job and no gap", gaps)
	}

	// THE HARNESS'S NANOSECOND: two events at one instant are dated a
	// nanosecond apart (Clock.Step), so a job started in the same step as
	// another is recorded a nanosecond late while its completion is where the
	// trace put it, and its runtime reads that much short. Within a microsecond
	// a duration is the trace's; anything more is the export's.
	for i := range min(len(exported.Arrivals), len(trace.Arrivals)) {
		got, want := &exported.Arrivals[i].Duration, trace.Arrivals[i].Duration
		if d := time.Duration(*got - want); d != 0 && d > -time.Microsecond && d < time.Microsecond {
			*got = want
		}
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
		{"quiet-beside-saturated", small, QuietBesideSaturated(3, QuietParams{Params: Params{Jobs: 30},
			Fleet: small})},
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

	// A JOB A RUNNER HAS TAKEN AND NOT YET STARTED IS QUEUED, NOT LOST: under
	// direct assignment ten seconds in, every runner minted is still in its
	// thirty-second boot, and its pool lease names no job yet.
	booting := Run(t, twoHosts(config.PlacementPack), IdleBacklog(9, Params{Jobs: 24}),
		Options{DirectAssignment: true, Until: DefaultStart.Add(10 * time.Second)})

	if len(booting.Missing) != 0 || len(booting.StillQueued) == 0 {
		t.Errorf("inside the boot: missing %v, %d still queued; want every arrived job still queued",
			booting.Missing, len(booting.StillQueued))
	}
}

// A JOB BILLET REFUSES TO BUY FOR IS COUNTED ONCE, per tier, under either
// protocol: a backlog larger than the fleet has jobs refused, never more than
// it has jobs however often each is offered again, and a fleet with room for
// every job refuses none.
func TestRefusedPurchasesAreCounted(t *testing.T) {
	for _, direct := range []bool{false, true} {
		opts := Options{DirectAssignment: direct}

		crowded := Run(t, twoHosts(config.PlacementPack), IdleBacklog(5, Params{Jobs: 24}), opts)

		refused := 0
		for _, n := range crowded.Declined {
			refused += n
		}

		if refused == 0 || refused > 24 {
			t.Errorf("direct %v: a backlog of 24 jobs on a fleet holding 8 to 16 refused %d jobs (%v); want some, "+
				"each counted once", direct, refused, crowded.Declined)
		}

		roomy := Run(t, twoHosts(config.PlacementPack), Burst(6, Params{Jobs: 4}), opts)
		for tier, n := range roomy.Declined {
			if n != 0 {
				t.Errorf("direct %v: four jobs on a fleet with room for eight refused %d on %s", direct, n, tier)
			}
		}
	}
}

// FAIR AND FILL ARE TOLD APART BY THE COMPARISON: a whole-host job waiting
// behind a stream of small ones on one host starts once the room it held
// frees under fair, and only when the stream ends under fill.
func TestFairHoldsTheLineWhereFillDoesNot(t *testing.T) {
	fleet := Fleet{
		Hosts: []Host{{Name: "only", VCPU: 16, Memory: 64 * config.GiB}},
		Tiers: []TierShape{
			{Label: "billet-4vcpu", VCPU: 4, Memory: 16 * config.GiB},
			{Label: "billet-16vcpu", VCPU: 16, Memory: 64 * config.GiB},
		},
		MaxVCPU: 16, MaxMemory: 64 * config.GiB,
	}

	// Four 4 vCPU jobs fill the host, the wide job arrives a minute later, and
	// a fresh 4 vCPU job arrives every two minutes for two hours, each running
	// eight minutes: under fill the stream keeps every fragment taken.
	var trace Trace

	job := func(at time.Time, tier string, d time.Duration) Arrival {
		return Arrival{At: at, Tier: tier, Repository: "web", Duration: Duration(d),
			WorkflowRef: "acme/web/.github/workflows/ci.yml@refs/heads/main"}
	}

	for i := range 4 {
		trace.Arrivals = append(trace.Arrivals, job(DefaultStart.Add(time.Duration(i)*time.Second), "billet-4vcpu",
			8*time.Minute))
	}

	trace.Arrivals = append(trace.Arrivals, job(DefaultStart.Add(time.Minute), "billet-16vcpu", 5*time.Minute))

	for i := range 60 {
		trace.Arrivals = append(trace.Arrivals, job(DefaultStart.Add(time.Duration(2*i+2)*time.Minute),
			"billet-4vcpu", 8*time.Minute))
	}

	trace.Normalize()

	c := Compare(t, fleet, trace, DefaultPolicies, Options{DirectAssignment: true})
	t.Log("\n" + c.Markdown())

	// UNDER FAIR IT STARTS WHEN THE FIRST FOUR END, eight minutes in, plus the
	// boot: the small jobs that arrived meanwhile were held. UNDER FILL THE
	// STREAM BACKFILLS every fragment, and it starts only once the stream ends,
	// two hours in.
	fair, fill := c.Reports[0].QueueWaitByShape()[16], c.Reports[1].QueueWaitByShape()[16]
	if fair.Count != 1 || fill.Count != 1 || fair.Max > 10*time.Minute || fill.Max < time.Hour {
		t.Errorf("the wide job waited %s under fair and %s under fill; want under ten minutes and over an hour",
			fair.Max, fill.Max)
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

	f, dropped, err := FleetFromDocument(doc)
	if err != nil || len(dropped) != 0 {
		t.Fatalf("FleetFromDocument: %v (dropped %v)", err, dropped)
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

	// A REMOTE HOST IS LEFT OUT AND NAMED, and a tier pinned to one is refused.
	doc.Hosts = append(doc.Hosts, jobtrace.Host{Name: "cloud", VCPU: 64, Memory: 256 * config.GiB,
		Provider: config.ProviderEC2})

	f, dropped, err = FleetFromDocument(doc)
	if err != nil || !reflect.DeepEqual(dropped, []string{"cloud (ec2)"}) || !reflect.DeepEqual(f, want) {
		t.Errorf("a fleet with an EC2 host gave %+v, dropped %v, %v; want the EC2 host left out and named", f,
			dropped, err)
	}

	doc.Tiers = append(doc.Tiers, jobtrace.Tier{Label: "billet-cloud", VCPU: 8, Memory: 32 * config.GiB,
		Node: "cloud"})
	if _, _, err := FleetFromDocument(doc); err == nil || !strings.Contains(err.Error(), `tier "billet-cloud"`) {
		t.Errorf("a tier pinned to the EC2 host gave %v; want it refused by name", err)
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

	fleet, dropped, err := FleetFromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}

	if len(dropped) > 0 {
		t.Logf("left out of the fleet, as remote backends the simulated one cannot model: %v", dropped)
	}

	return trace, fleet
}

// discardLogger keeps a record-size replay's diagnostics out of the test log,
// which would otherwise hold millions of lines.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
