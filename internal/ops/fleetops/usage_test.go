package fleetops

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

const usageDeployment = "0123456789abcdef0123456789abcdef"

// finishUsageLeases ends n leases on a ledger it opens and closes itself, which
// is what a control plane's restart is to the ledger: every number is taken from
// what the previous process committed, never from anything it held in memory.
func finishUsageLeases(t *testing.T, stateDir string, n int) []string {
	t.Helper()

	seedLedger(t, stateDir)

	// What the control plane that finished these minted on its first start.
	if _, err := state.DeploymentID(stateDir); err != nil {
		t.Fatalf("mint the deployment identity: %v", err)
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the ledger: %v", err)
		}
	}()

	tier := config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	finished := make([]string, 0, n)

	for range n {
		lease, err := a.Reserve(t.Context(), tier.Label)
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}

		finished = append(finished, lease.ID)
	}

	return finished
}

// exportUsage runs the command as an exporter would and reads its JSON lines.
func exportUsage(t *testing.T, cfgPath, since string, limit string) []usageRecord {
	t.Helper()

	env, out := bufferedEnv()
	args := []string{"export", "--config", cfgPath, "--json", "--limit", limit}
	if since != "" {
		args = append(args, "--since", since)
	}

	if err := Usage(t.Context(), env, args); err != nil {
		t.Fatalf("billet usage export: %v", err)
	}

	var records []usageRecord

	lines := bufio.NewScanner(out)
	for lines.Scan() {
		var r usageRecord
		if err := json.Unmarshal(lines.Bytes(), &r); err != nil {
			t.Fatalf("a line is not a record: %v\n%s", err, lines.Text())
		}

		records = append(records, r)
	}

	return records
}

// A LEASE APPEARS EXACTLY ONCE ACROSS ANY SEQUENCE OF CURSORS, whatever size the
// pages are and however many control planes wrote them. Leases finish, an exporter
// reads part of them, the ledger closes and opens again, more finish, and the
// exporter resumes from the cursor it kept: everything is seen, once, in the order
// it finished.
func TestALeaseIsExportedExactlyOnceAcrossARestart(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	want := finishUsageLeases(t, stateDir, 3)

	first := exportUsage(t, cfg, "", "2")
	if len(first) != 2 {
		t.Fatalf("a page of 2 printed %d records", len(first))
	}

	want = append(want, finishUsageLeases(t, stateDir, 2)...)

	var got []string
	for _, r := range first {
		got = append(got, r.LeaseID)
	}

	cursor := first[len(first)-1].Cursor

	for {
		page := exportUsage(t, cfg, cursor, "2")
		if len(page) == 0 {
			break
		}

		for _, r := range page {
			got = append(got, r.LeaseID)
		}

		cursor = page[len(page)-1].Cursor
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the exporter saw\n%v\nwant each lease once, in the order it finished\n%v", got, want)
	}

	if extra := exportUsage(t, cfg, cursor, "2"); len(extra) != 0 {
		t.Fatalf("an exporter at the end was handed %d records again", len(extra))
	}
}

type stubUsageLedger struct {
	place state.UsagePlace
	err   error
}

func (s stubUsageLedger) UsageRecordAt(context.Context, int64) (state.UsagePlace, error) {
	return s.place, s.err
}

