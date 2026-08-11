// Command billet runs a self-hosted GitHub Actions runner platform.
//
// One binary, two roles. `billet server` is the control plane: it long-polls
// GitHub for assigned jobs, owns the capacity ledger, and tells nodes what to
// launch. `billet node` is a compute host: it runs a provider and launches
// instances. `billet server --dev` runs both in one process, which is the
// single-machine deployment.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/docker"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
	"github.com/junioryono/billet/internal/wirecert"
	"github.com/junioryono/billet/internal/wiring"
)

// errNotImplemented marks a role that is scaffolded but cannot serve yet.
//
// It is returned immediately and non-zero rather than blocking. A process that
// idles until signalled looks healthy to systemd, Docker, and every uptime
// check, so a half-built control plane would be reported as running while no
// job is ever picked up. Failing loudly is the honest behaviour for pre-alpha.
var errNotImplemented = errors.New("not implemented yet")

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Explicit -h is a successful request for help, not a usage error.
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "billet: %v\n", err)
		os.Exit(1)
	}
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string) error
}

// commands takes the lifecycle so the two long-running roles can close over it.
//
// Only `server` and `node` can be hurried — the rest either exit on their own or
// have nothing running to wait for — so it is captured by those two rather than
// widening every command's signature or, worse, becoming package state.
func commands(lc *lifecycle) []command {
	return []command{
		{"server", "run the control plane (add --dev to also run a node here)",
			func(ctx context.Context, args []string) error { return cmdServer(ctx, lc, args) }},
		{"node", "run a compute host that dials a control plane",
			func(ctx context.Context, args []string) error { return cmdNode(ctx, lc, args) }},
		{"ca", "issue the certificates nodes authenticate with", cmdCA},
		{"check", "validate the config and state directory, then exit", cmdCheck},
		{"init", "generate a billet.yaml interactively", cmdInit},
		{"github-app", "create and install the GitHub App billet uses", cmdGitHubApp},
		{"teardown", "delete the scale sets billet created on GitHub", cmdTeardown},
		{"status", "show cluster status", cmdStatus},
		{"version", "print version information", cmdVersion},
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return nil
	}

	// Ctrl-C and SIGTERM cancel the context. Every long-running role drains rather
	// than dropping jobs on the floor: a runner killed mid-job leaves an orphaned
	// registration on GitHub that someone has to clean up by hand.
	//
	// THE FIRST ASKS, THE SECOND INSISTS, THE THIRD GIVES UP — from ONE
	// registration, because two registrations both receive every signal. See
	// lifecycle.escalate for what each level does and for the bug the single
	// registration exists to prevent.
	ctx, cancelGraceful := context.WithCancel(context.Background())
	defer cancelGraceful()

	lc := newLifecycle(cancelGraceful)

	// Buffered for three, because there are now three levels and a signal that
	// arrives while the goroutine is between receives must not be dropped.
	signals := make(chan os.Signal, 3)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(signals)

	go lc.escalate(signals, os.Exit)

	for _, c := range commands(lc) {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}
	usage()
	return fmt.Errorf("unknown command %q", args[0])
}

func usage() {
	fmt.Fprint(os.Stderr, "billet — self-hosted GitHub Actions runners\n\nusage: billet <command> [flags]\n\n")
	w := tabwriter.NewWriter(os.Stderr, 0, 0, 3, ' ', 0)
	// A lifecycle nothing will use: usage only reads names and summaries, and
	// building one here keeps commands() from needing a nil-safe path that only
	// this call site would ever exercise.
	for _, c := range commands(newLifecycle(func() {})) {
		fmt.Fprintf(w, "  %s\t%s\n", c.name, c.summary)
	}
	_ = w.Flush()
	fmt.Fprint(os.Stderr, "\nRun 'billet <command> -h' for details.\n")
}

// newFlagSet returns a FlagSet whose help output goes to stdout and which does
// not print its own error banner on top of ours.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	return fs
}

// parse rejects leftover positional arguments, so a typo like
// `billet server --dev extra` fails instead of being silently ignored.
func parse(fs *flag.FlagSet, args []string) error {
	return parseWithArgs(fs, args, 0)
}

// parseWithArgs parses flags for a command that takes positional arguments.
//
// A COMMAND MUST SAY HOW MANY IT WANTS. The default of zero is what catches a
// typo'd flag, which flag.Parse hands back as a positional rather than
// rejecting: `billet server -dvе` becomes an argument, the flag stays false, and
// the process runs in a mode nobody asked for. That protection is worth keeping,
// which is why this is opt-in per command rather than simply removed — `billet
// ca issue <node>` was written against the strict version and could not run at
// all.
func parseWithArgs(fs *flag.FlagSet, args []string, want int) error {
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() > want {
		return fmt.Errorf("unexpected argument %q", fs.Arg(want))
	}

	return nil
}

// parseWithName parses a command that takes one positional argument, whichever
// side of the flags it was written on.
//
// GO'S FLAG PACKAGE STOPS AT THE FIRST POSITIONAL, so `billet ca issue epyc-1
// --config x.yaml` leaves `--config x.yaml` sitting in the argument list: the
// config path is silently ignored and the command reads the default file. That
// is the order every operator writes — the subject first, the options after —
// and it is the order every example in the README uses.
//
// So the flags are parsed twice: once to reach the positional, once for whatever
// followed it.
func parseWithName(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil
	}

	name := rest[0]

	if err := fs.Parse(rest[1:]); err != nil {
		return "", err
	}

	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	return name, nil
}

// defaultConfigPath deliberately does NOT look in the working directory.
//
// A server started from an attacker-writable directory would otherwise silently
// adopt that directory's billet.yaml — which chooses the state directory, the
// GitHub App key path, and every tier's resources. For a process that is often
// run as root by a unit file, that is privileged config injection. Use --config
// to point anywhere else.
func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "billet", "billet.yaml")
	}
	return "/etc/billet/billet.yaml"
}

