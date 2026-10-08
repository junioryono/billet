package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/flightrecorder"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/supervise"
	"github.com/junioryono/billet/internal/version"
)

// Host is what a role needs from the process and the machine it runs on that
// the assembly cannot build from a config. cmd/billet provides it: the
// identity accesses from internal/hostauthority, the service manager's
// notifications, and the command's stdout.
type Host struct {
	// ServerAccess is the exclusion around the identity read, the ledger open
	// and the node wire's authority read.
	ServerAccess IdentityAccess
	// AuthorityLock is the authority lock adopting and publishing the shared
	// authority takes.
	AuthorityLock IdentityAccess
	// Ready tells the service manager this process is serving (READY=1).
	Ready func() error
	// Status replaces the service manager's one-line status (STATUS=).
	Status func(string) error
	// Out is where the lines an operator reads go.
	Out io.Writer
}

// ControlPlaneOptions are how the control plane was invoked, as against what
// its config says.
type ControlPlaneOptions struct {
	// Probe opens the upgrade probe's maintenance handle: the plane is built,
	// which validates the config and the ledger, and nothing more is done.
	Probe bool
	// Steering is what billet's own harnesses change; nil in a deployment.
	Steering *Steering
}

// Steering is what the end-to-end suite and the replay harness change about a
// control plane they assemble the way the CLI does, and nothing a config can
// say: a clock, a lease TTL and pacing short enough for a test, a provisioner
// that orders sessions. Every
// field is optional, and each is applied after what the assembly sets, so it
// can only steer what is already there.
type Steering struct {
	// Allocator are options for the allocator, after its placement.
	Allocator []alloc.Option
	// Plane are options for the node plane, after its registrar, sites,
	// catalogue and barrier store.
	Plane []nodeplane.Option
	// Server are options for the scheduler, after everything Schedule sets.
	Server []server.ControlPlaneOption
	// Provisioner wraps each target's provisioner.
	Provisioner func(server.Provisioner) server.Provisioner
	// Owner names this process to GitHub's message queue instead of the host
	// name.
	Owner string
}

// ControlPlane is a control plane opened and not yet this deployment's
// controller: its identity read, its ledger open in the mode its config and
// invocation call for, its allocator built (which validates the config). It
// has done nothing authoritative, and the only way on is BecomeController.
type ControlPlane struct {
	cfg        *config.Config
	host       Host
	db         *state.DB
	allocator  *alloc.Allocator
	deployment string
	standby    bool
	targets    []server.Target
	planeJIT   map[string]nodeplane.JITSource
	serverOpts []server.ControlPlaneOption
	steering   Steering

	// self is the address OpenControlPlane made this plane at, for the reason a
	// Controller records its own: the proofs were earned against what is there,
	// and a plane overwritten with another's value is not it.
	self *ControlPlane
}

