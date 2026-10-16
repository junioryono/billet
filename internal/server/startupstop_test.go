package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// requestCancelled is how the scale-set client reports a round trip its
// context ended: net/http's *url.Error, wrapping the context's error.
func requestCancelled() error {
	return &url.Error{Op: "Post", URL: "https://api.github.com/scaleset", Err: context.Canceled}
}

// startupLimits is a budget no test here comes near.
var startupLimits = alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}

// trustedTier is a tier whose runner-group policy Run checks before it
// reconciles anything.
func trustedTier() config.Tier {
	tr := tier("billet-4vcpu-a")
	tr.Trust = config.WorkloadTrusted
	tr.RunnerGroup = "trusted"
	tr.Workflows = []string{"acme/api/.github/workflows/ci.yml@refs/heads/main"}

	return tr
}

// A STOP THAT LANDS ANYWHERE IN STARTUP IS A STOP (#137). These steps return
// before any listener exists, so the classification at Run's end never sees
// their errors; without one of their own, a stop landing in a GitHub round trip
// or the startup reap came back as a failure, the process exited 1, and systemd
// held a failed unit that `billet server retire` refuses to act against.
//
// Each case lands the stop inside its step, in the order Run takes them; the
// ledger steps meet it through the real ledger, the GitHub steps through what
// the scale-set client returns for a cancelled round trip.
func TestAStopDuringStartupEndsRunCleanly(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		// step is what the step's own error says, which the stop's log line
		// carries: it proves the stop landed in this step and not a later one.
		step string
		arm  func(t *testing.T, stop context.CancelFunc, log *slog.Logger) *Server
	}{
		{
			// The ledger read of the scale sets billet created is the first thing
			// Run asks of anything, so a stop as Run begins lands in it.
			name: "the report of scale sets no tier declares",
			step: "read recorded scale sets",
			arm: func(t *testing.T, stop context.CancelFunc, log *slog.Logger) *Server {
				t.Helper()

				tiers := []config.Tier{tier("billet-4vcpu-a")}
				prov := &fakeProvisioner{}

				stop()

				return New(newAllocator(t, startupLimits, tiers), prov, tiers, "test-owner", log,
					WithCompletionLedger(openState(t)),
					WithTargets(Target{Config: testTarget, Provisioner: prov}))
			},
		},
		{
			name: "the trusted tier's runner-group check",
			step: "refuse trusted tier",
			arm: func(t *testing.T, stop context.CancelFunc, log *slog.Logger) *Server {
				t.Helper()

				tiers := []config.Tier{trustedTier()}
				prov := &fakeProvisioner{onValidate: func(string) error {
					stop()

					return requestCancelled()
				}}

				return New(newAllocator(t, startupLimits, tiers), prov, tiers, "test-owner", log)
			},
		},
		{
			name: "the scale set's reconciliation with GitHub",
			step: "reconcile scale set",
			arm: func(t *testing.T, stop context.CancelFunc, log *slog.Logger) *Server {
				t.Helper()

				tiers := []config.Tier{tier("billet-4vcpu-a")}
				prov := &fakeProvisioner{onEnsure: func(string) error {
					stop()

					return requestCancelled()
				}}

				return New(newAllocator(t, startupLimits, tiers), prov, tiers, "test-owner", log)
			},
		},
		{
			// GitHub answers and the stop lands as it does, so the ledger write
			// that records the set meets it.
			name: "the scale set's record in the ledger",
			step: "record scale set",
			arm: func(t *testing.T, stop context.CancelFunc, log *slog.Logger) *Server {
				t.Helper()

				tiers := []config.Tier{tier("billet-4vcpu-a")}
				prov := &fakeProvisioner{onEnsure: func(string) error {
					stop()

					return nil
				}}

				return New(newAllocator(t, startupLimits, tiers), prov, tiers, "test-owner", log,
					WithCompletionLedger(openState(t)),
					WithTargets(Target{Config: testTarget, Provisioner: prov}))
			},
		},
		{
			// With no completion ledger, what takes the context after GitHub is
			// the startup reap, against the allocator's ledger.
			name: "the startup reap",
			step: "reclaim abandoned capacity",
			arm: func(t *testing.T, stop context.CancelFunc, log *slog.Logger) *Server {
				t.Helper()

				tiers := []config.Tier{tier("billet-4vcpu-a")}
				prov := &fakeProvisioner{onEnsure: func(string) error {
					stop()

					return nil
				}}

				return New(newAllocator(t, startupLimits, tiers), prov, tiers, "test-owner", log)
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			ctx, stop := context.WithCancel(t.Context())
			defer stop()

			handler := newCapturingHandler()
			plane := c.arm(t, stop, slog.New(handler))

			if err := plane.Run(ctx); err != nil {
				t.Fatalf("a control plane stopped during %s returned %v, so the process exits "+
					"non-zero for a stop it was asked for", c.name, err)
			}

			if detail := handler.startupStop(); !strings.Contains(detail, c.step) {
				t.Errorf("the stop was meant to land in %s and the startup said it stopped in %q",
					c.name, detail)
			}
		})
	}
}

