package server

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
)

// stamp is a time as job_history keeps it.
func stamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }

// queueTimeReader is a provisioner whose target answers GitHub's queue times
// from a table, and keeps every run it was asked for.
type queueTimeReader struct {
	*fakeProvisioner

	mu    sync.Mutex
	runs  []int64
	times map[int64]map[string]time.Time
	err   error
}

func (r *queueTimeReader) RunQueueTimes(_ context.Context, owner, repository string, runID int64,
) (map[string]time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if owner != "acme" || repository != "api" {
		return nil, errors.New("asked for the wrong repository")
	}
	r.runs = append(r.runs, runID)
	if r.err != nil {
		return nil, r.err
	}
	return r.times[runID], nil
}

func (r *queueTimeReader) asked() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]int64(nil), r.runs...)
}

// GITHUB'S QUEUE TIME REACHES A STARTED JOB'S ROW FROM ITS RUN'S REST RECORD,
// one read per run however many of its jobs wait, matched by the runner's name
// and read from the run that actually started on a swapped runner, not the run
// it was launched for. A job the run does not list yet is asked about again
// after a backoff, not on every pass, and its wait is told once it is known.
func TestAStartedJobsQueueTimeComesFromItsRunsRecord(t *testing.T) {
	t.Parallel()

	launchedFor11, launchedFor12 := job11, job12
	p := newRunnerlessPool(t, launchedFor11, launchedFor12)
	runnerA := p.launchedFor11(t)
	runnerB, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, 12)
	if err != nil {
		t.Fatalf("the runner launched for 12: %v", err)
	}

	// Both runners start jobs of run 600, which neither was launched for.
	const ran = 600
	started := func(job dispatch.Job, runner alloc.PoolRunner, id int64) dispatch.Job {
		job.RunID = ran
		job.RunnerID, job.RunnerName = id, runner.RunnerName
		job.Owner, job.Repository = "acme", "api"
		return job
	}
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Statistics: &Statistics{TotalAssignedJobs: 2},
		Started: []dispatch.Job{started(job12, runnerA, 77), started(job13, runnerB, 78)}}); err != nil {
		t.Fatalf("start jobs 12 and 13: %v", err)
	}

	told := &waitsTold{}
	p.a.ObserveQueueWaits(told)

	now := time.Now()
	queuedA, queuedB := now.Add(-7*time.Minute).UTC().Truncate(time.Second), now.Add(-3*time.Minute).UTC().Truncate(time.Second)
	reader := &queueTimeReader{fakeProvisioner: &fakeProvisioner{}, times: map[int64]map[string]time.Time{
		ran: {provider.InstanceName(runnerA.LeaseID): queuedA},
	}}
	s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
	targets := map[string]Target{p.l.tier: {Config: config.GitHubTarget{Name: "acme"}, Provisioner: reader}}
	sweep := newQueueTimeSweep()

	s.fillQueueTimesOnce(t.Context(), targets, sweep, now)
	if got := reader.asked(); len(got) != 1 || got[0] != ran {
		t.Fatalf("the first pass read runs %v, want run %d once for both jobs", got, ran)
	}
	rowOf := func(lease string) string {
		t.Helper()
		got, err := p.a.Job(t.Context(), lease)
		if err != nil {
			t.Fatalf("Job %s: %v", lease, err)
		}
		return got.GitHubQueuedAt
	}
	if got := rowOf(runnerA.LeaseID); got != stamp(queuedA) {
		t.Errorf("runner A's row has queue time %q, want its job's created_at %s", got, stamp(queuedA))
	}
	if got := rowOf(runnerB.LeaseID); got != "" {
		t.Errorf("runner B's row has queue time %q before its run listed it", got)
	}

	s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(30*time.Second))
	if got := reader.asked(); len(got) != 1 {
		t.Errorf("a pass inside the backoff read runs %v again", got[1:])
	}

	// THE BINDING THAT NAMED RUN 600 IS GONE BEFORE THE NEXT READ, as it is once
	// a runner's compute is: the row's own record of the run that started is
	// what the sweep reads.
	if err := p.a.ForgetPoolRunner(t.Context(), runnerB.LeaseID); err != nil {
		t.Fatalf("delete runner B's pool binding: %v", err)
	}
	reader.mu.Lock()
	reader.times[ran][provider.InstanceName(runnerB.LeaseID)] = queuedB
	reader.mu.Unlock()
	s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(2*time.Minute))
	if got := rowOf(runnerB.LeaseID); got != stamp(queuedB) {
		t.Errorf("runner B's row has queue time %q after the backoff, want %s", got, stamp(queuedB))
	}
	if _, ok := sweep.retries[queueTimeRun{target: "acme", repository: "acme/api", run: ran}]; ok {
		t.Error("a run whose jobs all have their time is still scheduled for a retry")
	}

	s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(3*time.Minute))
	if got := reader.asked(); len(got) != 2 {
		t.Errorf("runs read %v; a pass with nothing awaiting a time asked GitHub anyway", got)
	}
	if _, waits := told.told(); len(waits) != 2 {
		t.Errorf("told %d waits, want one for each job", len(waits))
	}
}

