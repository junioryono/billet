package alloc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// computeAbsenceGrace is how long a host must report running NOTHING, without
// interruption, before that is believed.
//
// THE SAME SIZE AS custody's strayGrace AND quarantineGrace, and deliberately
// so: all three answer one question — how long can a create the daemon has
// already accepted stay invisible to a listing — and three different answers to
// it would be three different opinions about the same provider. The docker
// backend launches with `docker run --detach`, which pulls the image inline, so
// a multi-gigabyte runner image on a slow link is minutes rather than seconds.
//
// IT APPLIES TO EVERY BACKEND, and the version of this rule that did not was
// backwards. Custody grants a prompt absence only where the launch or the
// inventory was CAUSALLY OBSERVED — a fact about one entry billet watched — and
// a barrier has no such observation about compute whose lease is already gone,
// which is the entire class it exists for. So the window is unconditional here.
//
// Cheap to be wrong in one direction only. Waiting costs five minutes on a wait
// that is already unbounded; not waiting reports a host clear while a guest is
// still coming up, and `billet local down` stops services on that answer.
const computeAbsenceGrace = 5 * time.Minute

// BumpDispatch advances a host's launch-dispatch fence and returns the new value.
//
// IT COUNTS LAUNCHES, NOT COMMANDS. A destroy, sweep, tend or inventory cannot
// create compute, so charging them would void proofs for no reason.
//
// The CALLER's ordering is the invariant, not this function: the plane advances
// this BEFORE the launch becomes reachable in a node's queue, under one hold of
// its mutex. A bump taken outside that hold can be observed by a barrier that is
// then queued AHEAD of the launch, and the acknowledgement would be accepted
// with a launch still to run.
func (a *Allocator) BumpDispatch(ctx context.Context, node string) (int64, error) {
	var generation int64

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		generation = 0

		var err error

		generation, err = state.WriteQueries(tx).BumpDispatchGeneration(ctx, node)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A host with no row cannot be proved clear either — every clearance
			// walks the node table — so there is no fence to advance and nothing
			// about it that could later be believed.
			return nil
		case err != nil:
			return fmt.Errorf("alloc: advance the launch fence of node %s: %w", node, err)
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	return generation, nil
}

// NodeFence is the pair a barrier observation has to be taken against.
type NodeFence struct {
	// Epoch is the registration in force. A reconnect bumps it, which is what
	// makes a proof about a previous incarnation stop counting.
	Epoch int64
	// Dispatch is the launch-dispatch generation. A launch handed to this host
	// after the barrier was issued has already moved it.
	Dispatch int64
	// WireVersion is the protocol registration settled on, which decides whether
	// this host can be ASKED at all.
	WireVersion int
	// Live is whether the deployment could reach it when it last looked. It is
	// NOT part of the fence — see clearanceOf — and travels here only so one read
	// answers the whole question.
	Live bool
}

// NodeFenceOf reads the fence a barrier observation of this host must carry.
func (a *Allocator) NodeFenceOf(ctx context.Context, node string) (NodeFence, bool, error) {
	var (
		fence NodeFence
		found bool
	)

	err := a.db.View(ctx, func(q querier) error {
		found = false

		row, err := state.ReadQueries(q).ReadNodeBarrierFence(ctx, node)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the barrier fence of node %s: %w", node, err)
		}

		found = true
		fence = NodeFence{
			Epoch:       row.Epoch,
			Dispatch:    row.DispatchGeneration,
			WireVersion: int(row.WireVersion),
			Live:        row.Live == 1,
		}

		return nil
	})
	if err != nil {
		return NodeFence{}, false, err
	}

	return fence, found, nil
}

// ComputeBarrier is one durable request to prove the fleet is running nothing.
//
// A SINGLETON, AND SCOPED TO AN ADMISSION GENERATION. `billet drain` is a
// separate process with no handle to the running plane, so the request has to be
// durable and observed rather than called. Concurrent waiters join one barrier
// id — superseding would reset the continuous runs the earlier waiter is already
// most of the way through — and a resume moves the generation, after which the
// request can no longer mean anything and the plane drops it.
type ComputeBarrier struct {
	ID string
	// Generation is the admission generation this barrier was requested under.
	Generation  int64
	RequestedAt string
	RequestedBy string
}

