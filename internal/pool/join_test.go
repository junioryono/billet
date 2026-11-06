package pool

import (
	"context"
	"errors"
	"strings"
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

// witnessSource is a source that answers a fixed testimony and records each claim.
type witnessSource struct {
	*fakeSource
	answer hostsource.Testimony
	claims []hostsource.Claim
}

func (w *witnessSource) Witness(_ context.Context, c hostsource.Claim) hostsource.Testimony {
	w.claims = append(w.claims, c)

	return w.answer
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

// ONE CLAIM IS ASKED OF ITS SOURCE AT MOST ONCE PER INTERVAL, because a host asks every
// five seconds and the console read is throttled; another key or address is its own
// claim, and the verdict is asked again once the interval has passed.
func TestOneClaimIsAskedOncePerInterval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	src := &witnessSource{fakeSource: &fakeSource{}}
	j := NewJoins(orderLedger{"pa": {ID: "pa", Source: "aws", HostID: "i-1"}},
		map[string]hostsource.Source{"aws": src}, func() time.Time { return now })
	claim := hostsource.Claim{Order: "pa", Fingerprint: "SHA256:k", Address: "10.0.0.7"}
	for range 3 {
		j.JudgePoolJoin(t.Context(), claim)
	}
	other := claim
	other.Fingerprint = "SHA256:other"
	j.JudgePoolJoin(t.Context(), other)
	if len(src.claims) != 2 {
		t.Fatalf("asked %d times inside the interval, want once for each of two claims", len(src.claims))
	}
	now = now.Add(DefaultJudgeEvery)
	j.JudgePoolJoin(t.Context(), claim)
	if len(src.claims) != 3 {
		t.Fatalf("asked %d times once the interval passed, want a third", len(src.claims))
	}
	j.mu.Lock()
	n := len(j.last)
	j.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d verdicts kept past their interval, want only the fresh one", n)
	}
}