// A CURSOR THAT CANNOT BE TRUSTED IS REFUSED BY NAME, because the alternative is
// silently skipping what a restored ledger numbers over it, and a read that failed
// is reported as itself and never as a missing record.
func TestAResumeRefusesACursorThisLedgerCannotHonour(t *testing.T) {
	t.Parallel()

	const (
		lease = "feedfacefeedfacefeedfacefeedface"
		mark  = "0123456789abcdef"
	)

	good := usageCursor{usageDeployment, 7, lease, mark}.String()
	readFailed := errors.New("the database went away")

	for name, tc := range map[string]struct {
		since  string
		ledger stubUsageLedger
		want   string
		wantIs error
	}{
		"not a cursor":       {since: "yesterday", want: "not a cursor this billet wrote"},
		"a later format":     {since: strings.Replace(good, "u1.", "u2.", 1), want: "not a cursor this billet wrote"},
		"a zero place":       {since: "u1." + usageDeployment + ".0." + lease + "." + mark, want: "not a cursor this billet wrote"},
		"no mark":            {since: "u1." + usageDeployment + ".7." + lease, want: "not a cursor this billet wrote"},
		"another deployment": {since: usageCursor{"ffffffffffffffffffffffffffffffff", 7, lease, mark}.String(), want: "deployment ffffffffffffffffffffffffffffffff's cursor"},
		"restored, shorter": {
			since: good, ledger: stubUsageLedger{err: state.ErrNoUsageRecord},
			want: "holds no record 7",
		},
		"restored, numbered over": {
			since: good, ledger: stubUsageLedger{place: state.UsagePlace{
				LeaseID: "badbadbadbadbadbadbadbadbadbadba", Mark: mark,
			}},
			want: "is not the one --since names",
		},
		// THE LEASE AGREES AND THE MARK DOES NOT: a restored ledger gave this lease this
		// number again, and may have given another a lower one that the exporter would
		// skip.
		"restored, the same lease numbered again": {
			since: good, ledger: stubUsageLedger{place: state.UsagePlace{LeaseID: lease, Mark: "fedcba9876543210"}},
			want: "is not the one --since names",
		},
		"could not read": {since: good, ledger: stubUsageLedger{err: readFailed}, wantIs: readFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			after, err := resumeUsage(t.Context(), tc.ledger, usageDeployment, tc.since)
			if err == nil {
				t.Fatalf("resumed at %d, want a refusal", after)
			}

			if tc.wantIs != nil {
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("got %v, want the read's own failure", err)
				}
				if strings.Contains(err.Error(), "restored") {
					t.Fatalf("a failed read was reported as a restored ledger: %v", err)
				}

				return
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to say %q", err, tc.want)
			}
		})
	}

	after, err := resumeUsage(t.Context(), stubUsageLedger{place: state.UsagePlace{LeaseID: lease, Mark: mark}},
		usageDeployment, good)
	if err != nil || after != 7 {
		t.Fatalf("a cursor this ledger holds resumed at %d, %v; want 7", after, err)
	}

	after, err = resumeUsage(t.Context(), stubUsageLedger{err: readFailed}, usageDeployment, "")
	if err != nil || after != 0 {
		t.Fatalf("no cursor resumed at %d, %v; want the beginning without asking the ledger", after, err)
	}
}

func usageConfig(t *testing.T, weight config.MinuteWeight) *config.Config {
	t.Helper()

	return &config.Config{Tiers: []config.Tier{
		{Label: "weighted", VCPU: 8, MinuteWeight: weight},
		{Label: "plain", VCPU: 4},
	}}
}

func knownUsageRow() *state.UsageRow {
	return &state.UsageRow{
		Sequence: 9, Mark: "0123456789abcdef", LeaseID: "feedface", Tier: "plain", Target: "acme", Repository: "acme/api",
		Provider: string(config.ProviderEC2), InstanceType: "c7i.xlarge", Site: "us-west-2",
		VCPU: 4, Memory: 16 << 30, PriceMicrosPerHour: 360_000,
		BoughtAt:   "2026-11-03T10:00:00.000000000Z",
		StartedAt:  "2026-11-03T10:01:00.000000000Z",
		ResultAt:   "2026-11-03T10:03:00.500000000Z",
		FinishedAt: "2026-11-03T10:04:00.000000000Z",
		Result:     "succeeded", Conclusion: "done",
	}
}

// A FULLY KNOWN LEASE reports every number, and each is the arithmetic the format
// states: a part of a minute bills the minute, a 4 vCPU runner counts double, and
// the machine's cost runs for as long as the lease held it.
func TestAKnownLeaseReportsEveryNumber(t *testing.T) {
	t.Parallel()

	r := newUsageRecord(usageConfig(t, 0), usageDeployment, knownUsageRow())

	if len(r.Unknown) != 0 {
		t.Errorf("a fully known lease says %v is unknown", r.Unknown)
	}
	if r.Ownership != usageBought {
		t.Errorf("ownership = %q, want %q", r.Ownership, usageBought)
	}
	if want := "u1." + usageDeployment + ".9.feedface.0123456789abcdef"; r.Cursor != want {
		t.Errorf("cursor = %q, want %q", r.Cursor, want)
	}
	if r.VCPU == nil || *r.VCPU != 4 || r.MemoryBytes == nil || *r.MemoryBytes != 16<<30 {
		t.Errorf("shape = %v, %v", r.VCPU, r.MemoryBytes)
	}
	if r.Held.Seconds == nil || r.Held.Seconds.String() != "240.000" {
		t.Errorf("held = %v, want 240.000", r.Held.Seconds)
	}
	if r.Job.Seconds == nil || r.Job.Seconds.String() != "120.500" {
		t.Errorf("job = %v, want 120.500", r.Job.Seconds)
	}
	if r.BilledMinutes == nil || *r.BilledMinutes != 3 {
		t.Errorf("billed = %v, want 2m0.5s rounded up to 3", r.BilledMinutes)
	}
	if r.WeightSource != weightFromVCPU || r.MinuteWeight == nil || r.MinuteWeight.String() != "2" {
		t.Errorf("weight = %v from %q, want 2 from %q", r.MinuteWeight, r.WeightSource, weightFromVCPU)
	}
	if r.WeightedMinutes == nil || r.WeightedMinutes.String() != "6" {
		t.Errorf("weighted = %v, want 6", r.WeightedMinutes)
	}
	// $0.36 an hour for four minutes.
	if r.CostMicros == nil || *r.CostMicros != 24_000 {
		t.Errorf("cost = %v, want 24000", r.CostMicros)
	}
}

