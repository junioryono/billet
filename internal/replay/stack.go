package replay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider/simulated"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

// stack is billet, assembled the way cmd/billet assembles it, through
// internal/app: a control plane that claims its ledger, serves the node wire on
// loopback and schedules, and one node runtime per simulated host, all over the
// scripted Actions service.
type stack struct {
	db        *state.DB
	alloc     *alloc.Allocator
	plane     *nodeplane.Plane
	scheduler *app.Scheduler

	closeDB   func()
	stopNodes func()
	hurry     chan struct{}
	hurryOnce sync.Once
}

// Real-time bounds on the harness's own waits. None of them is a simulated
// duration; they are how long a real goroutine may take to do something the
// harness asked for before the replay is declared stuck.
const (
	registrationWait = 30 * time.Second
	shutdownWait     = 60 * time.Second
	// planePoll is the long-poll window nodes are told to wait out. Short, so a
	// stopping node loop returns quickly; it also sets how long a silent node
	// survives (four windows), which no simulated host approaches while the
	// plane stays on wall time.
	planePoll = 2 * time.Second
)

// harnessHost is the host a harness gives the control plane: no identity
// exclusion to take (each replay has its own directory), no service manager,
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

// buildStack stands billet up over the fleet.
//
// WHICH CLOCKS MOVE. The allocator and every simulated provider read the
// harness's clock: leases are dated, expired and archived in simulated time and
// an instance runs for a simulated duration. The node plane, the listener's
// heartbeat and the node loops stay on wall time, because their timings are
// facts about a real machine talking to a real one: a plane on the simulated
// clock would expire a node parked in a poll across a jump of hours. The reaper
// never ticks for the same reason the multi-day end-to-end scenario turns it
// off: a jumped clock manufactures an expiry continuous time cannot produce.
func buildStack(t *testing.T, log *slog.Logger, fleet Fleet, tiers []config.Tier, clock *Clock,
	actions *plane,
) *stack {
	t.Helper()

	client, err := scaleset.New(scaleset.Config{
		Target:         billetgithub.OrganizationTarget(DefaultOwner),
		GitHubURL:      actions.URL,
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     actions.PrivateKeyPEM(),
		AppID:          12345,
		APIURL:         actions.URL + "/api/v3",
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	cfg := &config.Config{
		Server: &config.ServerConfig{
			IdentityDir: t.TempDir(),
			Listen:      "127.0.0.1:0",
			MaxVCPU:     fleet.MaxVCPU,
			MaxMemory:   fleet.MaxMemory,
			Placement:   fleet.Placement,
		},
		Tiers: tiers,
	}

	// THE OPERATOR'S SECOND SIGNAL, wired as `billet node` and `billet server`
	// wire it. A replay ends with nothing running, so a drain ends by itself;
	// closing this at stop is what keeps a replay that failed half way from
	// hanging in cleanup instead of in the assertion that named the failure.
	hurry := make(chan struct{})

	// The one target, assembled the way the CLI assembles one, so the scale-set
	// record carries the owner's path and every tier resolves through it.
	targets := []app.Target{{
		Config: config.GitHubTarget{Name: config.DefaultTargetName, Org: DefaultOwner},
		Client: client,
	}}

	cp, err := app.OpenControlPlane(t.Context(), cfg, harnessHost(), targets, app.ControlPlaneOptions{
		Steering: &app.Steering{
			Allocator: []alloc.Option{alloc.WithClock(clock.Now)},
			Plane:     []nodeplane.Option{nodeplane.WithPollTimeout(planePoll)},
			Server: []server.ControlPlaneOption{
				// NEVER, for a replay: a jumped clock would expire every lease between two
				// heartbeats. The startup reap still runs, on an empty ledger.
				server.WithReapInterval(24 * time.Hour),
				server.WithDrainTimeout(time.Hour),
			},
			// THE CLI'S OWN ADAPTER, KEPT CONCRETE in the wrapper, so every
			// capability the scheduler finds by asserting it still is.
			Provisioner: func(p server.Provisioner) server.Provisioner {
				adapter, ok := p.(app.Provisioner)
				if !ok {
					t.Fatalf("the target's provisioner is a %T, not the CLI's adapter", p)
				}

				return orderedSessions{Provisioner: adapter, actions: actions}
			},
			Owner: owner,
		},
	})
	if err != nil {
		t.Fatalf("app.OpenControlPlane: %v", err)
	}

	// The controller's own loops end with this, at close; the plane's Run ends
	// with the context run hands it.
	loopCtx, cancelLoops := context.WithCancel(context.WithoutCancel(t.Context()))

	ctl, err := cp.BecomeController(loopCtx, cancelLoops)
	if err != nil {
		t.Fatalf("BecomeController: %v", err)
	}

	adopted, err := ctl.AdoptAuthority(t.Context())
	if err != nil {
		t.Fatalf("AdoptAuthority: %v", err)
	}

	forgotten, err := ctl.ForgetFleet(t.Context())
	if err != nil {
		t.Fatalf("ForgetFleet: %v", err)
	}

	wire, err := ctl.ServeWire(t.Context(), forgotten, adopted)
	if err != nil {
		t.Fatalf("ServeWire: %v", err)
	}

	closeDB := sync.OnceFunc(func() {
		wire.Stop()
		cancelLoops()

		if errs := ctl.Close(); len(errs) > 0 {
			t.Errorf("the controller's loops: %v", errs)
		}

		if err := cp.Close(); err != nil {
			t.Errorf("close the ledger: %v", err)
		}
	})
	t.Cleanup(closeDB)

	nodeCtx, cancelNodes := context.WithCancel(t.Context())

	var loops sync.WaitGroup

	for _, h := range fleet.Hosts {
		nc, err := app.NewNodeClient(&config.Config{Node: &config.NodeConfig{
			Name: h.Name, ServerAddr: "http://" + wire.Addr,
		}}, nil)
		if err != nil {
			t.Fatalf("NewNodeClient(%s): %v", h.Name, err)
		}

		prov, err := simulated.New(ctl.Deployment(), simulated.WithClock(clock.Now), simulated.WithLogger(log))
		if err != nil {
			t.Fatalf("simulated.New(%s): %v", h.Name, err)
		}

		runner := app.NewNodeRunner(nc, h.Name, prov, log)

		loops.Go(func() {
			err := app.RunNodeLoop(nodeCtx, nc, runner, nodeclient.LoopOptions{
				Provider:   config.ProviderSimulated,
				Deployment: ctl.Deployment(),
				Site:       h.Site,
				VCPU:       h.VCPU,
				Memory:     h.Memory,
				Log:        log,
				Backoff:    50 * time.Millisecond,
				Hurry:      hurry,
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("node %s stopped for a reason other than shutdown: %v", h.Name, err)
			}
		})
	}

	stopNodes := sync.OnceFunc(func() {
		cancelNodes()
		loops.Wait()
	})
	t.Cleanup(stopNodes)

	// REGISTERED BEFORE THE CONTROL PLANE HAS ANYTHING TO GIVE THEM, or the first
	// escrow finds a partial fleet and the replay measures startup order.
	deadline := time.Now().Add(registrationWait)

	for len(wire.Plane().Nodes()) < len(fleet.Hosts) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d hosts registered over the wire", len(wire.Plane().Nodes()), len(fleet.Hosts))
		}

		time.Sleep(20 * time.Millisecond)
	}

	published, err := ctl.PublishAuthority(t.Context(), wire)
	if err != nil {
		t.Fatalf("PublishAuthority: %v", err)
	}

	scheduler, err := ctl.Schedule(wire, published, app.ScheduleOptions{Hurry: hurry})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	return &stack{
		db: ctl.Ledger(), alloc: ctl.Allocator(), plane: wire.Plane(), scheduler: scheduler,
		closeDB: closeDB, stopNodes: stopNodes, hurry: hurry,
	}
}

// orderedSessions is the CLI's provisioner with one addition: a tier's session
// is asked for only once every tier before it in the fleet's order has its
// listener parked.
//
// THE SECOND THING THE HARNESS STEERS, BESIDE THE CLOCK, and it steers an order
// GitHub itself leaves to chance. Listeners start together and each buys a lease
// as soon as its first job is assigned; escrow is placement, so with two tiers
// of different sizes the host each lease lands on depends on which goroutine ran
// first, and every later job on that tier inherits it. Waiting
// HERE rather than in the scripted service's handler, because the scale-set
// client serialises its calls under one mutex and a request held open in the
// handler would hold every other tier's request behind it.
type orderedSessions struct {
	app.Provisioner

	actions *plane
}

// Session waits for this set's turn, then opens the session the real adapter
// opens.
func (o orderedSessions) Session(ctx context.Context, scaleSetID int, owner string) (server.Session, error) {
	if err := o.actions.awaitTurn(ctx, scaleSetID); err != nil {
		return nil, err
	}

	return o.Provisioner.Session(ctx, scaleSetID, owner)
}

// run starts the control plane and returns its stop.
//
// A Run that ends before it is asked to is reported at once rather than at
// stop, so a control plane that died at startup names itself instead of
// whichever wait timed out first.
func (s *stack) run(t *testing.T) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))

	var runErr error

	finished := make(chan struct{})

	go func() {
		runErr = s.scheduler.Run(ctx)

		close(finished)
	}()

	stopped := make(chan struct{})

	go func() {
		select {
		case <-finished:
			select {
			case <-stopped:
			default:
				t.Errorf("the control plane stopped on its own: %v", runErr)
			}
		case <-stopped:
		}
	}()

	stop := sync.OnceFunc(func() {
		close(stopped)
		cancel()
		s.hurryOnce.Do(func() { close(s.hurry) })

		limit := time.NewTimer(shutdownWait)
		defer limit.Stop()

		select {
		case <-finished:
		case <-limit.C:
			t.Error("the control plane did not stop")
		}
	})

	t.Cleanup(stop)

	return stop
}