// THE CONTROL PLANE RUNS THE SWEEP, as a background loop beside the listeners
// and after reconciliation, which is what gives it each tier's target. The
// sweep works in the tests above against a fake reader; what they cannot see is
// a Run that never starts it, which records no queue time and reads healthy.
func TestTheControlPlaneRunsTheQueueTimeSweep(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "Run" && fd.Recv != nil {
			run = fd
		}
	}
	if run == nil {
		t.Fatal("server.go no longer declares (*Server).Run")
	}

	var reconciled, swept token.Pos
	ast.Inspect(run.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "reconcile":
			reconciled = call.Pos()
		case "Background":
			ast.Inspect(call, func(m ast.Node) bool {
				if inner, ok := m.(*ast.CallExpr); ok {
					if s, ok := inner.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "fillQueueTimes" {
						swept = call.Pos()
					}
				}
				return true
			})
		}
		return true
	})
	if !swept.IsValid() {
		t.Fatal("(*Server).Run starts no background loop that calls fillQueueTimes")
	}
	if !reconciled.IsValid() || swept < reconciled {
		t.Errorf("(*Server).Run starts the queue-time sweep at %s, before reconcile at %s, which supplies "+
			"its targets", fset.Position(swept), fset.Position(reconciled))
	}
}

// A created_at AFTER THE JOB'S OWN HAND-OVER TO THE SCALE SET IS NOT WRITTEN,
// because a job is queued before it is handed over and both are GitHub's
// clock; the run backs off rather than being read on every pass.
func TestAQueueTimeAfterTheHandOverIsNotWritten(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	runner := p.launchedFor11(t)
	handed := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Millisecond)
	start := job11
	start.RunnerID, start.RunnerName = 77, runner.RunnerName
	start.Owner, start.Repository = "acme", "api"
	start.ScaleSetAssignedAt = handed
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{start},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11: %v", err)
	}

	for _, tc := range []struct {
		created time.Time
		want    string
	}{
		{handed.Add(time.Second), ""},
		{handed.Add(-time.Second), stamp(handed.Add(-time.Second))},
	} {
		reader := &queueTimeReader{fakeProvisioner: &fakeProvisioner{}, times: map[int64]map[string]time.Time{
			job11.RunID: {runner.RunnerName: tc.created},
		}}
		s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
		targets := map[string]Target{p.l.tier: {Config: config.GitHubTarget{Name: "acme"}, Provisioner: reader}}
		sweep := newQueueTimeSweep()
		s.fillQueueTimesOnce(t.Context(), targets, sweep, time.Now())

		got, err := p.a.Job(t.Context(), runner.LeaseID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if got.GitHubQueuedAt != tc.want {
			t.Errorf("created %s against a hand-over at %s wrote %q, want %q", tc.created, handed,
				got.GitHubQueuedAt, tc.want)
		}
		_, backedOff := sweep.retries[queueTimeRun{target: "acme", repository: "acme/api", run: job11.RunID}]
		if backedOff != (tc.want == "") {
			t.Errorf("created %s: the run is backed off %t, want %t", tc.created, backedOff, tc.want == "")
		}
	}
}

