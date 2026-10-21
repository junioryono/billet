package alloc

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// phaseClock is an allocator clock a test moves in ten-second steps, and the
// stamp each step writes.
type phaseClock struct{ now, base time.Time }

func newPhaseClock() *phaseClock {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	return &phaseClock{now: base, base: base}
}

func (c *phaseClock) read() time.Time { return c.now }
func (c *phaseClock) step(n int)      { c.now = c.base.Add(time.Duration(n) * 10 * time.Second) }
func (c *phaseClock) at(n int) string { return ts(c.base.Add(time.Duration(n) * 10 * time.Second)) }

// A JOB KEEPS WHEN EACH PART OF ITS LIFE BEGAN, AND WHICH PROCESS HELD IT, on
// the history row that outlives its lease (#28): the purchase, the assignment,
// the launch and the process that began it, the runner's registration and its
// id, the start, custody, teardown and the end, and the process that held the
// compute when it ended, here a replacement that adopted it.
func TestAJobKeepsWhenEachPartOfItsLifeBeganAndWhoHeldIt(t *testing.T) {
	clock := newPhaseClock()
	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)}, WithClock(clock.read))
	mustRegister(t, a, NodeRegistration{Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 16 * config.GiB, Incarnation: "process-1"})

	lease := reserve(t, a, "linux")

	clock.step(1)
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 500, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	clock.step(2)
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	clock.step(3)
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance(launching): %v", err)
	}

	clock.step(4)
	runner := PoolRunner{LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11, RunnerID: 91,
		RunnerName: "billet-" + lease.ID, Minted: true}
	if err := a.RegisterPoolRunner(t.Context(), runner); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	clock.step(5)
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 91, runner.RunnerName, 11, 500,
		"job-1", JobIdentity{}); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}

	// THE HOST'S PROCESS IS REPLACED, and the replacement adopts the compute.
	mustRegister(t, a, NodeRegistration{Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 16 * config.GiB, Incarnation: "process-2"})

	clock.step(6)
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseCustody); err != nil {
		t.Fatalf("Advance(custody): %v", err)
	}

	// A REPEATED REPORT OF A PHASE KEEPS THE FIRST TIME.
	clock.step(7)
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseCustody); err != nil {
		t.Fatalf("Advance(custody) again: %v", err)
	}

	clock.step(8)
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseTeardown); err != nil {
		t.Fatalf("Advance(teardown): %v", err)
	}

	clock.step(9)
	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rec, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}

	want := []JobPhase{
		{"bought", clock.at(0)}, {"assigned", clock.at(1)}, {"launched", clock.at(3)},
		{"registered", clock.at(4)}, {"started", clock.at(5)}, {"custody", clock.at(6)},
		{"teardown", clock.at(8)}, {"finished", clock.at(9)},
	}
	if got := rec.Phases(); !slices.Equal(got, want) {
		t.Errorf("the job's phases are\n  %v\nwant\n  %v", got, want)
	}

	if rec.LaunchIncarnation != "process-1" || rec.HolderIncarnation != "process-2" {
		t.Errorf("launched by %q and held at the end by %q; want process-1, then process-2",
			rec.LaunchIncarnation, rec.HolderIncarnation)
	}

	if rec.RunnerID != 91 || rec.QuarantinedAt != "" {
		t.Errorf("runner %d, quarantined %q; want runner 91 and no quarantine", rec.RunnerID, rec.QuarantinedAt)
	}
}