// A JOB'S LENGTH IS GITHUB'S WHERE GITHUB SAID BOTH ENDS. The times here are a real job's
// (2026-11-04): GitHub ran it for 243 seconds and billet received its JobStarted 47
// seconds in, so billet's own pair bills a minute less than the job took.
func TestAJobIsBilledOnGitHubsClockWhenItGaveBothEnds(t *testing.T) {
	t.Parallel()

	measured := func() *state.UsageRow {
		row := knownUsageRow()
		row.StartedAt = "2026-11-04T09:49:14.176805454Z"
		row.ResultAt = "2026-11-04T09:52:30.959812791Z"
		row.GitHubRunnerAssignedAt = "2026-11-04T09:48:27.000000000Z"
		row.GitHubFinishedAt = "2026-11-04T09:52:30.000000000Z"

		return row
	}

	r := newUsageRecord(usageConfig(t, 0), usageDeployment, measured())
	if r.JobClock != jobClockGitHub || r.Job.Seconds == nil || r.Job.Seconds.String() != "243.000" {
		t.Errorf("job = %v on %q, want 243.000 on %q", r.Job.Seconds, r.JobClock, jobClockGitHub)
	}
	if r.Job.From != "2026-11-04T09:48:27.000000000Z" || r.Job.To != "2026-11-04T09:52:30.000000000Z" {
		t.Errorf("job ran %s to %s, want GitHub's times", r.Job.From, r.Job.To)
	}
	if r.BilledMinutes == nil || *r.BilledMinutes != 5 {
		t.Errorf("billed = %v, want 243 s rounded up to 5", r.BilledMinutes)
	}

	// ONE END OF GITHUB'S IS NOT A LENGTH. Across two clocks a difference means nothing,
	// so billet's own pair stands in, and the record says whose it is.
	for name, mutate := range map[string]func(*state.UsageRow){
		"no finish from GitHub": func(r *state.UsageRow) { r.GitHubFinishedAt = "" },
		"no start from GitHub":  func(r *state.UsageRow) { r.GitHubRunnerAssignedAt = "" },
	} {
		row := measured()
		mutate(row)

		r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
		if r.JobClock != jobClockBillet || r.Job.Seconds == nil || r.Job.Seconds.String() != "196.783" {
			t.Errorf("%s: job = %v on %q, want billet's 196.783", name, r.Job.Seconds, r.JobClock)
		}
	}

	// GITHUB'S TWO TIMES ARE A JOB BILLET NEVER SAW START, and that is a known length.
	row := measured()
	row.StartedAt = ""

	r = newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.BilledMinutes == nil || *r.BilledMinutes != 5 || r.JobClock != jobClockGitHub {
		t.Errorf("billed %v on %q with no JobStarted received", r.BilledMinutes, r.JobClock)
	}

	// NOT A CLOCK WITH NOTHING TO READ, which a consumer has to be able to tell.
	row = knownUsageRow()
	row.StartedAt, row.ResultAt, row.Result = "", "", ""

	r = newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.JobClock != "" || r.Unknown["job_clock"] == "" {
		t.Errorf("a lease with no job has the clock %q, unknown %q", r.JobClock, r.Unknown["job_clock"])
	}
}

// A TIER'S OWN WEIGHT TAKES PRECEDENCE, exactly, in thousandths; and an unset one
// falls to the vCPU charged, not the vCPU the tier says today.
func TestATiersWeightCountsItsMinutes(t *testing.T) {
	t.Parallel()

	row := knownUsageRow()
	row.Tier = "weighted"

	r := newUsageRecord(usageConfig(t, 1500), usageDeployment, row)
	if r.WeightSource != weightFromTier || r.MinuteWeight.String() != "1.5" || r.WeightedMinutes.String() != "4.5" {
		t.Errorf("weight %v from %q, %v weighted; want 1.5 from the tier and 4.5", r.MinuteWeight,
			r.WeightSource, r.WeightedMinutes)
	}

	r = newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.WeightSource != weightFromVCPU || r.MinuteWeight.String() != "2" {
		t.Errorf("weight %v from %q, want 2 from the 4 vCPU the lease was charged, not the tier's 8",
			r.MinuteWeight, r.WeightSource)
	}
}