// OpenControlPlane opens a control plane over the scale-set targets the caller
// built (one client per target, so the server and teardown authenticate the
// same way).
func OpenControlPlane(
	ctx context.Context, cfg *config.Config, host Host, targets []Target, opts ControlPlaneOptions,
) (*ControlPlane, error) {
	serverTargets, planeJIT, err := BuildTargets(targets)
	if err != nil {
		return nil, err
	}

	var steering Steering
	if opts.Steering != nil {
		steering = *opts.Steering
	}

	if steering.Provisioner != nil {
		for i := range serverTargets {
			serverTargets[i].Provisioner = steering.Provisioner(serverTargets[i].Provisioner)
		}
	}

	// READ, NOT CLAIMED. The host-wide lock exists to stop two processes managing
	// one deployment's containers, and a control plane manages none — the node
	// takes that lock. A server that took it too would be holding the identity a
	// co-resident node needs, which is the single-machine deployment refusing to
	// start.
	//
	// The identity itself is still required: the node wire refuses a node whose
	// deployment differs from this plane's, so a server that never learned its
	// own would compare every node against "" and refuse the entire fleet — the
	// feature failing closed for a reason nobody could see.
	//
	// FOUNDED HERE IN THE ORDINARY CASE, before the database is opened. Whichever
	// role starts first mints it; the other reads that same file.
	// THE EXCLUSION AROUND THE IDENTITY READ AND THE OPEN, released before the
	// claim's wait (a standby can wait for days, and a backup must not wait with
	// it) and taken again after promotion around the authority load.
	release, err := host.ServerAccess(ctx, cfg.Server.IdentityDir)
	if err != nil {
		return nil, err
	}

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return nil, errors.Join(err, release())
	}

	// A STANDBY OPENS A HANDLE THAT CANNOT WRITE, which is what makes "does
	// nothing authoritative before promotion" a property of the store rather than
	// a rule this function has to keep. See state.OpenPostgresStandby.
	standby := !opts.Probe && cfg.Server.Controllers == config.ControllersActivePassive

	mode := LedgerControlPlane

	switch {
	case opts.Probe:
		mode = LedgerMaintenance
	case standby:
		mode = LedgerStandby
	}

	db, err := OpenLedger(ctx, cfg, mode)

	// THE ACCESS ENDS WITH THE OPEN, whatever the open said: what follows waits
	// on the claim, and nothing waits on a lock while it does.
	if err := errors.Join(err, release()); err != nil {
		return nil, errors.Join(fmt.Errorf("server state: %w", err), closeIfOpen(db))
	}

	// BUILT BEFORE THE CLAIM, DELIBERATELY, because it validates the CONFIG and
	// reaches the ledger for nothing. A tier the catalogue refuses, a missing
	// ceiling or a host policy that contradicts itself should stop this process at
	// startup rather than at a failover, which is the one moment nobody wants to
	// discover a config error. The scheduler's options are read from the config
	// here for the same reason.
	allocator, err := alloc.New(db, allocatorLimits(cfg), cfg.Tiers,
		append([]alloc.Option{alloc.WithPlacement(cfg.Server.Placement)}, steering.Allocator...)...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("capacity allocator: %w", err), db.Close())
	}

	// Everything billet.yaml says about the control plane, assembled in one place
	// inside the server package so the config-to-listener chain is testable
	// without spanning two packages. What Run adds is about how the process was
	// INVOKED, not about the file.
	serverOpts, err := server.OptionsFromConfig(cfg)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}

	cp := &ControlPlane{
		cfg:        cfg,
		host:       host,
		db:         db,
		allocator:  allocator,
		deployment: deployment,
		standby:    standby,
		targets:    serverTargets,
		planeJIT:   planeJIT,
		serverOpts: serverOpts,
		steering:   steering,
	}
	cp.self = cp

	return cp, nil
}

// Close closes the ledger. Close the Controller, if one was made, first.
func (cp *ControlPlane) Close() error { return cp.db.Close() }

// LeadershipLost reports whether this control plane's ledger has refused a
// write because another controller took the deployment.
func (cp *ControlPlane) LeadershipLost() bool { return cp.db.LeadershipLost() }

// BecomeController takes this deployment's controller claim, waiting for it on
// a standby, and starts the fence that ends the process through stop the
// moment a successor takes the claim away.
//
// EVERYTHING AUTHORITATIVE IS ON THE CONTROLLER IT RETURNS, and that is the
// whole of the standby design: a standby is this same control plane, stopped
// here until it can go on, and nothing it may not do before promotion is
// reachable without the value this returns.
func (cp *ControlPlane) BecomeController(ctx context.Context, stop func()) (*Controller, error) {
	claim, err := becomeController(ctx, cp.cfg, cp.host, cp.db, cp.deployment, cp.standby)
	if err != nil {
		return nil, err
	}

	ctl := &Controller{cp: cp, claim: claim, claimLost: cp.db.LeadershipLost}
	ctl.self = ctl

	// ONLY ONCE THE CLAIM IS HELD: a standby has no heartbeats to overrun and no
	// claim to lose, and may wait for days.
	if cp.cfg.Server.FlightRecorder {
		dir := filepath.Join(cp.cfg.Server.IdentityDir, flightrecorder.DirName)

		ctl.recorder, err = flightrecorder.Start(dir, slog.Default())
		if err != nil {
			return nil, err
		}
	}

	// THE LOOPS BESIDE THE PLANE ARE JOINED BEFORE THE LEDGER THEY WRITE CLOSES:
	// Close the Controller before the ControlPlane.
	ctl.loops = supervise.New(ctx, slog.Default())

	// AND A LOST CLAIM STOPS THE PROCESS, WHICH IS THE HALF THAT REFUSING A WRITE
	// DOES NOT DO. See stopWhenReplaced.
	ctl.loops.Background("controller fence", func(ctx context.Context) {
		stopWhenReplaced(ctx, cp.db.LeadershipLostSignal(), stop, ctl.recorder, slog.Default())
	})

	return ctl, nil
}