func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfigPath(), "path to billet.yaml")
}

func cmdServer(ctx context.Context, lc *lifecycle, args []string) error {
	fs := newFlagSet("billet server")
	cfgPath := addConfigFlag(fs)
	dev := fs.Bool("dev", false, "also run a node in this process (single-machine deployment)")
	dryRun := fs.Bool("dry-run", false,
		"connect to GitHub and advertise ZERO capacity: proves the whole path without accepting a job")
	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Server == nil {
		return fmt.Errorf("%s has no server section", *cfgPath)
	}
	if *dev && cfg.Node == nil {
		return fmt.Errorf("--dev needs a node section in %s", *cfgPath)
	}

	// ONLY --dev RUNS BOTH ROLES, so only --dev needs them to agree. Enforcing it
	// at load would refuse a shared configuration file on a node-only host merely
	// because the unused server section names a controller's lock directory.
	if *dev && !cfg.LockDirsAgree() {
		return fmt.Errorf(
			"--dev runs a server and a node in one process, but server.lock_dir (%q) and "+
				"node.lock_dir (%q) differ; they would take two locks for one deployment "+
				"identity and both manage the same containers",
			cfg.Server.LockDir, cfg.Node.LockDir)
	}

	if cfg.GitHub == nil {
		return fmt.Errorf("%s has no github section; run `billet github-app create` first", *cfgPath)
	}

	// STANDALONE `billet server` IS THE POINT OF THE SPLIT, and it used to be
	// refused. The old guard said "without --dev nothing in this process can
	// launch a job", which was true until a node could dial in from another
	// machine. It is false now, and leaving it would have made the whole feature
	// unreachable from the command line.
	//
	// A control plane with no nodes yet is not an error, but it is not free
	// either, and the earlier version of this comment said it was: "jobs queue
	// until one registers". They do not. Capacity is advertised from the budget
	// without consulting node availability, so a job assigned while the fleet is
	// empty is ACQUIRED and then fails to launch with ErrNoNode. billet releases
	// its own lease and has no way to decline the assignment, so it stays with
	// GitHub until the pickup deadline and is requeued a bounded number of times.
	// Start the nodes first.
	//
	// --dry-run remains for proving the GitHub path while advertising zero.

	return runServer(ctx, lc, cfg, *dryRun, *dev)
}

// runServer starts the control plane and blocks until it is told to stop.
// claimDeployment reads this installation's identity and takes the host-wide
// lock on it, returning both.
//
// MUST BE CALLED BEFORE state.Open AND BEFORE newProvider. state.Open creates
// files, runs integrity checks and applies migrations, so claiming afterwards
// let a process that is about to be REFUSED first migrate the database it was
// refused the right to use — start an old copied backup while the original is
// live and the copy is silently upgraded on its way to the error. newProvider is
// the other side of it: nothing may touch a container before the right to manage
// containers under this identity is established.
//
// Split out of runServer to be testable. That covers the logic and NOT the call
// site: deleting the call from runServer still leaves these tests green, and no
// test in this repo would notice. Recorded rather than papered over.
func claimDeployment(cfg *config.Config) (string, *state.DeploymentLock, error) {
	return claimIdentity(cfg.Server.StateDir, cfg.Server.LockDir, cfg.Server.AllowUnlockedDeployment)
}

// claimNodeDeployment is the same claim for a standalone node.
//
// SHARES claimIdentity with the server rather than repeating it, because the two
// roles can run on one host and the thing that must not diverge is exactly this:
// where the lock lands. A second copy of these four lines is how one of them
// keeps the default while the other honours a configured directory, after which
// both processes manage the same containers.
func claimNodeDeployment(cfg *config.Config, bundle *wirecert.Bundle) (string, *state.DeploymentLock, error) {
	// A NODE JOINS A DEPLOYMENT, IT DOES NOT FOUND ONE. Without a bundle there is
	// nothing to join — config validation has already established that means a
	// control plane inside this machine — so minting is correct. With one, the
	// certificate says which installation this host belongs to, and inventing a
	// different answer produces a node the control plane refuses forever.
	if bundle == nil {
		return claimIdentity(cfg.Node.StateDir, cfg.Node.LockDir, false)
	}

	deployment, err := bundle.Deployment()
	if err != nil {
		return "", nil, err
	}

	if _, err := state.AdoptDeploymentID(cfg.Node.StateDir, deployment); err != nil {
		return "", nil, err
	}

	return claimIdentity(cfg.Node.StateDir, cfg.Node.LockDir, false)
}

// claimIdentity reads an installation identity and takes the host-wide lock.
func claimIdentity(
	stateDir, lockDir string, allowUnplaceable bool,
) (string, *state.DeploymentLock, error) {
	deployment, err := state.DeploymentID(stateDir)
	if err != nil {
		return "", nil, err
	}

	// The state directory's own lock guards a PATH, so a copied directory is a
	// different inode and both copies lock happily — while both carry the same
	// deployment identity and therefore manage the same containers against the
	// same daemon. This lock is keyed by the identity, so the copy collides.
	lock, err := state.LockDeployment(deployment, state.LockOptions{
		Dir:              lockDir,
		AllowUnplaceable: allowUnplaceable,
	})
	if err != nil {
		return "", nil, err
	}

	if why := lock.Degraded(); why != "" {
		// Reached only because the operator asked for it. Still said out loud every
		// boot: billet is back to the directory lock alone, which is what it had
		// before this existed and still lets two copies of a state directory run at
		// once.
		slog.Default().Warn("starting WITHOUT a host-wide deployment lock because "+
			"allow_unlocked_deployment is set, so nothing stops a COPY of this state "+
			"directory from running alongside it and managing the same containers",
			"reason", why)

		return deployment, lock, nil
	}

	// LOGGED SO A MISMATCH IS VISIBLE. The default location is per-user, so two
	// billets that ought to collide can quietly pick different directories and
	// both start. The path is the only evidence of which collision domain this
	// process actually joined.
	slog.Default().Info("holding the host-wide deployment lock",
		"identity", deployment, "path", lock.Path())

	return deployment, lock, nil
}