// A TIER THAT IS GONE MAY HAVE NAMED A WEIGHT, so the arithmetic stands in under a source
// that says so rather than reading as a tier that named none.
func TestATierNoLongerDeclaredSaysItsWeightIsAStandIn(t *testing.T) {
	t.Parallel()

	row := knownUsageRow()
	row.Tier = "renamed-since"

	r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.WeightSource != weightFromVCPUTierGone || r.MinuteWeight == nil || r.MinuteWeight.String() != "2" {
		t.Errorf("weight %v from %q, want 2 from %q", r.MinuteWeight, r.WeightSource, weightFromVCPUTierGone)
	}
}

// UNKNOWN STAYS UNKNOWN: a value the ledger does not hold is absent and named with
// its reason, and is never a zero a billing system would add up.
func TestAFieldTheLedgerDoesNotKnowIsEmptyAndSaysWhy(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mutate func(*state.UsageRow)
		fields []string
		check  func(*testing.T, *usageRecord)
	}{
		"no target": {
			mutate: func(r *state.UsageRow) { r.Target = "" }, fields: []string{"target"},
		},
		"no repository": {
			mutate: func(r *state.UsageRow) { r.Repository = "" }, fields: []string{"repository"},
		},
		"no backend": {
			mutate: func(r *state.UsageRow) { r.Provider = "" }, fields: []string{"provider", "ownership"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.Ownership != "" {
					t.Errorf("ownership = %q for a lease with no backend", r.Ownership)
				}
			},
		},
		"a newer binary's backend": {
			mutate: func(r *state.UsageRow) { r.Provider = "lunar-lander" }, fields: []string{"ownership"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.Provider != "lunar-lander" {
					t.Errorf("provider = %q, want the ledger's word kept", r.Provider)
				}
			},
		},
		"no shape": {
			mutate: func(r *state.UsageRow) { r.VCPU, r.Memory = 0, 0 },
			fields: []string{"vcpu", "memory_bytes", "minute_weight", "weight_source", "weighted_minutes"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.VCPU != nil || r.MemoryBytes != nil {
					t.Errorf("shape = %v, %v, want both absent and not zero", r.VCPU, r.MemoryBytes)
				}
				if r.MinuteWeight == nil {
					return
				}
				t.Errorf("weight = %v with no shape and no tier weight", r.MinuteWeight)
			},
		},
		"no purchase time": {
			mutate: func(r *state.UsageRow) { r.BoughtAt = "" },
			fields: []string{"held.from", "held.seconds", "cost_micros"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.Held.Seconds != nil || r.CostMicros != nil {
					t.Errorf("held %v, cost %v from no purchase time", r.Held.Seconds, r.CostMicros)
				}
			},
		},
		"a bought machine with no price": {
			mutate: func(r *state.UsageRow) { r.PriceMicrosPerHour = 0 },
			fields: []string{"price_micros_per_hour", "cost_micros"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.PriceMicrosPerHour != nil || r.CostMicros != nil {
					t.Errorf("price %v, cost %v: a zero price read as free", r.PriceMicrosPerHour, r.CostMicros)
				}
			},
		},
		"a job with no result": {
			mutate: func(r *state.UsageRow) { r.ResultAt, r.Result = "", "" },
			fields: []string{"job.to", "job.seconds", "billed_minutes", "weighted_minutes", "result"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.BilledMinutes != nil || r.WeightedMinutes != nil {
					t.Errorf("billed %v, weighted %v with no end to the job", r.BilledMinutes, r.WeightedMinutes)
				}
				if r.Held.Seconds == nil {
					t.Error("the lease's held time went missing with the job's")
				}
			},
		},
		"a result with no start": {
			mutate: func(r *state.UsageRow) { r.StartedAt = "" },
			fields: []string{"job.from", "job.seconds", "billed_minutes", "weighted_minutes"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.BilledMinutes != nil {
					t.Errorf("billed %v for a job billet never saw start", r.BilledMinutes)
				}
			},
		},
		"a clock that stepped back": {
			mutate: func(r *state.UsageRow) { r.ResultAt = "2026-11-03T09:00:00.000000000Z" },
			fields: []string{"job.seconds", "billed_minutes", "weighted_minutes"},
			check: func(t *testing.T, r *usageRecord) {
				t.Helper()
				if r.Job.Seconds != nil || r.BilledMinutes != nil {
					t.Errorf("job %v, billed %v from a negative length", r.Job.Seconds, r.BilledMinutes)
				}
			},
		},
		"a time that is not one": {
			mutate: func(r *state.UsageRow) { r.StartedAt = "soon" },
			fields: []string{"job.seconds", "billed_minutes"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			row := knownUsageRow()
			tc.mutate(row)

			r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
			for _, field := range tc.fields {
				if strings.TrimSpace(r.Unknown[field]) == "" {
					t.Errorf("%s does not say why it is unknown: %v", field, r.Unknown)
				}
			}
			if tc.check != nil {
				tc.check(t, &r)
			}
		})
	}
}