// RequestComputeBarrier records that somebody wants the fleet proved clear, and
// returns the barrier now in force.
//
// IDEMPOTENT WITHIN A GENERATION. A second waiter under the same admission
// generation joins the existing barrier rather than minting one, because a new
// id resets every host's continuous-empty run and two waiters would otherwise
// starve each other indefinitely.
func (a *Allocator) RequestComputeBarrier(
	ctx context.Context, generation int64, actor string,
) (ComputeBarrier, error) {
	var barrier ComputeBarrier

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		barrier = ComputeBarrier{}

		q := state.WriteQueries(tx)

		current, found, err := readBarrierTx(ctx, q)
		if err != nil {
			return err
		}

		if found && current.Generation == generation {
			barrier = current

			return nil
		}

		id, err := newBarrierID()
		if err != nil {
			return err
		}

		barrier = ComputeBarrier{
			ID: id, Generation: generation,
			RequestedAt: ts(a.now().UTC()), RequestedBy: actor,
		}

		// THE PREVIOUS BARRIER'S OBSERVATIONS GO WITH IT. They were taken under a
		// generation somebody has since moved, so admission was open in between and
		// the fleet may have taken work — the exact thing a continuous run claims
		// did not happen.
		if err := q.DeleteEveryBarrierRun(ctx); err != nil {
			return fmt.Errorf("alloc: clear a superseded barrier's observations: %w", err)
		}

		if err := q.UpsertComputeBarrier(ctx, ledgerdb.UpsertComputeBarrierParams{
			BarrierID:           barrier.ID,
			AdmissionGeneration: barrier.Generation,
			RequestedAt:         barrier.RequestedAt,
			RequestedBy:         barrier.RequestedBy,
		}); err != nil {
			return fmt.Errorf("alloc: record a compute barrier: %w", err)
		}

		return nil
	})
	if err != nil {
		return ComputeBarrier{}, err
	}

	return barrier, nil
}

// AdmissionGeneration is the generation a compute barrier is scoped to, and
// whether the deployment is refusing work at it.
//
// BOTH, BECAUSE EITHER ALONE IS A LOOPHOLE. The generation moves on every seal
// and every resume, so comparing it catches a deployment that was reopened —
// including one reopened and resealed, which a boolean would miss entirely. The
// boolean catches the other direction: a barrier requested against an OPEN
// deployment, which no billet command does today and which would otherwise be
// serviced as if it meant something.
func (a *Allocator) AdmissionGeneration(ctx context.Context) (int64, bool, error) {
	admission, err := a.db.Admission(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("alloc: read admission for a compute barrier: %w", err)
	}

	return admission.Generation, admission.Sealed(), nil
}

// ComputeBarrierInForce reports the durable request, if there is one.
func (a *Allocator) ComputeBarrierInForce(ctx context.Context) (ComputeBarrier, bool, error) {
	var (
		barrier ComputeBarrier
		found   bool
	)

	err := a.db.View(ctx, func(q querier) error {
		var err error
		barrier, found, err = readBarrierTx(ctx, state.ReadQueries(q))

		return err
	})
	if err != nil {
		return ComputeBarrier{}, false, err
	}

	return barrier, found, nil
}

// DropComputeBarrier removes a request that can no longer mean anything, and its
// observations with it.
//
// FENCED ON THE ID, so a loop dropping the barrier it was working on cannot drop
// one a waiter has just replaced it with.
func (a *Allocator) DropComputeBarrier(ctx context.Context, id string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		current, found, err := readBarrierTx(ctx, q)
		if err != nil {
			return err
		}

		if !found || current.ID != id {
			return nil
		}

		if err := q.DeleteComputeBarrier(ctx); err != nil {
			return fmt.Errorf("alloc: drop the compute barrier: %w", err)
		}

		if err := q.DeleteEveryBarrierRun(ctx); err != nil {
			return fmt.Errorf("alloc: drop a barrier's observations: %w", err)
		}

		return nil
	})
}

func readBarrierTx(ctx context.Context, q state.ReadOps) (ComputeBarrier, bool, error) {
	row, err := q.ReadComputeBarrier(ctx)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ComputeBarrier{}, false, nil
	case err != nil:
		return ComputeBarrier{}, false, fmt.Errorf("alloc: read the compute barrier: %w", err)
	}

	return ComputeBarrier{
		ID:          row.BarrierID,
		Generation:  row.AdmissionGeneration,
		RequestedAt: row.RequestedAt,
		RequestedBy: row.RequestedBy,
	}, true, nil
}

