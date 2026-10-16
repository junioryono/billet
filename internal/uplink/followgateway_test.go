package uplink

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeGateway is a gatewaySetter that keeps whom it would ask.
type fakeGateway struct{ gateway string }

func (f *fakeGateway) Gateway() string { return f.gateway }

func (f *fakeGateway) setGateway(gateway string) (bool, error) {
	if gateway == f.gateway {
		return false, nil
	}
	f.gateway = gateway

	return true, nil
}

// A RUN FOLLOWS THE DEFAULT ROUTE'S GATEWAY: a new one is asked and judged
// afresh, its baseline and every cut's evidence of the old one forgotten; an
// unchanged one, or a lookup that could not tell, changes nothing.
func TestARunFollowsTheDefaultRoutesGateway(t *testing.T) {
	t.Parallel()

	logs := &records{}
	st := &stepper{}
	st.gateway.Observe([]string{"192.168.1.254"}, map[string]time.Duration{"192.168.1.254": time.Millisecond})
	ctl := newController()
	ctl.Down.cutGateway = measured{40 * time.Millisecond, true}
	ctl.Up.cutGateway = measured{40 * time.Millisecond, true}
	ctl.gatewayStalled = true
	ctl.lastGateway, ctl.lastGatewayAt = measured{30 * time.Millisecond, true}, at(0)
	pinger := &fakeGateway{gateway: "192.168.1.254"}

	followGateway(slog.New(logs), func() (string, error) { return "192.168.1.254", nil }, pinger, st, ctl)
	followGateway(slog.New(logs), func() (string, error) { return "", errors.New("no route") }, pinger, st, ctl)
	if pinger.gateway != "192.168.1.254" || !ctl.Down.cutGateway.known || st.gateway.baseline == nil ||
		logs.count("gateway changed") != 0 {
		t.Fatal("an unchanged gateway, or a lookup that could not tell, changed what is asked or known")
	}

	followGateway(slog.New(logs), func() (string, error) { return "192.168.1.1", nil }, pinger, st, ctl)
	if pinger.gateway != "192.168.1.1" {
		t.Errorf("the run still asks %s after the route moved to 192.168.1.1", pinger.gateway)
	}
	if ctl.Down.cutGateway.known || ctl.Up.cutGateway.known || ctl.gatewayStalled ||
		st.gateway.baseline != nil || ctl.lastGateway.known {
		t.Error("the old gateway's baseline, its stall or a cut's evidence of it outlived the change")
	}
	if logs.count("gateway changed") != 1 {
		t.Error("the change was not said")
	}
}

// THE PINGER ASKS THE NEW GATEWAY FROM ITS NEXT ROUND, beside the same
// reflectors, none when the route has no gateway, and only as a reflector when
// it is one; a change that asks nobody new is no change.
func TestThePingerAsksTheGatewayItIsMovedTo(t *testing.T) {
	t.Parallel()

	_, gateway, targets, err := probeTargets([]string{"1.1.1.1", "8.8.8.8"}, "192.168.1.254")
	if err != nil {
		t.Fatal(err)
	}
	p := &Pinger{reflectors: []string{"1.1.1.1", "8.8.8.8"}, gateway: gateway, targets: targets}

	for _, c := range []struct {
		move, gateway, targets string
		changed                bool
	}{
		{"192.168.1.254", "192.168.1.254", "1.1.1.1,8.8.8.8,192.168.1.254", false},
		{"192.168.1.1", "192.168.1.1", "1.1.1.1,8.8.8.8,192.168.1.1", true},
		{"8.8.8.8", "", "1.1.1.1,8.8.8.8", true},
		{"", "", "1.1.1.1,8.8.8.8", false},
		{"10.0.0.1", "10.0.0.1", "1.1.1.1,8.8.8.8,10.0.0.1", true},
	} {
		changed, err := p.setGateway(c.move)
		if err != nil || changed != c.changed || p.Gateway() != c.gateway || strings.Join(p.targets, ",") != c.targets {
			t.Errorf("moved to %q: changed %v (%v), asking gateway %q and %v; want %v, %q and %s",
				c.move, changed, err, p.Gateway(), p.targets, c.changed, c.gateway, c.targets)
		}
	}
}
