package alloc

import (
	"database/sql"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func quiescenceAllocator(t *testing.T) *Allocator {
	t.Helper()

	return newAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{tier("quiet-tier", 2, 4*config.GiB)})
}

func seal(t *testing.T, a *Allocator) {
	t.Helper()

	if _, err := a.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}
}

func quiescence(t *testing.T, a *Allocator) Quiescence {
	t.Helper()

	q, err := a.Quiescence(t.Context())
	if err != nil {
		t.Fatalf("Quiescence: %v", err)
	}

	return q
}

// QUIET MEANS BOTH HALVES: nothing running, AND nothing new can arrive. A
// deployment that happens to be idle is not drained — the next poll may fill it,
// and a shutdown that stopped there would be stopping into work it invited.
func TestQuietRequiresTheSealAsWellAsTheSilence(t *testing.T) {
	t.Parallel()

	a := quiescenceAllocator(t)

	// Idle and OPEN: not quiet, however empty it looks.
	if q := quiescence(t, a); q.Quiet() {
		t.Fatal("an open but idle deployment reported itself drained")
	}

	seal(t, a)

	if q := quiescence(t, a); !q.Quiet() {
		t.Fatalf("a sealed and idle deployment is not quiet: %+v", q)
	}
}

// A RUNNING JOB IS NOT QUIESCENCE, and it is named rather than counted: a report
// an operator cannot recognise their own work in is not a report.
func TestRunningWorkKeepsADeploymentFromQuiescing(t *testing.T) {
	t.Parallel()

	// THE REAL SEQUENCE: work that was already running when the operator sealed.
	// Nothing can be reserved after the seal, so a job that outlasts one always
	// started before it.
	a := quiescenceAllocator(t)

	lease, err := a.Reserve(t.Context(), "quiet-tier")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "test-host-firecracker"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	advanceTo(t, a, lease, PhaseBusy)
	seal(t, a)

	q := quiescence(t, a)
	if q.Quiet() {
		t.Fatal("a deployment running a job reported itself drained")
	}
	if len(q.Outstanding) != 1 {
		t.Fatalf("outstanding = %d, want the one running job: %+v", len(q.Outstanding), q.Outstanding)
	}

	o := q.Outstanding[0]
	if o.ID != lease.ID || o.Phase != PhaseBusy || o.Tier != "quiet-tier" {
		t.Errorf("the outstanding lease is %+v, want the running one", o)
	}
	// Its registration is not confirmed removed: nothing has deregistered it.
	if q.RegistrationUnconfirmed() != 1 {
		t.Errorf("unconfirmed = %d, want the running job", q.RegistrationUnconfirmed())
	}
}

// EVERY PHASE THAT IMPLIES COMPUTE OR ROUTING BLOCKS.
//
// THE LIST IS WRITTEN OUT HERE rather than ranged over from production. Reading
// `quiescencePhases` would make the test agree with whatever the code said:
// deleting a phase would delete the case that covers it, and the suite would
// stay green while a drain stopped waiting for compute that is still running.
// Measured — that mutation survived until this was written out.
func TestEveryPhaseHoldingComputeOrRoutingBlocks(t *testing.T) {
	t.Parallel()

	for _, phase := range []Phase{
		PhaseAssigned, PhaseLaunching, PhaseOnline, PhaseBusy,
		PhaseCustody, PhaseTeardown, PhaseQuarantine,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()

			a := quiescenceAllocator(t)

			lease, err := a.Reserve(t.Context(), "quiet-tier")
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "test-host-firecracker"); err != nil {
				t.Fatalf("Bind: %v", err)
			}

			setPhase(t, a, lease.ID, phase)
			seal(t, a)

			q := quiescence(t, a)
			if q.Quiet() {
				t.Fatalf("a lease in %s did not hold the drain", phase)
			}
			if len(q.Outstanding) != 1 || q.Outstanding[0].Phase != phase {
				t.Errorf("outstanding = %+v, want the %s lease", q.Outstanding, phase)
			}
		})
	}
}

// ESCROWED CAPACITY HOLDS THE DRAIN, and an earlier version of this asserted
// the opposite.
//
// The reasoning for excluding it was that such a lease holds no compute and
// carries no runner — true, and not the question. While the listener is alive it
// still ADVERTISES that lease, so GitHub can assign against it and it becomes
// running work with nothing having changed on the host and admission never
// having reopened. A barrier that sampled before that transition would report
// quiet about a deployment that was about to start a job.
func TestEscrowedCapacityHoldsTheDrain(t *testing.T) {
	t.Parallel()

	a := quiescenceAllocator(t)

	if _, err := a.Escrow(t.Context(), "quiet-tier", 3); err != nil {
		t.Fatalf("Escrow: %v", err)
	}
	seal(t, a)

	q := quiescence(t, a)
	if q.Quiet() {
		t.Fatal("a deployment still advertising escrowed capacity reported itself drained; " +
			"GitHub can assign against it and it becomes a running job")
	}
	if q.Escrowed() != 3 {
		t.Errorf("escrowed = %d, want the three held leases: %+v", q.Escrowed(), q.Outstanding)
	}

	// REPORTED SEPARATELY, because the two are different waits: running work ends
	// by finishing, escrow ends when the listener releases it.
	for _, o := range q.Outstanding {
		if o.Phase != PhaseCapacity {
			t.Errorf("outstanding holds a %s lease, want only escrow: %+v", o.Phase, o)
		}
	}

	// AND RELEASING IT QUIESCES, so this is a wait rather than a deadlock.
	for _, o := range q.Outstanding {
		lease, err := a.Lease(t.Context(), o.ID)
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}
		if err := a.Release(t.Context(), o.ID, lease.Epoch, PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}

	if q := quiescence(t, a); !q.Quiet() {
		t.Fatalf("a sealed deployment holding nothing is not quiet: %+v", q.Outstanding)
	}
}