// Controller is a control plane holding this deployment's controller claim.
// BecomeController is the only way to have one, and every step that only the
// controller may take is one of its methods.
type Controller struct {
	cp    *ControlPlane
	claim state.ControllerClaim
	loops *supervise.Group
	// recorder is the flight recorder, or nil when server.flight_recorder is
	// off; a nil one records nothing.
	recorder *flightrecorder.Recorder
	// claimLost is the ledger's LeadershipLost, which Close asks once the loops
	// are joined.
	claimLost func() bool

	// self is the address BecomeController made this Controller at. The proofs
	// name their controller by address, so a copy, or a Controller overwritten
	// with another's value, must hold no claim, or proofs one control plane
	// earned would serve another.
	self *Controller
}

// Close joins every loop the controller started, then the flight recorder's
// snapshots, so one taken as the process stopped is on disk before it exits.
//
// A LOST CLAIM IS RECORDED HERE TOO. The fence's watcher can see the plane's
// context end before it sees the claim's signal, when a refused write stopped
// the plane first, and then records nothing; asked again here, once every loop
// has returned, the latched fact is not missed. The recorder's rate limit keeps
// it to one snapshot when the watcher did record it.
func (c *Controller) Close() []error {
	errs := c.loops.Wait()

	if c.claimLost != nil && c.claimLost() {
		c.recorder.Snapshot(flightrecorder.LeadershipLost)
	}

	c.recorder.Stop()

	return errs
}

// heartbeatOverrun is what a listener tells when a heartbeat pass overruns.
func (c *Controller) heartbeatOverrun() { c.recorder.Snapshot(flightrecorder.HeartbeatOverrun) }

// Allocator is the capacity allocator this controller schedules with.
func (c *Controller) Allocator() *alloc.Allocator { return c.cp.allocator }

// Ledger is the ledger this controller holds the claim on.
func (c *Controller) Ledger() *state.DB { return c.cp.db }

// Deployment is the deployment identity this controller serves.
func (c *Controller) Deployment() string { return c.cp.deployment }

// errNotController refuses a step on a Controller that holds no claim: one not
// made by BecomeController.
var errNotController = errors.New("app: this step is the controller's, and this value holds no controller claim")

// held reports whether c was made by BecomeController and it and its control
// plane are still where they were made: it has the claim's epoch, which the
// ledger never writes as zero.
func (c *Controller) held() bool {
	return c != nil && c.self == c && c.cp != nil && c.cp.self == c.cp && c.claim.Epoch >= 1
}

// THE PROOFS DIFFER IN THEIR FIELDS' NAMES, not only their types' names: two
// struct types with the same fields convert into one another, so a caller could
// turn one step's proof into another's (TestTheProofsDoNotConvert).

// FleetForgotten is ForgetFleet's proof that this controller cleared the
// fleet's liveness. Only ForgetFleet makes one, and ServeWire refuses one any
// other controller made, or the zero value.
type FleetForgotten struct{ forgottenBy *Controller }

// AdoptedAuthority is AdoptAuthority's proof that this host holds the
// deployment's node-wire authority, not one of its own. Only AdoptAuthority
// makes one, and ServeWire refuses one any other controller made.
type AdoptedAuthority struct{ adoptedBy *Controller }

// AuthorityPublished is PublishAuthority's proof that the authority this
// controller serves with was offered to the identity store, whether or not the
// store took it. Schedule requires it, so a controller cannot schedule without
// having tried, and a successor is never left with nothing to adopt because a
// call went missing.
type AuthorityPublished struct{ publishedBy *Controller }