// nodeContribution is what this host offers: what it detected, unless its own
// config said otherwise.
//
// ONE DEFINITION FOR BOTH ROLES. `billet node` sends this over the wire and
// `billet server --dev` writes it to the ledger directly, and if the two
// resolved it differently the same config file would describe a different
// machine depending on which process happened to run it.
func nodeContribution(cfg *config.Config) (config.Contribution, error) {
	vcpu, memory, err := config.DetectHostCapacity()
	if err != nil {
		return config.Contribution{}, err
	}

	return cfg.Node.Contribution(vcpu, memory), nil
}

func runServer(ctx context.Context, lc *lifecycle, cfg *config.Config, dryRun, dev bool) error {
	// Built by the SHARED constructor, so the server and teardown authenticate
	// identically. Two near-identical constructions is how one of them ends up
	// pointed at a different organization than the other.
	client, err := newScaleSetClient(cfg)
	if err != nil {
		return err
	}

	// THE IDENTITY IS CLAIMED BEFORE THE DATABASE IS OPENED, and the ordering is
	// the point rather than tidiness. state.Open creates files, runs integrity
	// checks and applies migrations — so acquiring the lock afterwards let a
	// process that is about to be REFUSED first migrate the database it was
	// refused the right to use. Start an old copied backup while the original is
	// live and the copy would be silently upgraded on its way to the error.
	//
	// Nothing here touches a container either: this runs before newProvider.
	// CLAIMED FOR EVERY SERVER, not only for --dev. The node wire refuses a node
	// whose deployment identity differs from this control plane's, so a server
	// that never learned its own would compare every node against "" and refuse
	// the entire fleet — the feature failing closed for a reason nobody could see.
	deployment, err := state.DeploymentID(cfg.Server.StateDir)
	if err != nil {
		return err
	}

	if dev {
		var deploymentLock *state.DeploymentLock

		deployment, deploymentLock, err = claimDeployment(cfg)
		if err != nil {
			return err
		}

		defer func() {
			if err := deploymentLock.Release(); err != nil {
				slog.Default().Warn("could not release the deployment lock; a restart may "+
					"have to wait for the kernel to drop it", "error", err)
			}
		}()
	}

	db, err := state.Open(ctx, cfg.Server.StateDir)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	// Ctrl-C and SIGTERM stop the listeners through the context, which is what
	// releases escrowed capacity — see the listener's deferred release. A hard
	// kill skips that and leaves the reaper to expire it.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Everything billet.yaml says about the control plane, assembled in one place
	// inside the server package so the config-to-listener chain is testable
	// without spanning two packages. Whatever this command adds below is about
	// how it was INVOKED — a flag, a co-resident node — not about the file.
	opts, err := server.OptionsFromConfig(cfg)
	if err != nil {
		return err
	}

	// The second signal, reaching the drain that honours it.
	opts = append(opts, server.WithHurry(lc.hurry))

	if dryRun {
		opts = append(opts, server.AdvertiseNothing())

		fmt.Printf("billet server (DRY RUN): %d tiers, advertising ZERO capacity.\n", len(cfg.Tiers))
		fmt.Printf("Scale sets are created and polled; no job will be accepted.\n")
	} else {
		fmt.Printf("billet server: %d tiers, ceiling %d vCPU / %s\n",
			len(cfg.Tiers), cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	// The owner identifies this process to GitHub's message queue so a session
	// left by a crashed run is distinguishable from a live one.
	owner, err := os.Hostname()
	if err != nil || owner == "" {
		owner = "billet"
	}

	if dev {
		// deployment and its host-wide lock were claimed above, before the database
		// was opened.
		p, err := newProvider(cfg, deployment)
		if err != nil {
			return err
		}

		// REGISTERED BEFORE ANYTHING IS PLACED ON IT. A node exists in the ledger
		// because it said so, and placement compares a lease against the provider
		// the host REGISTERED rather than one a catalog claims — so Bind refuses
		// every lease until this row is here.
		//
		// --dev registers directly rather than over the wire, so the contribution
		// is resolved here exactly as nodeclient resolves it for a remote host.
		// Two paths to one ledger row, and they must agree about what this machine
		// offers or the same config would mean different things depending on
		// whether the node happened to be in this process.
		contribution, err := nodeContribution(cfg)
		if err != nil {
			return err
		}

		for _, w := range contribution.Warnings {
			slog.Default().Warn(w, "node", cfg.Node.Name)
		}

		if err := allocator.RegisterNode(ctx, alloc.NodeRegistration{
			Name:     cfg.Node.Name,
			Provider: cfg.Node.Provider,
			Site:     cfg.Node.Site,
			VCPU:     contribution.VCPU,
			Memory:   contribution.Memory,
		}); err != nil {
			return err
		}

		// Validation already parsed this, so it cannot fail here — but reading it
		// through the same accessor keeps one definition of what the string means.
		maxCustody, err := cfg.Node.MaxCustodyDuration()
		if err != nil {
			return err
		}

		runner := node.New(allocator, cfg.Node.Name, wiring.JITSource{Client: client}, p,
			cfg.Tiers, slog.Default(), node.WithMaxCustody(maxCustody))

		// CLEARED BEFORE A SINGLE JOB IS ADMITTED.
		//
		// Everything this backend is running belongs to a process that is gone —
		// this one has empty maps and can neither heartbeat those leases nor notice
		// their completion. Left alone, such a container runs forever on capacity
		// the reaper will shortly hand back out, so the host ends up over-committed
		// by exactly what the crash leaked.
		//
		// Fatal on failure, deliberately. Not knowing what is already running is
		// not the same as nothing running, and starting anyway turns a recoverable
		// mess into a compounding one.
		if err := runner.Recover(ctx); err != nil {
			return err
		}

		opts = append(opts, server.WithNodeRunner(runner))
	}

	// THE NODE WIRE IS SERVED WHETHER OR NOT ANY NODE EXISTS YET.
	//
	// A control plane that only opened its listener once a node was configured
	// would make the first node's setup a chicken-and-egg problem, and there is
	// nothing to guard: an empty fleet answers every request with "I do not know
	// you".
	nodes := nodeplane.New(slog.Default(), deployment, allocator.LeaseTTL(),
		nodeplane.WithRegistrar(allocator))

	stopWire, err := serveNodeWire(ctx, cfg, owner, nodes, allocator, wiring.NodeJIT{Client: client})
	if err != nil {
		return err
	}

	defer stopWire()

	// THE REMOTE PLANE DRIVES COMPUTE WHENEVER THERE IS NO LOCAL NODE.
	//
	// --dev already attached an in-process runner above, and it wins for that
	// process: a single-machine deployment should not send commands to itself
	// over a loopback socket to reach a runner it already holds. Without --dev
	// this is the only thing that can launch anything, and forgetting to attach
	// it — which is exactly what the first version of this branch did — leaves a
	// control plane that serves the node wire, accepts registrations, and then
	// never sends a single command.
	if !dev {
		opts = append(opts, server.WithNodeRunner(nodes.NewRunner()))
	}

	plane := server.New(allocator, wiring.Provisioner{Client: client}, cfg.Tiers, owner, slog.Default(), opts...)
	if err := plane.Run(ctx); err != nil {
		return err
	}

	fmt.Println("billet server: stopped")

	return nil
}

// nodeTLSHosts is what a node will type to reach this control plane.
//
// A WILDCARD LISTEN ADDRESS ANSWERS A DIFFERENT QUESTION than this one. It says
// which interfaces to accept on; it says nothing about the name a node dials,
// and a certificate minted for "0.0.0.0" matches nothing. The failure would land
// on the node as a name mismatch, on the far side of the deployment from the
// file that caused it — so it is refused here, where the file is.
func nodeTLSHosts(cfg *config.Config) ([]string, error) {
	if len(cfg.Server.NodeTLSHosts) > 0 {
		return cfg.Server.NodeTLSHosts, nil
	}

	host, _, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		return nil, fmt.Errorf("server.listen %q must be host:port: %w", cfg.Server.Listen, err)
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		return nil, fmt.Errorf(
			"server.listen is %q, which accepts on every interface and so does not say what a "+
				"node will dial. Set server.node_tls_hosts to the names and addresses nodes use "+
				"for this control plane; they become the subject names of the certificate it "+
				"serves", cfg.Server.Listen)
	}

	return []string{host}, nil
}

// provisioner adapts the scale-set client to what the control plane consumes.
//
// It exists because internal/scaleset returns its OWN ScaleSet type: the
// alternative is that package importing internal/server purely to name a
// two-field struct, which points the dependency the wrong way for a package
// whose job is to keep a preview API at arm's length.
// serveNodeWire opens the listener nodes dial.
//
// LOOPBACK IS THE ONLY ADDRESS SERVED WITHOUT mTLS. The wire identifies a node
// by the name in its certificate; without one, the only thing left is the name
// in the request path, which is a claim rather than a proof. Anything that could
// reach such a listener could bind another node's leases, take its commands,
// and — worst — ask for a JIT registration, a credential that registers a runner
// against the organisation.
//
// So a network address mints a certificate from the deployment's own authority
// and requires one back. There is nothing to configure and nothing to install:
// the CA lives beside the state directory, and `billet ca issue <node>` produces
// the bundle a node is given.
func serveNodeWire(
	ctx context.Context,
	cfg *config.Config, deployment string,
	nodes *nodeplane.Plane, store nodeplane.LeaseStore, jit nodeplane.JITSource,
) (func(), error) {
	addr := cfg.Server.Listen
	loopback := nodeplane.LoopbackOnly(addr)

	// THE CATALOGUE TRAVELS WITH THE WIRE, because a JIT request is checked
	// against the tier its lease names — including that tier's runner group,
	// which is how an operator keeps a tier away from every repository in the
	// organisation.
	handlerOpts := []nodeplane.HandlerOption{nodeplane.WithTiers(cfg.Tiers)}

	var tlsConf *tls.Config

	if !loopback {
		hosts, err := nodeTLSHosts(cfg)
		if err != nil {
			return nil, err
		}

		ca, err := wirecert.LoadOrCreateCA(cfg.Server.StateDir, deployment)
		if err != nil {
			return nil, err
		}

		bundle, err := ca.IssueServer(hosts)
		if err != nil {
			return nil, err
		}

		if tlsConf, err = wirecert.ServerTLS(bundle); err != nil {
			return nil, err
		}

		handlerOpts = append(handlerOpts, nodeplane.RequireClientCert())

		slog.Default().Info("the node wire requires client certificates",
			"hosts", hosts, "ca_expires", ca.NotAfter().Format(time.DateOnly))
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for nodes on %s: %w", addr, err)
	}

	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}

	srv := &http.Server{
		Handler: nodeplane.Handler(slog.Default(), nodes, store, jit, handlerOpts...),
		// A command poll is a LONG poll, so there is deliberately no write or idle
		// timeout that would cut one. The read-header timeout still bounds the one
		// phase a client controls before the handler is entered, which is what
		// stops a stalled connection holding a goroutine forever.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("the node listener stopped", "error", err)
		}
	}()

	slog.Default().Info("serving the node wire", "addr", ln.Addr().String())

	return func() {
		// A FRESH CONTEXT, deliberately. Shutdown runs while the caller's context
		// is already cancelled — that cancellation is what brought us here — so
		// deriving from it would abort the drain instantly and cut the very
		// connections this is meant to let finish.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Default().Warn("the node listener did not shut down cleanly", "error", err)
		}
	}, nil
}