// A LEASE THAT RAN NO JOB BILLS A KNOWN ZERO, which is not the same as an unknown:
// it held capacity and no job started, and the record says both.
func TestALeaseThatRanNoJobBillsAKnownZero(t *testing.T) {
	t.Parallel()

	row := knownUsageRow()
	row.StartedAt, row.ResultAt, row.Result = "", "", ""

	r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.BilledMinutes == nil || *r.BilledMinutes != 0 {
		t.Fatalf("billed = %v, want a known 0", r.BilledMinutes)
	}
	if r.WeightedMinutes == nil || r.WeightedMinutes.String() != "0" {
		t.Errorf("weighted = %v, want 0", r.WeightedMinutes)
	}
	if !strings.Contains(r.Unknown["job.from"], "no job started") {
		t.Errorf("the record does not say no job started: %v", r.Unknown)
	}
	if r.Held.Seconds == nil {
		t.Error("the capacity the lease held is missing")
	}
}

// A MISSING START PROVES NOTHING ABOUT A LEASE THAT ENDED BADLY. A JobStarted arrives
// tens of seconds late, so a lease failed or disrupted before it arrived may have been
// running a job, and a known zero would be a minute given away that GitHub can later
// contradict.
func TestALeaseThatEndedBadlyWithNoStartIsNotAKnownZero(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*state.UsageRow){
		"failed":    func(r *state.UsageRow) { r.Conclusion = "failed" },
		"a reason":  func(r *state.UsageRow) { r.FailureReason = "launch-failed" },
		"disrupted": func(r *state.UsageRow) { r.Disruption = "node-lost" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			row := knownUsageRow()
			row.StartedAt, row.ResultAt, row.Result = "", "", ""
			mutate(row)

			r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
			if r.BilledMinutes != nil {
				t.Errorf("billed = %d, want unknown", *r.BilledMinutes)
			}
			if r.Unknown["billed_minutes"] == "" || r.WeightedMinutes != nil {
				t.Errorf("weighted %v, unknown %v", r.WeightedMinutes, r.Unknown)
			}
		})
	}
}

// THE RESULT IS UNKNOWN WHEREVER IT IS EMPTY, including beside a length GitHub's own times
// gave: its write and theirs are separate, and one can fail.
func TestAnEmptyResultIsAlwaysNamed(t *testing.T) {
	t.Parallel()

	row := knownUsageRow()
	row.GitHubRunnerAssignedAt = "2026-11-04T09:48:27Z"
	row.GitHubFinishedAt = "2026-11-04T09:52:30Z"
	row.Result = ""

	r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.JobClock != jobClockGitHub || r.Unknown["result"] == "" {
		t.Errorf("clock %q, unknown %v", r.JobClock, r.Unknown)
	}
}

// AN OWNED MACHINE BOUGHT NOTHING, so it has no price to be unknown about.
func TestAnOwnedLeaseHasNoPriceToReport(t *testing.T) {
	t.Parallel()

	row := knownUsageRow()
	row.Provider, row.InstanceType, row.PriceMicrosPerHour = string(config.ProviderFirecracker), "", 0

	r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.Ownership != usageOwned {
		t.Errorf("ownership = %q, want %q", r.Ownership, usageOwned)
	}
	if len(r.Unknown) != 0 {
		t.Errorf("an owned lease reports %v unknown", r.Unknown)
	}
	if r.CostMicros != nil || r.PriceMicrosPerHour != nil {
		t.Errorf("an owned lease reports a price %v and cost %v", r.PriceMicrosPerHour, r.CostMicros)
	}
}

// marshalRecord is a record as a consumer reads it.
func marshalRecord(t *testing.T, r usageRecord) map[string]any {
	t.Helper()

	body, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	return fields
}

