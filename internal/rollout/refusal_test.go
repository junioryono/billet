package rollout

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// THE REASON A DISPATCH WAS REFUSED IS ON THE ROW, and it has exactly three
// behaviours: a refused dispatch writes it, an accepted dispatch clears it, and
// every other transition leaves it alone.

// refusalOf is epyc-1's record, the one host every fixture here asks about.
func refusalOf(t *testing.T, s *Store, r *Rollout) string {
	t.Helper()

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	for i := range nodes {
		if nodes[i].Node == "epyc-1" {
			return nodes[i].LastRefusal
		}
	}

	t.Fatalf("rollout %s has no row for epyc-1", r.ID)

	return ""
}

// A REFUSED DISPATCH RECORDS ITS REASON, and the record survives reopening the
// ledger, because it is what an operator reads after the control plane that
// wrote it has restarted.
func TestARefusedDispatchRecordsItsReason(t *testing.T) {
	dir := t.TempDir()

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	s := New(db)
	r := start(t, s)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Backoff: retryAfter,
		Refusal: "the host holds a converge guard for holder ci-42",
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if got := refusalOf(t, s, r); got != "the host holds a converge guard for holder ci-42" {
		t.Errorf("the refusal reads %q", got)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if got := refusalOf(t, New(db), r); got != "the host holds a converge guard for holder ci-42" {
		t.Errorf("after a reopen the refusal reads %q", got)
	}
}

// EVERY OTHER TRANSITION KEEPS THE RECORD: a block, an operator's retry and a
// backoff that carries no reason all leave the last refusal as it was, because
// an empty field on an ordinary advance is not an instruction to clear.
func TestANeutralAdvanceKeepsTheLastRefusal(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Backoff: retryAfter,
		Refusal: "guarded",
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	for _, req := range []AdvanceRequest{
		{RolloutID: r.ID, Node: "epyc-1", To: PhaseBlocked, Blocker: "an older wire"},
		{RolloutID: r.ID, Node: "epyc-1", To: PhasePending},
		{RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Backoff: retryAfter},
		{RolloutID: r.ID, Node: "epyc-1", To: PhaseDraining, DispatchEpoch: 3},
	} {
		if err := s.Advance(t.Context(), req); err != nil {
			t.Fatalf("Advance to %s: %v", req.To, err)
		}

		if got := refusalOf(t, s, r); got != "guarded" {
			t.Errorf("after an advance to %s the refusal reads %q, want guarded", req.To, got)
		}
	}
}

// THE RECORD IS CLEARED ONLY BY THE FLAG, never by an empty reason, and a
// request that both records and clears is refused rather than resolved.
func TestOnlyClearRefusalClearsIt(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Refusal: "guarded",
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Refusal: "again", ClearRefusal: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot both record a refusal and clear one") {
		t.Fatalf("an advance that records and clears: err = %v", err)
	}

	if got := refusalOf(t, s, r); got != "guarded" {
		t.Errorf("the refused advance changed the record to %q", got)
	}

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseDraining, DispatchEpoch: 2, ClearRefusal: true,
	}); err != nil {
		t.Fatalf("Advance with ClearRefusal: %v", err)
	}

	if got := refusalOf(t, s, r); got != "" {
		t.Errorf("after ClearRefusal the record reads %q, want empty", got)
	}
}