// newProvider builds the compute backend this host runs.
//
// Only docker exists today. firecracker needs Linux and /dev/kvm, tart needs
// Apple Silicon, ec2 needs an account — each is a separate implementation of the
// same interface, and each is refused explicitly rather than falling through to
// something that happens to compile.
func newProvider(cfg *config.Config, deployment string) (provider.Provider, error) {
	switch cfg.Node.Provider {
	case config.ProviderDocker:
		// Labelled with the DEPLOYMENT id, not the node name. Two billets on one
		// machine share a hostname — and therefore a default node name — while
		// keeping separate state directories, so a node-name label would let each
		// enumerate the other's containers and destroy them as orphans.
		return docker.New(deployment, docker.WithLogger(slog.Default())), nil

	case config.ProviderFirecracker, config.ProviderTart, config.ProviderEC2:
		return nil, fmt.Errorf("%w: the %s provider is not built yet; --dev currently runs the "+
			"docker backend, which shares the host kernel and is for trials rather than for "+
			"untrusted work", errNotImplemented, cfg.Node.Provider)

	default:
		return nil, fmt.Errorf("billet: unknown provider %q", cfg.Node.Provider)
	}
}

// nodeBundle loads the certificate this node presents, if it has one.
//
// Config validation is what refuses a network address without a bundle, so a nil
// return here means loopback — the one case where the control plane is inside
// this machine and there is nothing between the two to authenticate against.
func nodeBundle(cfg *config.Config) (*wirecert.Bundle, error) {
	if cfg.Node.TLS == nil {
		return nil, nil //nolint:nilnil // no bundle is a state, not an error
	}

	bundle, err := wirecert.LoadBundle(cfg.Node.TLS.CertPath, cfg.Node.TLS.KeyPath, cfg.Node.TLS.CAPath)
	if err != nil {
		return nil, err
	}

	// VALIDATED BEFORE ANYTHING IS WRITTEN FROM IT, because the deployment
	// identity this bundle names is about to be recorded permanently. A malformed
	// or mixed bundle would otherwise write deployment A into the state directory
	// and only then fail on the key pair — after which the CORRECT bundle for
	// deployment B is refused as an identity conflict, and an operator has to
	// clear state by hand for an enrollment that never succeeded.
	if _, err := wirecert.ClientTLS(bundle); err != nil {
		return nil, err
	}

	// CHECKED HERE, WHERE THE FILES ARE NAMED. The control plane refuses a
	// mismatch too, but it can only say "you are not who you claim" — this can say
	// which file on this host holds the wrong certificate, which is the sentence
	// an operator can act on.
	name, err := bundle.NodeName()
	if err != nil {
		return nil, err
	}

	if name != cfg.Node.Name {
		return nil, fmt.Errorf(
			"node.name is %q but %s was issued for %q; the control plane authorises by the "+
				"name in the certificate, so this node could only ever act as %q",
			cfg.Node.Name, cfg.Node.TLS.CertPath, name, name)
	}

	return &bundle, nil
}

