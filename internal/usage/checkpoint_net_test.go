package usage

import (
	"encoding/json"
	"testing"
	"time"
)

// A SERIES IS CONTINUED WHEN ITS NETWORK IS FOUND ONLY NOW: a tart VM an older
// node measured without its link is the same compute once the link is found,
// and its series is kept. A series that had a device continues only on that
// device, and other compute is refused as ever.
func TestASeriesIsContinuedWhenItsNetworkIsFoundOnlyNow(t *testing.T) {
	t.Parallel()

	withNet := func(target Target, dev string) Target {
		target.NetDevice, target.NetHostView, target.NetPID, target.NetPIDStart = dev, true, softnetPID, softnetStart

		return target
	}
	checkpointed := func(t *testing.T, target Target) Checkpoint {
		t.Helper()

		tr := newTree(t)
		h := newProcessHost()
		now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
		a := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
		a.reader.Process = h.reader.Process
		a.Start("vm", target, 6)
		now = now.Add(time.Second)
		a.Tick()
		cp, ok := a.Checkpoint("vm")
		if !ok {
			t.Fatal("no checkpoint of a measured VM")
		}
		data, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		back, err := ReadCheckpoint(data)
		if err != nil {
			t.Fatalf("ReadCheckpoint: %v", err)
		}

		return back
	}
	other := processTarget
	other.PIDStart++

	for name, tc := range map[string]struct {
		from, to Target
		carried  bool
	}{
		"the same compute":            {from: processTarget, to: processTarget, carried: true},
		"its link found only now":     {from: processTarget, to: withNet(processTarget, "vmenet0"), carried: true},
		"its link the same device":    {from: withNet(processTarget, "vmenet0"), to: withNet(processTarget, "vmenet0"), carried: true},
		"its link now another device": {from: withNet(processTarget, "vmenet0"), to: withNet(processTarget, "vmenet1")},
		"its link now lost":           {from: withNet(processTarget, "vmenet0"), to: processTarget},
		"another VM, its link found":  {from: processTarget, to: withNet(other, "vmenet0")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cp := checkpointed(t, tc.from)
			if err := cp.continues(tc.to, time.Second, true); (err == nil) != tc.carried {
				t.Errorf("continuing %+v as %+v answered %v, want carried %v", tc.from, tc.to, err, tc.carried)
			}
		})
	}
}
