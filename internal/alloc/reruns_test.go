package alloc

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// rerunRow is one job_history row as a test writes it. The zero fields are what a
// candidate has, so a case names only the clause it breaks.
type rerunRow struct {
	lease, disruption, result, repo, jobID, decision string
	startedAt                                        sql.NullString
	githubRunID                                      int64
	resultAt                                         time.Time
}

func newRerunAllocator(t *testing.T) *Allocator {
	t.Helper()

	return newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}})
}

// seedRerunRow writes the row, each field a candidate's unless the case changes it.
func seedRerunRow(t *testing.T, a *Allocator, r rerunRow) {
	t.Helper()

	if r.disruption == "" {
		r.disruption = string(DisruptionReclaimed)
	}
	if r.result == "" {
		r.result = "failed"
	}
	if r.repo == "" {
		r.repo = "acme/api"
	}
	if r.jobID == "" {
		r.jobID = "guid-" + r.lease
	}
	if r.githubRunID == 0 {
		r.githubRunID = 31
	}
	if r.resultAt.IsZero() {
		r.resultAt = time.Now().Add(-time.Hour)
	}
	if !r.startedAt.Valid {
		r.startedAt = sql.NullString{String: ts(r.resultAt.Add(-time.Hour)), Valid: true}
	}

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, started_at, repo, github_job_id, github_run_id,
			 disruption, disrupted_at, result, result_at, rerun_decision)
			VALUES ($1, 'billet-2vcpu', $2, $3, $4, $5, $6, $7, $2, $8, $9, $10)`,
			r.lease, ts(r.resultAt), r.startedAt, r.repo, r.jobID, r.githubRunID,
			r.disruption, r.result, ts(r.resultAt), r.decision)

		return err
	}); err != nil {
		t.Fatalf("seed lease %s: %v", r.lease, err)
	}
}

// A CANDIDATE IS A JOB OF A CAUSE ON THE CLOSED LIST THAT GITHUB SAW START AND
// REPORTED RED, THAT NOTHING HAS DECIDED, THAT CAN BE FOUND AT GITHUB. Each row below
// breaks one clause, so each clause is held by the row that would otherwise slip
// through it. Above all: a job that succeeded, a job with no disruption, and the
// host that merely went silent.
func TestOnlyAJobOfACauseThatRanAndWentRedIsACandidate(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)

	for _, cause := range RerunCauses {
		seedRerunRow(t, a, rerunRow{lease: "cause-" + string(cause), disruption: string(cause)})
	}

	for _, r := range []rerunRow{
		{lease: "node-forgotten", disruption: string(DisruptionNodeForgotten)},
		{lease: "held-past-limit", disruption: string(DisruptionHeldPastLimit)},
		{lease: "another-word", disruption: "something-newer"},
		{lease: "succeeded", result: resultSucceeded},
		{lease: "already-decided", decision: string(RerunUnproven)},
		{lease: "too-old", resultAt: time.Now().Add(-72 * time.Hour)},
	} {
		seedRerunRow(t, a, r)
	}

	// THE COLUMNS A SEED CANNOT LEAVE TO ITS ZERO, written after it.
	exec := func(stmt, lease string) {
		t.Helper()
		if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), stmt, lease)
			return err
		}); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	seedRerunRow(t, a, rerunRow{lease: "never-started"})
	exec(`UPDATE job_history SET started_at = NULL WHERE lease_id = $1`, "never-started")
	seedRerunRow(t, a, rerunRow{lease: "no-result"})
	exec(`UPDATE job_history SET result = '' WHERE lease_id = $1`, "no-result")
	seedRerunRow(t, a, rerunRow{lease: "no-disruption"})
	exec(`UPDATE job_history SET disruption = '' WHERE lease_id = $1`, "no-disruption")
	seedRerunRow(t, a, rerunRow{lease: "no-job"})
	exec(`UPDATE job_history SET github_job_id = '' WHERE lease_id = $1`, "no-job")
	seedRerunRow(t, a, rerunRow{lease: "no-repo"})
	exec(`UPDATE job_history SET repo = NULL WHERE lease_id = $1`, "no-repo")
	seedRerunRow(t, a, rerunRow{lease: "no-run"})
	exec(`UPDATE job_history SET github_run_id = 0, run_id = NULL WHERE lease_id = $1`, "no-run")
	seedRerunRow(t, a, rerunRow{lease: "assigned-run"})
	exec(`UPDATE job_history SET github_run_id = 0, run_id = 44 WHERE lease_id = $1`, "assigned-run")

	got, err := a.RerunCandidates(t.Context(), time.Now().Add(-48*time.Hour), RerunCursor{}, 100)
	if err != nil {
		t.Fatalf("RerunCandidates: %v", err)
	}

	byLease := make(map[string]RerunCandidate, len(got))
	for _, c := range got {
		byLease[c.LeaseID] = c
	}

	want := []string{"cause-reclaimed", "cause-guest-absent", "assigned-run"}
	for _, lease := range want {
		if _, ok := byLease[lease]; !ok {
			t.Errorf("%s is not a candidate", lease)
		}
	}
	for _, lease := range []string{"node-forgotten", "held-past-limit", "another-word", "succeeded", "already-decided", "too-old",
		"never-started", "no-result", "no-disruption", "no-job", "no-repo", "no-run"} {
		if _, ok := byLease[lease]; ok {
			t.Errorf("%s is a candidate", lease)
		}
	}

	// THE RUN IS THE ONE THAT STARTED, and the one assigned only for a row with none.
	if got := byLease["cause-reclaimed"].RunID; got != 31 {
		t.Errorf("a started job's run = %d, want 31", got)
	}
	if got := byLease["assigned-run"].RunID; got != 44 {
		t.Errorf("a row with no started run = %d, want its assigned 44", got)
	}
	if c := byLease["cause-reclaimed"]; c.Repository != "acme/api" || c.Disruption != DisruptionReclaimed ||
		c.ResultAt.IsZero() || c.Tier != "billet-2vcpu" {
		t.Errorf("candidate = %+v", c)
	}
}

// THE LIST OF CAUSES IS WRITTEN DOWN IN ONE PLACE, and held to it: a host that merely
// went silent may still be running the job, and a custody bound is an operator's
// choice that a job which hangs would meet again.
func TestTheCausesAreTheClosedList(t *testing.T) {
	t.Parallel()

	for _, cause := range RerunCauses {
		if !cause.Valid() {
			t.Errorf("%q is not a token billet writes", cause)
		}
		if cause == DisruptionNodeForgotten || cause == DisruptionHeldPastLimit {
			t.Errorf("%q is not proof billet's infrastructure ended a job", cause)
		}
	}
	if len(RerunCauses) != 2 {
		t.Errorf("there are %d causes; a new one is a decision for ADR-028", len(RerunCauses))
	}
}

// THE LIST IS NEWEST FIRST, PAGED FROM ITS CURSOR TO THE END, and a limit that could
// not bound it is refused. Two rows sharing a result time are told apart by lease.
func TestRerunCandidatesArePagedNewestFirst(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	now := time.Now()
	seedRerunRow(t, a, rerunRow{lease: "old", resultAt: now.Add(-3 * time.Hour)})
	seedRerunRow(t, a, rerunRow{lease: "new-b", resultAt: now.Add(-time.Hour)})
	seedRerunRow(t, a, rerunRow{lease: "new-a", resultAt: now.Add(-time.Hour)})

	var (
		seen   []string
		cursor RerunCursor
	)
	for range 5 {
		page, err := a.RerunCandidates(t.Context(), now.Add(-48*time.Hour), cursor, 1)
		if err != nil {
			t.Fatalf("RerunCandidates: %v", err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].LeaseID)
		cursor = page[0].Cursor
	}
	if fmt.Sprint(seen) != "[new-b new-a old]" {
		t.Errorf("pages = %v", seen)
	}

	if _, err := a.RerunCandidates(t.Context(), now, RerunCursor{}, 0); err == nil {
		t.Error("a limit of 0 was accepted")
	}
}

// A JOB A REQUEST IN DOUBT COVERS SAYS SO, which is what keeps it from expiring.
func TestACandidateSaysItsRequestInDoubt(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "bound"})
	if err := a.BeginRerun(t.Context(), RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 3,
		Kind: RerunFailedJobs}, []string{"bound"}); err != nil {
		t.Fatal(err)
	}

	got, err := a.RerunCandidates(t.Context(), time.Now().Add(-48*time.Hour), RerunCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Bound != 3 {
		t.Errorf("candidates = %+v", got)
	}

	open, err := a.OpenReruns(t.Context(), OpenRerunCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if tiers, err := a.BoundRerunTiers(t.Context(), open[0].RerunRequest); err != nil ||
		fmt.Sprint(tiers) != "[billet-2vcpu]" {
		t.Errorf("the bound tiers = %v, %v", tiers, err)
	}
	if len(open) != 1 || open[0].Attempt != 3 || open[0].RunID != 31 ||
		open[0].Repository != "acme/api" || open[0].Kind != RerunFailedJobs || open[0].Tries != 1 {
		t.Errorf("open = %+v", open)
	}

	if err := a.SettleRerun(t.Context(), open[0].RerunRequest, RerunRequested, ""); err != nil {
		t.Fatal(err)
	}
	if open, err := a.OpenReruns(t.Context(), OpenRerunCursor{}, 10); err != nil || len(open) != 0 {
		t.Errorf("after settling, open = %+v, %v", open, err)
	}
}

// A TIER'S ASKING STARTS ONCE AND IS KEPT, a tier that stops loses it, and asking again
// starts again.
func TestATiersAskingIsKeptUntilItStops(t *testing.T) {
	t.Parallel()

	clock := &struct{ now time.Time }{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}}, WithClock(func() time.Time { return clock.now }))
	first := clock.now

	asked, err := a.AskReruns(t.Context(), []string{"a", "b"})
	if err != nil || len(asked) != 2 || !asked["a"].Equal(first) {
		t.Fatalf("asked = %v, %v", asked, err)
	}

	clock.now = clock.now.Add(time.Hour)
	asked, err = a.AskReruns(t.Context(), []string{"a"})
	if err != nil || len(asked) != 1 || !asked["a"].Equal(first) {
		t.Fatalf("after b stopped = %v, %v", asked, err)
	}

	clock.now = clock.now.Add(time.Hour)
	asked, err = a.AskReruns(t.Context(), []string{"a", "b"})
	if err != nil || !asked["a"].Equal(first) || !asked["b"].Equal(clock.now) {
		t.Errorf("after b asked again = %v, %v", asked, err)
	}
}

// A DECISION IS THE FIRST ONE MADE. A job decided as unproven stays so, and is no
// longer a candidate.
func TestARerunDecisionIsWrittenOnce(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "job"})

	if err := a.RecordRerunDecision(t.Context(), "job", RerunUnproven, 3); err != nil {
		t.Fatalf("RecordRerunDecision: %v", err)
	}
	if err := a.RecordRerunDecision(t.Context(), "job", RerunLimit, 4); err != nil {
		t.Fatalf("a second decision: %v", err)
	}

	rec, err := a.Job(t.Context(), "job")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if rec.RerunDecision != RerunUnproven || rec.RerunAttempt != 3 || rec.RerunDecidedAt == "" {
		t.Errorf("job = decision %q, attempt %d, at %q", rec.RerunDecision, rec.RerunAttempt, rec.RerunDecidedAt)
	}

	got, err := a.RerunCandidates(t.Context(), time.Now().Add(-48*time.Hour), RerunCursor{}, 10)
	if err != nil {
		t.Fatalf("RerunCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a decided job is still a candidate: %+v", got)
	}
}

// ONE REQUEST PER ATTEMPT, BY THE SCHEMA, and a crash between writing it and hearing
// the answer leaves a row that says so, with the jobs it was for still undecided.
func TestARequestIsRecordedBeforeItLeavesAndOncePerAttempt(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "one"})
	seedRerunRow(t, a, rerunRow{lease: "two"})
	seedRerunRow(t, a, rerunRow{lease: "other"})

	request := RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: RerunFailedJobs}
	if err := a.BeginRerun(t.Context(), request, []string{"one", "two"}); err != nil {
		t.Fatalf("BeginRerun: %v", err)
	}

	open, doubted, err := a.OpenRerun(t.Context(), "acme/api", 31)
	if err != nil || !doubted {
		t.Fatalf("OpenRerun = %+v, %v, %v, want one in doubt", open, doubted, err)
	}
	if open.Attempt != 1 || open.Kind != RerunFailedJobs || open.Tries != 1 || open.JobID != 0 ||
		open.Repository != "acme/api" || open.RunID != 31 {
		t.Errorf("open = %+v", open)
	}

	if err := a.BeginRerun(t.Context(), request, []string{"other"}); err == nil {
		t.Fatal("a second request for the attempt was recorded")
	}
	// THE TRANSACTION TOOK ITS BINDING WITH IT.
	if rec, err := a.Job(t.Context(), "other"); err != nil || rec.RerunAttempt != 0 {
		t.Errorf("the refused request bound a job: %+v, %v", rec, err)
	}

	// UNDECIDED WHILE IN DOUBT, so a restart finds the jobs and their request.
	if rec, err := a.Job(t.Context(), "one"); err != nil || rec.RerunDecision != "" || rec.RerunAttempt != 1 {
		t.Errorf("job while in doubt: %+v, %v", rec, err)
	}

	if err := a.RetryRerun(t.Context(), request); err != nil {
		t.Fatalf("RetryRerun: %v", err)
	}
	if open, _, err := a.OpenRerun(t.Context(), "acme/api", 31); err != nil || open.Tries != 2 {
		t.Errorf("after a retry = %+v, %v, want 2 tries", open, err)
	}
}

// A SETTLED REQUEST IS ON EVERY JOB BOUND TO IT AND NO OTHER, COUNTS AGAINST THE LIMIT
// ONLY IF GITHUB ACCEPTED IT OR MAY HAVE, and is no longer in doubt.
func TestSettlingARequestRecordsItOnItsJobsAndCountsIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		decision RerunDecision
		counted  int64
	}{
		{"accepted", RerunRequested, 1},
		{"may have been accepted", RerunUnconfirmed, 1},
		{"never accepted", RerunFailed, 0},
		{"refused by a lowered limit", RerunLimit, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newRerunAllocator(t)
			seedRerunRow(t, a, rerunRow{lease: "bound"})
			seedRerunRow(t, a, rerunRow{lease: "unbound-same-run"})
			seedRerunRow(t, a, rerunRow{lease: "another-run", githubRunID: 32})
			seedRerunRow(t, a, rerunRow{lease: "another-repo", repo: "acme/web"})

			request := RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 2, Kind: RerunOneJob, JobID: 77}
			if err := a.BeginRerun(t.Context(), request, []string{"bound"}); err != nil {
				t.Fatalf("BeginRerun: %v", err)
			}
			// IN DOUBT IT HAS COME TO NOTHING YET, though it was asked.
			if got, _, asked, err := a.RerunOfAttempt(t.Context(), "acme/api", 31, 2); err != nil || !asked || got != "" {
				t.Errorf("a request in doubt = %q, %v, %v, want asked and undecided", got, asked, err)
			}
			// THE SAME ATTEMPT NUMBER ON OTHER RUNS, which a settlement must not reach.
			for _, lease := range []string{"another-run", "another-repo"} {
				if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(),
						`UPDATE job_history SET rerun_attempt = 2 WHERE lease_id = $1`, lease)
					return err
				}); err != nil {
					t.Fatalf("bind %s: %v", lease, err)
				}
			}

			if err := a.SettleRerun(t.Context(), request, tc.decision, "GitHub said no"); err != nil {
				t.Fatalf("SettleRerun: %v", err)
			}

			rec, err := a.Job(t.Context(), "bound")
			if err != nil {
				t.Fatalf("Job: %v", err)
			}
			if rec.RerunDecision != tc.decision || rec.RerunAttempt != 2 || rec.RerunDecidedAt == "" {
				t.Errorf("bound job = %q, attempt %d, at %q", rec.RerunDecision, rec.RerunAttempt, rec.RerunDecidedAt)
			}
			for _, lease := range []string{"unbound-same-run", "another-run", "another-repo"} {
				if rec, err := a.Job(t.Context(), lease); err != nil || rec.RerunDecision != "" {
					t.Errorf("%s was decided by a request not its own: %+v, %v", lease, rec, err)
				}
			}

			if count, err := a.RunReruns(t.Context(), "acme/api", 31); err != nil || count != tc.counted {
				t.Errorf("RunReruns = %d, %v, want %d", count, err, tc.counted)
			}
			if _, doubted, err := a.OpenRerun(t.Context(), "acme/api", 31); err != nil || doubted {
				t.Errorf("a settled request is still in doubt: %v, %v", doubted, err)
			}

			want := tc.decision
			got, kind, asked, err := a.RerunOfAttempt(t.Context(), "acme/api", 31, 2)
			if err != nil || !asked || got != want || kind != RerunOneJob {
				t.Errorf("RerunOfAttempt = %q, %q, %v, %v, want %q of one job", got, kind, asked, err, want)
			}
			if _, _, asked, err := a.RerunOfAttempt(t.Context(), "acme/api", 31, 3); err != nil || asked {
				t.Errorf("an attempt never asked about = %v, %v", asked, err)
			}

			// A SETTLED REQUEST'S ANSWER STANDS, whatever comes after, on the row and on
			// the job.
			other := RerunRequested
			if tc.decision == RerunRequested || tc.decision == RerunUnconfirmed {
				other = RerunFailed
			}
			if err := a.SettleRerun(t.Context(), request, other, "too late"); err != nil {
				t.Fatalf("a second settlement: %v", err)
			}
			if count, err := a.RunReruns(t.Context(), "acme/api", 31); err != nil || count != tc.counted {
				t.Errorf("after a second settlement RunReruns = %d, %v", count, err)
			}
			if rec, err := a.Job(t.Context(), "bound"); err != nil || rec.RerunDecision != tc.decision {
				t.Errorf("after a second settlement the job = %q, %v", rec.RerunDecision, err)
			}
			if got, _, _, err := a.RerunOfAttempt(t.Context(), "acme/api", 31, 2); err != nil || got != tc.decision {
				t.Errorf("after a second settlement the request = %q, %v", got, err)
			}
		})
	}
}

// THE LIMIT COUNTS ACROSS ATTEMPTS OF ONE RUN, and no other run's.
func TestAcceptedRerunsAreCountedAcrossAttempts(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)

	for attempt := int64(1); attempt <= 2; attempt++ {
		request := RerunRequest{Repository: "acme/api", RunID: 31, Attempt: attempt, Kind: RerunFailedJobs}
		if err := a.BeginRerun(t.Context(), request, nil); err != nil {
			t.Fatalf("BeginRerun: %v", err)
		}
		if err := a.SettleRerun(t.Context(), request, RerunRequested, ""); err != nil {
			t.Fatalf("SettleRerun: %v", err)
		}
	}

	if count, err := a.RunReruns(t.Context(), "acme/api", 31); err != nil || count != 2 {
		t.Errorf("RunReruns = %d, %v, want 2", count, err)
	}
	if count, err := a.RunReruns(t.Context(), "acme/api", 32); err != nil || count != 0 {
		t.Errorf("another run's = %d, %v, want 0", count, err)
	}
}

// THE DECISIONS ARE COUNTED BY TIER, WITHIN THE WINDOW, and a job undecided is none.
func TestRerunDecisionsAreCountedInTheirWindow(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "a"})
	seedRerunRow(t, a, rerunRow{lease: "b"})
	seedRerunRow(t, a, rerunRow{lease: "c"})
	seedRerunRow(t, a, rerunRow{lease: "undecided"})

	for lease, decision := range map[string]RerunDecision{"a": RerunUnproven, "b": RerunUnproven, "c": RerunLimit} {
		if err := a.RecordRerunDecision(t.Context(), lease, decision, 1); err != nil {
			t.Fatalf("RecordRerunDecision: %v", err)
		}
	}

	got, err := a.RerunDecisions(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("RerunDecisions: %v", err)
	}
	counts := map[RerunDecision]int64{}
	for _, c := range got {
		if c.Tier != "billet-2vcpu" {
			t.Errorf("tier = %q", c.Tier)
		}
		counts[c.Decision] = c.Jobs
	}
	if len(counts) != 2 || counts[RerunUnproven] != 2 || counts[RerunLimit] != 1 {
		t.Errorf("counts = %v", counts)
	}

	later, err := a.RerunDecisions(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("RerunDecisions: %v", err)
	}
	if len(later) != 0 {
		t.Errorf("past the window = %+v", later)
	}
}

// ListAttributedFailures AND THE JOB RECORD SAY WHAT WAS DECIDED, so the two
// commands an operator reads a red job with answer it.
func TestTheFailuresReportCarriesTheDecision(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "job"})
	if err := a.RecordRerunDecision(t.Context(), "job", RerunMixed, 2); err != nil {
		t.Fatalf("RecordRerunDecision: %v", err)
	}

	failures, err := a.AttributedFailures(t.Context(), time.Now().Add(-48*time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}
	if len(failures) != 1 || failures[0].RerunDecision != RerunMixed || failures[0].RerunAttempt != 2 {
		t.Errorf("failures = %+v", failures)
	}
}

// A SETTLEMENT REACHES A JOB WHOSE RUN IS ITS ASSIGNED ONE, which a row written before
// the run that started was recorded has, as ListJobsOfRun reads it.
func TestASettlementReachesAJobByItsAssignedRun(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "assigned"})
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE job_history SET github_run_id = 0, run_id = 31 WHERE lease_id = 'assigned'`)

		return err
	}); err != nil {
		t.Fatal(err)
	}

	request := RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: RerunFailedJobs}
	if err := a.BeginRerun(t.Context(), request, []string{"assigned"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SettleRerun(t.Context(), request, RerunRequested, ""); err != nil {
		t.Fatal(err)
	}
	if rec, err := a.Job(t.Context(), "assigned"); err != nil || rec.RerunDecision != RerunRequested {
		t.Errorf("the job = %q, %v", rec.RerunDecision, err)
	}
}

// THE REQUESTS IN DOUBT ARE PAGED IN THE ORDER THEY WERE MADE, each once, and a
// settled one is not among them.
func TestOpenRerunsArePagedOldestFirst(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	for _, run := range []int64{31, 32, 33} {
		if err := a.BeginRerun(t.Context(), RerunRequest{Repository: "acme/api", RunID: run, Attempt: 1,
			Kind: RerunFailedJobs}, nil); err != nil {
			t.Fatal(err)
		}
	}
	settled := RerunRequest{Repository: "acme/api", RunID: 34, Attempt: 1, Kind: RerunFailedJobs}
	if err := a.BeginRerun(t.Context(), settled, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SettleRerun(t.Context(), settled, RerunRequested, ""); err != nil {
		t.Fatal(err)
	}

	var (
		seen   []int64
		cursor OpenRerunCursor
	)
	for range 5 {
		page, err := a.OpenReruns(t.Context(), cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].RunID)
		cursor = page[0].Cursor
	}
	if fmt.Sprint(seen) != "[31 32 33]" {
		t.Errorf("pages = %v", seen)
	}
}

// A REQUEST'S TIERS ARE THOSE OF THE JOBS IT STILL COVERS: a job of its attempt decided
// some other way is not one it is limited by.
func TestABoundTierIsOneOfAnUndecidedJob(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "covered"})
	seedRerunRow(t, a, rerunRow{lease: "decided"})
	if err := a.RecordRerunDecision(t.Context(), "decided", RerunUnproven, 2); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE job_history SET tier = 'other' WHERE lease_id = 'decided'`)

		return err
	}); err != nil {
		t.Fatal(err)
	}

	request := RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 2, Kind: RerunOneJob, JobID: 7}
	if err := a.BeginRerun(t.Context(), request, []string{"covered"}); err != nil {
		t.Fatal(err)
	}

	tiers, err := a.BoundRerunTiers(t.Context(), request)
	if err != nil || fmt.Sprint(tiers) != "[billet-2vcpu]" {
		t.Errorf("tiers = %v, %v", tiers, err)
	}

	got, err := a.RunRerunCandidates(t.Context(), "acme/api", 31, RerunCursor{}, 10)
	if err != nil || len(got) != 1 || got[0].LeaseID != "covered" || got[0].Bound != 2 {
		t.Errorf("the run's candidates = %+v, %v", got, err)
	}
}

// A RUN'S CANDIDATES ARE PAGED TO THEIR END, and no other run's are among them.
func TestARunsCandidatesArePaged(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	now := time.Now()
	seedRerunRow(t, a, rerunRow{lease: "a", resultAt: now.Add(-time.Hour)})
	seedRerunRow(t, a, rerunRow{lease: "b", resultAt: now.Add(-2 * time.Hour)})
	seedRerunRow(t, a, rerunRow{lease: "c", resultAt: now.Add(-3 * time.Hour)})
	seedRerunRow(t, a, rerunRow{lease: "other-run", githubRunID: 32})

	var (
		seen   []string
		cursor RerunCursor
	)
	for range 5 {
		page, err := a.RunRerunCandidates(t.Context(), "acme/api", 31, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].LeaseID)
		cursor = page[0].Cursor
	}
	if fmt.Sprint(seen) != "[a b c]" {
		t.Errorf("pages = %v", seen)
	}
}

// THE LIST OF REQUESTS IN DOUBT NAMES A TIER THEY STILL COVER, and none once every job
// they covered is decided.
func TestAnOpenRerunNamesATierItCovers(t *testing.T) {
	t.Parallel()

	a := newRerunAllocator(t)
	seedRerunRow(t, a, rerunRow{lease: "covered"})
	request := RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: RerunFailedJobs}
	if err := a.BeginRerun(t.Context(), request, []string{"covered"}); err != nil {
		t.Fatal(err)
	}

	open, err := a.OpenReruns(t.Context(), OpenRerunCursor{}, 10)
	if err != nil || len(open) != 1 || open[0].Tier != "billet-2vcpu" {
		t.Fatalf("open = %+v, %v", open, err)
	}

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE job_history SET rerun_decision = 'expired' WHERE lease_id = 'covered'`)

		return err
	}); err != nil {
		t.Fatal(err)
	}
	if open, err := a.OpenReruns(t.Context(), OpenRerunCursor{}, 10); err != nil || len(open) != 1 || open[0].Tier != "" {
		t.Errorf("with its job decided, open = %+v, %v", open, err)
	}
}