func cmdNode(ctx context.Context, lc *lifecycle, args []string) error {
	fs := newFlagSet("billet node")
	cfgPath := addConfigFlag(fs)
	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Node == nil {
		return fmt.Errorf("%s has no node section", *cfgPath)
	}

	if cfg.Node.ServerAddr == "" {
		return fmt.Errorf("%s has no node.server_addr, so this host does not know which "+
			"control plane to dial", *cfgPath)
	}

	// LOADED BEFORE THE IDENTITY IS CLAIMED, because the certificate is what
	// decides the identity. A node JOINS a deployment; it does not found one.
	bundle, err := nodeBundle(cfg)
	if err != nil {
		return err
	}

	// THE NODE'S IDENTITY COMES FROM THE CERTIFICATE IT WAS ISSUED, falling back
	// to its own state directory only when there is no control plane beyond this
	// machine to join. It labels its compute with this, and the control plane
	// refuses a node whose deployment differs — otherwise a host would start
	// containers this installation could never attribute.
	// THE NODE'S LOCK MUST LAND WHERE THE SERVER'S DOES. Both roles can run on
	// one host, and a server honouring server.lock_dir while this used the
	// per-user default would take two different locks for one identity — after
	// which both manage the same containers and either can adopt or destroy the
	// other's live work. Sharing the claim is what keeps that true.
	deployment, lock, err := claimNodeDeployment(cfg, bundle)
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.Release(); err != nil {
			slog.Default().Warn("could not release the deployment lock", "error", err)
		}
	}()

	var tlsConf *tls.Config

	if bundle != nil {
		if tlsConf, err = wirecert.ClientTLS(*bundle); err != nil {
			return err
		}
	}

	client, err := nodeclient.New(nodeclient.Options{
		Base: cfg.Node.ServerAddr,
		Node: cfg.Node.Name,
		TLS:  tlsConf,
	})
	if err != nil {
		return err
	}

	p, err := newProvider(cfg, deployment)
	if err != nil {
		return err
	}

	maxCustody, err := cfg.Node.MaxCustodyDuration()
	if err != nil {
		return err
	}

	// THE CLIENT IS BOTH THE LEDGER AND THE MINT.
	//
	// It satisfies node.LeaseStore and node.JITSource, which is the whole reason
	// the runner needs no idea it is remote: the interfaces it already took are
	// the seam the network went through.
	runner := node.New(client, cfg.Node.Name, client, p, cfg.Tiers, slog.Default(),
		node.WithMaxCustody(maxCustody))

	fmt.Printf("billet node %s: dialing %s\n", cfg.Node.Name, cfg.Node.ServerAddr)

	// NO GUEST-OS CLAIM. A host's allowlist lives in the SERVER's fleet
	// configuration, not here, and Bind is what enforces it. Sending one from the
	// node would be a second authority for a fact the operator already stated in
	// one place — and the node's copy is the one nobody would think to update.
	// Parsed rather than trusted: Validate rejected a bad value when the file was
	// read, so this cannot normally fail — but reading it is the only thing that
	// makes node.drain_timeout take effect.
	drainTimeout, err := cfg.Node.DrainTimeoutDuration()
	if err != nil {
		return err
	}

	// RESOLVED ONCE, HERE, rather than on each re-registration. A drain
	// re-registers, and a node that came back reporting a different contribution
	// would move the fleet's arithmetic underneath work it is still holding.
	contribution, err := nodeContribution(cfg)
	if err != nil {
		return err
	}

	for _, w := range contribution.Warnings {
		slog.Default().Warn(w, "node", cfg.Node.Name)
	}

	return nodeclient.Run(ctx, client, runner, nodeclient.LoopOptions{
		Provider:     cfg.Node.Provider,
		Deployment:   deployment,
		Site:         cfg.Node.Site,
		VCPU:         contribution.VCPU,
		Memory:       contribution.Memory,
		Log:          slog.Default(),
		SweepEvery:   5 * time.Minute,
		DrainTimeout: drainTimeout,
		// The second signal, reaching the wait that honours it.
		Hurry: lc.hurry,
	})
}

