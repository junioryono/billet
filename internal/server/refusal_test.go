package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

// These are #248: a purchase that buys nothing says why, the order or the
// allocator, and the status report names the waiter that holds the line.

// logLines is a JSON log a test can read back by message.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

// with returns every record whose message starts with msg.
func (l *logLines) with(t *testing.T, msg string) []map[string]any {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	var out []map[string]any

	scanner := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("a log line is not JSON: %v: %s", err, scanner.Text())
		}

		if text, _ := record["msg"].(string); strings.HasPrefix(text, msg) {
			out = append(out, record)
		}
	}

	return out
}

// loggedListeners is orderedListeners with a log the test reads.
func loggedListeners(t *testing.T, tiers []config.Tier, vcpu int, order config.AdmissionOrder,
) (*alloc.Allocator, *launchLog, *logLines, []*Listener) {
	t.Helper()

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: vcpu, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	registerHost(t, a)

	lines := &logLines{}
	logger := slog.New(slog.NewJSONHandler(lines, &slog.HandlerOptions{Level: slog.LevelInfo}))

	log := &launchLog{}
	s := New(a, nil, tiers, "refusal-test", logger, WithNodeRunner(log), WithAdmissionOrder(order))

	listeners := make([]*Listener, 0, len(tiers))
	for i := range tiers {
		listeners = append(listeners, NewListener(a, tiers[i].Label, &fakeSession{}, s.listenerOpts(nil)...))
	}

	return a, log, lines, listeners
}

const declineMessage = "assigned a request with no escrow to back it; declining it"

// A DECLINE THE ORDER CAUSED NAMES THE WAITER THAT HOLDS THE LINE.
//
// The 2026-09-25 incident logged 224 of these with no reason, and could not
// tell the order holding room for another tier from the allocator refusing.
func TestADeclineTheOrderCausedNamesTheWaiter(t *testing.T) {
	t.Parallel()

	a, _, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	freeOneLease(t, a)

	if err := small.handle(t.Context(), directlyAssigned(1, "J-small", 3)); err != nil {
		t.Fatalf("small tier handle: %v", err)
	}

	declines := lines.with(t, declineMessage)
	if len(declines) != 1 {
		t.Fatalf("logged %d declines, want the one the order caused: %v", len(declines), declines)
	}

	if got := declines[0]["held_by"]; got != "b-large" {
		t.Errorf("the decline names held_by %v, want the waiting large tier: %v", got, declines[0])
	}

	if reason, _ := declines[0]["reason"].(string); !strings.Contains(reason, "admission order") {
		t.Errorf("the decline's reason %q does not say the admission order refused it", reason)
	}
}

// A DECLINE THE ALLOCATOR CAUSED SAYS WHAT IT HAS NO ROOM IN, and names no
// waiter: under fill nothing waits in line, so every refusal is the allocator's.
func TestADeclineTheAllocatorCausedSaysWhy(t *testing.T) {
	t.Parallel()

	_, _, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	small := listeners[0]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := small.handle(t.Context(), directlyAssigned(1, "J-small", 3)); err != nil {
		t.Fatalf("small tier handle: %v", err)
	}

	declines := lines.with(t, declineMessage)
	if len(declines) != 1 {
		t.Fatalf("logged %d declines, want one: %v", len(declines), declines)
	}

	if got, named := declines[0]["held_by"]; named {
		t.Errorf("an allocator refusal named a waiter %v: %v", got, declines[0])
	}

	reason, _ := declines[0]["reason"].(string)
	if !strings.Contains(reason, "the allocator has no room") || !strings.Contains(reason, "deployment ceiling") {
		t.Errorf("the decline's reason %q does not say the allocator refused it for the ceiling", reason)
	}
}

// THE POOL PATH SAYS IT ONCE, not on every poll: a waiting tier reconciles
// before every poll, and a line each time would bury everything else.
func TestThePoolPathSaysWhyOncePerReason(t *testing.T) {
	t.Parallel()

	_, _, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	for range 3 {
		if err := large.reconcilePool(t.Context(), 1); err != nil {
			t.Fatalf("large tier reconcile: %v", err)
		}
	}

	said := lines.with(t, "assigned work is waiting")
	if len(said) != 1 {
		t.Fatalf("the refused tier said why %d times over three polls, want once: %v", len(said), said)
	}

	if said[0]["tier"] != "b-large" {
		t.Errorf("the line names tier %v, want b-large: %v", said[0]["tier"], said[0])
	}

	if reason, _ := said[0]["reason"].(string); !strings.Contains(reason, "deployment ceiling") {
		t.Errorf("the reason %q does not name the ceiling the small tier spent", reason)
	}
}