// EACH PHASE'S TIME IS WRITTEN ONCE BY THE STATEMENT ITSELF, not only by the
// state machine's having no way back into a phase: a second write keeps the
// first time, and the launch's process moves with its time or not at all.
func TestAPhaseTimeIsWrittenOnce(t *testing.T) {
	clock := newPhaseClock()
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)}, WithClock(clock.read))

	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 500, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	write := func(step int, phase Phase, incarnation string) {
		t.Helper()

		if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
			return recordJobPhaseTx(t.Context(), tx, lease.ID, phase, clock.base.Add(
				time.Duration(step)*10*time.Second), incarnation)
		}); err != nil {
			t.Fatalf("record %s: %v", phase, err)
		}
	}

	for _, phase := range []Phase{PhaseLaunching, PhaseCustody, PhaseTeardown, PhaseQuarantine} {
		write(1, phase, "first")
		write(2, phase, "second")
	}

	rec, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}

	for name, got := range map[string]string{
		"launched": rec.LaunchedAt, "custody": rec.CustodyAt, "teardown": rec.TeardownAt,
		"quarantined": rec.QuarantinedAt,
	} {
		if got != clock.at(1) {
			t.Errorf("%s is %q after a second write, want the first, %q", name, got, clock.at(1))
		}
	}

	if rec.LaunchIncarnation != "first" {
		t.Errorf("the launch's process is %q after a second write, want the first", rec.LaunchIncarnation)
	}
}

// A RUNNER BILLET ADOPTED RATHER THAN MINTED HAS NO REGISTRATION TIME, and its
// id, unknown when it was journaled, is kept once a start learns it.
func TestAnAdoptedRunnerKeepsItsIDAndNoRegistrationTime(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 500, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	name := "billet-" + lease.ID
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 11, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 91, name, 11, 500, "job-1",
		JobIdentity{}); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}

	rec, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if rec.RunnerID != 91 || rec.RegisteredAt != "" {
		t.Errorf("the adopted runner's history has runner %d registered %q; want runner 91 and no "+
			"registration time", rec.RunnerID, rec.RegisteredAt)
	}
}

// A RUNNER'S HISTORY KEEPS WHAT EVERY JOURNALING LEARNS OF IT: a mint after a
// member was journaled without one records the registration and its id, and a
// recovery that found and retired a runner keeps the id it found.
func TestEveryJournalingOfARunnerKeepsWhatItLearned(t *testing.T) {
	t.Run("a mint after an adopted member", func(t *testing.T) {
		a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
			[]config.Tier{tier("linux", 2, 4*config.GiB)})
		lease := reserve(t, a, "linux")
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 500, 11); err != nil {
			t.Fatalf("Assign: %v", err)
		}

		member := PoolRunner{LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11, RunnerName: "billet-" + lease.ID}
		if err := a.RegisterPoolRunner(t.Context(), member); err != nil {
			t.Fatalf("RegisterPoolRunner (adopted): %v", err)
		}
		member.RunnerID, member.Minted = 91, true
		if err := a.RegisterPoolRunner(t.Context(), member); err != nil {
			t.Fatalf("RegisterPoolRunner (minted): %v", err)
		}

		rec, err := a.Job(t.Context(), lease.ID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if rec.RegisteredAt == "" || rec.RunnerID != 91 {
			t.Errorf("after the mint the history has registered %q and runner %d; want a time and 91",
				rec.RegisteredAt, rec.RunnerID)
		}
	})

	t.Run("an idle recovery that found the runner", func(t *testing.T) {
		a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
			[]config.Tier{tier("linux", 2, 4*config.GiB)})
		lease := reserve(t, a, "linux")
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 500, 11); err != nil {
			t.Fatalf("Assign: %v", err)
		}

		if _, err := a.RetireRecoveredPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
			LaunchRequestID: 11, RunnerID: 92, RunnerName: "billet-" + lease.ID}); err != nil {
			t.Fatalf("RetireRecoveredPoolRunner: %v", err)
		}

		rec, err := a.Job(t.Context(), lease.ID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if rec.RunnerID != 92 || rec.RegisteredAt != "" {
			t.Errorf("after the recovery the history has runner %d registered %q; want 92 and no time",
				rec.RunnerID, rec.RegisteredAt)
		}
	})
}