// A RUN THAT CANNOT BE READ BACKS OFF, doubling up to its bound; a tier this
// control plane does not serve, and a target that cannot read queue times, are
// never asked.
func TestAQueueTimeReadThatFailsBacksOffAndAnUnservedTierIsNotAsked(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	runner := p.launchedFor11(t)
	start := job11
	start.RunnerID, start.RunnerName = 77, runner.RunnerName
	start.Owner, start.Repository = "acme", "api"
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{start},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11: %v", err)
	}

	reader := &queueTimeReader{fakeProvisioner: &fakeProvisioner{}, err: errors.New("injected: GitHub is down")}
	s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
	sweep := newQueueTimeSweep()
	run := queueTimeRun{target: "acme", repository: "acme/api", run: job11.RunID}

	s.fillQueueTimesOnce(t.Context(), map[string]Target{"another-tier": {Provisioner: reader}}, sweep, time.Now())
	s.fillQueueTimesOnce(t.Context(), map[string]Target{p.l.tier: {Provisioner: &fakeProvisioner{}}}, sweep,
		time.Now())
	if got := reader.asked(); len(got) != 0 {
		t.Fatalf("asked for runs %v of a tier not served or a target that cannot read them", got)
	}

	targets := map[string]Target{p.l.tier: {Config: config.GitHubTarget{Name: "acme"}, Provisioner: reader}}
	now := time.Now()
	var waits []time.Duration
	for range 9 {
		s.fillQueueTimesOnce(t.Context(), targets, sweep, now)
		retry := sweep.retries[run]
		waits = append(waits, retry.wait)
		now = retry.after
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, time.Hour, time.Hour, time.Hour}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("the backoffs were %v, want %v", waits, want)
		}
	}
	if got := len(reader.asked()); got != 9 {
		t.Errorf("read the run %d times in 9 passes each at its retry, want 9", got)
	}
}

// A THROTTLE OR A REFUSAL PAUSES THE APP INSTALLATION, for fifteen minutes and
// an hour, and each pass reads at most three runs of an installation, so a
// burst of runs or a missing permission costs it a bounded number of requests.
func TestAThrottledOrRefusedTargetPausesAndAPassReadsABoundedNumberOfRuns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err   error
		pause time.Duration
	}{
		{nil, 0},
		{ErrQueueTimesThrottled, queueTimeThrottled},
		{ErrQueueTimesRefused, queueTimeRefused},
	} {
		p := newRunnerlessPool(t, job11, job12, job13,
			dispatch.Job{RequestID: 14, RunID: 104, JobID: "job-14"})
		var msgs []dispatch.Job
		for i, request := range []int64{11, 12, 13, 14} {
			runner, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, request)
			if err != nil {
				t.Fatalf("the runner launched for %d: %v", request, err)
			}
			job := dispatch.Job{RequestID: request, RunID: 700 + request, JobID: runner.LeaseID,
				RunnerID: int64(80 + i), RunnerName: runner.RunnerName, Owner: "acme", Repository: "api"}
			msgs = append(msgs, job)
		}
		if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: msgs,
			Statistics: &Statistics{TotalAssignedJobs: len(msgs)}}); err != nil {
			t.Fatalf("start four jobs of four runs: %v", err)
		}

		reader := &queueTimeReader{fakeProvisioner: &fakeProvisioner{}, err: tc.err,
			times: map[int64]map[string]time.Time{}}
		s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
		targets := map[string]Target{p.l.tier: {Config: config.GitHubTarget{Name: "acme"}, Provisioner: reader}}
		sweep := newQueueTimeSweep()
		now := time.Now()

		s.fillQueueTimesOnce(t.Context(), targets, sweep, now)
		want := queueTimeRunsPerInstallation
		if tc.err != nil {
			want = 1
		}
		if got := len(reader.asked()); got != want {
			t.Errorf("with %v the first pass read %d runs, want %d", tc.err, got, want)
		}
		if tc.err == nil {
			continue
		}
		if got := sweep.paused["target acme"]; !got.Equal(now.Add(tc.pause)) {
			t.Errorf("with %v the target is paused until %s, want %s later", tc.err, got, tc.pause)
		}
		s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(tc.pause-time.Second))
		if got := len(reader.asked()); got != 1 {
			t.Errorf("with %v a pass inside the pause read %d runs in all, want only the first", tc.err, got)
		}
		s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(tc.pause))
		if got := len(reader.asked()); got != 2 {
			t.Errorf("with %v the pass after the pause read %d runs in all, want one more", tc.err, got)
		}
	}
}

