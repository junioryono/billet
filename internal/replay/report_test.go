package replay

import (
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// The report's own arithmetic, driven directly: what the sweep does with a
// charge the ledger never ended, and what the read does with a history that
// fills its bound. Neither state arises in a healthy replay, which is why the
// scenarios cannot reach them and these do.

func oneHost() Fleet {
	return Fleet{
		Hosts:     []Host{{Name: "only", VCPU: 8, Memory: 32 * config.GiB}},
		Tiers:     []TierShape{{Label: "billet-4vcpu", VCPU: 4, Memory: 8 * config.GiB}},
		MaxVCPU:   8,
		MaxMemory: 32 * config.GiB,
	}
}

// chargeAt is one 4 vCPU charge on the host, from escrow to archive; a zero
// archive is a charge the ledger never ended.
func chargeAt(from, to time.Time) Record {
	return Record{
		Tier: "billet-4vcpu", Node: "only", VCPU: 4, Memory: 8 * config.GiB,
		ChargedFrom: from, FinishedAt: to,
	}
}

// AN OPEN CHARGE THAT BEGINS AT THE LAST INSTANT STILL COUNTS. Closed at that
// instant it would be an empty interval, released before it charged under the
// tie rule, and the one lease a swallowed completion leaves behind would be the
// one the proof could not see.
func TestAnOpenChargeAtTheLastInstantIsStillCounted(t *testing.T) {
	t.Parallel()

	start := DefaultStart

	r := &Report{Fleet: oneHost(), Records: []Record{
		chargeAt(start, start.Add(10*time.Minute)),
		chargeAt(start.Add(5*time.Minute), start.Add(10*time.Minute)),
		// Escrowed at the instant the others end, never archived: with the host
		// full until then, this is 12 vCPU on an 8 vCPU host from that instant on.
		chargeAt(start.Add(10*time.Minute), time.Time{}),
		chargeAt(start.Add(10*time.Minute), time.Time{}),
		chargeAt(start.Add(10*time.Minute), time.Time{}),
	}}

	for i := range r.Records {
		r.Records[i].Seq = int64(i + 1)
	}

	if peak := r.PeakDeploymentVCPU(); peak != 12 {
		t.Fatalf("the deployment peaked at %d vCPU, want 12 with the three open charges counted", peak)
	}

	violations := r.checkCapacity()
	if len(violations) == 0 {
		t.Fatal("three open 4 vCPU charges on an 8 vCPU host were not reported as an overcommit")
	}

	if !strings.Contains(violations[0], "host only carried 12 vCPU") {
		t.Errorf("the violation does not name the host and the load: %v", violations)
	}
}

// A RECORD WHOSE TIMESTAMPS CONTRADICT THE LIFECYCLE IS REFUSED.
func TestARecordOutOfOrderIsRefused(t *testing.T) {
	t.Parallel()

	start := DefaultStart
	minute := func(n int) time.Time { return start.Add(time.Duration(n) * time.Minute) }

	good := Record{Arrival: minute(1), ChargedFrom: minute(0), AssignedAt: minute(2), StartedAt: minute(3), FinishedAt: minute(9)}
	if err := good.checkOrder(); err != nil {
		t.Fatalf("a well-ordered record was refused: %v", err)
	}

	cancelled := Record{Arrival: minute(1), ChargedFrom: minute(0), AssignedAt: minute(2), FinishedAt: minute(3)}
	if err := cancelled.checkOrder(); err != nil {
		t.Fatalf("an assigned-then-cancelled record with no start was refused: %v", err)
	}

	for name, rec := range map[string]Record{
		"no assignment":            {Arrival: minute(1), ChargedFrom: minute(0), FinishedAt: minute(3)},
		"assigned before escrow":   {Arrival: minute(1), ChargedFrom: minute(2), AssignedAt: minute(1), FinishedAt: minute(3)},
		"started before assigned":  {Arrival: minute(1), ChargedFrom: minute(0), AssignedAt: minute(3), StartedAt: minute(2), FinishedAt: minute(9)},
		"started before arrival":   {Arrival: minute(5), ChargedFrom: minute(0), AssignedAt: minute(2), StartedAt: minute(3), FinishedAt: minute(9)},
		"finished before assigned": {Arrival: minute(1), ChargedFrom: minute(0), AssignedAt: minute(4), FinishedAt: minute(3)},
		"finished before started":  {Arrival: minute(1), ChargedFrom: minute(0), AssignedAt: minute(2), StartedAt: minute(5), FinishedAt: minute(4)},
	} {
		if err := rec.checkOrder(); err == nil {
			t.Errorf("a record %s was accepted", name)
		}
	}
}

// A HISTORY THAT FILLS ITS BOUND IS REFUSED, one short of it is not.
func TestAHistoryThatFillsItsBoundIsRefused(t *testing.T) {
	t.Parallel()

	if err := complete(41, 42); err != nil {
		t.Fatalf("a history one row short of the bound was refused: %v", err)
	}

	err := complete(42, 42)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("a history that fills its bound was accepted, or the refusal does not say why: %v", err)
	}
}

// A CHARGE THE SWEEP COULD NOT JUDGE IS REFUSED, never swept past.
func TestAChargeTheSweepCannotJudgeIsRefused(t *testing.T) {
	t.Parallel()

	fleet := oneHost()
	start := DefaultStart

	for name, c := range map[string]charge{
		"no escrow time":         {to: start, vcpu: 4, memory: config.GiB, node: "only"},
		"archived before escrow": {from: start.Add(time.Minute), to: start, vcpu: 4, memory: config.GiB, node: "only"},
		"zero vcpu":              {from: start, to: start.Add(time.Minute), vcpu: 0, memory: config.GiB, node: "only"},
		"zero memory":            {from: start, to: start.Add(time.Minute), vcpu: 4, memory: 0, node: "only"},
		"an unknown host":        {from: start, to: start.Add(time.Minute), vcpu: 4, memory: config.GiB, node: "elsewhere"},
	} {
		if err := fleet.checkCharge(c); err == nil {
			t.Errorf("a charge with %s was accepted", name)
		}
	}

	good := charge{from: start, to: start.Add(time.Minute), vcpu: 4, memory: config.GiB, node: "only"}
	if err := fleet.checkCharge(good); err != nil {
		t.Errorf("a well-formed charge was refused: %v", err)
	}

	open := charge{from: start, vcpu: 4, memory: config.GiB, node: "only"}
	if err := fleet.checkCharge(open); err != nil {
		t.Errorf("an open charge was refused: %v", err)
	}
}