// THE RECORD IS BOUNDED AND VALID UTF-8 ON BOTH ENGINES: a longer reason is cut
// at a rune boundary and says so, and bytes that are not UTF-8 are replaced
// rather than refused or stored.
func TestARefusalIsBoundedAndStorable(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	// Enough three-byte runes past the bound that a byte-count cut would land
	// inside one of them.
	long := strings.Repeat("a", maxRefusalBytes-7) + strings.Repeat("€", 8)
	if len(long) <= maxRefusalBytes {
		t.Fatalf("the fixture is %d bytes, not over the bound", len(long))
	}

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Refusal: long,
	}); err != nil {
		t.Fatalf("Advance with a long refusal: %v", err)
	}

	got := refusalOf(t, s, r)

	if len(got) > maxRefusalBytes {
		t.Errorf("the stored refusal is %d bytes, over the bound of %d", len(got), maxRefusalBytes)
	}

	if !strings.HasSuffix(got, refusalCutMarker) {
		t.Errorf("a cut refusal does not say it was cut: %q", got[len(got)-20:])
	}

	if !utf8.ValidString(got) {
		t.Error("the cut landed inside a rune")
	}

	if !strings.HasPrefix(got, strings.Repeat("a", maxRefusalBytes-7)) {
		t.Error("the cut lost text before the bound")
	}

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Refusal: "bad \xff\xfe bytes",
	}); err != nil {
		t.Fatalf("Advance with invalid UTF-8: %v", err)
	}

	if got := refusalOf(t, s, r); got != "bad � bytes" || !utf8.ValidString(got) {
		t.Errorf("invalid bytes were stored as %q", got)
	}

	// NUL IS VALID UTF-8 AND NOT STORABLE TEXT ON POSTGRESQL, so it is replaced
	// too, or the refusal, the attempt count and the backoff in the same
	// transaction would all be lost there.
	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Refusal: "failed\x00detail",
	}); err != nil {
		t.Fatalf("Advance with a NUL: %v", err)
	}

	if got := refusalOf(t, s, r); got != "failed�detail" || strings.ContainsRune(got, 0) {
		t.Errorf("a NUL was stored as %q", got)
	}

	// Exactly at the bound nothing is cut.
	exact := strings.Repeat("b", maxRefusalBytes)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Refusal: exact,
	}); err != nil {
		t.Fatalf("Advance at the bound: %v", err)
	}

	if got := refusalOf(t, s, r); got != exact {
		t.Errorf("a refusal exactly at the bound was changed (%d bytes, suffix %q)", len(got), got[len(got)-8:])
	}
}

// readingDispatcher is a dispatcher that reads the rollout's rows while it is
// being asked, the way a node inspecting the controller's status during its own
// dispatch would, and then answers as told.
type readingDispatcher struct {
	store   *Store
	rollout *Rollout
	fail    error
	seen    []string
	told    []string
}

func (d *readingDispatcher) Upgrade(ctx context.Context, node, _, _, _ string, _ int64) error {
	nodes, err := d.store.Nodes(ctx, d.rollout.ID)
	if err != nil {
		return err
	}

	for i := range nodes {
		if nodes[i].Node == node {
			d.seen = append(d.seen, nodes[i].LastRefusal)
		}
	}

	if d.fail != nil {
		return d.fail
	}

	d.told = append(d.told, node)

	return nil
}