// TWO TARGETS OF ONE APP INSTALLATION SHARE ITS BUDGET AND ITS PAUSE, because
// GitHub counts the installation's requests, not billet's targets.
func TestTargetsOfOneInstallationShareItsBudgetAndItsPause(t *testing.T) {
	t.Parallel()

	shared := config.GitHubTarget{Name: "acme-api", InstallationID: 22}
	other := config.GitHubTarget{Name: "acme-web", InstallationID: 22}
	if a, b := installationOf(Target{Config: shared}), installationOf(Target{Config: other}); a != b {
		t.Fatalf("two targets of installation 22 count against %q and %q", a, b)
	}
	if a, b := installationOf(Target{Config: config.GitHubTarget{Name: "x"}}),
		installationOf(Target{Config: config.GitHubTarget{Name: "y"}}); a == b {
		t.Errorf("two targets naming no installation share %q", a)
	}

	p := newRunnerlessPool(t, job11, job12, job13, dispatch.Job{RequestID: 14, RunID: 104, JobID: "job-14"})
	var started []dispatch.Job
	for i, request := range []int64{11, 12, 13, 14} {
		runner, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, request)
		if err != nil {
			t.Fatalf("the runner launched for %d: %v", request, err)
		}
		started = append(started, dispatch.Job{RequestID: request, RunID: 800 + request, JobID: runner.LeaseID,
			RunnerID: int64(90 + i), RunnerName: runner.RunnerName, Owner: "acme", Repository: "api"})
	}
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: started,
		Statistics: &Statistics{TotalAssignedJobs: len(started)}}); err != nil {
		t.Fatalf("start four jobs of four runs: %v", err)
	}

	reader := &queueTimeReader{fakeProvisioner: &fakeProvisioner{}, times: map[int64]map[string]time.Time{}}
	s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
	// The tier's target is one of the two; what the budget counts is the
	// installation, which the second target would share.
	targets := map[string]Target{p.l.tier: {Config: shared, Provisioner: reader}}
	sweep := newQueueTimeSweep()
	sweep.paused[installationOf(Target{Config: other})] = time.Now().Add(time.Hour)
	s.fillQueueTimesOnce(t.Context(), targets, sweep, time.Now())
	if got := reader.asked(); len(got) != 0 {
		t.Errorf("read runs %v while another target of the same installation is paused", got)
	}

	// And the budget: four runs, two on each target of the installation, are
	// three reads in one pass, not three each.
	var work []queueTimeWork
	for i, target := range []config.GitHubTarget{shared, shared, other, other} {
		work = append(work, queueTimeWork{
			run:          queueTimeRun{target: target.Name, repository: "acme/api", run: int64(970 + i)},
			installation: installationOf(Target{Config: target}), reader: reader,
		})
	}
	s.readQueueTimeWork(t.Context(), work, newQueueTimeSweep(), time.Now())
	if got := reader.asked(); len(got) != queueTimeRunsPerInstallation {
		t.Errorf("two targets of one installation read runs %v in one pass, want %d in all", got,
			queueTimeRunsPerInstallation)
	}
}

// THE LEDGER IS PAGED, AND A PASS THAT STOPS AT ITS LAST PAGE RESUMES THERE, so
// jobs it cannot read yet never hide older ones it can; the newest page is read
// on every pass, so a job that just started does not wait for the backlog; and
// once a pass reaches the oldest the next begins at the newest again.
func TestTheQueueTimeSweepPagesTheLedgerAndResumes(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11, job12, job13, dispatch.Job{RequestID: 14, RunID: 104, JobID: "job-14"})
	start := func(id int64, requests ...int64) {
		t.Helper()
		var started []dispatch.Job
		for _, request := range requests {
			runner, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, request)
			if err != nil {
				t.Fatalf("the runner launched for %d: %v", request, err)
			}
			started = append(started, dispatch.Job{RequestID: request, RunID: 900 + request, JobID: runner.LeaseID,
				RunnerID: 80 + request, RunnerName: runner.RunnerName, Owner: "acme", Repository: "api"})
		}
		if err := p.l.handle(t.Context(), &Message{MessageID: id, Started: started,
			Statistics: &Statistics{TotalAssignedJobs: 4}}); err != nil {
			t.Fatalf("start jobs %v: %v", requests, err)
		}
	}
	start(2, 11, 12, 13)

	reader := &queueTimeReader{fakeProvisioner: &fakeProvisioner{}, times: map[int64]map[string]time.Time{}}
	s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
	targets := map[string]Target{p.l.tier: {Config: config.GitHubTarget{Name: "acme"}, Provisioner: reader}}
	sweep := newQueueTimeSweep()
	sweep.pageRows, sweep.pages = 1, 2
	now := time.Now()

	// Two pages of one: two of the three runs, and the cursor stops there.
	s.fillQueueTimesOnce(t.Context(), targets, sweep, now)
	if got := reader.asked(); len(got) != 2 || sweep.cursor == (alloc.QueueTimeCursor{}) {
		t.Fatalf("the first pass read runs %v and left cursor %+v; want two runs and a cursor", got,
			sweep.cursor)
	}

	// A job starts while the backlog is half read. The next pass reads the
	// newest page, which is that job, and one page past the cursor, which is the
	// third run: the two already read are inside their backoff.
	start(3, 14)
	s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(time.Second))
	got := reader.asked()
	seen := map[int64]bool{}
	for _, run := range got {
		seen[run] = true
	}
	if len(got) != 4 || !seen[911] || !seen[912] || !seen[913] || !seen[914] {
		t.Fatalf("two passes read runs %v, want 911 to 914 once each, the newly started 914 among them", got)
	}

	// Past the oldest is an empty page, which sends the cursor back to the newest.
	s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(2*time.Second))
	if sweep.cursor != (alloc.QueueTimeCursor{}) {
		t.Error("a pass that reached the oldest job left the cursor where it stopped")
	}
	if got := len(reader.asked()); got != 4 {
		t.Errorf("a pass with every run backed off read %d runs in all, want still 4", got)
	}
}