// ForgetFleet clears every node's liveness before anything registers.
//
// NOTHING IS LIVE UNTIL IT SAYS SO AGAIN. Liveness is the plane's judgement and
// this plane has just started, so it has none: its map is empty. Rows left by
// the previous process would otherwise back advertisements for machines this
// one has never heard from. Every node re-registers over the wire within a
// poll, so the cost is a brief zero that is also the truth.
func (c *Controller) ForgetFleet(ctx context.Context) (FleetForgotten, error) {
	if !c.held() {
		return FleetForgotten{}, errNotController
	}

	if err := c.cp.allocator.ForgetEveryNode(ctx); err != nil {
		return FleetForgotten{}, fmt.Errorf("server: could not clear the fleet's liveness: %w", err)
	}

	return FleetForgotten{forgottenBy: c}, nil
}

// AdoptAuthority gives this host the deployment's node-wire authority before
// anything reads one.
//
// The node wire's single authority read goes through LoadOrCreateCA, which
// CREATES one when the directory is empty. On a promoted standby that has never
// held this deployment's CA that would mint a RIVAL authority, after which every
// node in the fleet fails to verify the control plane and drops off at once —
// while the control plane itself looks perfectly healthy. This is what makes a
// failover a failover rather than an outage. A no-op unless this deployment
// keeps its identity in a store.
func (c *Controller) AdoptAuthority(ctx context.Context) (AdoptedAuthority, error) {
	if !c.held() {
		return AdoptedAuthority{}, errNotController
	}

	cp := c.cp
	if err := AdoptSharedAuthority(ctx, cp.cfg, cp.host.AuthorityLock, cp.deployment, slog.Default()); err != nil {
		return AdoptedAuthority{}, fmt.Errorf("node-wire authority: %w", err)
	}

	return AdoptedAuthority{adoptedBy: c}, nil
}

// ServingWire is the node wire this controller is serving. Stop it before the
// Controller is closed.
type ServingWire struct {
	*ServedWire

	nodes    *nodeplane.Plane
	servedBy *Controller
}

// Plane is the node plane this wire serves: who is registered, and the runner
// that reaches them.
func (w *ServingWire) Plane() *nodeplane.Plane { return w.nodes }

// ServeWire serves the node wire, which needs the fleet forgotten and the
// authority adopted first: a node registering before the first would be
// forgotten by it, and the wire's authority read before the second could mint
// a rival authority. A proof another controller made, or a zero one, is refused.
//
// THE NODE WIRE IS SERVED WHETHER OR NOT ANY NODE EXISTS YET. A control plane
// that only opened its listener once a node was configured would make the
// first node's setup a chicken-and-egg problem, and there is nothing to guard:
// an empty fleet answers every request with "I do not know you".
func (c *Controller) ServeWire(ctx context.Context, fleet FleetForgotten, adopted AdoptedAuthority) (*ServingWire, error) {
	if !c.held() || fleet.forgottenBy != c || adopted.adoptedBy != c {
		return nil, errors.New("app: the node wire is served only after this controller forgot the fleet " +
			"and adopted the authority")
	}

	cp := c.cp

	planeOpts := append([]nodeplane.Option{
		nodeplane.WithRegistrar(cp.allocator),
		// The declared places, so a node claiming one nobody declared is refused
		// here rather than recorded. A node's own config cannot make this check —
		// sites are the control plane's to declare and the node's file has no
		// reason to list them.
		nodeplane.WithSites(cp.cfg.Sites),
		// The catalogue lives here, and a launch carries the shape a node needs, so
		// no node keeps a copy that can drift from this one.
		nodeplane.WithTierCatalog(cp.cfg.Tiers),
		// The durable half of a compute barrier. `billet drain` and
		// `billet local down` write a request into the ledger; this is what
		// observes it, because a sealed idle deployment dispatches nothing at all
		// and there is no other moment at which the fleet would be asked.
		nodeplane.WithBarrierStore(cp.allocator),
	}, cp.steering.Plane...)

	nodes := nodeplane.New(slog.Default(), cp.deployment, cp.allocator.LeaseTTL(), planeOpts...)

	wire, err := ServeNodeWire(ctx, cp.cfg, cp.host.ServerAccess, nodes, cp.allocator,
		cp.planeJIT, cp.allocator, cp.allocator, cp.db)
	if err != nil {
		return nil, err
	}

	return &ServingWire{ServedWire: wire, nodes: nodes, servedBy: c}, nil
}