// BarrierObservation is one host's fenced answer to "what are you running".
type BarrierObservation struct {
	Node      string
	BarrierID string
	// Fence is what the caller captured BEFORE it asked. Both halves are compared
	// against the ledger inside the recording transaction.
	Fence NodeFence
	// Empty says the host read its provider and found no billet compute. A host
	// that could not read its provider, or that reported instances, is not empty
	// and its run is discarded.
	Empty bool
}

// RecordBarrierObservation stores one fenced observation, or ends the run.
//
// THE WHOLE CONTENT OF THIS FUNCTION IS ITS REFUSALS. A launch dispatched after
// the barrier was issued has already advanced dispatch_generation, so this write
// matches nothing and the round is simply lost — which is the point. The
// alternative shape, where a launch INVALIDATES an acknowledgement, races the
// response it is invalidating: nonce N is queued, a later launch clears N, and
// the node then reports N and writes it back as valid while that launch is still
// waiting behind it.
//
// A NON-EMPTY OR FAILED ANSWER DELETES THE ROW. What is stored is a CONTINUOUS
// RUN, not a snapshot, so an interruption ends it rather than ageing it.
func (a *Allocator) RecordBarrierObservation(ctx context.Context, obs BarrierObservation) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		current, found, err := readBarrierTx(ctx, q)
		if err != nil {
			return err
		}

		if !found || current.ID != obs.BarrierID {
			return nil
		}

		fence, err := q.ReadNodeFence(ctx, obs.Node)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the barrier fence of node %s: %w", obs.Node, err)
		}

		if fence.Epoch != obs.Fence.Epoch || fence.DispatchGeneration != obs.Fence.Dispatch {
			// The registration moved, or a launch was dispatched. Either way this
			// answer describes a host in a state that is no longer current, and any
			// run it was continuing is over.
			if err := q.DeleteBarrierRun(ctx, obs.Node); err != nil {
				return fmt.Errorf("alloc: discard a stale barrier run for %s: %w", obs.Node, err)
			}

			return nil
		}

		if !obs.Empty {
			if err := q.DeleteBarrierRun(ctx, obs.Node); err != nil {
				return fmt.Errorf("alloc: end the barrier run for %s: %w", obs.Node, err)
			}

			return nil
		}

		now := ts(a.now().UTC())

		// THE RUN'S START IS PRESERVED, and only where every part of its identity
		// matches. Taking `excluded.empty_since` unconditionally would restart the
		// clock on every sample, so a host could never cross the grace no matter how
		// long it stayed empty.
		if err := q.RecordBarrierRun(ctx, ledgerdb.RecordBarrierRunParams{
			Node:               obs.Node,
			BarrierID:          obs.BarrierID,
			NodeEpoch:          fence.Epoch,
			DispatchGeneration: fence.DispatchGeneration,
			Now:                now,
		}); err != nil {
			return fmt.Errorf("alloc: record a barrier observation for %s: %w", obs.Node, err)
		}

		return nil
	})
}

// InvalidateBarrierRun discards a host's continuous-empty run.
//
// CALLED WHEN A REGISTRATION BEGINS, not when it commits. The ledger's epoch —
// which is what fences a run — does not move until the registration's write
// lands, and `billet drain` reads this ledger from ANOTHER PROCESS where the
// plane's in-flight-registration state is invisible. So between a replacement
// arriving and its epoch committing, a completed run reads as current and the
// fleet can be reported clear about a host whose new incarnation may be holding
// compute the old one never saw.
//
// UNCONDITIONAL, and cheap because it names one row. Discarding a run that did
// not need discarding costs one barrier round; keeping one that did costs a
// stopped service on a machine running somebody's job.
// IT READS BEFORE IT WRITES, and that is not a micro-optimisation. This now runs
// on EVERY registration, ahead of the revocation check — so a credential an
// operator has taken back can call it as fast as it can open connections, and an
// unconditional `db.Tx` would let it reserve SQLite's single writer slot over and
// over, starving every scheduling decision in the process. The overwhelmingly
// common case is a host with no run at all, and the read pool answers that.
//
// The gap between the read and the skipped write is closed by the epoch: a run
// recorded in it belongs to an incarnation whose registration is about to move
// the epoch, which discards it. Where that registration is REFUSED and the epoch
// never moves, the run can only have come from another process answering under
// the same name — which is the two-hosts-one-identity residual this design
// documents rather than closes.
func (a *Allocator) InvalidateBarrierRun(ctx context.Context, node string) error {
	var found bool

	if err := a.db.View(ctx, func(q querier) error {
		var err error
		found, err = state.ReadQueries(q).BarrierRunExists(ctx, node)

		return err
	}); err != nil {
		return fmt.Errorf("alloc: look for node %s's idle run: %w", node, err)
	}

	if !found {
		return nil
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).DeleteBarrierRun(ctx, node); err != nil {
			return fmt.Errorf("alloc: discard node %s's idle run: %w", node, err)
		}

		return nil
	})
}