// THE COORDINATOR WRITES THE REASON ON A FAILED DISPATCH AND CLEARS IT ON AN
// ACCEPTED ONE, and the clear happens AFTER the dispatch was accepted, not
// before it was attempted: a reader during the second dispatch still sees the
// first refusal, so a status read never shows a host as unrefused while its
// dispatch is still in flight.
func TestTheCoordinatorRecordsARefusalAndClearsItOnAcceptance(t *testing.T) {
	_, s := open(t)

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops",
		Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	store := New(s.db, WithClock(clock))
	dispatch := &readingDispatcher{store: store, rollout: r,
		fail: errors.New("the host holds a converge guard for holder ci-42")}

	c := NewCoordinator(store, fleet, dispatch, targetVersion, 14,
		WithCoordinatorClock(clock), WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller

	if err := c.Tick(t.Context()); err == nil {
		t.Fatal("the injected dispatch failure was not reported")
	}

	if got := refusalOf(t, store, r); got != "the host holds a converge guard for holder ci-42" {
		t.Fatalf("after a refused dispatch the record reads %q", got)
	}

	if len(dispatch.seen) != 1 || dispatch.seen[0] != "" {
		t.Errorf("the first dispatch saw %q, want an empty record", dispatch.seen)
	}

	// Past the backoff the host accepts.
	dispatch.fail = nil
	now = now.Add(retryAfter + time.Minute)

	tick(t, c)

	if len(dispatch.told) != 1 {
		t.Fatalf("the host was not told on the retry: %v", dispatch.told)
	}

	if len(dispatch.seen) != 2 || dispatch.seen[1] != "the host holds a converge guard for holder ci-42" {
		t.Errorf("the second dispatch saw %q; the refusal was cleared before the dispatch was accepted",
			dispatch.seen)
	}

	if got := refusalOf(t, store, r); got != "" {
		t.Errorf("after an accepted dispatch the record reads %q, want empty", got)
	}

	if got := phaseOf(t, store, r); got != PhaseDraining {
		t.Errorf("the accepted host is %s, want draining", got)
	}
}

// fencingDispatcher accepts the dispatch and then fences the ledger, so the
// Advance that would clear the refusal fails after the updater is already
// running: the shape of a control plane whose ledger write failed at exactly
// that moment.
type fencingDispatcher struct {
	dir  string
	told int
}

func (d *fencingDispatcher) Upgrade(context.Context, string, string, string, string, int64) error {
	d.told++

	if _, err := state.WriteMaintenanceFence(d.dir, "a write failure staged by the test"); err != nil {
		return err
	}

	return nil
}

// A REFUSAL IS CLEARED BY THE CONVERGENCE TOO. The accepted dispatch's clear can
// fail after the host is already upgrading; the host then registers on the
// target and is walked from pending to committed, and without this the old
// refusal would outlive the upgrade it described.
func TestAConvergedHostLosesTheRefusalItsAcceptedDispatchCouldNotClear(t *testing.T) {
	dir := t.TempDir()

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	s := New(db)

	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops",
		Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	store := New(db, WithClock(clock))

	// A refusal on the row first, the way a guarded host leaves one.
	if err := store.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending, Backoff: retryAfter,
		Refusal: "the host holds a converge guard for holder ci-42",
	}); err != nil {
		t.Fatal(err)
	}

	dispatch := &fencingDispatcher{dir: dir}
	c := NewCoordinator(store, fleet, dispatch, targetVersion, 14,
		WithCoordinatorClock(clock), WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller

	now = now.Add(retryAfter + time.Minute)

	// The dispatch is accepted and the clear that follows fails on the fence.
	err = c.Tick(t.Context())
	if !errors.Is(err, state.ErrMaintenance) {
		t.Fatalf("the tick after the fenced write: err = %v, want ErrMaintenance", err)
	}

	if dispatch.told != 1 {
		t.Fatalf("the host was told %d times, want 1", dispatch.told)
	}

	if err := state.ClearMaintenanceFence(dir, "a write failure staged by the test"); err != nil {
		t.Fatal(err)
	}

	if got := refusalOf(t, store, r); got != "the host holds a converge guard for holder ci-42" {
		t.Fatalf("after the failed clear the record reads %q; the fixture stages nothing", got)
	}

	if got := phaseOf(t, store, r); got != PhasePending {
		t.Fatalf("after the failed clear the host is %s, want pending", got)
	}

	// The updater ran anyway: the host comes back on the target.
	fleet.set("epyc-1", targetVersion, 14, true)
	fleet.digest("epyc-1", targetDigest)

	tick(t, c)

	if got := phaseOf(t, store, r); got != PhaseCommitted {
		t.Fatalf("the host that came back on the target is %s, want committed", got)
	}

	if got := refusalOf(t, store, r); got != "" {
		t.Errorf("a converged host still carries the refusal %q", got)
	}
}

// A HOST THAT CONVERGED BY AN OPERATOR'S HAND AFTER ONLY A REFUSED DISPATCH IS
// NOT STUCK ON THAT REFUSAL EITHER: the record means the refusal the host is
// still stuck on, and its commit clears it whatever moved the host.
func TestAConvergedHostLosesARefusalNothingAccepted(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops",
		Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	dispatch := &fakeDispatcher{fail: errors.New("the host holds a converge guard for holder ci-42")}
	c := NewCoordinator(s, fleet, dispatch, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller

	if err := c.Tick(t.Context()); err == nil {
		t.Fatal("the injected dispatch failure was not reported")
	}

	if got := refusalOf(t, s, r); got == "" {
		t.Fatal("the refusal was not recorded")
	}

	// Nothing accepted; the operator moved the host by hand and it registers on
	// the target.
	fleet.set("epyc-1", targetVersion, 14, true)
	fleet.digest("epyc-1", targetDigest)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Fatalf("the host is %s, want committed", got)
	}

	if got := refusalOf(t, s, r); got != "" {
		t.Errorf("a converged host still carries the refusal %q", got)
	}
}