// PublishAuthority puts the authority this host serves with into the
// deployment's identity store.
//
// AFTER THE WIRE RATHER THAN BEFORE IT, because on a first controller the wire
// is what creates the authority: publishing earlier would publish nothing. On
// every later start the bytes are identical and this is a write nobody sees.
// NOT FATAL, and reported rather than swallowed: the control plane is serving
// by now, and a store that cannot be written is a reason to look at IAM rather
// than to take a working deployment offline.
// It returns the proof Schedule needs whether or not the store took the
// authority; it refuses only a wire this controller is not serving.
func (c *Controller) PublishAuthority(ctx context.Context, wire *ServingWire) (AuthorityPublished, error) {
	if !c.held() || wire == nil || wire.servedBy != c {
		return AuthorityPublished{}, errors.New("app: the authority is published only by the controller " +
			"serving the wire it was read for")
	}

	cp := c.cp
	PublishSharedAuthority(ctx, cp.cfg, cp.host.AuthorityLock, cp.deployment, slog.Default())

	return AuthorityPublished{publishedBy: c}, nil
}

// ScheduleOptions are how the controller was invoked.
type ScheduleOptions struct {
	// Hurry is closed by the second signal, reaching the drain that honours it.
	Hurry <-chan struct{}
	// DryRun advertises nothing: scale sets are created and polled and no job
	// is accepted.
	DryRun bool
	// NodeRunner replaces the node plane's runner, for billet's own harness
	// that runs the node runtime in process against the allocator; the wire is
	// still served. Nil in a deployment.
	NodeRunner dispatch.Runner
}