// THE PHASES ARE IN THE ORDER THEY HAPPENED: GitHub's start arrives
// independently of the lease's phase, so custody taken of a launch whose answer
// was lost can come before the start, and the timeline shows it first rather
// than a step back in time.
func TestThePhasesAreInTheOrderTheyHappened(t *testing.T) {
	rec := JobRecord{
		AssignedAt: "2026-10-20T12:00:01.000000000Z", LaunchedAt: "2026-10-20T12:00:02.000000000Z",
		CustodyAt: "2026-10-20T12:00:05.000000000Z", StartedAt: "2026-10-20T12:00:09.000000000Z",
		FinishedAt: "2026-10-20T12:03:00.000000000Z",
	}

	var names []string
	for _, p := range rec.Phases() {
		names = append(names, p.Name)
	}
	if want := []string{"assigned", "launched", "custody", "started", "finished"}; !slices.Equal(names, want) {
		t.Errorf("the phases are %v, want %v", names, want)
	}

	// A STAMP IN ANOTHER PRECISION, as an older binary may have written one, is
	// placed by its instant: as text, "…:00Z" sorts after "…:00.5Z".
	mixed := JobRecord{BoughtAt: "2026-10-20T12:00:00Z", AssignedAt: "2026-10-20T12:00:00.500000000Z"}
	if phases := mixed.Phases(); len(phases) != 2 || phases[0].Name != "bought" {
		t.Errorf("phases of mixed precision are %v, want bought before assigned", phases)
	}

	// A STAMP THAT DOES NOT PARSE KEEPS ITS PLACE, and the ones around it are
	// still put in order: here custody, recorded before the launch, comes first.
	malformed := JobRecord{LaunchedAt: "2026-10-20T12:00:09.000000000Z", RegisteredAt: "not a time",
		CustodyAt: "2026-10-20T12:00:05.000000000Z"}
	var around []string
	for _, p := range malformed.Phases() {
		around = append(around, p.Name)
	}
	if want := []string{"custody", "registered", "launched"}; !slices.Equal(around, want) {
		t.Errorf("phases around a stamp that does not parse are %v, want %v", around, want)
	}

	if got := (JobRecord{RunID: 600, GitHubRunID: 700}).StartedRun(); got != 700 {
		t.Errorf("a job started in run 700 on a lease assigned 600 is run %d's", got)
	}
	if got := (JobRecord{RunID: 600}).StartedRun(); got != 600 {
		t.Errorf("a job that never started, assigned run 600, is run %d's", got)
	}
}

// A QUARANTINE IS KEPT ON THE HISTORY WHEN THE REAPER MAKES IT: the lease's
// holder stopped renewing with compute behind it, and when that happened is
// part of the job's story however the quarantine ends.
func TestAQuarantineTheReaperMakesIsKeptOnTheJobsHistory(t *testing.T) {
	clock := newPhaseClock()
	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)}, WithClock(clock.read))
	mustRegister(t, a, NodeRegistration{Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 16 * config.GiB, Incarnation: "process-1"})

	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 500, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance(launching): %v", err)
	}

	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("ExpireForTest: %v", err)
	}

	clock.step(30)
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	clock.step(40)
	if err := a.ResolveQuarantine(t.Context(), lease.ID, PhaseFailed); err != nil {
		t.Fatalf("ResolveQuarantine: %v", err)
	}

	rec, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}

	if rec.QuarantinedAt != clock.at(30) || rec.FinishedAt != clock.at(40) {
		t.Errorf("quarantined %q and finished %q; want %q and %q", rec.QuarantinedAt, rec.FinishedAt,
			clock.at(30), clock.at(40))
	}
}