// EACH OF THE SNAPSHOT'S READS CAN FAIL AFTER THE TRANSACTION WAS ENTERED, and
// the failure is the snapshot's, never an empty part: the binding, the open
// rollout, the history, the nodes and the registrations, the last of them after
// every other field was collected.
func TestAStatusSnapshotReportsTheReadThatFailed(t *testing.T) {
	db, s := open(t)
	registerNode(t, db, "epyc-1", "v0.3.26", "inc-a")

	r := start(t, s)

	if err := s.Finish(t.Context(), r.ID, StateAborted, "so the history read runs"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Start(t.Context(), StartRequest{
		Channel: "stable", TargetVersion: "v0.5.0", TargetDigest: otherDigest,
		PriorVersion: "v0.4.0", Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: []string{"epyc-1"},
	}); err != nil {
		t.Fatal(err)
	}

	steps := []string{"binding", "rollout", "nodes", "registrations"}

	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			failing := &failingReads{step: step, err: errors.New("the " + step + " read failed, staged by the test")}

			snapshotReads = func(reads state.ReadOps) state.ReadOps {
				failing.ReadOps = reads

				return failing
			}

			t.Cleanup(func() { snapshotReads = nil })

			snapshot, err := s.StatusSnapshot(t.Context())
			if !errors.Is(err, failing.err) {
				t.Fatalf("StatusSnapshot: err = %v, want the injected failure", err)
			}

			if !failing.ran {
				t.Fatal("the failing read never ran for this step")
			}

			if snapshot.Rollout != nil || snapshot.Nodes != nil || snapshot.Registrations != nil || snapshot.Binding != "" {
				t.Errorf("a failed snapshot carries parts: %+v", snapshot)
			}
		})
	}

	// The history read runs only when no rollout is open.
	t.Run("history", func(t *testing.T) {
		open, err := s.Open(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if err := s.Finish(t.Context(), open.ID, StateAborted, "so the history read runs"); err != nil {
			t.Fatal(err)
		}

		failing := &failingReads{step: "history", err: errors.New("the history read failed, staged by the test")}

		snapshotReads = func(reads state.ReadOps) state.ReadOps {
			failing.ReadOps = reads

			return failing
		}

		t.Cleanup(func() { snapshotReads = nil })

		if _, err := s.StatusSnapshot(t.Context()); !errors.Is(err, failing.err) {
			t.Fatalf("StatusSnapshot: err = %v, want the injected failure", err)
		}

		if !failing.ran {
			t.Fatal("the failing history read never ran")
		}
	})
}

// failingReads is the snapshot's reads with one of them failing, by name.
type failingReads struct {
	state.ReadOps
	step string
	err  error
	ran  bool
}

func (f *failingReads) fail(step string) error {
	if step != f.step {
		return nil
	}

	f.ran = true

	return f.err
}

func (f *failingReads) ReadDeploymentBinding(ctx context.Context) (ledgerdb.ReadDeploymentBindingRow, error) {
	if err := f.fail("binding"); err != nil {
		return ledgerdb.ReadDeploymentBindingRow{}, err
	}

	return f.ReadOps.ReadDeploymentBinding(ctx)
}

func (f *failingReads) ReadRolloutInState(ctx context.Context, st string) (ledgerdb.Rollout, error) {
	if err := f.fail("rollout"); err != nil {
		return ledgerdb.Rollout{}, err
	}

	return f.ReadOps.ReadRolloutInState(ctx, st)
}