// Schedule assembles everything that schedules: the liveness and barrier loops,
// the node plane's runner, the rollout coordinator and starter, the
// staged-credential sweep and the scale-set listeners. It tells the service
// manager this process is serving, and returns the Scheduler whose Run polls
// GitHub. An error here is the assembly's or the service manager's, never
// GitHub's.
func (c *Controller) Schedule(
	wire *ServingWire, published AuthorityPublished, opts ScheduleOptions,
) (*Scheduler, error) {
	if !c.held() || wire == nil || wire.servedBy != c || published.publishedBy != c {
		return nil, errors.New("app: scheduling needs the node wire this controller serves and its " +
			"authority published")
	}

	cp := c.cp

	// The second signal, reaching the drain that honours it.
	//
	// AND THE FENCE, REACHING THE TEARDOWN THAT HONOURS IT. `db.LeadershipLost`
	// latches inside the write transaction the ledger refused, so by the time any
	// listener is unwinding it is already true — which is what lets every one of
	// them stop without destroying compute, closing a session or handing capacity
	// back to a deployment that is no longer theirs.
	//
	// AND A STOP WHILE ADMISSION IS OPEN IS A RESTART, handed over rather than
	// drained (#365): `billet drain` and `local down` seal first and still drain.
	serverOpts := append([]server.ControlPlaneOption{}, cp.serverOpts...)
	serverOpts = append(serverOpts, server.WithHurry(opts.Hurry), server.WithLeadershipLost(cp.db.LeadershipLost),
		server.WithCompletionLedger(cp.db), server.WithTargets(cp.targets...),
		server.WithStopHandoff())

	// WHETHER OR NOT THERE IS A RECORDER, which a nil one makes a no-op: one
	// timer a pass is nothing beside the pass, and wiring that depends on a
	// branch is wiring a reversed branch removes.
	serverOpts = append(serverOpts, server.WithHeartbeatOverrun(c.heartbeatOverrun))

	if opts.DryRun {
		serverOpts = append(serverOpts, server.AdvertiseNothing())

		fmt.Fprintf(cp.host.Out, "billet server (DRY RUN): %d tiers, advertising ZERO capacity.\n", len(cp.cfg.Tiers))
		fmt.Fprintf(cp.host.Out, "Scale sets are created and polled; no job will be accepted.\n")
	} else {
		fmt.Fprintf(cp.host.Out, "billet server: %d tiers, ceiling %d vCPU / %s\n",
			len(cp.cfg.Tiers), cp.cfg.Server.MaxVCPU, cp.cfg.Server.MaxMemory)
	}

	// The owner identifies this process to GitHub's message queue so a session
	// left by a crashed run is distinguishable from a live one.
	owner, err := os.Hostname()
	if err != nil || owner == "" {
		owner = "billet"
	}

	if cp.steering.Owner != "" {
		owner = cp.steering.Owner
	}

	// A TIMER, BECAUSE NOTHING ELSE ASKS. A node's liveness now decides what its
	// tier advertises, and an idle deployment never launches, lists or destroys —
	// so without this a host that crashed on a quiet afternoon would keep its
	// capacity advertised until somebody happened to need it.
	c.loops.Background("node liveness", func(ctx context.Context) { wire.nodes.Watch(ctx) })

	// AND A SECOND ONE, for the same reason. A drain asks the fleet what it is
	// running through a durable request row, because the command that wants the
	// answer runs in another process; nothing on a sealed, idle deployment would
	// otherwise ever put that question to a node.
	c.loops.Background("compute barrier", func(ctx context.Context) { wire.nodes.BarrierLoop(ctx) })

	// THE REMOTE PLANE DRIVES ALL COMPUTE, and it is the only thing that can. A
	// control plane without it serves the node wire, accepts registrations, and then
	// never sends a single command.
	planeRunner := wire.nodes.NewRunner()

	// AND THE ROLLOUT COORDINATOR, which is what makes `billet rollout start` mean
	// anything: without it the decision is a durable record nobody acts on, so
	// every rollout stays open forever and blocks the next one. GIVEN THE NODE
	// PLANE'S RUNNER, because that is the only thing that can reach a host.
	coordinator := rollout.NewCoordinator(
		rollout.New(cp.db),
		LedgerFleet{Alloc: cp.allocator},
		PlaneDispatcher{Runner: planeRunner},
		version.Version(),
		nodeapi.VersionNodeUpgrade,
		rollout.WithCoordinatorLogger(slog.Default()),
	)

	// AND THE STARTER, which is what makes `release.automatic` true: the
	// coordinator converges a rollout that exists, and this is what makes one
	// exist when the channel advances. It resolves the channel through the same
	// functions `billet rollout start` does, so the two cannot disagree about
	// what a target is.
	starter, err := NewRolloutStarter(cp.cfg, rollout.New(cp.db), LedgerFleet{Alloc: cp.allocator},
		releasesource.Host(version.Version(),
			releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
			state.LatestSchemaVersion(), firecracker.GuestContract))
	if err != nil {
		return nil, err
	}

	serverOpts = append(serverOpts,
		server.WithNodeRunner(nodeRunner(planeRunner, opts.NodeRunner)),
		server.WithRolloutCoordinator(coordinator, 0),
		server.WithRolloutStarter(starter, 0),
		// AND THE SWEEP OF STAGED CODEBUILD REGISTRATIONS a dead node never reaped.
		// It needs what only this process has: the ledger, which is the sole
		// authority for deleting one, and the host's AWS credentials — the same
		// chain the backup upload uses. Which paths it sweeps comes from the
		// fleet's registrations, so a deployment with no codebuild node sweeps
		// nothing and resolves no credential.
		server.WithStagedCredentialSweeper(
			NewControllerCredentialSweep(cp.allocator, cp.db, awscreds.Default(), slog.Default())),
	)

	serverOpts = append(serverOpts, cp.steering.Server...)

	plane := server.New(cp.allocator, nil, cp.cfg.Tiers, owner, slog.Default(), serverOpts...)

	// READINESS IS REPORTED BEFORE THE LISTENERS OPEN THEIR SESSIONS, AND MOVING IT
	// AFTER THEM WOULD BE A RESTART LOOP.
	//
	// A tier's session can now be held by a control plane that was killed rather
	// than stopped, and GitHub does not hand one over: server.openSession waits for
	// GitHub to expire it, which takes as long as it takes. The unit is
	// Type=notify with TimeoutStartSec=120 and Restart=on-failure, so withholding
	// READY=1 until every session is open means systemd kills billet at two
	// minutes, restarts it, and it waits again — forever, because nothing about
	// restarting makes a remote session expire sooner.
	//
	// SO READINESS MEANS "THIS PROCESS IS SERVING", WHICH IS TRUE. The node wire is
	// listening, the reaper is running, and every tier whose session opened is
	// polling. A tier still waiting says so in the journal every thirty seconds.
	if err := cp.host.Ready(); err != nil {
		return nil, fmt.Errorf("server readiness: %w", err)
	}

	return &Scheduler{plane: plane, db: cp.db}, nil
}