// InvalidateEveryBarrierRun discards what EVERY host had proved.
//
// FOR THE ARRIVAL THAT CANNOT BE ATTRIBUTED. A loopback wire requires no
// certificate, so a registration whose body cannot be decoded — an unknown field
// from a node rolled ahead of the control plane, an oversized body — names no
// host at all, and those refusals are permanent ones a node does not retry.
// Something arrived, billet cannot tell what, and "I could not tell" must not
// read as "nothing changed".
//
// OVER-INVALIDATION IS THE POINT, and it is cheap where it happens: a loopback
// deployment is one machine, so this is one host's barrier round. It is never
// reached on the real wire, where the certificate names the host.
func (a *Allocator) InvalidateEveryBarrierRun(ctx context.Context) error {
	var found bool

	if err := a.db.View(ctx, func(q querier) error {
		var err error
		found, err = state.ReadQueries(q).AnyBarrierRunExists(ctx)

		return err
	}); err != nil {
		return fmt.Errorf("alloc: look for the fleet's idle runs: %w", err)
	}

	if !found {
		return nil
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).DeleteEveryBarrierRun(ctx); err != nil {
			return fmt.Errorf("alloc: discard the fleet's idle runs: %w", err)
		}

		return nil
	})
}

// ClearanceState is what billet can say about ONE host's compute.
//
// THE ZERO VALUE IS NOT "PROVED", and that is the whole reason this is an enum
// rather than a bool beside a count. An earlier inventory shape paired a
// `Current bool` with a `Running int`, and the natural thing to write against it
// is `Current && Running == 0` — which is exactly the clearance nothing here may
// be turned into by accident.
type ClearanceState int

const (
	// ClearanceUnknown is the fail-closed zero value.
	ClearanceUnknown ClearanceState = iota
	// ClearanceProved is a fenced, continuously empty run past the grace.
	ClearanceProved
	// ClearanceRunning is the host saying it IS running billet compute. The one
	// answer here worth acting on.
	ClearanceRunning
	// ClearanceSettling is an empty run that has not yet lasted long enough.
	ClearanceSettling
	// ClearanceWaiting is a host that has not given a fenced answer under this
	// barrier — it has not been asked yet, or its answer did not arrive.
	ClearanceWaiting
	// ClearanceUnreachable is a host this deployment cannot reach, so it cannot
	// be asked. It is NOT excluded: its compute may be running.
	ClearanceUnreachable
	// ClearanceBelowProtocol is a host whose registration negotiated a wire with
	// no inventory command. It can never answer, and must never be assumed.
	ClearanceBelowProtocol
)