// AND ONLY A STOP IS. The GitHub steps failing for a reason of their own are
// failures: a cancellation of some inner context while nobody asked the
// process to stop, and a real fault that arrived alongside the stop.
func TestAFailureDuringStartupIsStillAFailure(t *testing.T) {
	t.Parallel()

	fault := errors.New("the scale-set client failed for a reason of its own")

	for _, c := range []struct {
		name string
		// stop says whether the process is asked to stop as the step fails.
		stop bool
		err  error
	}{
		{"an inner cancellation while running", false, requestCancelled()},
		{"a fault beside the stop", true, errors.Join(requestCancelled(), fault)},
	} {
		for _, step := range []struct {
			name  string
			tiers []config.Tier
			prov  func(failed func() error) *fakeProvisioner
		}{
			{
				name:  "the trusted tier's runner-group check",
				tiers: []config.Tier{trustedTier()},
				prov: func(failed func() error) *fakeProvisioner {
					return &fakeProvisioner{onValidate: func(string) error { return failed() }}
				},
			},
			{
				name:  "the scale set's reconciliation with GitHub",
				tiers: []config.Tier{tier("billet-4vcpu-a")},
				prov: func(failed func() error) *fakeProvisioner {
					return &fakeProvisioner{onEnsure: func(string) error { return failed() }}
				},
			},
		} {
			t.Run(step.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()

				ctx, stop := context.WithCancel(t.Context())
				defer stop()

				failed := func() error {
					if c.stop {
						stop()
					}

					return c.err
				}

				err := New(newAllocator(t, startupLimits, step.tiers), step.prov(failed), step.tiers,
					"test-owner", nil).Run(ctx)
				if err == nil || !strings.Contains(err.Error(), "context canceled") {
					t.Fatalf("%s during %s returned %v, want the failure", c.name, step.name, err)
				}

				if c.stop && !errors.Is(err, fault) {
					t.Errorf("a fault beside the stop during %s was dropped: %v", step.name, err)
				}
			})
		}
	}
}

// startupStop is the detail of the line Run logs when a stop lands in its
// startup, or empty when it logged none.
func (h *capturingHandler) startupStop() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	for i := range *h.records {
		r := &(*h.records)[i]
		if r.Message != "stopped while reconciling this control plane's scale sets" {
			continue
		}

		var detail string

		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "detail" {
				detail = a.Value.String()
			}

			return true
		})

		return detail
	}

	return ""
}

// A TIER THAT ENDS WITH A CANCELLATION WHILE NOBODY ASKED THE PLANE TO STOP HAS
// FAILED. A session response billet cannot act on ends its tier whatever else
// is true, and one cut short by an inner context carries context.Canceled; the
// classification that asked errors.Is of every branch read that as the stop,
// so Run answered nil, the process exited 0, and Restart=on-failure had nothing
// to restart.
func TestATierEndingOnACancellationNobodyAskedForIsAFailure(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}

	cut := fmt.Errorf("%w: the runner lookup was cut short: %w", ErrUntrustworthySession, context.Canceled)

	prov := &fakeProvisioner{newSession: func(string) Session {
		return &fakeSession{onGet: func() (*Message, error) { return nil, cut }}
	}}

	// A FAILURE GUARD, never the condition: a Run that reopened the session
	// instead of ending would come back at this deadline with no error.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := New(newAllocator(t, startupLimits, tiers), prov, tiers, "test-owner", nil).Run(ctx)
	if !errors.Is(err, ErrUntrustworthySession) {
		t.Fatalf("a tier that ended on a cancellation nobody asked for returned %v, want its failure", err)
	}

	if ctx.Err() != nil {
		t.Errorf("Run returned only once its guard had expired: %v", err)
	}
}

// A STOP ACROSS MANY TIERS IS STILL A STOP. Each tier's error is asked on its
// own: their join, for a deployment this size, is more than the cause walk
// will read, and a stop read through it comes back as could-not-tell.
func TestAStopAcrossManyTiersIsStillAStop(t *testing.T) {
	t.Parallel()

	stopped, stop := context.WithCancel(t.Context())
	stop()

	errs := make([]error, 0, 300)
	for i := range 300 {
		errs = append(errs, fmt.Errorf("server: listener billet-%d: %w", i, context.Canceled))
	}

	// THE REASON THIS IS ASKED TIER BY TIER, so the test fails if the walk's budget
	// grows past it and this stops proving anything.
	if state.StoppedBy(stopped, errors.Join(errs...)) {
		t.Fatal("the join of 300 tiers' stops is within the cause walk's budget, so this case " +
			"no longer exceeds it; grow it")
	}

	if !allStopped(stopped, errs) {
		t.Error("300 tiers that each ended on the stop were not read as a stop")
	}

	withFault := make([]error, 0, len(errs)+1)
	withFault = append(withFault, errs...)
	withFault = append(withFault, errors.New("a tier failed for a reason of its own"))
	if allStopped(stopped, withFault) {
		t.Error("a tier's own failure among 300 stops was read as a stop")
	}

	if allStopped(t.Context(), errs) {
		t.Error("tiers that ended on a cancellation with nobody stopping were read as a stop")
	}
}

// AND A TIER THAT ENDS ON NOTHING BUT A CANCELLATION, with nobody asking the
// plane to stop, has failed too. There is no other cause here to make it a
// failure, so this is the case only the context answers: every cause is a
// cancellation, and it is not the stop.
func TestATierEndingOnlyOnACancellationNobodyAskedForIsAFailure(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	prov := &fakeProvisioner{sessionErr: requestCancelled()}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := New(newAllocator(t, startupLimits, tiers), prov, tiers, "test-owner", nil).Run(ctx)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "open session") {
		t.Fatalf("a tier whose session open was cancelled with nobody stopping returned %v, want "+
			"that failure", err)
	}

	if ctx.Err() != nil {
		t.Errorf("Run returned only once its guard had expired: %v", err)
	}
}