// Scheduler is a controller's scheduling, assembled and announced, not yet
// polling.
type Scheduler struct {
	plane *server.Server
	db    *state.DB
}

// Run polls GitHub and schedules until ctx ends. A controller fenced out of its
// deployment returns state.ErrLeadershipLost, whatever else happened; any other
// error is the scheduler's own.
func (s *Scheduler) Run(ctx context.Context) error {
	// A LOST LEADERSHIP EXITS NON-ZERO, AND THE RESTART THAT FOLLOWS IS THE POINT
	// RATHER THAN A LOOP TO BE AVOIDED. The packaged unit is Restart=on-failure,
	// so systemd starts this process again and ClaimController either takes the
	// deployment back — exactly right when the successor was itself transient — or
	// is refused with ErrControllerHeld naming the holder and its epoch. A clean
	// exit would leave a deployment whose partition has healed with no controller
	// at all, silently, which is the failure this whole fence exists to make
	// impossible.
	err := s.plane.Run(ctx)

	// ASKED BEFORE THE ERROR IS CLASSIFIED, and asked at all because the plane
	// stops through a CANCELLED CONTEXT here, which Run reports as a clean stop.
	// Returning nil would exit 0 — a control plane that was fenced out of its own
	// deployment reporting a successful shutdown, and systemd leaving it stopped.
	if s.db.LeadershipLost() {
		return fmt.Errorf("%w. Nothing running here was destroyed and no capacity was "+
			"handed back; the controller that replaced this one adopts both. If that "+
			"replacement was itself transient, restarting is how this host takes the "+
			"deployment back", state.ErrLeadershipLost)
	}

	return err
}

// closeIfOpen closes a handle an open may or may not have produced.
func closeIfOpen(db *state.DB) error {
	if db == nil {
		return nil
	}

	return db.Close()
}

// becomeController takes this deployment's controller claim, waiting for it if
// this host is one of an active/passive pair.
//
// ONE FUNCTION FOR BOTH LAYOUTS, because the difference between them is a single
// question — is a held claim a MISTAKE or the thing this process is here for —
// and everything downstream is identical. A standby is not a second kind of
// control plane; it is the same one, stopped at this line until it can go on.
//
// READY=1 IS SENT BEFORE THE WAIT, AND IT HAS TO BE. The packaged unit is
// Type=notify with TimeoutStartSec=120, so a standby that withheld readiness
// until promotion would be killed at two minutes and restarted forever. A
// waiting standby IS doing its job, and the STATUS line is what says which job
// that is. IT IS SENT AGAIN AFTER PROMOTION by Run's ordinary readiness call,
// which is not redundant: sd_notify READY=1 is idempotent, and the second one
// carries the point at which the listeners are actually up.
func becomeController(
	ctx context.Context,
	cfg *config.Config,
	host Host,
	db *state.DB,
	deployment string,
	standby bool,
) (state.ControllerClaim, error) {
	log := slog.Default()

	if !standby {
		claim, err := db.ClaimController(ctx, controllerName(cfg), deployment)
		if err != nil {
			return state.ControllerClaim{}, fmt.Errorf("controller claim: %w", err)
		}

		log.Info("claimed this deployment's controller",
			"holder", claim.Holder, "epoch", claim.Epoch)

		return claim, nil
	}

	if err := host.Ready(); err != nil {
		return state.ControllerClaim{}, fmt.Errorf("server standby readiness: %w", err)
	}

	fmt.Fprintln(host.Out, "billet server: standing by; this host takes over when the controller's "+
		"database session ends")

	// RATE-LIMITED IN THE LOG AND NOT IN THE STATUS. A standby may wait for days,
	// so a line per poll is a journal nobody can read — but `systemctl status`
	// shows only the latest STATUS, so refreshing that costs nothing and is the
	// one place an operator looks.
	var (
		lastLogged time.Time
		waits      int
	)

	claim, err := db.AwaitController(ctx, controllerName(cfg), deployment,
		func(held state.ControllerClaim) {
			waits++

			describe := "the ledger records no holder"
			if held.Holder != "" {
				describe = fmt.Sprintf("%s holds it at epoch %d", held.Holder, held.Epoch)
			}

			//nolint:errcheck // a status line is a diagnostic; failing to send one is not a reason to stop.
			_ = host.Status("standby: waiting for the controller claim (" + describe + ")")

			if waits == 1 || time.Since(lastLogged) > standbyLogInterval {
				lastLogged = time.Now()

				log.Info("standing by for this deployment's controller",
					"holder", held.Holder, "epoch", held.Epoch)
			}
		})
	if err != nil {
		return state.ControllerClaim{}, fmt.Errorf("controller claim: %w", err)
	}

	//nolint:errcheck // as above.
	_ = host.Status("controller")

	log.Info("promoted to this deployment's controller",
		"holder", claim.Holder, "epoch", claim.Epoch)

	return claim, nil
}