// A JOB THE NEWEST PAGE AND THE BACKLOG BOTH READ IS GATHERED ONCE, so a second
// copy's write cannot back off a run whose time the first recorded.
func TestAJobBothScansReadIsGatheredOnce(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11, job12, job13)
	var started []dispatch.Job
	for _, request := range []int64{11, 12, 13} {
		runner, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, request)
		if err != nil {
			t.Fatalf("the runner launched for %d: %v", request, err)
		}
		started = append(started, dispatch.Job{RequestID: request, RunID: 990, JobID: runner.LeaseID,
			RunnerID: 60 + request, RunnerName: runner.RunnerName, Owner: "acme", Repository: "api"})
	}
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: started,
		Statistics: &Statistics{TotalAssignedJobs: 3}}); err != nil {
		t.Fatalf("start three jobs of one run: %v", err)
	}

	now := time.Now()
	newest, err := p.a.JobsAwaitingQueueTime(t.Context(), now.Add(-queueTimeWindow), alloc.QueueTimeCursor{}, 1)
	if err != nil || len(newest) != 1 {
		t.Fatalf("the newest job awaiting a queue time: %v, %v", newest, err)
	}

	reader := &queueTimeReader{fakeProvisioner: &fakeProvisioner{}}
	s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
	targets := map[string]Target{p.l.tier: {Config: config.GitHubTarget{Name: "acme"}, Provisioner: reader}}
	sweep := newQueueTimeSweep()
	// Pages of two from a cursor one row in: the newest page holds the first two
	// jobs and the backlog's the second and third, so the second is read twice.
	sweep.pageRows, sweep.pages, sweep.cursor = 2, 2, newest[0].Cursor

	work, ok := s.gatherQueueTimeWork(t.Context(), targets, sweep, now)
	if !ok || len(work) != 1 {
		t.Fatalf("gathered %d runs (%t), want the one", len(work), ok)
	}
	leases := map[string]int{}
	for _, job := range work[0].jobs {
		leases[job.LeaseID]++
	}
	if len(work[0].jobs) != 3 || len(leases) != 3 {
		t.Errorf("gathered %d jobs of %d leases, want each of the three once", len(work[0].jobs), len(leases))
	}
}

// budgetedReader is a queueTimeReader whose installation states a budget.
type budgetedReader struct {
	*queueTimeReader

	remaining, limit int64
	reset            time.Time
	stated           bool
}

func (r *budgetedReader) RESTHeadroom() (int64, int64, time.Time, bool) {
	return r.remaining, r.limit, r.reset, r.stated
}

