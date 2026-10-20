package fleetops

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// retireEveryJob finishes lease and deletes the detail of every job in the
// ledger at stateDir, as the control plane does past its retention.
func retireEveryJob(t *testing.T, stateDir, lease string) {
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
	tier := config.Tier{Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04"}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	retireWith(t, a, lease)
}

// retireWith finishes lease and deletes the detail of every job a can reach.
func retireWith(t *testing.T, a *alloc.Allocator, lease string) {
	t.Helper()

	held, err := a.Lease(t.Context(), lease)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if err := a.Release(t.Context(), held.ID, held.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	ids, err := a.JobsWithRetirableDetail(t.Context(), time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("JobsWithRetirableDetail: %v", err)
	}
	got, err := a.RetireJobDetail(t.Context(), ids)
	if err != nil || got.Jobs != 1 {
		t.Fatalf("RetireJobDetail retired %+v (%v), want the one job", got, err)
	}
}

// A RETIRED JOB SAYS ITS DETAIL WAS RETIRED, in the text report, the JSON
// record and the page, rather than that its series, destinations or guest
// report were never measured; its usage summary is still shown.
func TestARetiredJobSaysItsDetailWasRetired(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)
	usage := &alloc.JobUsage{
		Source: alloc.UsageSourceHost, Unmeasured: []string{alloc.UsageEnergy},
		Samples: 60, IntervalMillis: 1000, WindowMillis: 60_000,
		CPUUserMicros: 1_000_000, MemoryPeakBytes: 1 << 30, NetRxBytes: 1 << 20,
		Destinations: &alloc.JobDestinations{Destinations: []alloc.JobDestination{
			{Addr: "140.82.112.3", SentBytes: 1, ReceivedBytes: 2, Connections: 1}}},
	}
	lease := seedJob(t, stateDir, jobSeed{requestID: 77, job: &forgedJob, usage: usage,
		series: &alloc.UsageSeries{Codec: alloc.UsageSeriesCodec, Data: []byte{0, 1, 2}}})
	retireEveryJob(t, stateDir, lease)

	text := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})
	for _, want := range []string{"detail     retired ", "past server.job_detail_retention",
		"destinations retired ", "usage      measured by the host epyc-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "destinations not measured") {
		t.Errorf("a retired job's destinations read as never measured:\n%s", text)
	}

	raw := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--json", lease}); err != nil {
			t.Errorf("billet jobs show --json: %v", err)
		}
	})
	var record struct {
		DetailRetiredAt string `json:"detail_retired_at"`
		Usage           struct {
			SeriesUnavailable string `json:"series_unavailable"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatalf("decode the record: %v\n%s", err, raw)
	}
	if record.DetailRetiredAt == "" || !strings.HasPrefix(record.Usage.SeriesUnavailable, "retired ") {
		t.Errorf("the record says detail_retired_at %q and series_unavailable %q; want both to say retired",
			record.DetailRetiredAt, record.Usage.SeriesUnavailable)
	}

	page := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--html", lease}); err != nil {
			t.Errorf("billet jobs show --html: %v", err)
		}
	})
	if strings.Count(page, "past server.job_detail_retention") < 2 {
		t.Errorf("the page does not say the job's detail and its destinations were retired")
	}
}

// A RETIREMENT THAT COMMITS WHILE A REPORT IS READ IS IN THE REPORT. The first
// build read the job unretired; the detail is retired before the marker is read
// again, and what is printed is the rebuilt report, which says so, rather than
// the first, which would show the detail as kept or missing without a reason.
// Not parallel: it sets the package's afterJobShown.
func TestARetirementDuringAReportIsInTheReport(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)
	usage := &alloc.JobUsage{Source: alloc.UsageSourceHost, Unmeasured: []string{alloc.UsageEnergy},
		Samples: 60, IntervalMillis: 1000, WindowMillis: 60_000}
	lease := seedJob(t, stateDir, jobSeed{requestID: 78, job: &forgedJob, usage: usage,
		series: &alloc.UsageSeries{Codec: alloc.UsageSeriesCodec, Data: []byte{0, 1, 2}}})

	afterJobShown = func(a *alloc.Allocator) {
		afterJobShown = nil
		retireWith(t, a, lease)
	}
	t.Cleanup(func() { afterJobShown = nil })

	text := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})
	if !strings.Contains(text, "detail     retired ") {
		t.Errorf("a report read across a retirement does not say the detail was retired:\n%s", text)
	}
}
