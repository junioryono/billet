// Command billet runs a self-hosted GitHub Actions runner platform.
//
// One binary, two roles. `billet server` is the control plane: it long-polls
// GitHub for assigned jobs, owns the capacity ledger, and tells nodes what to
// launch. `billet node` is a compute host: it runs a provider and launches
// instances. A single-machine deployment runs both, side by side, talking over
// loopback — there is no combined mode and no flag for one.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/store/ceph"
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

// exitError carries a specific exit status out of a command.
//
// MOST FAILURES ARE JUST FAILURES and exit 1. A few are ANSWERS rather than errors:
// `billet runner check` reporting that the runner image is due to be rebuilt is a
// fact a monitor acts on differently from "billet could not run", and differently
// again from "GitHub has already stopped queueing jobs". Collapsing those into one
// status means a cron entry cannot tell a task from an outage, and will end up
// treating both like whichever it saw first.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// exitStatus is what the process exits with for an error.
//
// THE CONCRETE TYPE, NOT AN ANONYMOUS INTERFACE. `interface{ ExitCode() int }` also
// matches *exec.ExitError, which every failed subprocess in this program produces —
// so `rbd` exiting 2 made BILLET exit 2, which is the status `billet runner check`
// documents as "the runner image is due to be rebuilt". A monitor reading that would
// act on a storage error as though it were a scheduled task. Measured: a verify
// against a missing image exited 2, carrying rbd's status.
//
// A FUNCTION RATHER THAN FOUR LINES IN main, because the first test written for this
// replicated the decision instead of exercising it, and passed against the very bug
// it described.
func exitStatus(err error) int {
	var coded *exitError
	if errors.As(err, &coded) {
		return coded.code
	}

	return 1
}

// ExitCode is what the process should exit with.
func (e *exitError) ExitCode() int { return e.code }

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Explicit -h is a successful request for help, not a usage error.
			os.Exit(0)
		}

		fmt.Fprintf(os.Stderr, "billet: %v\n", err)
		os.Exit(exitStatus(err))
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
		{"server", "run the control plane (run `billet node` alongside it to run jobs here)",
			func(ctx context.Context, args []string) error { return cmdServer(ctx, lc, args) }},
		{"node", "run a compute host that dials a control plane",
			func(ctx context.Context, args []string) error { return cmdNode(ctx, lc, args) }},
		{"nodes", "approve the machines asking to join this deployment", cmdNodes},
		{"ca", "issue the certificates nodes authenticate with", cmdCA},
		{"leases", "show capacity held for compute nobody has accounted for", cmdLeases},
		{"check", "validate the config and state directory, then exit", cmdCheck},
		{"init", "generate a billet.yaml interactively", cmdInit},
		{"ami", "build the machine image the ec2 backend launches", cmdAMI},
		{"runner", "report how close the pinned actions/runner is to being refused", cmdRunner},
		{"images", "verify the golden image a microVM guest boots from", cmdImages},
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

	// Ctrl-C and SIGTERM cancel the context. Every long-running role drains rather than
	// dropping jobs: a runner killed mid-job leaves an orphaned registration on GitHub.
	//
	// THE FIRST ASKS, THE SECOND INSISTS, THE THIRD GIVES UP — from ONE registration,
	// because two both receive every signal. See lifecycle.escalate.
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
// `billet server --dry-run extra` fails instead of being silently ignored.
func parse(fs *flag.FlagSet, args []string) error {
	return parseWithArgs(fs, args, 0)
}

// parseWithArgs parses flags for a command that takes positional arguments.
//
// A COMMAND MUST SAY HOW MANY IT WANTS. The default of zero catches a typo'd flag,
// which flag.Parse hands back as a positional rather than rejecting: `billet server
// -dvе` becomes an argument, the flag stays false, and the process runs in a mode
// nobody asked for.
func parseWithArgs(fs *flag.FlagSet, args []string, want int) error {
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() > want {
		return fmt.Errorf("unexpected argument %q", fs.Arg(want))
	}

	return nil
}

// parseWithName parses a command that takes one positional argument, whichever side
// of the flags it was written on.
//
// GO'S FLAG PACKAGE STOPS AT THE FIRST POSITIONAL, so `billet ca issue epyc-1
// --config x.yaml` leaves the config path sitting in the argument list, silently
// ignored — and that is the order every operator writes and every README example
// uses. So the flags are parsed twice.
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
	if cfg.GitHub == nil {
		return fmt.Errorf("%s has no github section; run `billet github-app create` first", *cfgPath)
	}

	// THE CONTROL PLANE RUNS NO COMPUTE. A single machine runs `billet server` and
	// `billet node` as two processes over loopback.
	//
	// A control plane with no nodes advertises nothing, so an empty fleet is harmless:
	// GitHub is told zero and assigns nothing.
	//
	// --dry-run remains for proving the GitHub path while advertising zero.

	return runServer(ctx, lc, cfg, *dryRun)
}