// A RUN RESOLVES TO THE LEASES OF ITS JOBS: those whose job started in it, and
// those assigned to it whose job never started. A lease assigned one run's job
// whose runner GitHub gave another run's belongs to the other run, the one
// that started on it. Oldest assignment first.
func TestARunResolvesToTheLeasesOfItsJobs(t *testing.T) {
	clock := newPhaseClock()
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)}, WithClock(clock.read))

	assigned := func(step int, run, request int64, startedIn int64) string {
		t.Helper()

		clock.step(step)
		lease := reserve(t, a, "linux")
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, run, request); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		if startedIn != 0 {
			if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{
				JobID: "job-" + lease.ID, RunID: startedIn,
			}); err != nil {
				t.Fatalf("RecordJobIdentity: %v", err)
			}
		}

		return lease.ID
	}

	// Assigned 600's job, given 700's by GitHub: it is 700's.
	moved := assigned(1, 600, 11, 700)
	// Assigned 600's job and started it.
	started := assigned(2, 600, 12, 600)
	// Assigned 600's job, never started: still 600's.
	waiting := assigned(3, 600, 13, 0)
	// Assigned 500's job, given 600's.
	joined := assigned(4, 500, 14, 600)

	for run, want := range map[int64][]string{
		600: {started, waiting, joined},
		700: {moved},
		500: nil,
		999: nil,
	} {
		got, more, err := a.JobsOfRun(t.Context(), run)
		if err != nil || more {
			t.Fatalf("JobsOfRun(%d): more %v, err %v", run, more, err)
		}

		if !slices.Equal(got, want) {
			t.Errorf("run %d resolves to %v, want %v", run, got, want)
		}
	}

	if _, _, err := a.JobsOfRun(t.Context(), 0); err == nil ||
		!strings.Contains(err.Error(), "a workflow run id is positive") {
		t.Errorf("run 0, which no job has: %v, want it refused as no run's id", err)
	}
}

// A RUN WITH MORE JOBS THAN THE BOUND SHOWS ITS OLDEST, IN THE ORDER THEY WERE
// ASSIGNED, and says some were left out: eight jobs whose random lease ids
// cannot all happen to sort in assignment order, against a bound of five.
func TestARunPastTheBoundShowsItsOldestJobsAndSaysSo(t *testing.T) {
	saved := jobsOfRunLimit
	jobsOfRunLimit = 5
	t.Cleanup(func() { jobsOfRunLimit = saved })

	clock := newPhaseClock()
	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)}, WithClock(clock.read))

	var assigned []string
	for i := range 8 {
		clock.step(i)
		lease := reserve(t, a, "linux")
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 800, int64(20+i)); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		assigned = append(assigned, lease.ID)
	}

	got, more, err := a.JobsOfRun(t.Context(), 800)
	if err != nil {
		t.Fatalf("JobsOfRun: %v", err)
	}
	if !more || !slices.Equal(got, assigned[:5]) {
		t.Errorf("run 800 resolves to %v (more %v), want the five assigned first, %v, and more", got, more,
			assigned[:5])
	}

	if got, more, err := a.JobsOfRun(t.Context(), 801); err != nil || more || len(got) != 0 {
		t.Errorf("a run with no jobs resolves to %v (more %v, err %v), want nothing and no more", got, more, err)
	}
}

// WHAT THE HISTORY ROW HOLDS IS WHAT A JOB'S RECORD READS: the run that
// started, the caches the node observed, the reason billet failed the lease
// and the disruption it recorded, each read back as written.
func TestAJobsRecordReadsWhatItsHistoryHolds(t *testing.T) {
	tiers := []config.Tier{tier("linux", 4, 8*config.GiB)}
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	lease := launchingLease(t, a, "linux")

	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{JobID: "job-1", RunID: 700}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}
	observed := CacheObservation{ImageCache: ImageCacheWarm, CacheGeneration: "g1",
		ActionsCache: ActionsCacheServed, BuildCaches: BuildCaches{Sticky: BuildCacheWarm,
			Git: BuildCacheCold, Bazel: BuildCacheUnused, Go: BuildCacheDisabled}}
	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch, observed); err != nil {
		t.Fatalf("RecordCacheObservation: %v", err)
	}
	if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch, "spot interruption"); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	rec, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if rec.GitHubRunID != 700 || rec.RunID != 1 {
		t.Errorf("the record's runs are started %d and assigned %d, want 700 and 1", rec.GitHubRunID, rec.RunID)
	}
	if rec.Caches != observed {
		t.Errorf("the record's caches are %+v, want %+v", rec.Caches, observed)
	}
	if rec.Disruption != string(DisruptionReclaimed) || rec.DisruptedAt == "" {
		t.Errorf("the record's disruption is %q at %q, want %q with its time", rec.Disruption, rec.DisruptedAt,
			DisruptionReclaimed)
	}
}
