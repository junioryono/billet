package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/hostsource"
)

type orderLedger map[string]alloc.PoolOrder

func (l orderLedger) ReadPoolOrder(_ context.Context, id string) (alloc.PoolOrder, error) {
	if id == "unreadable" {
		return alloc.PoolOrder{}, errors.New("database is locked")
	}
	o, ok := l[id]
	if !ok {
		return alloc.PoolOrder{}, alloc.ErrPoolOrderClosed
	}

	return o, nil
}

// witnessSource is a source that answers a fixed testimony and records each claim and
// the deadline it was asked under; held, when set, keeps it answering until closed.
type witnessSource struct {
	*fakeSource
	mu        sync.Mutex
	answer    hostsource.Testimony
	claims    []hostsource.Claim
	deadlines []time.Duration
	entered   chan struct{}
	held      chan struct{}
}

func (w *witnessSource) Witness(ctx context.Context, c hostsource.Claim) hostsource.Testimony {
	w.mu.Lock()
	w.claims = append(w.claims, c)
	if d, ok := ctx.Deadline(); ok {
		w.deadlines = append(w.deadlines, time.Until(d))
	}
	w.mu.Unlock()
	if w.held != nil {
		w.entered <- struct{}{}
		<-w.held
	}

	return w.answer
}

func (w *witnessSource) asked() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.claims)
}

// THE JUDGE ASKS THE ORDER'S OWN SOURCE ABOUT THE HOST THE ORDER PLACED, and nothing
// else: the host is the order's, whatever the claim said, and an order with no host, a
// source gone from the config, one with no channel to its hosts, or an order that
// cannot be read is could-not-tell.
func TestTheJudgeAsksTheOrdersSourceAboutItsHost(t *testing.T) {
	t.Parallel()
	vouch := &witnessSource{fakeSource: &fakeSource{}, answer: hostsource.Testimony{Verdict: hostsource.VerdictVouches}}
	ledger := orderLedger{
		"pa":     {ID: "pa", Source: "aws", HostID: "i-1"},
		"blind":  {ID: "blind", Source: "pnap", HostID: "srv-1"},
		"nohost": {ID: "nohost", Source: "aws"},
		"gone":   {ID: "gone", Source: "removed", HostID: "i-2"},
	}
	j := NewJoins(ledger, map[string]hostsource.Source{"aws": vouch, "pnap": &fakeSource{}}, nil)
	got := j.JudgePoolJoin(t.Context(), hostsource.Claim{Order: "pa", HostID: "i-forged", Fingerprint: "SHA256:k"})
	if got.Verdict != hostsource.VerdictVouches || len(vouch.claims) != 1 || vouch.claims[0].HostID != "i-1" {
		t.Fatalf("judged %+v from claims %+v; want the order's host i-1 asked", got, vouch.claims)
	}
	for order, want := range map[string]string{
		"blind": "has no channel", "nohost": "not recorded", "gone": "not configured",
		"missing": "no such pool order", "unreadable": "could not be read",
	} {
		got := j.JudgePoolJoin(t.Context(), hostsource.Claim{Order: order, Fingerprint: "SHA256:k"})
		if got.Verdict != hostsource.VerdictUnknown || !strings.Contains(got.Reason, want) {
			t.Errorf("%s: %+v, want could-not-tell saying %q", order, got, want)
		}
	}
	if len(vouch.claims) != 1 {
		t.Fatalf("the source was asked %d times, want once", len(vouch.claims))
	}
}

