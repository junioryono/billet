package e2e

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
)

// harnessHost is the host the harness gives a control plane: no identity
// exclusion to take (each stack has its own directory), no service manager,
// and nowhere for the operator's lines to go.
func harnessHost() app.Host {
	none := func(context.Context, string) (func() error, error) { return func() error { return nil }, nil }

	return app.Host{
		ServerAccess:  none,
		AuthorityLock: none,
		Ready:         func() error { return nil },
		Status:        func(string) error { return nil },
		Out:           io.Discard,
	}
}

// assembled is a control plane assembled through internal/app the way
// runServer assembles one, as far as the wire: opened, claimed, the authority
// adopted, the fleet forgotten and the node wire served on loopback. schedule
// finishes it, once the harness's nodes are registered.
type assembled struct {
	ctl  *app.Controller
	wire *app.ServingWire
	// close stops the wire, joins the controller's loops and closes the
	// ledger, which the next incarnation over the same directory needs.
	// Idempotent; the cleanup calls it too.
	close func()
}

// harnessConfig is the config a stack's control plane is assembled from: its
// state directory, a loopback wire, the deployment's ceiling and the catalogue.
//
// AUTOMATIC UPDATES OFF. An absent release block turns them on, and the starter
// Schedule assembles would then ask the public release channel from inside a
// test, and could start a rollout in the test's ledger from what it answered.
func harnessConfig(dir string, maxVCPU int, maxMemory config.ByteSize, tiers []config.Tier) *config.Config {
	automatic := false

	return &config.Config{
		Server: &config.ServerConfig{
			IdentityDir: dir,
			Listen:      "127.0.0.1:0",
			MaxVCPU:     maxVCPU,
			MaxMemory:   maxMemory,
		},
		Release: &config.ReleaseConfig{Automatic: &automatic},
		Tiers:   tiers,
	}
}

// harnessOwner is what the harness's control plane names itself to the fake
// Actions service's message queue, rather than this machine's host name.
const harnessOwner = "billet-test"

// openAssembled assembles a control plane up to its served wire.
func openAssembled(
	t *testing.T, cfg *config.Config, targets []app.Target, steering *app.Steering,
) *assembled {
	t.Helper()

	cp, err := app.OpenControlPlane(t.Context(), cfg, harnessHost(), targets,
		app.ControlPlaneOptions{Steering: steering})
	if err != nil {
		t.Fatalf("app.OpenControlPlane: %v", err)
	}

	// THE CONTROLLER'S OWN LOOPS END AT CLOSE, not with the test's context:
	// t.Context() is cancelled before the cleanups run, and a restarted stack
	// closes this one itself before it opens the same directory.
	loops, stopLoops := context.WithCancel(context.WithoutCancel(t.Context()))

	ctl, err := cp.BecomeController(loops, func() {})
	if err != nil {
		stopLoops()
		_ = cp.Close()

		t.Fatalf("BecomeController: %v", err)
	}

	c := &assembled{ctl: ctl}

	var closed sync.Once

	c.close = func() {
		closed.Do(func() {
			if c.wire != nil {
				c.wire.Stop()
			}

			stopLoops()

			if errs := ctl.Close(); len(errs) > 0 {
				t.Errorf("the controller's loops: %v", errs)
			}

			if err := cp.Close(); err != nil {
				t.Errorf("close the ledger: %v", err)
			}
		})
	}

	t.Cleanup(c.close)

	adopted, err := ctl.AdoptAuthority(t.Context())
	if err != nil {
		t.Fatalf("AdoptAuthority: %v", err)
	}

	forgotten, err := ctl.ForgetFleet(t.Context())
	if err != nil {
		t.Fatalf("ForgetFleet: %v", err)
	}

	c.wire, err = ctl.ServeWire(t.Context(), forgotten, adopted)
	if err != nil {
		t.Fatalf("ServeWire: %v", err)
	}

	return c
}

// schedule publishes the authority and assembles the scheduler, as runServer
// does once the wire is up.
func (c *assembled) schedule(t *testing.T, opts app.ScheduleOptions) *app.Scheduler {
	t.Helper()

	published, err := c.ctl.PublishAuthority(t.Context(), c.wire)
	if err != nil {
		t.Fatalf("PublishAuthority: %v", err)
	}

	scheduler, err := c.ctl.Schedule(c.wire, published, opts)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	return scheduler
}

// awaitRegistered waits until n nodes are registered over the wire.
//
// REGISTERED BEFORE THE CONTROL PLANE HAS ANYTHING TO GIVE THEM. Otherwise the
// first launch legitimately finds no node and the test measures startup order
// rather than the wire.
func (c *assembled) awaitRegistered(t *testing.T, n int) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for len(c.wire.Plane().Nodes()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d nodes registered over the wire", len(c.wire.Plane().Nodes()), n)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