// claimNodeDeployment reads this host's identity and takes the host-wide lock on
// it.
//
// THE LOCK IS THE NODE'S ALONE, because the node is the role that manages
// containers. It stops two processes carrying one deployment identity from
// managing the same compute, and a control plane manages none — so the server
// takes no lock, and `server.lock_dir` is gone.
//
// That is not tidying, it is a requirement. The lock is EXCLUSIVE per identity,
// so a server that took it would keep a node on the same machine from ever
// starting — and one machine running both is the single-machine deployment,
// which is now precisely `billet server` and `billet node` side by side.
//
// MUST BE CALLED BEFORE newProvider: nothing may touch a container before the
// right to manage containers under this identity is established.
func claimNodeDeployment(cfg *config.Config, bundle *wirecert.Bundle) (string, *state.DeploymentLock, error) {
	// A NODE JOINS A DEPLOYMENT, IT DOES NOT FOUND ONE, and the whole question is what
	// tells it which one. A certificate answers directly. Without one, the node can
	// only reach a control plane inside this machine — validation guarantees that,
	// because a certless node may dial nothing but loopback — and if this file also
	// describes that control plane, its state directory holds the answer.
	//
	// Falling back to the node's OWN directory is not an option, and fails invisibly:
	// a state directory with no identity MINTS a fresh random one, so the node invents
	// a deployment nobody has heard of, the plane refuses it for belonging elsewhere,
	// and that refusal is ErrRefused — which the node loop reads as a verdict rather
	// than an outage, so the process exits and nothing repairs it.
	deployment, err := nodeDeploymentID(cfg, bundle)
	if err != nil {
		return "", nil, err
	}

	if deployment != "" {
		if _, err := state.AdoptDeploymentID(cfg.Node.StateDir, deployment); err != nil {
			return "", nil, err
		}
	}

	return claimIdentity(cfg.Node.StateDir, cfg.Node.LockDir, cfg.Node.AllowUnlockedDeployment)
}

