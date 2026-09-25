package nodeplane_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/server"
)

// A LAUNCH REFUSED BECAUSE THE NODE IS DRAINING TAKES THE HOST OUT OF
// PLACEMENT, with the fence of the registration the plane was talking to, so the
// plane stops dispatching launches the node will only refuse. A plain failure
// marks nothing.
func TestADrainingRefusalOverTheWireTakesTheHostOutOfPlacement(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		draining bool
	}{
		{"a draining refusal", true},
		{"an ordinary failure", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := &fakeRegistrar{}
			p, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg),
				nodeplane.WithCommandTimeout(60*time.Second))
			c := dial(t, base)

			reported := make(chan error, 1)

			go func() {
				cmd, ok, err := c.Poll(t.Context())
				if err != nil || !ok {
					reported <- err

					return
				}

				reported <- c.Report(t.Context(), nodeapi.CommandResult{ID: cmd.ID,
					Error: "this node is draining and will not start new work", Draining: tc.draining})
			}()

			lease := &alloc.Lease{ID: "l1", Tier: "billet-2vcpu", VCPU: 2, Memory: 8 * config.GiB,
				GuestOS: config.GuestLinux, Providers: []config.ProviderKind{config.ProviderDocker}, Epoch: 1}

			if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}); err == nil {
				t.Fatal("a refused launch was reported as started")
			}

			if err := <-reported; err != nil {
				t.Fatalf("the node could not report: %v", err)
			}

			told := reg.drainings()

			if !tc.draining {
				if len(told) != 0 {
					t.Errorf("an ordinary launch failure took the host out of placement: %+v", told)
				}

				return
			}

			if len(told) != 1 || told[0].name != "n1" || told[0].epoch != 1 ||
				told[0].incarnation != c.Incarnation() {
				t.Errorf("the ledger was told %+v, want n1 at epoch 1 from %s", told, c.Incarnation())
			}
		})
	}
}

// THE MARK IS FENCED TO THE PROCESS THAT REFUSED. A replacement that registers
// under the name between the refusal and its report must not be the one marked
// draining: it would stay out of placement for as long as it ran without
// registering again.
func TestADrainingRefusalMarksTheProcessThatRefusedNotItsReplacement(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistrar{}
	p, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg),
		nodeplane.WithCommandTimeout(60*time.Second))
	first := dial(t, base)

	lease := &alloc.Lease{ID: "l1", Tier: "billet-2vcpu", VCPU: 2, Memory: 8 * config.GiB,
		GuestOS: config.GuestLinux, Providers: []config.ProviderKind{config.ProviderDocker}, Epoch: 1}

	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}) }()

	cmd, ok, err := first.Poll(t.Context())
	if err != nil || !ok {
		t.Fatalf("Poll = ok %v, err %v", ok, err)
	}

	second := dial(t, base)
	if second.Incarnation() == first.Incarnation() {
		t.Fatal("the replacement has the same incarnation, so this proves nothing")
	}

	if err := first.Report(t.Context(), nodeapi.CommandResult{ID: cmd.ID,
		Error: "this node is draining and will not start new work", Draining: true}); err != nil {
		t.Fatalf("the process that took the launch could not report it: %v", err)
	}

	if err := <-launched; err == nil {
		t.Fatal("a refused launch was reported as started")
	}

	told := reg.drainings()
	if len(told) != 1 || told[0].incarnation != first.Incarnation() || told[0].epoch != 1 {
		t.Errorf("the ledger was told %+v, want the refusing process %s at epoch 1, not its "+
			"replacement %s", told, first.Incarnation(), second.Incarnation())
	}
}

// A WITHDRAWAL CROSSES THE WIRE AS THE CURRENT PROCESS'S OWN STATEMENT, and
// each of the plane's three answers arrives on the node as the error it
// branches on.
//
// The route is forNewWork, so a superseded process is refused by the guard
// before the plane sees the request; the plane refuses again underneath, which
// the plane's own tests cover. What this pins is the WIRE: the code each answer
// travels as, because the node's retry decision is made from it.
func TestANodeWithdrawsOverTheWire(t *testing.T) {
	t.Parallel()

	t.Run("the current process withdraws", func(t *testing.T) {
		t.Parallel()

		reg := &fakeRegistrar{}
		p, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg))
		c := dial(t, base)

		if err := c.Withdraw(t.Context()); err != nil {
			t.Fatalf("Withdraw: %v", err)
		}

		if got := p.Nodes(); len(got) != 0 {
			t.Errorf("a node that withdrew over the wire is still in the fleet: %v", got)
		}

		told := reg.withdrawals()
		if len(told) != 1 {
			t.Fatalf("the ledger was told %d withdrawal(s), want one: %+v", len(told), told)
		}

		// THE FENCE THE REGISTRATION RETURNED, and the incarnation the request
		// carried — not the node name alone.
		if told[0].name != "n1" || told[0].epoch != 1 || told[0].incarnation != c.Incarnation() {
			t.Errorf("the ledger was told %+v, want n1 at epoch 1 from %s", told[0], c.Incarnation())
		}
	})

	t.Run("a superseded process is refused", func(t *testing.T) {
		t.Parallel()

		reg := &fakeRegistrar{}
		p, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg))
		first := dial(t, base)
		second := dial(t, base)

		if err := first.Withdraw(t.Context()); !errors.Is(err, nodeclient.ErrSuperseded) {
			t.Fatalf("a superseded process's withdrawal = %v, want ErrSuperseded", err)
		}

		if got := p.CurrentIncarnationForTest("n1"); got != second.Incarnation() {
			t.Errorf("the name resolves to %q, want the replacement %q", got, second.Incarnation())
		}

		if told := reg.withdrawals(); len(told) != 0 {
			t.Errorf("a superseded process's withdrawal reached the ledger: %+v", told)
		}
	})

	t.Run("a node the plane does not know is told to register", func(t *testing.T) {
		t.Parallel()

		_, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(&fakeRegistrar{}))

		c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}

		if err := c.Withdraw(t.Context()); !errors.Is(err, nodeclient.ErrUnregistered) {
			t.Fatalf("an unregistered node's withdrawal = %v, want ErrUnregistered", err)
		}
	})

	t.Run("a ledger that cannot record it answers unavailable", func(t *testing.T) {
		t.Parallel()

		reg := &fakeRegistrar{withdrawErr: errors.New("ledger unavailable")}
		p, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg))
		c := dial(t, base)

		err := c.Withdraw(t.Context())
		if err == nil {
			t.Fatal("a withdrawal the ledger could not record was reported as done")
		}

		// NEITHER VERDICT: the node retries this one, and both verdicts make it stop.
		if errors.Is(err, nodeclient.ErrSuperseded) || errors.Is(err, nodeclient.ErrUnregistered) {
			t.Fatalf("a ledger outage arrived as a verdict: %v", err)
		}

		if !strings.Contains(err.Error(), "503") {
			t.Errorf("a ledger outage did not arrive as 503, which is what tells the node to "+
				"try again: %v", err)
		}

		if got := p.Nodes(); len(got) != 1 {
			t.Errorf("the node was dropped from the fleet although the ledger never heard: %v", got)
		}
	})
}