// A TIER AT ITS OWN CAP PUBLISHES WHY, THOUGH IT NEVER WAITS IN LINE: its
// refusal is its own max_concurrent, so the order leaves it out, and the
// report is then the only place its reason is said.
func TestATierAtItsCapPublishesItsRefusal(t *testing.T) {
	t.Parallel()

	capped := tierOf("a-capped", 4)
	capped.MaxConcurrent = 1

	a, _, _, listeners := loggedListeners(t, []config.Tier{capped}, 64, config.AdmissionFair)

	if err := listeners[0].reconcilePool(t.Context(), 3); err != nil {
		t.Fatalf("capped tier reconcile: %v", err)
	}

	listeners[0].reportCapacity(t.Context(), nil, "")

	got, err := a.CapacityReport(t.Context(), "a-capped")
	if err != nil {
		t.Fatalf("read the capped tier's report: %v", err)
	}

	if got.Listener.WaitingSince != "" {
		t.Fatalf("a tier at its own cap waits in line (%+v), so this case cannot show its reason outside it",
			got.Listener)
	}

	if !strings.Contains(got.Listener.Refusal, "max_concurrent of 1") {
		t.Errorf("the capped tier's report says its purchase was refused for %q, want its own cap",
			got.Listener.Refusal)
	}
}

// AN OFFER DECLINED PAST THE ESCROW ALREADY HELD SAYS WHY. One lease is held
// before two offers arrive and the allocator has no room for the second, so
// the refill buys nothing while the tier holds some escrow: the second offer's
// decline must still carry the allocator's reason.
func TestAnOfferDeclinedPastHeldEscrowSaysWhy(t *testing.T) {
	t.Parallel()

	_, _, lines, listeners := loggedListeners(t, []config.Tier{tierOf("a-work", 4)}, 4, config.AdmissionFair)
	l := listeners[0]

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("stage the one lease the ceiling allows: %v", err)
	}

	if err := l.handle(t.Context(), &Message{MessageID: 1, Available: []dispatch.Job{
		{RequestID: 11, RunID: 101}, {RequestID: 12, RunID: 102},
	}}); err != nil {
		t.Fatalf("handle two offers: %v", err)
	}

	declines := lines.with(t, "declining an offer with no escrow to back it")
	if len(declines) != 1 {
		t.Fatalf("logged %d offer declines, want the one past the held lease: %v", len(declines), declines)
	}

	if reason, _ := declines[0]["reason"].(string); !strings.Contains(reason, "deployment ceiling") {
		t.Errorf("the decline's reason %q does not name the ceiling that refused the second lease", reason)
	}
}

// THE STATUS REPORT NAMES WHO HOLDS THE LINE. A tier waiting behind another is
// published with that tier's label, asked of the queue when the report is
// written, so `billet status` can say which waiter holds it.
func TestTheReportNamesTheWaiterThatHoldsTheLine(t *testing.T) {
	t.Parallel()

	// A third tier, which can grow once the small tier's jobs end and so waits
	// in line behind the large one; the small tier, at the most the ceiling
	// lets it hold, never waits at all.
	a, _, _, listeners := loggedListeners(t, append(contenders(), tierOf("c-medium", 4)), 8,
		config.AdmissionFair)
	small, large, medium := listeners[0], listeners[1], listeners[2]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	if err := medium.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("medium tier reconcile: %v", err)
	}

	medium.reportCapacity(t.Context(), nil, "")
	large.reportCapacity(t.Context(), nil, "")

	behind, err := a.CapacityReport(t.Context(), "c-medium")
	if err != nil {
		t.Fatalf("read the medium tier's report: %v", err)
	}

	if behind.Listener.WaitingSince == "" {
		t.Fatalf("the medium tier is not waiting, so this case cannot show who holds it: %+v", behind.Listener)
	}

	if behind.Listener.HeldBy != "b-large" || behind.Listener.HeldBySince == "" {
		t.Errorf("the medium tier's report says held by %q since %q, want the large tier and when it began waiting",
			behind.Listener.HeldBy, behind.Listener.HeldBySince)
	}

	head, err := a.CapacityReport(t.Context(), "b-large")
	if err != nil {
		t.Fatalf("read the large tier's report: %v", err)
	}

	if head.Listener.HeldBy != "" {
		t.Errorf("the waiter at the head of the line reports being held by %q", head.Listener.HeldBy)
	}

	if !strings.Contains(head.Listener.Refusal, "deployment ceiling") {
		t.Errorf("the head's report says its last purchase was refused for %q, want the ceiling",
			head.Listener.Refusal)
	}
}