// cmdCheck is the explicit "is this deployment sane" command. It is the only
// path that opens — and therefore migrates — the state database, so that
// mutating durable state is always something the operator asked for.
// newScaleSetClient builds the GitHub client from config, reading the key with
// the same hardened reader `billet check` uses.
//
// Shared so teardown and the server authenticate identically. A second,
// slightly-different construction is how one of them ends up talking to a
// different organization than the other.
func newScaleSetClient(cfg *config.Config) (*scaleset.Client, error) {
	if cfg.GitHub == nil {
		return nil, errors.New("no github section in the config")
	}

	key, err := readPrivateKey(cfg.GitHub.PrivateKeyPath)
	if err != nil {
		return nil, err
	}

	appIdentity := cfg.GitHub.ClientID
	if appIdentity == "" {
		appIdentity = strconv.FormatInt(cfg.GitHub.AppID, 10)
	}

	return scaleset.New(scaleset.Config{
		ConfigURL:      "https://github.com/" + cfg.GitHub.Org,
		ClientID:       appIdentity,
		InstallationID: cfg.GitHub.InstallationID,
		PrivateKey:     string(key),
	}, slog.Default())
}

// cmdTeardown removes the scale sets billet created.
//
// It exists because there is no other way to remove them. A scale set created
// through the API has no delete control in GitHub's UI — the org's runner list
// shows it with no options menu, and its detail page offers statistics and
// nothing else. Without this command, billet creates objects on somebody's
// organization that they cannot clean up by hand.
//
// Deliberately NOT part of any automatic path. Nothing about stopping the server
// should delete a scale set: an operator restarting billet, or running it on a
// second host, would find their tiers dismantled underneath them. Teardown is a
// thing an operator asks for, once, on purpose.
func cmdTeardown(ctx context.Context, args []string) error {
	fs := newFlagSet("billet teardown")
	cfgPath := addConfigFlag(fs)
	tier := fs.String("tier", "", "delete this tier's scale set")
	all := fs.Bool("all", false, "delete every tier's scale set")
	force := fs.Bool("force", false,
		"delete even if the scale set's labels are not this tier's (requires --tier)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// Checked HERE rather than inside the client. config.Load accepts a node-only
	// config with no github section, and everything below reads cfg.GitHub.Org —
	// so without this a node config panics instead of explaining itself.
	if cfg.GitHub == nil {
		return fmt.Errorf("%s has no github section, so it names no organization to "+
			"delete anything from", *cfgPath)
	}

	// "Delete everything" is NEVER the default for a destructive command.
	//
	// It used to be: an omitted --tier selected every tier, which is
	// indistinguishable from `--tier "$TIER"` with TIER unset. A script with an
	// empty variable would have deleted every scale set in the org while looking
	// like it asked for one.
	switch {
	case *all && *tier != "":
		return errors.New("pass either --tier or --all, not both")
	case *all && *force:
		return errors.New("--force applies to one scale set at a time, so it needs --tier")
	case !*all && *tier == "":
		return errors.New("name a tier with --tier, or pass --all to delete every tier's " +
			"scale set")
	}

	wanted := cfg.Tiers

	if *tier != "" {
		wanted = nil

		for i := range cfg.Tiers {
			if cfg.Tiers[i].Label == *tier {
				wanted = append(wanted, cfg.Tiers[i])
			}
		}

		if len(wanted) == 0 {
			return fmt.Errorf("no tier named %q in %s", *tier, *cfgPath)
		}
	}

	if len(wanted) == 0 {
		return errors.New("the config declares no tiers, so there is nothing to delete")
	}

	client, err := newScaleSetClient(cfg)
	if err != nil {
		return err
	}

	// The ACTUAL objects, fetched before anything is destroyed. An operator
	// confirming a destructive act should be shown what is on GitHub, not the
	// names they typed into their own config.
	fmt.Printf("This deletes the following from %s:\n\n", cfg.GitHub.Org)

	var found int

	for i := range wanted {
		t := &wanted[i]

		set, labels, err := client.Describe(ctx, t.Label, t.RunnerGroup)
		if err != nil {
			return err
		}

		if set == nil {
			fmt.Printf("  %-32s not present\n", t.Label)

			continue
		}

		found++

		fmt.Printf("  %-32s id %d, group %s, labels %v\n", t.Label, set.ID, set.Group, labels)
	}

	if found == 0 {
		fmt.Println("\nNothing to do.")

		return nil
	}

	fmt.Println("\nRunners already registered to them are removed by GitHub.")

	if !*yes {
		if err := confirmOrganization(ctx, cfg.GitHub.Org); err != nil {
			return err
		}
	}

	for i := range wanted {
		t := &wanted[i]

		deleted, err := client.DeleteScaleSet(ctx, t.Label, t.RunnerGroup, []string{t.Label}, *force)
		if err != nil {
			return err
		}

		// Reported distinctly. Absence is scoped to the runner group being asked
		// about, so a tier created under a different group reports "not present"
		// here while the original survives — and an operator who reads that as
		// "deleted" walks away from an object that is still there.
		if !deleted {
			fmt.Printf("%s: nothing in runner group %q; if it was created under a different "+
				"group it is still there\n", t.Label, groupOrDefault(t.RunnerGroup))
		}
	}

	fmt.Println("Done.")

	return nil
}

// groupOrDefault names the runner group a tier resolves to, for messages.
func groupOrDefault(group string) string {
	if group == "" {
		return scaleset.DefaultRunnerGroup
	}

	return group
}

// confirmOrganization makes the operator type the organization name.
//
// Typed confirmation rather than y/N: this is destructive against somebody's
// organization, and the cost of a stray keystroke is a tier that silently stops
// accepting work.
//
// Read on a goroutine so the context still wins. fmt.Scanln does not observe
// cancellation, so a Ctrl-C at the prompt would otherwise cancel ctx and leave
// the process blocked on stdin.
func confirmOrganization(ctx context.Context, org string) error {
	fmt.Printf("\nType the organization name to confirm: ")

	typed := make(chan string, 1)
	failed := make(chan error, 1)

	go func() {
		var answer string

		if _, err := fmt.Scanln(&answer); err != nil {
			failed <- err

			return
		}

		typed <- answer
	}()

	select {
	case <-ctx.Done():
		fmt.Println()

		return ctx.Err()
	case err := <-failed:
		return fmt.Errorf("teardown cancelled: %w", err)
	case answer := <-typed:
		if answer != org {
			return errors.New("teardown cancelled")
		}

		return nil
	}
}

// cmdCA issues the certificates the node wire authenticates with.
//
// RUN ON THE CONTROL PLANE, because that is where the authority's private key
// is and where it stays. The bundle it writes is copied to the node — the key
// travels once, by an operator, rather than over a wire that does not yet trust
// anybody.
func cmdCA(_ context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet ca issue <node> [--out <dir>] | billet ca show")
	}

	switch args[0] {
	case "issue":
		return cmdCAIssue(args[1:])
	case "show":
		return cmdCAShow(args[1:])
	}

	return fmt.Errorf("unknown ca command %q; try issue or show", args[0])
}

