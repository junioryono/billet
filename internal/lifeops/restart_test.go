package lifeops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

const shaperUnit = "billet-uplink.service"

// restartCalls is every `systemctl restart` the fixture was asked for.
func restartCalls(a *answers) [][]string {
	return slices.DeleteFunc(slices.Clone(a.calls), func(call []string) bool { return call[0] != "restart" })
}

// A RUNNING, ENABLED UNIT IS RESTARTED AND PROVED; ANYTHING ELSE IS LEFT ALONE.
// The shaper is restarted onto a replaced binary only where it runs and an
// operator left it enabled: restarting a stopped one would start what somebody
// stopped, and one that is not installed has nothing to restart.
func TestRestartIfRunningRestartsOnlyAnEnabledRunningUnit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		over    map[string]string
		restart bool
		how     string
	}{
		{name: "enabled and active", over: nil, restart: true},
		{name: "enabled and restarting itself", over: map[string]string{"ActiveState": "activating"}, restart: true},
		{name: "disabled and active", over: map[string]string{"UnitFileState": "disabled"}, how: "not enabled"},
		{name: "static", over: map[string]string{"UnitFileState": "static"}, how: "not enabled"},
		{name: "enabled-runtime", over: map[string]string{"UnitFileState": "enabled-runtime"}, how: "not enabled"},
		{name: "masked", over: map[string]string{"LoadState": "masked"}, how: "LoadState=masked"},
		{name: "enabled and stopped", over: map[string]string{"ActiveState": "inactive"}, how: "not running"},
		{name: "enabled and failed", over: map[string]string{"ActiveState": "failed"}, how: "not running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{shaperUnit: measured(tc.over)}}
			c, _ := h.converger(t, a)

			// AFTER A RESTART THE UNIT IS RUNNING, as systemd would report it.
			run := c.inspector.run
			c.inspector.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
				if args[0] == "restart" {
					a.reply[shaperUnit] = measured(nil)
				}

				return run(ctx, bin, args)
			}

			got, err := c.RestartIfRunning(t.Context(), shaperUnit)
			if err != nil {
				t.Fatalf("RestartIfRunning: %v", err)
			}

			calls := restartCalls(a)

			if !tc.restart {
				if got.Restarted || len(calls) != 0 {
					t.Fatalf("a unit that should be left alone was restarted (%+v, %v)", got, calls)
				}

				if !strings.Contains(got.How, tc.how) {
					t.Errorf("left alone saying %q, want it to say %q", got.How, tc.how)
				}

				return
			}

			// The `--` is part of the contract: a unit name may begin with a dash.
			if !got.Restarted || len(calls) != 1 || !slices.Equal(calls[0], []string{"restart", "--", shaperUnit}) {
				t.Fatalf("a running, enabled unit answered %+v with restarts %v; want one restart of %s",
					got, calls, shaperUnit)
			}
		})
	}
}

// A UNIT THAT IS NOT THERE IS LEFT ALONE: absence is a value systemd states.
func TestRestartIfRunningLeavesAnAbsentUnitAlone(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{}}
	c, _ := h.converger(t, a)

	got, err := c.RestartIfRunning(t.Context(), shaperUnit)
	if err != nil || got.Restarted || len(restartCalls(a)) != 0 {
		t.Fatalf("an absent unit answered %+v, %v with restarts %v; want it left alone", got, err, restartCalls(a))
	}

	if !strings.Contains(got.How, "not-found") {
		t.Errorf("an absent unit was left alone saying %q, want not-found", got.How)
	}
}

// WHAT SYSTEMD DID NOT STATE IS AN ERROR, NEVER "NOT RUNNING", and nothing is
// restarted on it: a missing answer, a word billet has no rule for, a manager
// that could not be asked.
func TestRestartIfRunningCannotTellAndSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name string
		over map[string]string
		fail error
	}{
		{name: "no LoadState", over: map[string]string{"LoadState": "REMOVE"}},
		{name: "a broken unit", over: map[string]string{"LoadState": "bad-setting"}},
		{name: "no UnitFileState", over: map[string]string{"UnitFileState": "REMOVE"}},
		{name: "no ActiveState", over: map[string]string{"ActiveState": "REMOVE"}},
		{name: "an unknown ActiveState", over: map[string]string{"ActiveState": "maintenance"}},
		{name: "systemd could not be asked", fail: errors.New("Failed to connect to bus")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			a := &answers{reply: map[string]string{shaperUnit: measured(tc.over)}, fail: tc.fail}
			c, _ := h.converger(t, a)

			got, err := c.RestartIfRunning(t.Context(), shaperUnit)
			if err == nil {
				t.Fatalf("an answer billet cannot read came back as %+v and no error", got)
			}

			if len(restartCalls(a)) != 0 {
				t.Fatalf("a unit whose state could not be told was restarted: %v", restartCalls(a))
			}
		})
	}
}

// A RESTART THAT FAILS, OR A UNIT THAT DOES NOT HOLD ITS NEW PROCESS, IS AN
// ERROR, not a restart.
func TestRestartIfRunningReportsARestartThatDidNotTake(t *testing.T) {
	h := newHost(t)
	a := &answers{reply: map[string]string{shaperUnit: measured(nil)}}
	c, _ := h.converger(t, a)

	run := c.inspector.run
	c.inspector.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if args[0] == "restart" {
			a.calls = append(a.calls, args)

			return nil, errors.New("Job for billet-uplink.service failed")
		}

		return run(ctx, bin, args)
	}

	if got, err := c.RestartIfRunning(t.Context(), shaperUnit); err == nil || !strings.Contains(err.Error(), "restart "+shaperUnit) {
		t.Fatalf("a failed restart answered %+v, %v; want an error naming the restart", got, err)
	}

	h2 := newHost(t)
	a2 := &answers{reply: map[string]string{}}
	c2, ops := h2.converger(t, a2)
	c2.inspector.run = func(_ context.Context, _ string, args []string) ([]byte, error) {
		a2.calls = append(a2.calls, args)
		if args[0] != "show" {
			return nil, nil
		}

		// THE SECOND SAMPLE, after the settle window, is a new process.
		if ops.released {
			return []byte(filterProperties(measured(map[string]string{"MainPID": "2200", "NRestarts": "1"}), args)), nil
		}

		return []byte(filterProperties(measured(nil), args)), nil
	}

	if got, err := c2.RestartIfRunning(t.Context(), shaperUnit); err == nil || !strings.Contains(err.Error(), "did not hold") {
		t.Fatalf("a shaper that died after its restart answered %+v, %v; want it reported as not holding", got, err)
	}
}