// THE RECORD'S NAMES ARE THE FORMAT, nested ones included, so a renamed Go field or
// an omitempty cannot move one under a billing system without this failing. A null
// is there, spelled, so a consumer can tell it from a field this schema lacks.
func TestTheRecordFormatIsWhatTheDocumentStates(t *testing.T) {
	t.Parallel()

	wantTop := []string{
		"schema", "cursor", "sequence", "lease_id", "deployment", "target", "repository", "tier", "site",
		"provider", "ownership", "vcpu", "memory_bytes", "held", "job", "job_clock", "billed_minutes", "minute_weight",
		"weight_source", "weighted_minutes", "instance_type", "price_micros_per_hour", "cost_micros",
		"conclusion", "result", "unknown",
	}
	wantInterval := []string{"from", "to", "seconds"}

	known := marshalRecord(t, newUsageRecord(usageConfig(t, 0), usageDeployment, knownUsageRow()))

	empty := knownUsageRow()
	empty.Provider, empty.VCPU, empty.Memory, empty.PriceMicrosPerHour = "", 0, 0, 0
	empty.BoughtAt, empty.StartedAt, empty.ResultAt = "", "", ""
	unknown := marshalRecord(t, newUsageRecord(usageConfig(t, 0), usageDeployment, empty))

	if known["schema"] != float64(usageRecordSchema) || usageRecordSchema != 1 {
		t.Errorf("schema = %v; a change a consumer reads differently is a new number, and a document to match",
			known["schema"])
	}

	for name, fields := range map[string]map[string]any{"known": known, "unknown": unknown} {
		got := slices.Sorted(maps.Keys(fields))
		if want := slices.Sorted(slices.Values(wantTop)); !slices.Equal(got, want) {
			t.Errorf("%s record has keys %v, want %v", name, got, want)
		}

		for _, interval := range []string{"held", "job"} {
			nested, ok := fields[interval].(map[string]any)
			if !ok {
				t.Fatalf("%s record's %s is %T", name, interval, fields[interval])
			}

			if got := slices.Sorted(maps.Keys(nested)); !slices.Equal(got, slices.Sorted(slices.Values(wantInterval))) {
				t.Errorf("%s record's %s has keys %v, want %v", name, interval, got, wantInterval)
			}
		}
	}

	// A NUMBER IS A NUMBER AND AN UNKNOWN IS A NULL, never a zero or a string.
	for field, want := range map[string]any{
		"vcpu": float64(4), "billed_minutes": float64(3), "minute_weight": float64(2), "weighted_minutes": float64(6),
	} {
		if known[field] != want {
			t.Errorf("known record's %s = %#v, want %#v", field, known[field], want)
		}
	}
	for _, field := range []string{"vcpu", "memory_bytes", "minute_weight", "price_micros_per_hour", "cost_micros"} {
		if unknown[field] != nil {
			t.Errorf("unknown record's %s = %#v, want null", field, unknown[field])
		}
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "reference", "usage-export.md"))
	if err != nil {
		t.Fatalf("read the record's documentation: %v", err)
	}

	for _, field := range append(wantTop, wantInterval...) {
		if !strings.Contains(string(doc), "`"+field+"`") {
			t.Errorf("docs/reference/usage-export.md does not name the record's field %q", field)
		}
	}
}

// A NUMBER TOO LARGE TO STATE IS UNKNOWN, and never wraps into a negative charge.
func TestAnOverflowingLeaseIsUnknownNotNegative(t *testing.T) {
	t.Parallel()

	row := knownUsageRow()
	row.PriceMicrosPerHour = math.MaxInt64
	row.FinishedAt = "2026-11-03T12:00:00.000000000Z"

	r := newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.CostMicros != nil || r.Unknown["cost_micros"] == "" {
		t.Errorf("cost = %v, unknown %q; want it stated as too large", r.CostMicros, r.Unknown["cost_micros"])
	}

	row = knownUsageRow()
	row.VCPU = math.MaxInt64

	r = newUsageRecord(usageConfig(t, 0), usageDeployment, row)
	if r.MinuteWeight != nil || r.Unknown["minute_weight"] == "" {
		t.Errorf("weight = %v, unknown %q; a vCPU no machine has weighs nothing", r.MinuteWeight, r.Unknown["minute_weight"])
	}

	if _, err := json.Marshal(r); err != nil {
		t.Fatalf("the record does not encode: %v", err)
	}
}

// THE EXPORT ONLY REPORTS. Nothing that decides what runs reads a minute weight, and
// config offers no helper that would let it do so without the name appearing.
func TestNoSchedulerReadsAMinuteWeight(t *testing.T) {
	t.Parallel()

	for _, pkg := range []string{"alloc", "server", "nodeplane", "node", "app", "state", "config"} {
		dir := filepath.Join("..", "..", pkg)

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}

		read := 0

		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}

			path := filepath.Join(dir, e.Name())

			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			read++

			// The field's declaration and validation, and the type's own file.
			if pkg == "config" && (e.Name() == "tier.go" || e.Name() == "minuteweight.go" ||
				e.Name() == "tiersizes.go") {
				continue
			}

			if strings.Contains(string(body), "MinuteWeight") {
				t.Errorf("%s reads a minute weight; the export reports and feeds nothing back", path)
			}
		}

		if read == 0 {
			t.Errorf("%s has no source to check, so this test moved out from under it", dir)
		}
	}
}