func cmdCAIssue(args []string) error {
	fs := newFlagSet("billet ca issue")
	cfgPath := addConfigFlag(fs)
	out := fs.String("out", "", "directory to write the bundle to (default ./<node>-billet-tls)")

	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if name == "" {
		return errors.New("usage: billet ca issue <node> [--out <dir>]")
	}

	// VALIDATED HERE, not on first connection. The common name IS the node's
	// identity on the wire, so a certificate whose name the server would never
	// accept is a bundle an operator installs, restarts a host for, and only then
	// discovers is useless.
	if err := config.ValidateNodeName("node", name); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return fmt.Errorf("%s has no server section, so it does not hold a certificate "+
			"authority; run this on the control plane", *cfgPath)
	}

	deployment, err := state.DeploymentID(cfg.Server.StateDir)
	if err != nil {
		return err
	}

	ca, err := wirecert.LoadOrCreateCA(cfg.Server.StateDir, deployment)
	if err != nil {
		return err
	}

	bundle, err := ca.IssueNode(name)
	if err != nil {
		return err
	}

	dir := *out
	if dir == "" {
		dir = name + "-billet-tls"
	}

	if err := bundle.Write(dir); err != nil {
		return err
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}

	fmt.Printf("billet ca: wrote a bundle for node %q to %s\n\n", name, abs)
	fmt.Print("Copy that directory to the node, then point its config at the files:\n\n")
	fmt.Printf("  node:\n    name: %s\n    tls:\n      cert: /etc/billet/tls/node.crt\n"+
		"      key:  /etc/billet/tls/node.key\n      ca:   /etc/billet/tls/ca.crt\n\n", name)
	fmt.Print("node.key is a private key: keep it 0600 and do not copy it anywhere else.\n")

	return nil
}

func cmdCAShow(args []string) error {
	fs := newFlagSet("billet ca show")
	cfgPath := addConfigFlag(fs)

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return fmt.Errorf("%s has no server section, so it does not hold a certificate "+
			"authority", *cfgPath)
	}

	deployment, err := state.DeploymentID(cfg.Server.StateDir)
	if err != nil {
		return err
	}

	ca, err := wirecert.LoadOrCreateCA(cfg.Server.StateDir, deployment)
	if err != nil {
		return err
	}

	fmt.Printf("deployment %s\nauthority   %s\nexpires     %s\n",
		deployment, wirecert.CADir(cfg.Server.StateDir), ca.NotAfter().Format(time.RFC3339))

	return nil
}