// controllerName is what this process calls itself in the controller claim.
//
// A DIAGNOSTIC, NOT AN IDENTITY. Nothing compares it and nothing decides from
// it; what excludes a second controller is a lock. It exists so a refusal can
// tell an operator with two machines which one to go and stop, which "already
// claimed" cannot.
func controllerName(cfg *config.Config) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		// The identity directory is the fallback because it is stable and
		// already in the operator's config. A blank holder would make the
		// refusal say nothing at all.
		return cfg.Server.IdentityDir
	}

	return fmt.Sprintf("%s (pid %d)", host, os.Getpid())
}

// standbyLogInterval paces what a waiting standby writes to the journal.
//
// A STANDBY MAY WAIT FOR DAYS, which is the ordinary state of a healthy pair, so
// the log has to be quiet enough to read afterwards and frequent enough that
// "this host is standing by" is visible without asking. The systemd STATUS line
// beside it is refreshed on every poll, because that one is a replacement rather
// than an append.
const standbyLogInterval = 5 * time.Minute

// stopWhenReplaced ends the control plane the moment its ledger refuses a write
// because a successor has claimed the deployment.
//
// REFUSING THE WRITE IS NOT STOPPING THE PROCESS, and that gap is the whole
// reason this exists. Every background writer in the control plane is
// deliberately patient with an error it cannot classify — a heartbeat keeps its
// lease rather than dropping it, the reaper logs and tries again, a cleanup
// retry backs off — because the alternative is a database blip failing builds.
// All of that is right for a blip and wrong for a lost claim: it leaves a
// replaced controller polling GitHub, holding its message session, and running
// the cleanup loop that calls Runner.Destroy, which never touches the ledger and
// is therefore fenced by nothing.
//
// A SIGNAL RATHER THAN A CHECK ON SOME PATH, because every path that could do
// the checking is one this process may sit inside for a whole long poll.
//
// IT RETURNS ON THE CONTEXT TOO, so an ordinary shutdown does not leave it
// blocked on a channel that will never close.
//
// THE FLIGHT RECORDER IS ASKED BEFORE THE STOP, so the window it writes ends
// where the claim was lost rather than in the teardown after it.
func stopWhenReplaced(
	ctx context.Context, replaced <-chan struct{}, stop func(), recorder *flightrecorder.Recorder,
	log *slog.Logger,
) {
	select {
	case <-ctx.Done():
	case <-replaced:
		log.Error("this process is no longer this deployment's controller; stopping. " +
			"Nothing running here is destroyed and no capacity is handed back — the " +
			"controller that replaced this one adopts both")
		recorder.Snapshot(flightrecorder.LeadershipLost)
		stop()
	}
}

// nodeRunner is the runner the scheduler dispatches through: the node plane's,
// unless a harness steers its own in.
func nodeRunner(plane, steered dispatch.Runner) dispatch.Runner {
	if steered != nil {
		return steered
	}

	return plane
}