func (s ClearanceState) String() string {
	switch s {
	case ClearanceProved:
		return "proved clear"
	case ClearanceRunning:
		return "SAYS IT IS RUNNING WORK"
	case ClearanceSettling:
		return "empty, still settling"
	case ClearanceWaiting:
		return "has not answered"
	case ClearanceUnreachable:
		return "unreachable, so it cannot be asked"
	case ClearanceBelowProtocol:
		return "too old to be asked"
	case ClearanceUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// NodeClearance is what one host contributes to the fleet's answer.
type NodeClearance struct {
	Node  string
	State ClearanceState
	// EmptySince is when the current continuous empty run began, or empty.
	EmptySince string
	// ClearAt is when that run crosses the grace, or empty.
	ClearAt string
	// WireVersion is what registration settled on, for the diagnostic a
	// ClearanceBelowProtocol host needs.
	WireVersion int
}

// Exclusion is a host a person removed from the expected set.
type Exclusion struct {
	Node string
	// Proven says whether the removal was authorised by a current clearance. An
	// UNPROVEN exclusion is billet admitting it does not know what is on that
	// machine, and a report that treats the two the same is what this exists to
	// prevent.
	Proven bool
	Actor  string
	At     string
}

// ComputeClearance is the fleet's answer to "is anything still running".
//
// SEPARATE FROM Quiescence, DELIBERATELY. That type means exactly "sealed, and
// no non-terminal ledger lease", and its comments already disclaim machine
// proof. Folding this into Quiet() would silently change what every existing
// caller of it is told.
type ComputeClearance struct {
	// Requested says a durable barrier is in force. Without one, nothing here is
	// evidence of anything: no host has been asked.
	Requested  bool
	BarrierID  string
	Generation int64
	// AdmissionGeneration and AdmissionSealed are the ledger's admission read in
	// the SAME SNAPSHOT as everything else here.
	//
	// WITHOUT THEM A PROOF OUTLIVES THE SEAL IT WAS TAKEN UNDER. The plane drops
	// a barrier whose generation has moved, but that is asynchronous cleanup and
	// not a fence: between somebody resuming, taking work, and resealing, and the
	// plane's next pass, every host's run is still stored, still fenced by an
	// epoch and a dispatch generation nothing has moved, and would read as clear.
	// A drain would exit 0 against a deployment that was open in between.
	AdmissionGeneration int64
	AdmissionSealed     bool
	// Nodes is every EXPECTED host, ordered by name.
	Nodes []NodeClearance
	// Excluded is every host a person removed from that set.
	Excluded []Exclusion
}

// Clear reports whether every expected host has been proved to be running
// nothing.
//
// IT SAYS NOTHING ABOUT EXCLUSIONS, on purpose. An unproven exclusion cannot
// make this false without making a forced decommission useless, and it must not
// make it quietly true either — so it changes the SENTENCE a caller prints,
// through Unproven, rather than the boolean.
func (c ComputeClearance) Clear() bool {
	if !c.Requested {
		return false
	}

	// THE SEAL THE BARRIER WAS TAKEN UNDER MUST STILL BE THE ONE IN FORCE. A
	// resume moves the generation, so a barrier whose generation no longer
	// matches describes a fleet that has been free to take work since — however
	// proved each host looked at the moment it answered.
	if !c.AdmissionSealed || c.AdmissionGeneration != c.Generation {
		return false
	}

	for _, n := range c.Nodes {
		if n.State != ClearanceProved {
			return false
		}
	}

	return true
}

// Stale reports that the barrier belongs to an admission generation the ledger
// has moved past, so nothing under it can mean anything any more.
func (c ComputeClearance) Stale() bool {
	return c.Requested && (!c.AdmissionSealed || c.AdmissionGeneration != c.Generation)
}

// Blocking lists the expected hosts that are not proved clear, in the order a
// report should name them: what is running first.
func (c ComputeClearance) Blocking() []NodeClearance {
	var out []NodeClearance

	for _, want := range []ClearanceState{
		ClearanceRunning, ClearanceBelowProtocol, ClearanceUnreachable,
		ClearanceWaiting, ClearanceSettling, ClearanceUnknown,
	} {
		for _, n := range c.Nodes {
			if n.State == want {
				out = append(out, n)
			}
		}
	}

	return out
}

// Unproven names the hosts excluded from the expected set without proof.
func (c ComputeClearance) Unproven() []string {
	var out []string

	for _, e := range c.Excluded {
		if !e.Proven {
			out = append(out, e.Node)
		}
	}

	return out
}

// BarrierWireVersion is the oldest wire that can answer an inventory command.
//
// HELD HERE RATHER THAN IMPORTED, because internal/nodeapi imports this package
// and the dependency cannot go both ways. nodeapi's own constant is the
// authority; a test there pins the two together so they cannot drift.
const BarrierWireVersion = 14

// ComputeClear reports what the fleet has proved about the compute it holds.
//
// ON THE READ-ONLY POOL. A drain asks this on a cadence while the control plane
// is working, and a question must not reserve the single writer slot to answer
// itself.
func (a *Allocator) ComputeClear(ctx context.Context) (ComputeClearance, error) {
	var out ComputeClearance

	err := a.db.View(ctx, func(tx querier) error {
		out = ComputeClearance{}

		q := state.ReadQueries(tx)

		// ONE SNAPSHOT FOR ALL OF IT, the same rule Quiescence follows: read
		// separately, a barrier replaced between two of these reads yields
		// observations from one barrier beside the identity of another.
		barrier, found, err := readBarrierTx(ctx, q)
		if err != nil {
			return err
		}

		out.Requested = found
		out.BarrierID = barrier.ID
		out.Generation = barrier.Generation

		// IN THE SAME SNAPSHOT, the same rule Quiescence follows for its own two
		// halves: read separately, a resume committing in between yields "sealed,
		// and every host proved" about a deployment that is open — a composite
		// that was never true at any instant, and the one a drain would act on.
		admission, err := state.ReadAdmission(ctx, tx)
		if err != nil {
			return fmt.Errorf("alloc: read admission for a compute barrier: %w", err)
		}

		out.AdmissionGeneration = admission.Generation
		out.AdmissionSealed = admission.Sealed()

		runs, err := barrierRunsTx(ctx, q, barrier.ID, found)
		if err != nil {
			return err
		}

		rows, err := q.ListFleetClearance(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list the fleet for a compute barrier: %w", err)
		}

		for _, row := range rows {
			name := row.Name
			live := int(row.Live)
			epoch, dispatch := row.Epoch, row.DispatchGeneration
			wire := int(row.WireVersion)
			decommissionedAt := row.DecommissionedAt
			proven := int(row.DecommissionProven)
			actor := row.DecommissionActor

			if decommissionedAt != "" {
				out.Excluded = append(out.Excluded, Exclusion{
					Node: name, Proven: proven == 1, Actor: actor, At: decommissionedAt,
				})

				continue
			}

			out.Nodes = append(out.Nodes, a.clearanceOf(name, wire, live == 1,
				NodeFence{Epoch: epoch, Dispatch: dispatch}, runs[name], found))
		}

		return nil
	})
	if err != nil {
		return ComputeClearance{}, err
	}

	return out, nil
}

// clearanceOf decides one host's state from its fence and its run.
//
// LIVENESS IS NOT A FENCE, and the ordering here says so: a host with a valid
// run that has crossed the grace is PROVED even if the deployment can no longer
// reach it. Nothing could have been dispatched to it — its dispatch generation
// has not moved — and a reconnect bumps its epoch, which discards the run. A
// liveness term here would instead make a host that answered and then went quiet
// permanently unprovable.
func (a *Allocator) clearanceOf(
	name string, wire int, live bool, fence NodeFence, run barrierRun, requested bool,
) NodeClearance {
	out := NodeClearance{Node: name, WireVersion: wire}

	if !requested {
		out.State = ClearanceWaiting

		return out
	}

	// A HOST SAYING IT IS RUNNING SOMETHING DOMINATES EVERYTHING, INCLUDING A RUN
	// THAT HAS ALREADY CROSSED THE GRACE.
	//
	// The two can coexist, and that is the whole point: a create the provider had
	// accepted but not yet listed is invisible to the barrier's samples — which is
	// the delayed create the grace exists for — and then appears in the host's
	// ordinary sweep. NO FENCE MOVES, because the launch was dispatched before the
	// barrier and the registration has not changed, so the completed run would go
	// on reading as proof while the host is explicitly saying otherwise.
	//
	// SAFE IN ONE DIRECTION ONLY, which is why it may block and could never
	// clear: this is telemetry, already stale when it arrives (see NodeInventory).
	// Believing it when it says "running" costs a drain some patience; believing
	// it when it says "empty" would be the rejected stamped-inventory design.
	// The barrier's own empty answers write zero here through the same resolver,
	// so a genuine proof is never blocked by its own telemetry.
	if run.running {
		out.State = ClearanceRunning

		return out
	}

	if run.fencedBy(fence) {
		out.EmptySince = run.emptySince
		out.ClearAt = ts(run.clearAt())

		// THROUGH THE SHARED PREDICATE, so this and a decommission's own
		// transaction cannot answer differently about one host.
		//
		// Equivalent to run.proved() HERE — the guards above have already
		// established the fence and the absence of a positive inventory, and a
		// mutant that swaps them survives for that reason. It is written this way
		// so there is one definition of "proved" to edit rather than two that can
		// drift apart, which is the failure it exists to prevent rather than one
		// this call currently prevents.
		if provenBy(run, fence) {
			out.State = ClearanceProved

			return out
		}

		// SETTLING IS A CLAIM THAT ANOTHER ANSWER CAN FINISH THIS, AND ONLY A LIVE
		// HOST CAN GIVE ONE. The proof is two observations — an empty answer taken at or
		// after `empty_since + grace` — so a run that has not crossed yet needs
		// ANOTHER answer, and a host billet cannot reach will not give one. If it
		// ever comes back its epoch moves and this run is discarded, so no future
		// makes this particular run into proof.
		//
		// Reported as settling, `ClearAt` passes and the line does not change: an
		// operator is left waiting on an answer that cannot arrive. MEASURED ON THE REFERENCE HOST — a node killed with
		// SIGKILL sat at "empty, still settling (clear at 05:28:21)" at 05:28:44,
		// with nothing left that could advance it.
		//
		// A PROVED run still outranks liveness, which is the rule above and is
		// unchanged: nothing can have been dispatched to a host whose dispatch
		// generation has not moved, so a host that finished its run and then went
		// quiet stays proved.
		if !live {
			out.State = ClearanceUnreachable

			return out
		}

		out.State = ClearanceSettling

		return out
	}

	// NO VALID RUN. Why there is none decides what an operator can do about it,
	// and the two hopeless cases are named rather than left as "waiting" — a host
	// that CANNOT answer is not one that has not answered yet.
	switch {
	case wire > 0 && wire < BarrierWireVersion:
		out.State = ClearanceBelowProtocol
	case !live:
		out.State = ClearanceUnreachable
	default:
		out.State = ClearanceWaiting
	}

	return out
}

// barrierRun is one host's continuous empty run under the barrier in force.
type barrierRun struct {
	found          bool
	epoch          int64
	dispatch       int64
	emptySince     string
	emptySinceTime time.Time
	observedAtTime time.Time
	// running is set from the host's last reported inventory, so a host with no
	// run can still be reported as ACTIVELY running rather than merely silent.
	running bool
}

// fencedBy reports whether this run still describes the host's current state.
func (r barrierRun) fencedBy(fence NodeFence) bool {
	return r.found && r.epoch == fence.Epoch && r.dispatch == fence.Dispatch
}

// clearAt is the EARLIEST MOMENT AN EMPTY ANSWER WOULD PROVE THIS RUN, not a
// moment at which it clears itself. Time alone establishes nothing: the proof is
// an observation taken at or after this, so a run whose host has stopped
// answering never completes however long it sits here.
func (r barrierRun) clearAt() time.Time {
	return r.emptySinceTime.Add(computeAbsenceGrace)
}

// proved reports whether this run has established that the host holds nothing.
//
// THE EVIDENCE IS TWO OBSERVATIONS, NOT ONE OBSERVATION AND A CLOCK. What is
// claimed is that the host has been continuously empty across the grace, and
// what establishes it is an empty answer taken AT OR AFTER the grace elapsed —
// so the run's proven span is `observed_at - empty_since`, and the current time
// does not enter it at all.
//
// The first version compared `now` against `empty_since + grace`, which is
// elapsed wall-clock authorising a claim about a machine — the thing this
// codebase refuses everywhere else, and it failed exactly where the grace exists
// to help. A host that answered empty ONCE and then vanished (a launch the
// daemon had accepted but not yet listed, and a node that disconnected a second
// later) would age into "proved clear" five minutes on, with the epoch and
// dispatch fences both still matching because nothing had happened to move them.
// `local down` then stops services on a host that is running somebody's job.
//
// Silence AFTER a proof is established is still fine, and is the case
// TestAProvedHostThatGoesSilentStaysProved covers: nothing can be dispatched
// without moving the dispatch generation, and a reconnect moves the epoch.
func (r barrierRun) proved() bool {
	return !r.observedAtTime.Before(r.clearAt())
}

func barrierRunsTx(
	ctx context.Context, q state.ReadOps, barrierID string, found bool,
) (map[string]barrierRun, error) {
	out := map[string]barrierRun{}

	if found {
		rows, err := q.ListBarrierRuns(ctx, barrierID)
		if err != nil {
			return nil, fmt.Errorf("alloc: read the barrier's observations: %w", err)
		}

		for _, row := range rows {
			run := barrierRun{
				epoch:      row.NodeEpoch,
				dispatch:   row.DispatchGeneration,
				emptySince: row.EmptySince,
			}

			// BOTH ENDS OF THE RUN. observed_at is what makes the proof an
			// observation rather than an elapsed clock — see barrierRun.proved.
			if run.emptySinceTime, run.observedAtTime, err = runSpan(
				row.EmptySince, row.ObservedAt); err != nil {
				return nil, fmt.Errorf("alloc: read node %s's idle run: %w", row.Node, err)
			}

			run.found = true
			out[row.Node] = run
		}
	}

	// THE HOST'S OWN LAST WORD, for the hosts with no valid run. It is telemetry
	// and can never clear anything — see NodeInventory — but a host SAYING it is
	// running something is a fact, and a report that called that "has not
	// answered" would bury the one line an operator needs.
	reporting, err := q.ListHostsReportingCompute(ctx)
	if err != nil {
		return nil, fmt.Errorf("alloc: read what the fleet last reported running: %w", err)
	}

	for _, node := range reporting {
		run := out[node]
		run.running = true
		out[node] = run
	}

	return out, nil
}

// provedTx reports whether one host is, RIGHT NOW AND INSIDE THIS TRANSACTION,
// established as running nothing.
//
// IT EXISTS BECAUSE A PROOF IS NOT A BOOLEAN A CALLER MAY CARRY. `billet nodes
// decommission` used to read the fleet's clearance, reduce this host's state to
// `true`, and hand that to a separate write — and a host can re-register in
// between, which is precisely the incarnation change the epoch fence exists to
// catch. The exclusion would then be recorded as PROVEN about a machine that had
// just come back and might be running something, which is the laundering the
// whole membership rule is built to prevent.
func provedTx(ctx context.Context, tx querier, node string) (bool, error) {
	q := state.ReadQueries(tx)

	barrier, found, err := readBarrierTx(ctx, q)
	if err != nil {
		return false, err
	}

	if !found {
		return false, nil
	}

	admission, err := state.ReadAdmission(ctx, tx)
	if err != nil {
		return false, fmt.Errorf("alloc: read admission for a compute barrier: %w", err)
	}

	if !admission.Sealed() || admission.Generation != barrier.Generation {
		return false, nil
	}

	fenceRow, err := q.ReadNodeFence(ctx, node)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("alloc: read the barrier fence of node %s: %w", node, err)
	}

	fence := NodeFence{Epoch: fenceRow.Epoch, Dispatch: fenceRow.DispatchGeneration}

	runRow, err := q.ReadBarrierRun(ctx, ledgerdb.ReadBarrierRunParams{
		Node:      node,
		BarrierID: barrier.ID,
	})

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("alloc: read node %s's idle run: %w", node, err)
	}

	run := barrierRun{
		epoch:      runRow.NodeEpoch,
		dispatch:   runRow.DispatchGeneration,
		emptySince: runRow.EmptySince,
	}

	if run.emptySinceTime, run.observedAtTime, err = runSpan(
		runRow.EmptySince, runRow.ObservedAt); err != nil {
		return false, fmt.Errorf("alloc: read node %s's idle run: %w", node, err)
	}

	run.found = true

	// THE HOST'S LAST WORD IS READ HERE TOO, so this and clearanceOf cannot
	// disagree about the same host at the same instant. Without it a machine
	// actively reporting compute could be decommissioned as PROVEN — permanently
	// out of the expected set, on a run whose samples simply predated the
	// instance becoming visible.
	if run.running, err = q.HostReportsCompute(ctx, node); err != nil {
		return false, fmt.Errorf("alloc: read what node %s last reported running: %w", node, err)
	}

	return provenBy(run, fence), nil
}

// provenBy is the ONE definition of what a proof is.
//
// Both clearance paths go through it — the fleet report and the decommission's
// own transaction — because two readings of "proved" that can disagree about the
// same host at the same instant is how an exclusion gets recorded as proven
// against a report that says otherwise.
func provenBy(run barrierRun, fence NodeFence) bool {
	return run.fencedBy(fence) && !run.running && run.proved()
}

// runSpan parses the two ends of a stored run.
func runSpan(emptySince, observedAt string) (time.Time, time.Time, error) {
	since, err := time.Parse(timestampFormat, emptySince)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("read when it went empty: %w", err)
	}

	seen, err := time.Parse(timestampFormat, observedAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("read when it last said so: %w", err)
	}

	return since, seen, nil
}

func newBarrierID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("alloc: generate a barrier id: %w", err)
	}

	return hex.EncodeToString(buf), nil
}