func (f *failingReads) ListRolloutHistory(ctx context.Context, maxRows int64) ([]ledgerdb.Rollout, error) {
	if err := f.fail("history"); err != nil {
		return nil, err
	}

	return f.ReadOps.ListRolloutHistory(ctx, maxRows)
}

func (f *failingReads) ListRolloutNodes(ctx context.Context, id string) ([]ledgerdb.ListRolloutNodesRow, error) {
	if err := f.fail("nodes"); err != nil {
		return nil, err
	}

	return f.ReadOps.ListRolloutNodes(ctx, id)
}

func (f *failingReads) ListNodeRegistrations(ctx context.Context) ([]ledgerdb.ListNodeRegistrationsRow, error) {
	if err := f.fail("registrations"); err != nil {
		return nil, err
	}

	return f.ReadOps.ListNodeRegistrations(ctx)
}

// registerNode writes one registration the way the allocator does, returning
// the epoch the ledger assigned.
func registerNode(t *testing.T, db *state.DB, name, release, incarnation string) int64 {
	t.Helper()

	var epoch int64

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		var err error

		epoch, err = state.WriteQueries(tx).UpsertNodeRegistration(t.Context(), ledgerdb.UpsertNodeRegistrationParams{
			Name: name, Provider: "docker", TotalVcpu: 8, TotalMemory: 1 << 34,
			LastSeenAt: "2026-09-08T12:00:00Z", NodeRelease: release, WireMin: 12, WireMax: 14,
			WireVersion: 14, NodeDigest: otherDigest, Incarnation: incarnation, HighestRelease: release,
		})

		return err
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}

	return epoch
}

// REGISTRATIONS ARE THE LEDGER'S REGISTRATIONS, NOT A ROLLOUT'S ROWS: every
// registered host is reported with the incarnation and epoch it holds now,
// whether or not a rollout names it, and a rollout member that never registered
// is not invented.
func TestRegistrationsReportEveryHostsCurrentRegistration(t *testing.T) {
	db, s := open(t)

	// Three registrations of one host from three processes: the epoch counts
	// them and the incarnation is the latest process's.
	epochs := []int64{
		registerNode(t, db, "epyc-1", "v0.3.26", "inc-a"),
		registerNode(t, db, "epyc-1", "v0.3.26", "inc-b"),
		registerNode(t, db, "epyc-1", "v0.4.0", "inc-c"),
	}

	if epochs[0] != 1 || epochs[1] != 2 || epochs[2] != 3 {
		t.Fatalf("three registrations gave epochs %v, want 1 2 3", epochs)
	}

	// A host outside any rollout, registered once.
	registerNode(t, db, "outside-1", "v0.3.26", "inc-x")

	// A rollout naming epyc-1 and a host that never registered.
	r := start(t, s, "epyc-1", "never-registered")

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseDraining, DispatchEpoch: 1,
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	regs, err := s.Registrations(t.Context())
	if err != nil {
		t.Fatalf("Registrations: %v", err)
	}

	want := []Registration{
		{Name: "epyc-1", Live: true, Epoch: 3, Incarnation: "inc-c", Release: "v0.4.0",
			Digest: otherDigest, HighestRelease: "v0.4.0"},
		{Name: "outside-1", Live: true, Epoch: 1, Incarnation: "inc-x", Release: "v0.3.26",
			Digest: otherDigest, HighestRelease: "v0.3.26"},
	}

	if len(regs) != len(want) {
		t.Fatalf("Registrations returned %d rows, want %d: %+v", len(regs), len(want), regs)
	}

	for i := range want {
		if regs[i] != want[i] {
			t.Errorf("registration %d is %+v, want %+v", i, regs[i], want[i])
		}
	}

	// THE ROLLOUT ROW'S DISPATCH EPOCH IS NOT THE REGISTRATION'S: the rollout
	// recorded 1 and the host has registered three times since.
	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}

	for i := range nodes {
		if nodes[i].Node == "epyc-1" && nodes[i].DispatchEpoch != 1 {
			t.Errorf("the rollout row's dispatch epoch moved to %d", nodes[i].DispatchEpoch)
		}
	}
}