// A PAGE'S LAST LINE IS THE CURSOR TO KEEP, in the table an operator reads too.
func TestTheTextExportEndsWithTheCursor(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	finished := finishUsageLeases(t, stateDir, 2)

	env, out := bufferedEnv()
	if err := Usage(t.Context(), env, []string{"export", "--config", cfg}); err != nil {
		t.Fatalf("billet usage export: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header, two rows and a cursor, got:\n%s", out.String())
	}
	if !strings.HasPrefix(lines[3], "cursor u1.") || !strings.Contains(lines[3], "."+finished[1]+".") {
		t.Errorf("the last line is %q, want the last lease's cursor", lines[3])
	}

	env, out = bufferedEnv()
	if err := Usage(t.Context(), env, []string{"export", "--config", cfg,
		"--since", strings.TrimPrefix(lines[3], "cursor ")}); err != nil {
		t.Fatalf("billet usage export --since: %v", err)
	}
	if !strings.Contains(out.String(), "no finished lease past this cursor") {
		t.Errorf("a caught-up export said:\n%s", out.String())
	}
}

// A LEDGER WITH NO IDENTITY IS REFUSED, and none is minted: records labelled with one
// this command just made would be another deployment's history under a new name.
func TestUsageNeverMintsAnIdentity(t *testing.T) {
	for name, seeded := range map[string]bool{"beside a ledger": true, "on a host that never ran one": false} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			cfg := writeJobsConfig(t, stateDir)

			if seeded {
				seedLedger(t, stateDir)
			}

			env, _ := bufferedEnv()

			err := Usage(t.Context(), env, []string{"export", "--config", cfg})
			if err == nil || !strings.Contains(err.Error(), "holds no deployment identity") {
				t.Fatalf("got %v, want a refusal naming the missing identity", err)
			}

			if _, found, err := state.PeekDeploymentID(stateDir); err != nil || found {
				t.Fatalf("after the refusal the identity is found=%v, %v; the command minted one", found, err)
			}

			// NOR A LEDGER: a command that only reports creates nothing to report on.
			if _, err := os.Stat(state.LedgerPath(stateDir)); !seeded && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused export left a ledger behind: %v", err)
			}
		})
	}
}

// A NODE'S FILE IS REFUSED BY NAME: the identity is read before anything opens, so nothing
// further on is there to say this file has no control plane.
func TestUsageRefusesANodesConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfg, []byte("node:\n  name: n\n  server_addr: 10.0.0.1:7717\n  provider: docker\n"+
		"  state_dir: "+dir+"\n  tls:\n    cert: "+filepath.Join(dir, "c")+"\n    key: "+filepath.Join(dir, "k")+
		"\n    ca: "+filepath.Join(dir, "a")+"\n"), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	for _, name := range []string{"c", "k", "a"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	env, _ := bufferedEnv()

	err := Usage(t.Context(), env, []string{"export", "--config", cfg})
	if err == nil || !strings.Contains(err.Error(), "no server section") {
		t.Fatalf("got %v, want a refusal naming the missing section", err)
	}
}

// A LATE RESULT FILLS A RECORD UNDER THE NUMBER IT HAD, so nothing past a reader's cursor
// says so: the document tells a reader to read such a lease again, from an older cursor,
// and that has to work and find the same place.
func TestALateResultFillsARecordAReaderMustReadAgain(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	finished := finishUsageLeases(t, stateDir, 1)

	first := exportUsage(t, cfg, "", "1")
	if len(first) != 1 || first[0].Result != "" {
		t.Fatalf("before the result the export read %+v", first)
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), finished[0], "succeeded", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	if past := exportUsage(t, cfg, first[0].Cursor, "1"); len(past) != 0 {
		t.Fatalf("a reader past the lease was handed %+v; the record does not move", past)
	}

	again := exportUsage(t, cfg, "", "1")
	if len(again) != 1 || again[0].Result != "succeeded" || again[0].Cursor != first[0].Cursor {
		t.Fatalf("read again: %+v, want the result under the cursor it had, %s", again, first[0].Cursor)
	}
}

