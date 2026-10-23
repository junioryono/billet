package metrics

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// THE IDLE POWER POLICY'S METRICS COUNT WHAT THEY ARE TOLD: every transition
// and outcome from the start at zero, a value outside them under other, and
// the launch restore's latency absent until one is told and then the latest.
func TestTheIdlePowerPolicysMetricsCountWhatTheyAreTold(t *testing.T) {
	t.Parallel()

	reg, err := New("node")
	if err != nil {
		t.Fatal(err)
	}
	o, err := reg.PowerObserver([]string{"cap", "restore"}, []string{"ok", "failed"})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := Listen(t.Context(), "127.0.0.1:0", reg.Handler(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	code, body := get(t, srv, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics answered %d", code)
	}
	if strings.Contains(body, "billet_node_power_last_launch_restore_seconds ") {
		t.Error("a launch restore's latency is reported before any launch restored anything")
	}
	if !strings.Contains(body, `billet_node_power_changes_total{outcome="failed",transition="restore"} 0`+"\n") {
		t.Error("a transition nothing has happened to yet is not reported at zero")
	}

	o.PowerChanged("cap", "ok")
	o.PowerChanged("restore", "ok")
	o.PowerChanged("restore", "ok")
	o.PowerChanged("restore", "failed")
	o.PowerChanged("a transition from somewhere else", "ok")
	o.PowerLaunchRestore(3 * time.Second)
	o.PowerLaunchRestore(1500 * time.Microsecond)

	_, body = get(t, srv, "/metrics")
	for _, want := range []string{
		`billet_node_power_changes_total{outcome="ok",transition="cap"} 1`,
		`billet_node_power_changes_total{outcome="ok",transition="restore"} 2`,
		`billet_node_power_changes_total{outcome="failed",transition="restore"} 1`,
		`billet_node_power_changes_total{outcome="failed",transition="cap"} 0`,
		`billet_node_power_changes_total{outcome="ok",transition="other"} 1`,
		"billet_node_power_last_launch_restore_seconds 0.0015",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics does not carry %q", want)
		}
	}
	if strings.Contains(body, "somewhere else") {
		t.Error("a value outside the lists became a label")
	}
}
