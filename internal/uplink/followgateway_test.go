package uplink

import (
	"errors"
	"log/slog"
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
	st := &stepper{lastGateway: gatewayRose(30 * time.Millisecond)}
	st.gateway.Observe([]string{"192.168.1.254"}, map[string]time.Duration{"192.168.1.254": time.Millisecond})
	ctl := newController()
	ctl.Down.cutGateway = measured{40 * time.Millisecond, true}
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
	if ctl.Down.cutGateway.known || st.gateway.baseline != nil || st.lastGateway.DelayKnown {
		t.Error("the old gateway's baseline or a cut's evidence of it outlived the change")
	}
	if logs.count("gateway changed") != 1 {
		t.Error("the change was not said")
	}
}