// nodeDeploymentID is the identity this host must claim, or "" when only its own
// state directory can say.
//
// The certificate outranks the config file: a bundle is proof issued BY the
// control plane, while a `server:` section is merely a description sitting next
// to the node's own. They agree in every sane deployment, and where they do not,
// the one the plane will actually check is the certificate.
func nodeDeploymentID(cfg *config.Config, bundle *wirecert.Bundle) (string, error) {
	if bundle != nil {
		return bundle.Deployment()
	}

	if cfg.Server != nil {
		// Founding it here is correct if the server has not started yet: whichever
		// role runs first mints the identity, and the other reads that same file.
		return state.DeploymentID(cfg.Server.StateDir)
	}

	// A node whose file says nothing about the control plane it dials. Its own
	// directory is the only answer available, so it must already hold the right
	// one — see the node.state_dir note in billet.example.yaml.
	return "", nil
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
// ONE DEFINITION, ONE CALLER, because `billet node` is the only way a host joins.
// Two paths resolving it independently would let the same file describe a
// different machine depending on which process read it.
func nodeContribution(cfg *config.Config) (config.Contribution, error) {
	// NOT MEASURED WHEN THE WORK RUNS SOMEWHERE ELSE. An ec2 node is an
	// orchestrator: it calls an API and the compute appears in a region, so this
	// machine's cores are a default for nothing — config validation requires the
	// numbers outright. Detecting anyway would spend a syscall on an answer whose
	// only use would be comparing it against a declaration it has no relationship
	// with.
	if !cfg.Node.Provider.RunsOnHost() {
		return cfg.Node.Contribution(0, 0), nil
	}

	vcpu, memory, err := config.DetectHostCapacity()
	if err != nil {
		return config.Contribution{}, err
	}

	return cfg.Node.Contribution(vcpu, memory), nil
}

// runServer starts the control plane and blocks until it is told to stop.
func runServer(ctx context.Context, lc *lifecycle, cfg *config.Config, dryRun bool) error {
	// Built by the SHARED constructor, so the server and teardown authenticate
	// identically. Two near-identical constructions is how one of them ends up
	// pointed at a different organization than the other.
	client, err := newScaleSetClient(cfg)
	if err != nil {
		return err
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
	deployment, err := state.DeploymentID(cfg.Server.StateDir)
	if err != nil {
		return err
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
	}, cfg.Tiers, alloc.WithPlacement(cfg.Server.Placement))
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

	// NOTHING IS LIVE UNTIL IT SAYS SO AGAIN, and this has to happen BEFORE
	// anything registers.
	//
	// Liveness is the plane's judgement and this plane has just started, so it has
	// none: its map is empty. Rows left by the previous process would otherwise
	// back advertisements for machines this one has never heard from.
	//
	// Every node re-registers over the wire within a poll, so the cost is a brief
	// zero that is also the truth. That was NOT true while --dev ran a node in
	// this process: it registered straight into the ledger and never dialled the
	// wire, so a sweep after it registered marked it dead with no second chance —
	// the colocated runner advertised zero forever, on the one deployment shape
	// with no other machine to fall back on. Deleting that path deleted the
	// ordering hazard with it.
	if err := allocator.ForgetEveryNode(ctx); err != nil {
		return fmt.Errorf("server: could not clear the fleet's liveness: %w", err)
	}

	// THE NODE WIRE IS SERVED WHETHER OR NOT ANY NODE EXISTS YET.
	//
	// A control plane that only opened its listener once a node was configured
	// would make the first node's setup a chicken-and-egg problem, and there is
	// nothing to guard: an empty fleet answers every request with "I do not know
	// you".
	nodes := nodeplane.New(slog.Default(), deployment, allocator.LeaseTTL(),
		nodeplane.WithRegistrar(allocator),
		// The declared places, so a node claiming one nobody declared is refused
		// here rather than recorded. A node's own config cannot make this check —
		// sites are the control plane's to declare and the node's file has no
		// reason to list them.
		nodeplane.WithSites(cfg.SiteNames()),
		// The catalogue lives here, and a launch carries the shape a node needs, so
		// no node keeps a copy that can drift from this one.
		nodeplane.WithTierCatalog(cfg.Tiers))

	stopWire, err := serveNodeWire(ctx, cfg, nodes, allocator,
		wiring.NodeJIT{Client: client}, allocator, allocator)
	if err != nil {
		return err
	}

	defer stopWire()

	// A TIMER, BECAUSE NOTHING ELSE ASKS. A node's liveness now decides what its
	// tier advertises, and an idle deployment never launches, lists or destroys —
	// so without this a host that crashed on a quiet afternoon would keep its
	// capacity advertised until somebody happened to need it.
	go nodes.Watch(ctx)

	// THE REMOTE PLANE DRIVES ALL COMPUTE, and it is the only thing that can. A
	// control plane without it serves the node wire, accepts registrations, and then
	// never sends a single command.
	opts = append(opts, server.WithNodeRunner(nodes.NewRunner()))

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
// THE DEPLOYMENT IS READ HERE RATHER THAN PASSED IN, because a parameter of
// that type is one a caller can fill with the wrong string and the compiler
// cannot tell. It was filled with the hostname, and both boot orders failed
// silently: minted against a hostname, the authority produces node certificates
// carrying a deployment no node can parse and the plane refuses forever; minted
// by `billet ca issue` against the real id, this function refuses to start at
// all. Neither shows up on loopback, which is every local run.
//
// state.DeploymentID reads the file the rest of the process already read, so
// there is nothing to keep in step.
func serveNodeWire(
	ctx context.Context,
	cfg *config.Config,
	nodes *nodeplane.Plane, store nodeplane.LeaseStore, jit nodeplane.JITSource,
	revocations nodeplane.Revocations, enrollments nodeplane.Enrollments,
) (func(), error) {
	addr := cfg.Server.Listen
	loopback := nodeplane.LoopbackOnly(addr)

	deployment, err := state.DeploymentID(cfg.Server.StateDir)
	if err != nil {
		return nil, err
	}

	var handlerOpts []nodeplane.HandlerOption

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

		// SIGNED BY THE AUTHORITY THE FLEET STILL TRUSTS. During a rotation that
		// is the PREVIOUS one: a node that has not renewed yet knows only that, and
		// presenting a certificate from the new authority would make the control
		// plane unverifiable to it — over the very wire it would need to recover.
		// The server follows the fleet rather than leading it.
		serving, err := wirecert.ServingCA(cfg.Server.StateDir, deployment, ca)
		if err != nil {
			return nil, err
		}

		bundle, err := serving.IssueServer(hosts)
		if err != nil {
			return nil, err
		}

		// AND EVERY AUTHORITY IS TRUSTED FOR CLIENTS, so a node holding either an
		// old or a new certificate is recognised while the overlap runs.
		trust, err := wirecert.TrustBundle(cfg.Server.StateDir, ca)
		if err != nil {
			return nil, err
		}

		bundle.CAPEM = trust

		if tlsConf, err = wirecert.ServerTLS(bundle); err != nil {
			return nil, err
		}

		if age, rotating := wirecert.RotationAge(cfg.Server.StateDir); rotating {
			slog.Default().Warn("a certificate authority rotation is running; nodes adopt the "+
				"new one as they renew, and `billet ca retire` finishes it once they all have",
				"started", age.Round(time.Hour))
		}

		handlerOpts = append(handlerOpts,
			nodeplane.RequireClientCert(),
			// A CREDENTIAL CAN BE TAKEN BACK, and it is checked on every request
			// rather than only at registration: a node holds one long poll open for
			// the better part of a minute and re-registers rarely.
			nodeplane.WithRevocations(revocations),
			// AND RENEWED BEFORE IT EXPIRES, by the node itself. Without this a
			// fleet enrolled on one afternoon expires on one afternoon a year later.
			nodeplane.WithRenewal(ca),
			nodeplane.WithTrustBundle(trust),
			// AND A WAY IN FOR A MACHINE THAT HAS NOTHING YET. Asking grants
			// nothing: the request waits until an operator compares its fingerprint
			// against what the node printed.
			nodeplane.WithEnrollment(enrollments))

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

// serverHostname is the name a node checks its control plane's certificate
// against.
//
// Taken from node.server_addr, which is the address the node actually dials, so
// the certificate is verified against the thing that was reached rather than
// against whatever the certificate happens to claim.
func serverHostname(addr string) (string, error) {
	s := addr
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("node.server_addr %q is not an address billet can dial: %w", addr, err)
	}

	if u.Hostname() == "" {
		return "", fmt.Errorf("node.server_addr %q names no host, so there is nothing to verify "+
			"the control plane's certificate against", addr)
	}

	return u.Hostname(), nil
}

// newProvider builds the compute backend this host runs.
//
// docker, ec2 and firecracker exist. tart needs Apple Silicon and a licence
// carve-out — each is a separate implementation of the same interface, and it is
// refused explicitly rather than falling through to something that happens to
// compile.
func newProvider(cfg *config.Config, deployment string) (provider.Provider, error) {
	switch cfg.Node.Provider {
	case config.ProviderDocker:
		// Labelled with the DEPLOYMENT id, not the node name. Two billets on one
		// machine share a hostname — and therefore a default node name — while
		// keeping separate state directories, so a node-name label would let each
		// enumerate the other's containers and destroy them as orphans.
		return docker.New(deployment, docker.WithLogger(slog.Default())), nil

	case config.ProviderEC2:
		// Config validation is what guarantees this is non-nil, and the constructor
		// refuses an empty deployment identity for the same reason docker is
		// labelled with one: instances are TAGGED with it, and List feeds a loop
		// that terminates, so two installations sharing a tag is a way for one to
		// destroy the other's live jobs.
		if cfg.Node.EC2 == nil {
			return nil, errors.New("billet: node.ec2 is missing; the provider is ec2")
		}

		return ec2.New(deployment, *cfg.Node.EC2, ec2.WithLogger(slog.Default()))

	case config.ProviderFirecracker:
		// BOTH BLOCKS ARE GUARANTEED BY VALIDATION — node.ceph is required for this
		// backend and refused for every other, and node.firecracker likewise — so
		// these are the guards that keep that true rather than cases that happen.
		if cfg.Node.Firecracker == nil {
			return nil, errors.New("billet: node.firecracker is missing; the provider is firecracker")
		}

		if cfg.Node.Ceph == nil {
			return nil, errors.New("billet: node.ceph is missing; every guest boots from a clone " +
				"of a golden image in the site's cluster")
		}

		// THE STORAGE IS BUILT HERE AND HANDED IN, because a provider and a store
		// are siblings that may not import each other. This is the one place that
		// knows about both.
		store, err := ceph.New(*cfg.Node.Ceph)
		if err != nil {
			return nil, err
		}

		return firecracker.New(deployment, *cfg.Node.Firecracker, store,
			firecracker.WithLogger(slog.Default()))

	case config.ProviderTart:
		return nil, fmt.Errorf("%w: the tart provider is not built yet; billet currently runs the "+
			"firecracker backend on bare metal, the ec2 backend, which launches one instance per "+
			"job, and the docker backend, which shares the host kernel and is for trials",
			errNotImplemented)

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

	// THE CERTIFICATE IS THE NAME, and an absent node.name is filled in from it
	// rather than defaulted from the hostname. The control plane authorises by
	// this name, so a machine whose hostname differs from it would otherwise be
	// refused for a value the operator never chose.
	if cfg.Node.Name == "" {
		cfg.Node.Name = name

		return &bundle, nil
	}

	if name != cfg.Node.Name {
		return nil, fmt.Errorf(
			"node.name is %q but %s was issued for %q; the control plane authorises by the "+
				"name in the certificate, so this node could only ever act as %q. Remove "+
				"node.name to take it from the certificate",
			cfg.Node.Name, cfg.Node.TLS.CertPath, name, name)
	}

	return &bundle, nil
}

func cmdNode(ctx context.Context, lc *lifecycle, args []string) error {
	fs := newFlagSet("billet node")
	cfgPath := addConfigFlag(fs)
	enroll := fs.Bool("enroll", false,
		"ask the control plane to admit this machine, then wait for an operator to approve it")
	caFingerprint := fs.String("ca-fingerprint", "",
		"the control plane's CA fingerprint, from `billet ca show` (required with --enroll)")
	joinToken := fs.String("join-token", "",
		"a short-lived token from `billet ca token` (required with --enroll)")

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

	// BEFORE ANYTHING ELSE, because enrolling is what produces the bundle
	// everything below reads.
	if *enroll {
		return enrollNode(ctx, cfg, *caFingerprint, *joinToken)
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

	// A ROTATING IDENTITY, so a renewal takes effect without a restart. The
	// callback is answered per handshake, which is what makes it safe to replace
	// the certificate under a node holding long-lived connections.
	var (
		tlsConf  *tls.Config
		identity *wirecert.Rotating
	)

	if bundle != nil {
		identity, err = wirecert.NewRotating(
			cfg.Node.TLS.CertPath, cfg.Node.TLS.KeyPath, cfg.Node.TLS.CAPath)
		if err != nil {
			return err
		}

		// SAID OUT LOUD, because nothing is broken and something did go wrong. A
		// renewal was interrupted partway through installing itself and this node
		// came back on the generation it was replacing — which still works, and
		// which is closer to expiry than the one that did not land.
		if stale := identity.StaleCopies(); stale != nil {
			slog.Default().Warn("a superseded certificate generation could not be removed, so a "+
				"second copy of this node's private key is still on disk; delete it",
				"error", stale)
		}

		if identity.RolledBack() {
			slog.Default().Warn("a certificate renewal was interrupted before it finished "+
				"installing; this node is running on the one it replaced and will try again",
				"expires", identity.Leaf().NotAfter.Format(time.DateOnly))
		}

		host, hostErr := serverHostname(cfg.Node.ServerAddr)
		if hostErr != nil {
			return hostErr
		}

		tlsConf = identity.ClientTLS(host)
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
	runner := node.New(client, cfg.Node.Name, client, p, slog.Default(),
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
		Identity:     identity,
		SweepEvery:   5 * time.Minute,
		DrainTimeout: drainTimeout,
		// The second signal, reaching the wait that honours it.
		Hurry: lc.hurry,
	})
}

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

	// "Delete everything" is NEVER the default for a destructive command. An omitted
	// --tier is indistinguishable from `--tier "$TIER"` with TIER unset, so a script
	// with an empty variable would delete every scale set in the org while looking
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
func cmdCA(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet ca issue <node> [--out <dir>] | billet ca token | " +
			"billet ca rotate | billet ca retire | billet ca revoke <node> | " +
			"billet ca revocations | billet ca show")
	}

	switch args[0] {
	case "issue":
		return cmdCAIssue(ctx, args[1:])
	case "revoke":
		return cmdCARevoke(ctx, args[1:])
	case "revocations":
		return cmdCARevocations(ctx, args[1:])
	case "token":
		return cmdCAToken(ctx, args[1:])
	case "rotate":
		return cmdCARotate(args[1:])
	case "retire":
		return cmdCARetire(args[1:])
	case "show":
		return cmdCAShow(args[1:])
	}

	return fmt.Errorf(
		"unknown ca command %q; try issue, token, rotate, retire, revoke, revocations or show",
		args[0])
}

// cmdCARevoke withdraws a node's certificate.
//
// BY SERIAL, taken from the bundle the operator issued. A name would be the
// obvious handle and is the wrong one: a name is legitimately re-issued to a
// replacement machine, so revoking it would refuse the replacement too. The
// serial names the one credential being taken back.
//
// WRITES TO THE LEDGER, so it takes effect on the next request the revoked host
// makes rather than at the next restart of anything.
func cmdCARevoke(ctx context.Context, args []string) error {
	fs := newFlagSet("billet ca revoke")
	cfgPath := addConfigFlag(fs)
	certPath := fs.String("cert", "", "the certificate to revoke (default <node>-billet-tls/node.crt)")
	reason := fs.String("reason", "", "why, recorded alongside it")

	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("revoking is done on the control plane, and this config has no server section")
	}

	path := *certPath
	if path == "" {
		path = filepath.Join(name+"-billet-tls", "node.crt")
	}

	serial, err := serialFromCert(path)
	if err != nil {
		return err
	}

	// Revoking matters most while the control plane is UP, so it must not need
	// the directory lock that plane is holding. See state.OpenAdmin.
	db, err := state.OpenAdmin(ctx, cfg.Server.StateDir)
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

	if err := allocator.RevokeCert(ctx, serial, name, *reason); err != nil {
		return err
	}

	fmt.Printf("Revoked %s (node %s)\n", serial, name)
	fmt.Printf("\nIt is refused on the next request that certificate makes. Issue a replacement\n")
	fmt.Printf("with `billet ca issue %s` if the machine is coming back.\n", name)

	return nil
}

// cmdCARevocations lists what has been withdrawn.
func cmdCARevocations(ctx context.Context, args []string) error {
	fs := newFlagSet("billet ca revocations")
	cfgPath := addConfigFlag(fs)

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("the revocation list lives on the control plane, and this config has no server section")
	}

	db, err := state.OpenAdmin(ctx, cfg.Server.StateDir)
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

	revoked, err := allocator.RevokedCerts(ctx)
	if err != nil {
		return err
	}

	if len(revoked) == 0 {
		fmt.Println("No certificates have been revoked.")

		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERIAL\tNODE\tREVOKED\tREASON")

	for _, r := range revoked {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Serial, r.Node, r.RevokedAt, r.Reason)
	}

	return w.Flush()
}