// A TERMINAL LEASE HOLDS NOTHING. Counting them would mean a deployment never
// quiesces after its first job.
func TestFinishedWorkDoesNotHoldTheDrain(t *testing.T) {
	t.Parallel()

	a := quiescenceAllocator(t)

	lease, err := a.Reserve(t.Context(), "quiet-tier")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "test-host-firecracker"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	advanceTo(t, a, lease, PhaseBusy)

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	seal(t, a)

	if q := quiescence(t, a); !q.Quiet() {
		t.Fatalf("a finished job held the drain: %+v", q.Outstanding)
	}
}

// DEREGISTRATION SEPARATES WORK WHOSE REGISTRATION IS STILL OUT THERE FROM
// COMPUTE BILLET IS ONLY FINISHING WITH. Both hold the drain; the flag says
// which is which, and says no more than it proves — an unset flag covers a
// lease that never registered at all.
func TestDeregistrationIsReportedSeparately(t *testing.T) {
	t.Parallel()

	a := quiescenceAllocator(t)

	lease, err := a.Reserve(t.Context(), "quiet-tier")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "test-host-firecracker"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	setPhase(t, a, lease.ID, PhaseTeardown)
	seal(t, a)

	if q := quiescence(t, a); q.RegistrationUnconfirmed() != 1 {
		t.Fatalf("a teardown whose deregistration is unconfirmed is not reported as such: %+v",
			q.Outstanding)
	}

	markDeregistered(t, a, lease.ID)

	q := quiescence(t, a)
	if q.Quiet() {
		t.Fatal("a teardown stopped holding the drain once its runner was deregistered; its " +
			"compute is still being destroyed")
	}
	if q.RegistrationUnconfirmed() != 0 {
		t.Errorf("a deregistered lease is still counted as unconfirmed: %+v", q.Outstanding)
	}
}

// AN UNREADABLE ADMISSION STATE IS NOT AN OPEN ONE, here as everywhere else.
//
// The earlier version of this wrote an ordinary SEAL and asserted the
// deployment reported itself sealed — which the unknown handling has nothing to
// do with, and which would have passed with that handling deleted.
func TestQuiescenceTreatsAnUnreadableAdmissionStateAsSealed(t *testing.T) {
	t.Parallel()

	a := quiescenceAllocator(t)

	// The same reason the other two carry: the illegal row is written by turning
	// CHECK constraints off, which is a PRAGMA. Without this the PostgreSQL run
	// reaches the skip below and reports a syntax error as "this SQLite build
	// enforces the check regardless" — a skip whose stated reason is untrue,
	// which is worse than no skip at all.
	skipOnPostgres(t, "PRAGMA ignore_check_constraints is how the illegal row is written")

	// Written underneath the constraint, the way a future migration or a
	// hand-edited ledger could.
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`PRAGMA ignore_check_constraints = ON`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			`UPDATE admission SET mode = 'quiescing' WHERE id = 1`)

		return err
	}); err != nil {
		t.Skipf("this SQLite build enforces the check regardless: %v", err)
	}

	if admission, err := a.db.Admission(t.Context()); err != nil {
		t.Fatalf("Admission: %v", err)
	} else if admission.Mode != state.AdmissionUnknown {
		t.Skipf("the fixture did not reach the case: mode is %v", admission.Mode)
	}

	q := quiescence(t, a)
	if !q.Sealed {
		t.Fatal("an admission state billet could not read was treated as open, so a drain " +
			"would have called an unreadable deployment quiet")
	}
}

// setPhase moves a lease straight to a phase, including ones the ordinary
// transitions only reach through a failure. The drain must wait for a lease in
// any of them however it got there.
func setPhase(t *testing.T, a *Allocator, leaseID string, phase Phase) {
	t.Helper()

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE leases SET phase = $1 WHERE id = $2`, string(phase), leaseID)

		return err
	}); err != nil {
		t.Fatalf("set phase %s: %v", phase, err)
	}
}

// markDeregistered records that GitHub's runner registration has been removed,
// which is the durable fact separating work GitHub can still route from compute
// billet is only finishing with.
func markDeregistered(t *testing.T, a *Allocator, leaseID string) {
	t.Helper()

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE leases SET deregistered = 1 WHERE id = $1`, leaseID)

		return err
	}); err != nil {
		t.Fatalf("mark deregistered: %v", err)
	}
}