// AN ORDER'S SOURCE IS ASKED AT MOST ONCE PER INTERVAL, WHATEVER KEY OR ADDRESS A CALLER
// PRESENTS: a host asks every five seconds, the console read is throttled, and a caller
// chooses its key. The claim asked gets its verdict back inside the interval; any other
// claim on the order is could-not-tell without asking, until the interval passes. Each
// ask carries a deadline inside the listener's write timeout.
func TestAnOrdersSourceIsAskedOncePerInterval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	src := &witnessSource{fakeSource: &fakeSource{}, answer: hostsource.Testimony{Verdict: hostsource.VerdictVouches}}
	j := NewJoins(orderLedger{"pa": {ID: "pa", Source: "aws", HostID: "i-1"}, "pb": {ID: "pb", Source: "aws", HostID: "i-2"}},
		map[string]hostsource.Source{"aws": src}, func() time.Time { return now })
	claim := hostsource.Claim{Order: "pa", Fingerprint: "SHA256:k", Address: "10.0.0.7"}
	for range 3 {
		if got := j.JudgePoolJoin(t.Context(), claim); got.Verdict != hostsource.VerdictVouches {
			t.Fatalf("the claim asked: %+v", got)
		}
	}
	otherKey, otherAddress := claim, claim
	otherKey.Fingerprint, otherAddress.Address = "SHA256:other", "10.0.0.8"
	for _, c := range []hostsource.Claim{otherKey, otherAddress} {
		if got := j.JudgePoolJoin(t.Context(), c); got.Verdict != hostsource.VerdictUnknown {
			t.Fatalf("another claim on the order inside the interval: %+v, want could-not-tell", got)
		}
	}
	if got := j.JudgePoolJoin(t.Context(), hostsource.Claim{Order: "pb", Fingerprint: "SHA256:k"}); got.Verdict != hostsource.VerdictVouches {
		t.Fatalf("another order inside the interval: %+v", got)
	}
	if n := src.asked(); n != 2 {
		t.Fatalf("asked %d times inside the interval, want once for each of two orders", n)
	}
	now = now.Add(DefaultJudgeEvery)
	j.JudgePoolJoin(t.Context(), otherKey)
	if n := src.asked(); n != 3 {
		t.Fatalf("asked %d times once the interval passed, want a third", n)
	}
	for _, d := range src.deadlines {
		if d <= 0 || d > JudgeTimeout {
			t.Fatalf("asked with %v left, want a deadline within %v", d, JudgeTimeout)
		}
	}
	if len(src.deadlines) != 3 {
		t.Fatalf("%d asks carried a deadline, want all 3", len(src.deadlines))
	}
	j.mu.Lock()
	n, m := len(j.last), len(j.asked)
	j.mu.Unlock()
	if n != 1 || m != 1 {
		t.Fatalf("%d verdicts and %d asks kept past their interval, want only the fresh one", n, m)
	}
}

// ONE ASK OF AN ORDER AT A TIME: a second request while the source is still being asked
// is could-not-tell at once, rather than a second ask of the source.
func TestAnOrdersSourceIsAskedOneAtATime(t *testing.T) {
	t.Parallel()
	src := &witnessSource{fakeSource: &fakeSource{}, answer: hostsource.Testimony{Verdict: hostsource.VerdictVouches},
		entered: make(chan struct{}), held: make(chan struct{})}
	j := NewJoins(orderLedger{"pa": {ID: "pa", Source: "aws", HostID: "i-1"}}, map[string]hostsource.Source{"aws": src}, nil)
	done := make(chan hostsource.Testimony)
	go func() {
		done <- j.JudgePoolJoin(t.Context(), hostsource.Claim{Order: "pa", Fingerprint: "SHA256:k"})
	}()
	<-src.entered
	if got := j.JudgePoolJoin(t.Context(), hostsource.Claim{Order: "pa", Fingerprint: "SHA256:k"}); got.Verdict != hostsource.VerdictUnknown {
		t.Fatalf("a request while the order is being asked: %+v", got)
	}
	close(src.held)
	if got := <-done; got.Verdict != hostsource.VerdictVouches {
		t.Fatalf("the ask in flight answered %+v", got)
	}
	if n := src.asked(); n != 1 {
		t.Fatalf("asked %d times, want once", n)
	}
}

// A VERDICT IS NOT REUSED ONCE THE ORDER HAS MOVED: a contradiction given before a
// person released a hold would otherwise hold the order again at once, without asking
// anything.
func TestAVerdictIsNotReusedOnceTheOrderMoved(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	src := &witnessSource{fakeSource: &fakeSource{}, answer: hostsource.Testimony{Verdict: hostsource.VerdictContradicts}}
	ledger := orderLedger{"pa": {ID: "pa", Source: "aws", HostID: "i-1", Updated: now}}
	j := NewJoins(ledger, map[string]hostsource.Source{"aws": src}, func() time.Time { return now })
	claim := hostsource.Claim{Order: "pa", Fingerprint: "SHA256:k"}
	if got := j.JudgePoolJoin(t.Context(), claim); got.Verdict != hostsource.VerdictContradicts {
		t.Fatalf("first: %+v", got)
	}
	o := ledger["pa"]
	o.Updated = now.Add(time.Second)
	ledger["pa"] = o
	if got := j.JudgePoolJoin(t.Context(), claim); got.Verdict == hostsource.VerdictContradicts {
		t.Fatalf("the verdict from before the order moved was reused: %+v", got)
	}
}
