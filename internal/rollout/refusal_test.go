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