// AN INSTALLATION DOWN TO THE LAST FIFTH OF ITS HOUR'S BUDGET IS LEFT IT until
// GitHub's next hour begins, because the runner-group checks and the cache
// authority draw on the same budget and a missing queue time decides nothing.
// A budget no answer has stated, or one whose hour has ended, holds nothing
// back: the read's own throttle still pauses the installation.
func TestAnInstallationDownToItsReserveIsNotRead(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11, job12, job13)
	var started []dispatch.Job
	for i, request := range []int64{11, 12, 13} {
		runner, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, request)
		if err != nil {
			t.Fatalf("the runner launched for %d: %v", request, err)
		}
		started = append(started, dispatch.Job{RequestID: request, RunID: 600 + request, JobID: runner.LeaseID,
			RunnerID: int64(40 + i), RunnerName: runner.RunnerName, Owner: "acme", Repository: "api"})
	}
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: started,
		Statistics: &Statistics{TotalAssignedJobs: len(started)}}); err != nil {
		t.Fatalf("start three jobs of three runs: %v", err)
	}

	now := time.Now()
	reset := now.Add(20 * time.Minute)
	reader := &budgetedReader{queueTimeReader: &queueTimeReader{fakeProvisioner: &fakeProvisioner{},
		times: map[int64]map[string]time.Time{}}, remaining: 999, limit: 5000, reset: reset, stated: true}
	s := New(p.a, reader, []config.Tier{tier(p.l.tier)}, "test", nil)
	targets := map[string]Target{p.l.tier: {Config: config.GitHubTarget{Name: "acme", InstallationID: 22},
		Provisioner: reader}}
	sweep := newQueueTimeSweep()

	s.fillQueueTimesOnce(t.Context(), targets, sweep, now)
	if got := reader.asked(); len(got) != 0 {
		t.Errorf("an installation with 999 of 5000 left read runs %v", got)
	}
	if !sweep.reserved["installation 22"] {
		t.Error("the sweep does not remember the installation it is leaving its reserve to")
	}

	// A fifth left exactly is not yet the reserve, and a pass reads all it may.
	reader.remaining = 1000
	s.fillQueueTimesOnce(t.Context(), targets, sweep, now.Add(time.Second))
	if got := len(reader.asked()); got != queueTimeRunsPerInstallation {
		t.Errorf("an installation with 1000 of 5000 left read %d runs, want %d", got, queueTimeRunsPerInstallation)
	}
	if sweep.reserved["installation 22"] {
		t.Error("an installation read again is still remembered as held")
	}

	// Every run is inside its backoff after an empty answer; past it, nothing
	// left before the reset reads nothing, and the reset itself reads again.
	later := now.Add(2 * queueTimeRetryFirst)
	reader.remaining, reset = 0, later.Add(time.Minute)
	reader.reset = reset
	s.fillQueueTimesOnce(t.Context(), targets, sweep, reset.Add(-time.Second))
	if got := len(reader.asked()); got != queueTimeRunsPerInstallation {
		t.Errorf("an installation with nothing left before its reset read %d runs in all, want still %d", got,
			queueTimeRunsPerInstallation)
	}
	s.fillQueueTimesOnce(t.Context(), targets, sweep, reset)
	if got := len(reader.asked()); got != 2*queueTimeRunsPerInstallation {
		t.Errorf("an installation whose hour has ended read %d runs in all, want %d more", got,
			queueTimeRunsPerInstallation)
	}

	// Targets of one installation each keep the statement their own requests
	// drew, and one in reserve holds the installation for all of them.
	low := &budgetedReader{queueTimeReader: reader.queueTimeReader, remaining: 500, limit: 5000,
		reset: now.Add(time.Hour), stated: true}
	ample := &budgetedReader{queueTimeReader: reader.queueTimeReader, remaining: 4000, limit: 5000,
		reset: now.Add(time.Hour), stated: true}
	before := len(reader.asked())
	var shared []queueTimeWork
	for i, h := range []*budgetedReader{ample, low, ample} {
		shared = append(shared, queueTimeWork{
			run:          queueTimeRun{target: "acme-" + strconv.Itoa(i), repository: "acme/api", run: int64(980 + i)},
			installation: "installation 22", reader: h, headroom: h,
		})
	}
	s.readQueueTimeWork(t.Context(), shared, newQueueTimeSweep(), now)
	if got := len(reader.asked()) - before; got != 0 {
		t.Errorf("an installation one of whose targets saw it in reserve read %d runs through the others", got)
	}

	// A budget no answer has stated holds nothing back.
	reader.stated, reader.reset = false, reset.Add(time.Hour)
	s.fillQueueTimesOnce(t.Context(), targets, sweep, reset.Add(4*queueTimeRetryFirst))
	if got := len(reader.asked()); got != 3*queueTimeRunsPerInstallation {
		t.Errorf("an installation that has stated no budget read %d runs in all, want %d more", got,
			queueTimeRunsPerInstallation)
	}
}