// serialFromCert reads the serial out of a PEM certificate on disk.
func serialFromCert(path string) (string, error) {
	// The path is the operator's own argument on their own machine, naming a
	// certificate they issued. There is no boundary here to cross: `billet ca
	// revoke` is already a command that writes to the deployment's ledger.
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s is not a PEM certificate", path)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}

	return wirecert.Serial(cert), nil
}

func cmdCAIssue(ctx context.Context, args []string) error {
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

	// RECORDED BEFORE IT IS WRITTEN DOWN, and fatal if it fails.
	//
	// Two facts go in: the admission trail, so a fleet built by issuing directly
	// is visible to the same list that shows what is waiting, and the SERIAL,
	// without which this credential can never be revoked. Handing an operator a
	// bundle billet cannot take back is worse than handing them an error, and the
	// error is recoverable — nothing has been written yet, so re-running is safe.
	if err := recordIssued(ctx, *cfgPath, name, bundle); err != nil {
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
	fmt.Printf("  fingerprint  %s\n\n", wirecert.Fingerprint(mustSPKI(bundle)))
	fmt.Print("Copy that directory to the node, then point its config at the files:\n\n")
	fmt.Printf("  node:\n    tls:\n      cert: /etc/billet/tls/node.crt\n" +
		"      key:  /etc/billet/tls/node.key\n      ca:   /etc/billet/tls/ca.crt\n\n")
	fmt.Print("node.name comes from the certificate, so it does not have to be written.\n")
	fmt.Print("node.key is a private key: keep it 0600 and do not copy it anywhere else.\n")

	return nil
}

// mustSPKI is the bundle's public key bytes, or nothing if it cannot be read.
// Used only for display beside a bundle that has already been written.
func mustSPKI(b wirecert.Bundle) []byte {
	leaf, err := wirecert.LeafOf(b)
	if err != nil {
		return nil
	}

	return leaf.RawSubjectPublicKeyInfo
}

// recordIssued writes a directly-issued certificate into the admission trail.
func recordIssued(ctx context.Context, cfgPath, name string, bundle wirecert.Bundle) error {
	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		return fmt.Errorf("read back the certificate just issued to %s: %w", name, err)
	}

	a, closeDB, err := controlPlaneAllocator(ctx, cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	if err := recordIssuedCert(ctx, a, bundle, name, alloc.CertIssued); err != nil {
		return err
	}

	displaced, err := a.RecordIssued(ctx, name, wirecert.FingerprintOfCert(leaf), string(bundle.CertPEM))
	if err != nil {
		return err
	}

	// SAID OUT LOUD, because this is the one path that can quietly retire a
	// fingerprint an operator has already compared and trusted.
	if displaced != "" {
		fmt.Printf("\nNOTE: %s was already admitted as %s.\nThat key can no longer be used "+
			"under this name; revoke its certificate if the machine still holds it.\n",
			name, displaced)
	}

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

	fmt.Printf("deployment  %s\nauthority   %s\nexpires     %s\nfingerprint %s\n",
		deployment, wirecert.CADir(cfg.Server.StateDir), ca.NotAfter().Format(time.RFC3339),
		ca.Fingerprint())

	if left, capping := ca.Capping(); capping {
		fmt.Printf("\nWARNING: this authority expires in %s, which is less than a certificate's\n",
			left.Round(24*time.Hour))
		fmt.Printf("full life, so every certificate it issues from now on is SHORTER than the\n")
		fmt.Printf("last — and when it expires, every node stops at once. Nothing will error\n")
		fmt.Printf("before that. Plan a rotation: issue a new authority, let nodes pick it up\n")
		fmt.Printf("through renewal while both are trusted, then retire the old one.\n")
	}

	fmt.Printf("\nGive the fingerprint to a node that is enrolling, so it can tell this control\n")
	fmt.Printf("plane from anything else that answers:\n\n")
	fmt.Printf("  billet node --enroll --ca-fingerprint %s\n", ca.Fingerprint())

	return nil
}

// cmdCheck is the explicit "is this deployment sane" command.
//
// It opens the ledger through OpenAdmin, so it answers WHILE the control plane
// is running — which is exactly when an operator reaches for it, and when
// opening exclusively made it useless. It still migrates when nothing holds the
// directory, so a first run sets the schema up; against a live plane it verifies
// and refuses rather than upgrading a schema that plane is using.
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

		// `billet check` is the command an operator reaches for WHEN SOMETHING IS
		// WRONG, which is exactly when the server is running. Opening the ledger
		// exclusively made it unusable at that moment.
		db, err := state.OpenAdmin(ctx, cfg.Server.StateDir)
		if err != nil {
			return fmt.Errorf("server state: %w", err)
		}
		defer db.Close()

		// ASKED FOR EXPLICITLY, because opening no longer does it. The scan reads
		// the whole file, so every other operator command skips it — but proving
		// the deployment is sane is what THIS command is for, and a ledger that
		// fails it is the thing an operator most needs to be told about.
		if err := db.IntegrityCheck(ctx); err != nil {
			return fmt.Errorf("server state: %w", err)
		}

		fmt.Printf("state    %s (ok, integrity verified)\n", cfg.Server.StateDir)
	}

	if cfg.Node != nil {
		fmt.Printf("node     %s via %s -> %s\n", cfg.Node.Name, cfg.Node.Provider, cfg.Node.ServerAddr)

		// Config validation proves the ec2 block is COHERENT. It cannot prove this
		// machine can act on it, and the difference is a deployment that validates
		// and then fails on the first job of the day.
		if cfg.Node.Provider == config.ProviderEC2 && cfg.Node.EC2 != nil {
			if err := checkEC2Credentials(ctx, cfg.Node.EC2); err != nil {
				return err
			}
		}

		if cfg.Node.Ceph != nil {
			if err := checkCephCluster(ctx, cfg.Node.Ceph); err != nil {
				return err
			}
		}

		// AFTER THE STORAGE, because the storage belongs to the SITE and this belongs
		// to the machine: a cluster a host cannot reach is wrong for every node there,
		// while a missing /dev/kvm is wrong for this one. An operator reading a wall
		// of output acts on the first thing it names, and the shared fault is the more
		// useful one to name first.
		if cfg.Node.Provider == config.ProviderFirecracker && cfg.Node.Firecracker != nil {
			if err := checkFirecrackerHost(ctx, cfg); err != nil {
				return err
			}
		}
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

// checkEC2Credentials proves this machine can act on its ec2 configuration.
//
// THE SAME DISTINCTION checkPrivateKey MAKES, one credential over: config
// validation proves the block is coherent, and coherence is not the question an
// operator running `billet check` is asking. A node whose credentials do not
// resolve validates perfectly and then fails on the first job of the day, with a
// 403 that names neither the missing environment variable nor the absent instance
// role.
//
// It costs a link-local request on a machine with no AWS environment variables,
// bounded by the metadata client's own short timeout, because the common failure
// is that this is not an EC2 instance at all.
func checkEC2Credentials(ctx context.Context, cfg *config.EC2Config) error {
	creds, err := ec2.DefaultCredentials().Credentials(ctx)
	if err != nil {
		return fmt.Errorf("node.ec2: this host cannot resolve aws credentials, so it could not "+
			"launch anything: %w", err)
	}

	// RESOLVING IS NOT WORKING, so the credentials are then USED. One read-only
	// DescribeInstances proves the region and endpoint answer and that this
	// identity is permitted to ask — which is the difference between a config that
	// parses and a node that can do its job, and the same distinction
	// checkPrivateKey makes by parsing the key rather than stat-ing it.
	//
	// The same credentials that were just reported, so what is proved is what was
	// named rather than whatever a second resolution might return.
	if err := ec2.CheckReachable(ctx, *cfg,
		ec2.WithCredentials(ec2.StaticCredentials(creds))); err != nil {
		return fmt.Errorf("node.ec2: credentials for %s resolved but could not call the ec2 api "+
			"in %s: %w", creds.AccessKeyID, cfg.Region, err)
	}

	// THE ACCESS KEY ID AND NOTHING ELSE. It is an identifier rather than a
	// secret, and printing it is the difference between "billet is using the wrong
	// role" and an operator staring at a working config. The secret and the
	// session token are never rendered anywhere.
	fmt.Printf("aws      %s in %s, subnet %s, %d instance shape(s), credentials %s (can describe)\n",
		spotLabel(cfg.Spot), cfg.Region, cfg.SubnetID, len(cfg.InstanceTypes), creds.AccessKeyID)

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. A read-only call says
	// nothing about permission to LAUNCH, and an operator who reads "ok" and then
	// watches every job fail on an IAM denial has been misled by this line.
	fmt.Printf("         (describe only — launching also needs at least ec2:RunInstances, " +
		"ec2:TerminateInstances, ec2:CreateTags and ec2:DescribeImages, plus iam:PassRole " +
		"if node.ec2.instance_profile is set)\n")

	// SAID OUT LOUD RATHER THAN INFERRED FROM AN ABSENT KEY. A deployment that
	// expected to run fork pull requests on rented machines and finds them queuing
	// forever has no other way to see why.
	if len(cfg.UntrustedSecurityGroupIDs) == 0 {
		fmt.Printf("         untrusted work will be refused: no untrusted_security_group_ids\n")
	}

	return nil
}

// checkFirecrackerHost proves this machine can act on its microVM configuration.
//
// THE SAME DISTINCTION checkPrivateKey AND checkEC2Credentials MAKE. Config
// validation proves the block is coherent; it cannot prove firecracker is
// installed, that /dev/kvm can be opened, that the jail account exists or that the
// bridge does. A node that is wrong about any of those validates perfectly and
// then fails on the first job of the day.
//
// FATAL, because only a firecracker node reaches here, so this file describes a
// machine that is meant to run jobs and cannot. Reporting it and exiting zero would
// make `billet check` say a host is fine when nothing on it can launch.
func checkFirecrackerHost(ctx context.Context, cfg *config.Config) error {
	// A PROVIDER BUILT PURELY TO ASK, so the preflight exercises the constructor an
	// operator's node will use — including the two rules that are easiest to get
	// wrong and invisible afterwards: which directory the jailer will name after
	// this binary, and whether a socket under it would fit in a unix address.
	//
	// The storage is not consulted here; checkCephCluster does that on its own, and
	// a nil disk would make this refuse for the wrong reason.
	p, err := firecracker.New(deploymentForCheck, *cfg.Node.Firecracker, noRootDisk{})
	if err != nil {
		return err
	}

	report, err := p.CheckHost(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("microvm  %s, %s\n", report.Firecracker, report.Jailer)
	fmt.Printf("         jails in %s, one uid per guest from %d (%d available)\n",
		report.JailDir, report.JailUIDMin, report.JailUIDCount)

	untrusted := "untrusted work will be refused: no untrusted_bridge"
	if report.UntrustedBridge != "" {
		untrusted = "untrusted work runs on " + report.UntrustedBridge
	}

	fmt.Printf("         guests on %s; %s\n", report.Bridge, untrusted)

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. Opening /dev/kvm says
	// nothing about the jailer's ability to chroot, mknod or place a cgroup, all of
	// which need root — and an operator who reads "ok" and then watches every
	// launch fail has been misled by this line.
	fmt.Printf("         (read only — launching also needs root, to chroot, to create the root " +
		"disk's device node inside the jail, and to attach a tap to the bridge)\n")

	return nil
}

// deploymentForCheck identifies nothing. The provider requires a deployment because
// it marks the jails it creates with one, and this constructs a provider only to
// ask it questions about the host.
const deploymentForCheck = "billet-preflight"

// noRootDisk stands in for the storage a preflight does not use.
//
// The provider refuses a nil one — every guest boots from a clone, so a nil
// interface would panic on the first job — and `billet check` proves the cluster
// separately, through the ceph client, where the diagnostic is about storage rather
// than about microVMs.
type noRootDisk struct{}

func (noRootDisk) ResolveGeneration(_ context.Context, image string) (string, error) {
	return image, nil
}

func (noRootDisk) CloneRoot(context.Context, string, string) (string, error) {
	return "", errors.New("billet: the preflight does not clone a root disk")
}

func (noRootDisk) DiscardRoot(context.Context, string) error { return nil }

// KernelFor answers "nothing recorded", which is the truthful answer from a node
// with no cluster to have recorded anything in — and the caller treats it as the
// fallback case rather than an error.
func (noRootDisk) KernelFor(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// checkCephCluster proves this machine can act on its storage configuration.
//
// THE SAME DISTINCTION checkPrivateKey AND checkEC2Credentials MAKE, one backend
// over. Config validation proves the block is coherent; it cannot prove the
// monitors answer, the keyring authenticates, or the pools were ever created. A
// node that is wrong about any of those validates perfectly and then fails on the
// first job of the day, with a librados error naming none of them.
//
// A MISSING rbd IS FATAL, and the reason is which configs reach here. Only a
// firecracker node may carry a ceph block, so this file describes a machine that
// is meant to run jobs — and one without the client package cannot map a single
// volume. A control plane is not affected: with no node section there is nothing
// to check. Reporting it and exiting zero would make `billet check` say a host is
// fine when nothing on it can launch.
func checkCephCluster(ctx context.Context, cfg *config.CephConfig) error {
	client, err := ceph.New(*cfg)
	if err != nil {
		// WHAT THE CONFIG NAMES, and nothing the sentinel already says. Every
		// wrapper renders the message beneath it, so repeating the remedy here put
		// "install ceph-common" on the operator's terminal twice in one sentence.
		if errors.Is(err, ceph.ErrNoClient) {
			return fmt.Errorf("node.ceph names %s and %s, so this host is meant to run jobs and "+
				"cannot map a volume: %w", cfg.ImagePool, cfg.CachePool, err)
		}

		return err
	}

	// THE REPORT IS PRINTED EVEN WHEN THE CHECK FAILS, when there is one. A cluster
	// billet reached and then refused has told the operator something — which pools
	// it found, how they are replicated — and throwing that away because the last
	// question answered badly makes the diagnostic harder to act on, not easier.
	report, err := client.CheckReachable(ctx)
	if report.User != "" {
		printCephReport(cfg, report)
	}

	if err != nil {
		if errors.Is(err, ceph.ErrCloneV1) {
			return fmt.Errorf("node.ceph: %w", err)
		}

		// HONEST ABOUT WHAT FAILED, which is not always the pools. This wrapper said
		// "could not read the pools" for every failure, including ones where both
		// pools listed perfectly and it was a later cluster fact that billet could
		// not make sense of — telling an operator to go and look at the one thing
		// that worked. The inner error already names the step; this one says only
		// what the CLI knows, which is that the preflight did not finish.
		return fmt.Errorf("node.ceph: the ceph preflight did not complete, so billet cannot say "+
			"this host could launch anything: %w", err)
	}

	return nil
}

// printCephReport puts what the cluster said on the operator's terminal.
func printCephReport(cfg *config.CephConfig, report ceph.Report) {
	fmt.Printf("ceph     client.%s -> %s\n", report.User, cfg.ConfPathOrDefault())

	for _, p := range report.Pools {
		// THE REPLICATION IS SHOWN RATHER THAN JUDGED, with one exception below.
		// How many copies a pool keeps is the operator's decision and billet has no
		// standing to refuse it — but it is invisible from the config file, and an
		// operator who believes their golden images are mirrored deserves to find
		// out here rather than after a drive dies.
		replication := "replication unknown"
		if p.Size > 0 {
			replication = fmt.Sprintf("size %d, min_size %d", p.Size, p.MinSize)
		}

		// THE CLONE FORMAT ONLY WHEN SOMEBODY HAS OVERRIDDEN IT. `auto` is the
		// default and the common case, and a column that says `auto` on every line
		// of every healthy deployment is a column nobody reads.
		override := ""
		if p.CloneFormat != "" && p.CloneFormat != "auto" {
			override = fmt.Sprintf("  clone format forced to %s", p.CloneFormat)
		}

		fmt.Printf("         %-16s %3d image(s)  %-24s %s%s\n",
			p.Name, p.Images, replication, p.Purpose, override)
	}

	for _, p := range report.Pools {
		if p.Size == 1 {
			fmt.Printf("         %s keeps ONE copy: a single drive failure loses everything in "+
				"it\n", p.Name)
		}
	}

	if report.CloneV2 {
		// BOTH SETTINGS, because either can be the one making it true. A cluster on
		// luminous with rbd_default_clone_format forced to 2 clones the new way, and
		// printing only the release would have an operator reading "luminous" beside
		// "clone v2" with no way to see why they agree.
		fmt.Printf("         clone v2 (require-min-compat-client %s), so a cache generation can "+
			"be reclaimed while a job still holds a clone of it\n", report.MinCompatClient)
	}

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. Listing a pool proves the
	// monitors answered and the keyring authenticated; it proves nothing about
	// permission to create, clone or remove an image, which is what a launch does.
	fmt.Printf("         (read only — launching also needs create, clone, snapshot and remove in " +
		"both pools; `ceph auth get-or-create client.<user> mon 'profile rbd' osd 'profile rbd " +
		"pool=<images>, profile rbd pool=<cache>'` grants exactly that)\n")
}

// spotLabel names the market a node buys in, because it decides whether a build
// can be killed by somebody else.
func spotLabel(spot bool) string {
	if spot {
		return "spot (a reclaim fails the build; github does not requeue it)"
	}

	return "on-demand"
}