func cmdCheck(ctx context.Context, args []string) error {
	fs := newFlagSet("billet check")
	cfgPath := addConfigFlag(fs)
	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("config   %s\n", *cfgPath)

	if cfg.GitHub != nil {
		fmt.Printf("org      %s (app %d, installation %d)\n",
			cfg.GitHub.Org, cfg.GitHub.AppID, cfg.GitHub.InstallationID)
		if err := checkPrivateKey(cfg.GitHub.PrivateKeyPath); err != nil {
			return err
		}
	}

	if cfg.Server != nil {
		fmt.Printf("listen   %s\n", cfg.Server.Listen)
		fmt.Printf("ceiling  %d vCPU, %s\n", cfg.Server.MaxVCPU, cfg.Server.MaxMemory)

		db, err := state.Open(ctx, cfg.Server.StateDir)
		if err != nil {
			return fmt.Errorf("server state: %w", err)
		}
		defer db.Close()

		fmt.Printf("state    %s (ok)\n", cfg.Server.StateDir)
	}

	if cfg.Node != nil {
		fmt.Printf("node     %s via %s -> %s\n", cfg.Node.Name, cfg.Node.Provider, cfg.Node.ServerAddr)
	}

	// Per-host policy decides what each machine may run and how many macOS
	// guests it may hold. Validating it without showing it means an operator who
	// restricted a host has no way to confirm billet read what they meant.
	for i := range cfg.Nodes {
		p := &cfg.Nodes[i]

		backend := "provider unset"
		if p.Provider != "" {
			backend = string(p.Provider)
		}

		guests := "no guest_os allowlist"
		if len(p.GuestOS) > 0 {
			guests = fmt.Sprintf("guest_os %v", p.GuestOS)
		}

		// Say WHERE the number came from, and do not report a macOS capability
		// the host cannot have. A Firecracker host printed "max 2 macOS (Apple
		// default)", which is true of the config field and false of the machine:
		// macOS guests need Apple hardware, so only the Tart provider can run
		// them. And a host whose allowlist excludes macOS has an effective limit
		// of zero — reporting that as "0 (default)" reads as though billet's
		// default were zero, sending the operator to the wrong field entirely.
		var macOS string

		switch {
		case p.Provider != "" && p.Provider != config.ProviderTart:
			macOS = fmt.Sprintf("macOS n/a (%s cannot run macOS guests)", p.Provider)
		case !p.AllowsGuestOS(config.GuestMacOS):
			macOS = "no macOS (excluded by guest_os)"
		case p.MacOSVMLimit == nil:
			macOS = fmt.Sprintf("max %d macOS (Apple default)", p.MacOSLimit())
		default:
			macOS = fmt.Sprintf("max %d macOS", p.MacOSLimit())
		}

		fmt.Printf("  policy %-14s %-12s %-24s %s\n", p.Name, backend, guests, macOS)
	}

	fmt.Printf("tiers    %d\n", len(cfg.Tiers))

	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]

		intercept := ""
		if t.Intercept {
			intercept = "  cache-intercept"
		}
		// The whole list, joined. Printing t.Provider alone showed a BLANK backend
		// for any tier written with `providers:` — and `billet check` is the one
		// place an operator looks to confirm what they configured, so a field that
		// silently reads empty is worse than no field at all.
		backends := make([]string, 0, 2)
		for _, kind := range t.AcceptableProviders() {
			backends = append(backends, string(kind))
		}

		reserved := ""
		if t.Reserved > 0 {
			reserved = fmt.Sprintf("  reserved:%d", t.Reserved)
		}

		fmt.Printf("  %-34s %2d vCPU  %8s  %s/%s%s%s\n",
			t.Label, t.VCPU, t.Memory, strings.Join(backends, ","), t.GuestOS,
			reserved, intercept)
	}

	// HOW MUCH OF THE MACHINE IS SPOKEN FOR, shown whenever anything is reserved.
	//
	// A reservation is held back for as long as it is UNMET, which for a tier
	// that gets no work is forever — so an operator can quietly make their
	// machine smaller with no error and no log line, because from billet's point
	// of view nothing is wrong. This is the one place they look to confirm what
	// they configured, so the total belongs here rather than in a comment they
	// have already read.
	var (
		reservedVCPU   int
		reservedMemory config.ByteSize
	)

	for i := range cfg.Tiers {
		reservedVCPU += cfg.Tiers[i].ReservedVCPU()
		reservedMemory += cfg.Tiers[i].ReservedMemory()
	}

	if reservedVCPU > 0 && cfg.Server != nil && cfg.Server.MaxVCPU > 0 {
		// FLOAT, and one decimal place. The integer form multiplied before
		// dividing, so it could wrap on a large budget — and more usefully, it
		// printed 1 of 128 vCPU as "0%", which reads as "nothing is reserved"
		// when something is.
		share := float64(reservedVCPU) * 100 / float64(cfg.Server.MaxVCPU)

		fmt.Printf("reserved %d of %d vCPU (%.1f%%) and %s of %s, held back from other tiers "+
			"whether or not they are used\n",
			reservedVCPU, cfg.Server.MaxVCPU, share, reservedMemory, cfg.Server.MaxMemory)
	}
	return nil
}

func cmdInit(_ context.Context, args []string) error {
	fs := newFlagSet("billet init")
	if err := parse(fs, args); err != nil {
		return err
	}
	return fmt.Errorf("%w: writes a billet.yaml after asking for a provider and tier shapes", errNotImplemented)
}

func cmdStatus(_ context.Context, args []string) error {
	fs := newFlagSet("billet status")
	if err := parse(fs, args); err != nil {
		return err
	}
	return fmt.Errorf("%w: needs the capacity ledger to report against", errNotImplemented)
}

func cmdVersion(_ context.Context, args []string) error {
	fs := newFlagSet("billet version")
	if err := parse(fs, args); err != nil {
		return err
	}

	// The release version, not just the revision. This printed a bare commit sha,
	// which is true and unhelpful: an operator comparing what is installed against
	// what was released has to go and look the sha up.
	fmt.Printf("billet %s\n", version.String())

	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Printf("  go %s\n", info.GoVersion)
	}

	return nil
}