// A LEASE STILL WAITING FOR ITS NUMBER IS SAID ON STANDARD ERROR, and never as a record:
// a reader caught up to the end has not read the whole history until this stops.
func TestAnExportSaysWhenOlderLeasesAreStillBeingNumbered(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	finishUsageLeases(t, stateDir, 1)

	run := func() (string, string) {
		t.Helper()

		var out, errs bytes.Buffer

		env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}
		if err := Usage(t.Context(), env, []string{"export", "--config", cfg, "--json"}); err != nil {
			t.Fatalf("billet usage export: %v", err)
		}

		return out.String(), errs.String()
	}

	if _, errs := run(); strings.Contains(errs, "still being numbered") {
		t.Fatalf("nothing is waiting and the export said:\n%s", errs)
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, finished_at)
			VALUES ('`+strings.Repeat("a", 32)+`', 'billet-2vcpu', 't', '2026-01-01T00:00:00.000000000Z')`)

		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	out, errs := run()
	if !strings.Contains(errs, "still being numbered") {
		t.Errorf("a lease is waiting and standard error said %q", errs)
	}
	if strings.Contains(out, strings.Repeat("a", 32)) || strings.Contains(out, "still being numbered") {
		t.Errorf("the waiting lease reached the records:\n%s", out)
	}
}

// stagedUsagePage is a ledger whose last batch commits between two reads, whichever
// comes first: the page is empty and nothing is waiting once it has been asked.
type stagedUsagePage struct{ paged bool }

func (s *stagedUsagePage) UsageStillNumbering(context.Context) (bool, error) { return !s.paged, nil }

func (s *stagedUsagePage) UsageSince(context.Context, int64, int) ([]state.UsageRow, error) {
	s.paged = true

	return nil, nil
}

// THE LAST BATCH CANNOT MAKE AN EMPTY PAGE LOOK LIKE THE END. Read the other way round,
// this interleaving prints nothing and says nothing, and a reader stops short of it.
func TestAnEmptyPageBesideABatchCommittingStillSaysSo(t *testing.T) {
	t.Parallel()

	rows, waiting, err := readUsagePage(t.Context(), &stagedUsagePage{}, 0, 10)
	if err != nil {
		t.Fatalf("readUsagePage: %v", err)
	}
	if len(rows) != 0 || !waiting {
		t.Fatalf("rows %d, waiting %v; an empty page read as the end of a history still being numbered", len(rows), waiting)
	}
}

// A BACKUP TAKEN WHILE A LEASE WAITED FOR ITS NUMBER CANNOT PASS AN OLD CURSOR. Restored,
// the ledger closes a lease nobody read and gives the waiting lease the number and the
// time it had, so only the mark tells the two histories apart.
func TestACursorFromBeforeARestoreIsRefusedWhenTheSameLeaseTookTheSameNumber(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	finishUsageLeases(t, stateDir, 0)

	waiting := strings.Repeat("b", 32)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, finished_at)
			VALUES ('`+waiting+`', 'billet-2vcpu', 't', '2026-01-01T00:00:00.000000000Z')`)

		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	backup := t.TempDir()
	copyLedger(t, stateDir, backup)

	number := func() {
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

		a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)
		if err != nil {
			t.Fatalf("alloc.New: %v", err)
		}

		if n, err := a.NumberUsage(t.Context(), []string{waiting}); err != nil || n != 1 {
			t.Fatalf("NumberUsage = %d, %v", n, err)
		}
	}

	finishUsageLeases(t, stateDir, 1)
	number()

	records := exportUsage(t, cfg, "", "10")
	if len(records) != 2 || records[1].LeaseID != waiting || records[1].Sequence != 2 {
		t.Fatalf("before the restore the export read %+v", records)
	}

	cursor := records[1].Cursor

	copyLedger(t, backup, stateDir)

	unread := finishUsageLeases(t, stateDir, 1)
	number()

	env, out := bufferedEnv()

	err = Usage(t.Context(), env, []string{"export", "--config", cfg, "--json", "--since", cursor})
	if err == nil || !strings.Contains(err.Error(), "is not the one --since names") {
		t.Fatalf("got %v and printed %q; the cursor would skip %s, which nobody has read", err, out.String(), unread[0])
	}
	if out.Len() != 0 {
		t.Errorf("a refused resume printed:\n%s", out.String())
	}
}

// copyLedger replaces what is in to with the closed ledger in from, sidecars included.
func copyLedger(t *testing.T, from, to string) {
	t.Helper()

	stale, err := filepath.Glob(state.LedgerPath(to) + "*")
	if err != nil {
		t.Fatalf("list the ledger's files: %v", err)
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}

	files, err := filepath.Glob(state.LedgerPath(from) + "*")
	if err != nil || len(files) == 0 {
		t.Fatalf("no ledger in %s (%v)", from, err)
	}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		name := strings.TrimPrefix(path, state.LedgerPath(from))
		if err := os.WriteFile(state.LedgerPath(to)+name, body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func TestUsageRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no subcommand":      {nil, "usage: billet usage export"},
		"another subcommand": {[]string{"import"}, "usage: billet usage export"},
		"a zero limit":       {[]string{"export", "--limit", "0"}, "--limit takes 1 to 10000"},
		"too large a limit":  {[]string{"export", "--limit", "10001"}, "--limit takes 1 to 10000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, _ := bufferedEnv()

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()

			if err := Usage(ctx, env, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to say %q", err, tc.want)
			}
		})
	}
}
