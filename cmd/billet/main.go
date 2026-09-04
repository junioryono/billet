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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/provider/docker"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/provider/tart"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	storecontract "github.com/junioryono/billet/internal/store"
	"github.com/junioryono/billet/internal/store/ceph"
	"github.com/junioryono/billet/internal/store/ebss3"
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
	if coded, ok := errors.AsType[*exitError](err); ok {
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
		{"cache", "manage transparent Actions caching and install its conformance gate", cmdCache},
		{"check", "validate the config and state directory, then exit", cmdCheck},
		{"init", "generate a billet.yaml interactively", cmdInit},
		{"ami", "build and verify the machine image the ec2 backend launches", cmdAMI},
		{"runner", "report how close the pinned actions/runner is to being refused", cmdRunner},
		{"images", "verify the golden image a microVM guest boots from", cmdImages},
		{"github-app", "create and install the GitHub App billet uses", cmdGitHubApp},
		{"teardown", "delete the scale sets billet created on GitHub", cmdTeardown},
		{"decommission", "remove the ec2 instances and cache billet made outside Terraform",
			cmdDecommission},
		{"local", "run the billet services on this machine, and back up or restore what makes " +
			"them this deployment", cmdLocal},
		{"drain", "stop admitting new work and let what is running finish", cmdDrain},
		{"resume", "start admitting work again after a drain", cmdResume},
		{"force-destroy", "DESTROY compute that is still running a job, failing those builds",
			cmdForceDestroy},
		{"rollout", "move this whole deployment to one release, and watch it converge",
			cmdRollout},
		{"host-upgrade", "replace billet on THIS machine transactionally, with rollback",
			cmdHostUpgrade},
		{"release", "record which signed manifest produced the billet installed here",
			cmdRelease},
		{"acceptance", "stand an ISOLATED deployment up beside this one, run a real job on " +
			"it, and destroy exactly what it made", cmdAcceptance},
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
	upgradeProbe := fs.Bool("upgrade-probe", false,
		"open candidate state without polling, dispatching, or accepting workload")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *dryRun && *upgradeProbe {
		return errors.New("billet server: --dry-run and --upgrade-probe are mutually exclusive")
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

	return runServer(ctx, lc, cfg, *dryRun, *upgradeProbe)
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
		return state.DeploymentID(cfg.Server.IdentityDir)
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

func runServer(
	ctx context.Context,
	lc *lifecycle,
	cfg *config.Config,
	dryRun, upgradeProbe bool,
) error {
	// Built by the SHARED constructor, so the server and teardown authenticate
	// identically. Two near-identical constructions is how one of them ends up
	// pointed at a different organization than the other.
	client, err := newScaleSetClient(ctx, cfg)
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
	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	// A STANDBY OPENS A HANDLE THAT CANNOT WRITE, which is what makes "does
	// nothing authoritative before promotion" a property of the store rather than
	// a rule this function has to keep. See state.OpenPostgresStandby.
	standby := !upgradeProbe && cfg.Server.Controllers == config.ControllersActivePassive

	var db *state.DB

	switch {
	case upgradeProbe:
		db, err = openStateMaintenance(ctx, cfg)
	case standby:
		db, err = openStateStandby(ctx, cfg)
	default:
		db, err = openState(ctx, cfg)
	}

	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	// BUILT BEFORE THE CLAIM, DELIBERATELY, because it validates the CONFIG and
	// reaches the ledger for nothing. A tier the catalogue refuses, a missing
	// ceiling or a host policy that contradicts itself should stop this process at
	// startup rather than at a failover, which is the one moment nobody wants to
	// discover a config error.
	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers, alloc.WithPlacement(cfg.Server.Placement))
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}
	if upgradeProbe {
		if err := notifyReady(); err != nil {
			return fmt.Errorf("server upgrade-probe readiness: %w", err)
		}
		fmt.Println("billet server: upgrade probe ready; workload polling and dispatch are disabled")
		<-ctx.Done()

		return nil
	}

	// Ctrl-C and SIGTERM stop the listeners through the context, which is what
	// releases escrowed capacity — see the listener's deferred release. A hard
	// kill skips that and leaves the reaper to expire it.
	//
	// AND IT IS INSTALLED BEFORE THE CLAIM, because a standby may wait here for
	// days and an operator stopping one must not have to kill it.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// THE CONTROLLER CLAIM, BEFORE ANYTHING POLLS GITHUB OR DISPATCHES A NODE.
	//
	// The exclusive lock on the state directory excludes a second control plane
	// on THIS HOST, which is the whole of the problem while the ledger is a file
	// and half of it once the ledger is a database two machines can reach. This
	// is the other half, and its position is the point: a claim taken after the
	// listeners are up is a claim taken after two controllers have both admitted
	// work.
	//
	// EVERYTHING AUTHORITATIVE IS BELOW THIS LINE, and that is the whole of the
	// standby design. There is no second implementation of a control plane: a
	// standby is this same function, stopped here until it can go on.
	if err := becomeController(ctx, cfg, db, deployment, standby); err != nil {
		return err
	}

	// AND A LOST CLAIM STOPS THEM THE SAME WAY, WHICH IS THE HALF THAT REFUSING A
	// WRITE DOES NOT DO. See stopWhenReplaced.
	go stopWhenReplaced(ctx, db.LeadershipLostSignal(), stop, slog.Default())

	// THE DEPLOYMENT'S AUTHORITY, BEFORE ANYTHING READS ONE.
	//
	// serveNodeWire's single authority read goes through LoadOrCreateCA, which
	// CREATES one when the directory is empty. On a promoted standby that has
	// never held this deployment's CA that would mint a RIVAL authority, after
	// which every node in the fleet fails to verify the control plane and drops
	// off at once — while the control plane itself looks perfectly healthy. This
	// is what makes a failover a failover rather than an outage.
	//
	// A NO-OP unless this deployment keeps its identity in a store.
	if err := adoptSharedAuthority(ctx, cfg, deployment, slog.Default()); err != nil {
		return fmt.Errorf("node-wire authority: %w", err)
	}

	// Everything billet.yaml says about the control plane, assembled in one place
	// inside the server package so the config-to-listener chain is testable
	// without spanning two packages. Whatever this command adds below is about
	// how it was INVOKED — a flag, a co-resident node — not about the file.
	opts, err := server.OptionsFromConfig(cfg)
	if err != nil {
		return err
	}

	// The second signal, reaching the drain that honours it.
	//
	// AND THE FENCE, REACHING THE TEARDOWN THAT HONOURS IT. `db.LeadershipLost`
	// latches inside the write transaction the ledger refused, so by the time any
	// listener is unwinding it is already true — which is what lets every one of
	// them stop without destroying compute, closing a session or handing capacity
	// back to a deployment that is no longer theirs.
	opts = append(opts, server.WithHurry(lc.hurry), server.WithLeadershipLost(db.LeadershipLost),
		server.WithCompletionLedger(db), server.WithOrganization(cfg.GitHub.Org))

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
		nodeplane.WithSites(cfg.Sites),
		// The catalogue lives here, and a launch carries the shape a node needs, so
		// no node keeps a copy that can drift from this one.
		nodeplane.WithTierCatalog(cfg.Tiers),
		// The durable half of a compute barrier. `billet drain` and
		// `billet local down` write a request into the ledger; this is what
		// observes it, because a sealed idle deployment dispatches nothing at all
		// and there is no other moment at which the fleet would be asked.
		nodeplane.WithBarrierStore(allocator))

	wire, err := serveNodeWire(ctx, cfg, nodes, allocator,
		wiring.NodeJIT{Client: client}, allocator, allocator, db)
	if err != nil {
		return err
	}

	defer wire.stop()

	// AND THE AUTHORITY GOES INTO THE STORE, now that there certainly is one.
	//
	// AFTER THE WIRE RATHER THAN BEFORE IT, because on a first controller the wire
	// is what creates the authority: publishing earlier would publish nothing. On
	// every later start the bytes are identical and this is a write nobody sees.
	//
	// NOT FATAL, and reported rather than swallowed: the control plane is serving
	// by now, and a store that cannot be written is a reason to look at IAM rather
	// than to take a working deployment offline. What it costs is that the other
	// controller has nothing to adopt.
	publishSharedAuthority(ctx, cfg, deployment, slog.Default())

	// A TIMER, BECAUSE NOTHING ELSE ASKS. A node's liveness now decides what its
	// tier advertises, and an idle deployment never launches, lists or destroys —
	// so without this a host that crashed on a quiet afternoon would keep its
	// capacity advertised until somebody happened to need it.
	go nodes.Watch(ctx)

	// AND A SECOND ONE, for the same reason. A drain asks the fleet what it is
	// running through a durable request row, because the command that wants the
	// answer runs in another process; nothing on a sealed, idle deployment would
	// otherwise ever put that question to a node.
	go nodes.BarrierLoop(ctx)

	// THE REMOTE PLANE DRIVES ALL COMPUTE, and it is the only thing that can. A
	// control plane without it serves the node wire, accepts registrations, and then
	// never sends a single command.
	planeRunner := nodes.NewRunner()

	// AND THE ROLLOUT COORDINATOR, which is what makes `billet rollout start` mean
	// anything: without it the decision is a durable record nobody acts on, so
	// every rollout stays open forever and blocks the next one.
	//
	// GIVEN THE NODE PLANE'S RUNNER, because that is the only thing that can reach
	// a host. It is wired here rather than inside server.New for the same reason
	// the runner is: what a control plane can talk to is a property of how this
	// process was assembled, not of the scheduler.
	coordinator := rollout.NewCoordinator(
		rollout.New(db),
		ledgerFleet{alloc: allocator},
		planeDispatcher{runner: planeRunner},
		version.Version(),
		nodeapi.VersionNodeUpgrade,
		rollout.WithCoordinatorLogger(slog.Default()),
	)

	// AND THE STARTER, which is what makes `release.automatic` true: the
	// coordinator converges a rollout that exists, and this is what makes one
	// exist when the channel advances. It resolves the channel through the same
	// functions `billet rollout start` does, so the two cannot disagree about
	// what a target is.
	starter, err := newRolloutStarter(cfg, rollout.New(db), ledgerFleet{alloc: allocator},
		releasesource.Host(version.Version(),
			releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
			state.LatestSchemaVersion(), firecracker.GuestContract))
	if err != nil {
		return err
	}

	opts = append(opts,
		server.WithNodeRunner(planeRunner),
		server.WithRolloutCoordinator(coordinator, 0),
		server.WithRolloutStarter(starter, 0),
		// AND THE SWEEP OF STAGED CODEBUILD REGISTRATIONS a dead node never reaped.
		// Wired here because it needs what only this process has: the ledger, which
		// is the sole authority for deleting one, and the host's AWS credentials —
		// the same chain the backup upload uses. Which paths it sweeps comes from
		// the fleet's registrations, so a deployment with no codebuild node sweeps
		// nothing and resolves no credential.
		server.WithStagedCredentialSweeper(
			newControllerCredentialSweep(allocator, db, awscreds.Default(), slog.Default())),
	)

	plane := server.New(allocator, wiring.Provisioner{Client: client}, cfg.Tiers, owner, slog.Default(), opts...)

	// READINESS IS REPORTED BEFORE THE LISTENERS OPEN THEIR SESSIONS, AND MOVING IT
	// AFTER THEM WOULD BE A RESTART LOOP.
	//
	// A tier's session can now be held by a control plane that was killed rather
	// than stopped, and GitHub does not hand one over: server.openSession waits for
	// GitHub to expire it, which takes as long as it takes. The unit is
	// Type=notify with TimeoutStartSec=120 and Restart=on-failure, so withholding
	// READY=1 until every session is open means systemd kills billet at two
	// minutes, restarts it, and it waits again — forever, because nothing about
	// restarting makes a remote session expire sooner. That is strictly worse than
	// a control plane that is up: the node wire would go down with it on every
	// cycle, and with it the registrations and heartbeats that hold running
	// compute.
	//
	// SO READINESS MEANS "THIS PROCESS IS SERVING", WHICH IS TRUE. The node wire is
	// listening, the reaper is running, and every tier whose session opened is
	// polling. A tier still waiting says so in the journal every thirty seconds,
	// which is what `systemctl status` shows — the absence is visible where an
	// operator looks, rather than being converted into a unit that cannot start.
	if err := notifyReady(); err != nil {
		return fmt.Errorf("server readiness: %w", err)
	}
	// A LOST LEADERSHIP EXITS NON-ZERO, AND THE RESTART THAT FOLLOWS IS THE POINT
	// RATHER THAN A LOOP TO BE AVOIDED.
	//
	// The packaged unit is Restart=on-failure, so systemd starts this process
	// again and ClaimController either takes the deployment back — which is
	// exactly right when the successor was itself transient, a session dropped by
	// a pooling proxy or a database failover — or is refused with ErrControllerHeld
	// naming the holder and its epoch. That refusal repeating every RestartSec is
	// a real misconfiguration being reported, and it is the reason billet does not
	// exit 0 here: a clean exit would leave a deployment whose partition has
	// healed with no controller at all, silently, which is the failure this whole
	// fence exists to make impossible.
	err = plane.Run(ctx)

	// ASKED BEFORE THE ERROR IS CLASSIFIED, and asked at all because the plane
	// stops through a CANCELLED CONTEXT here, which Run reports as a clean stop.
	// Returning nil would exit 0 — a control plane that was fenced out of its own
	// deployment reporting a successful shutdown, and systemd leaving it stopped.
	if db.LeadershipLost() {
		return fmt.Errorf("%w. Nothing running here was destroyed and no capacity was "+
			"handed back; the controller that replaced this one adopts both. If that "+
			"replacement was itself transient, restarting is how this host takes the "+
			"deployment back", state.ErrLeadershipLost)
	}

	if err != nil {
		return explainGitHubAccess(ctx, cfg, err)
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
//
// BOTH LISTENERS PRESENT ONE CERTIFICATE, so a bootstrap address bound to a
// different concrete host than the node wire contributes its own name. An
// enrolling node verifies this certificate by the hostname it dialled, right
// after the fingerprint check, so a missing subject name is a handshake failure
// during enrollment and nothing else. An explicit node_tls_hosts is taken as
// written: an operator naming the addresses their fleet uses is answering this
// question themselves, and the bootstrap name has to be among them.
func nodeTLSHosts(cfg *config.Config) ([]string, error) {
	if len(cfg.Server.NodeTLSHosts) > 0 {
		return cfg.Server.NodeTLSHosts, nil
	}

	host, err := dialledHost("server.listen", cfg.Server.Listen)
	if err != nil {
		return nil, err
	}

	hosts := []string{host}

	// A WILDCARD BOOTSTRAP ADDRESS IS NOT AN ERROR HERE. It says which interfaces
	// to accept enrollments on, and the node wire's own concrete host above is
	// already a name that reaches this machine — so there is nothing missing,
	// unlike the wildcard node wire, which leaves nothing at all.
	if bootstrap := strings.TrimSpace(cfg.Server.BootstrapListen); bootstrap != "" {
		if bootstrapHost, err := dialledHost("server.bootstrap_listen", bootstrap); err == nil &&
			bootstrapHost != host {
			hosts = append(hosts, bootstrapHost)
		}
	}

	return hosts, nil
}

// dialledHost is the concrete name in a listen address, or an error naming what
// to set instead.
func dialledHost(key, addr string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%s %q must be host:port: %w", key, addr, err)
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		return "", fmt.Errorf(
			"%s is %q, which accepts on every interface and so does not say what a "+
				"node will dial. Set server.node_tls_hosts to the names and addresses nodes use "+
				"for this control plane; they become the subject names of the certificate it "+
				"serves", key, addr)
	}

	return host, nil
}

// wireOption adjusts how serveNodeWire builds its listeners.
type wireOption func(*wireLimits)

// wireLimits are the two connection budgets, which are separate on purpose — an
// anonymous caller occupies the enrollment listener's and can never occupy the
// node wire's — plus how long a connection may take to prove itself.
type wireLimits struct {
	operational, bootstrap int
	handshake              time.Duration
}

// withConnectionLimits is for tests, which need both budgets SMALL AND EQUAL.
// At the production sizes a test cannot tell one shared budget from two: filling
// the enrollment listener's 64 leaves 448 of a shared 512, so the node wire is
// served either way and the assertion proves only that 64 is a bound.
func withConnectionLimits(operational, bootstrap int) wireOption {
	return func(l *wireLimits) { l.operational, l.bootstrap = operational, bootstrap }
}

// withHandshakeTimeout is for tests, which otherwise have to SLEEP past the
// production five seconds to observe anything about the bound. Several of them
// do, and measured, that is enough added wall clock to make an already
// load-sensitive package fail under a full -race run.
func withHandshakeTimeout(d time.Duration) wireOption {
	return func(l *wireLimits) { l.handshake = d }
}

// stopServing drains a listener, and stops it outright if draining runs out of
// time.
//
// SHUTDOWN ALONE IS NOT A STOP. It returns ctx.Err() on expiry and leaves every
// active connection and handler RUNNING, with their request contexts uncancelled
// — so an enrollment blocked in the ledger outlives serveNodeWire and is still
// using the database when runServer's `defer db.Close()` runs, one defer later.
// Close is what ends them.
//
// A FRESH CONTEXT, deliberately. This runs while the caller's context is already
// cancelled — that cancellation is what brought us here — so deriving from it
// would abort the drain instantly and cut the very connections this exists to
// let finish.
func stopServing(ctx context.Context, srv *http.Server, what string) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Default().Warn("the "+what+" did not drain in time; closing it, which ends any "+
			"request still in flight", "error", err)

		if err := srv.Close(); err != nil {
			slog.Default().Warn("the "+what+" did not close cleanly", "error", err)
		}
	}
}

// servedWire is what a running node wire reports about itself.
//
// THE ADDRESSES ARE RESOLVED, NOT CONFIGURED. A wildcard or a zero port in the
// config says what to bind, not what was bound, and the two listeners' real
// addresses are the only way to say anything about them afterwards.
type servedWire struct {
	stop func()
	// addr is the operational wire. bootstrap is the enrollment listener, empty
	// when this deployment does not enroll over the network.
	addr      string
	bootstrap string
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
// and requires one back — in the HANDSHAKE, so a caller with nothing to present
// never reaches billet's HTTP server and cannot occupy a connection an enrolled
// node needs. There is nothing to configure and nothing to install: the CA lives
// beside the state directory, and `billet ca issue <node>` produces the bundle a
// node is given.
//
// THAT IS WHY THERE ARE TWO LISTENERS. The two routes a machine with no
// certificate needs cannot require one, so they are served by serveBootstrapWire
// on server.bootstrap_listen, with a budget of its own; serveBootstrapWire says
// what sharing one budget cost.
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
	cachePolicy nodeplane.CachePolicy,
	opts ...wireOption,
) (*servedWire, error) {
	limits := wireLimits{
		operational: nodeWireConnectionLimit,
		bootstrap:   bootstrapConnectionLimit,
	}
	for _, opt := range opts {
		opt(&limits)
	}

	addr := cfg.Server.Listen
	loopback := nodeplane.LoopbackOnly(addr)

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return nil, err
	}

	var (
		hosts         []string
		stopBootstrap = func() {}
		bootstrapAddr string
		// startBootstrap opens the enrollment listener once the node wire holds
		// its own socket. Nil on a loopback wire, which has no certificates and
		// so nothing to enroll into.
		startBootstrap func() error
	)

	if !loopback {
		if hosts, err = nodeTLSHosts(cfg); err != nil {
			return nil, err
		}
	}

	// ASSEMBLED IN internal/wiring, NOT HERE, AND IT HANDS BACK THE HANDLER
	// RATHER THAN THE PIECES. What the server presents, what it accepts and what
	// renewal signs with are three answers that must describe one moment and one
	// authority — and this file is excluded from coverage and takes six
	// collaborators to stand up, so nothing here can be asserted. Returning the
	// options and installing them from this file was the first attempt and had
	// the same shape as the bug it fixed: deleting the line that installed them
	// left every test green.
	wire, err := wiring.BuildNodeWire(wiring.NodeWireRequest{
		StateDir:    cfg.Server.IdentityDir,
		Deployment:  deployment,
		Hosts:       hosts,
		Loopback:    loopback,
		Log:         slog.Default(),
		Plane:       nodes,
		Leases:      store,
		JIT:         jit,
		Revocations: revocations,
		Enrollments: enrollments,
		CachePolicy: cachePolicy,
	})
	if err != nil {
		return nil, err
	}

	if wire.Rotating {
		slog.Default().Warn("a certificate authority rotation is running; nodes adopt the "+
			"new one as they renew, and `billet ca retire` finishes it once they all have",
			"started", wire.RotationAge.Round(time.Hour))
	}

	if !loopback {
		slog.Default().Info("the node wire requires client certificates",
			"hosts", hosts, "ca_expires", wire.IssuingExpiry.Format(time.DateOnly))

		// THE ENROLLMENT SURFACE IS SOMEWHERE ELSE OR NOWHERE. The two routes a
		// machine with no certificate needs cannot live on the wire above, because
		// they would drag its connection budget down to whatever an anonymous
		// caller feels like taking.
		//
		// IT TAKES THE SAME ONE READ OF THE AUTHORITY, and that matters for the
		// same reason the read exists: what this listener PRESENTS and what it
		// hands an enrolling machine to TRUST have to describe one moment, or a
		// `billet ca retire` landing between them admits a node against an
		// authority the control plane has stopped presenting. Presents signs the
		// certificate both listeners serve; Issuing is what approvals are signed
		// by and what `billet ca show` reports; Trust is every authority a node
		// should accept while an overlap runs. BuildNodeWire hands all three back
		// from that one read, which is what makes "the same moment" a fact rather
		// than a convention two call sites have to keep.
		//
		// DEFERRED UNTIL THE NODE WIRE HAS ITS SOCKET, and that ordering is the
		// whole reason this is a closure rather than a call. Config validation
		// refuses two addresses that name one socket, but it cannot see every
		// overlap a host can produce — and whichever listener binds SECOND is the
		// one that reports the collision. Binding enrollment first therefore
		// reported an operator's mistake against server.listen, which was fine.
		startBootstrap = func() error {
			var err error

			stopBootstrap, bootstrapAddr, err = serveBootstrapWire(
				ctx, cfg, wire.Bootstrap, wire.Serving, limits.bootstrap, limits.handshake)

			return err
		}
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for nodes on %s: %w", addr, err)
	}

	if startBootstrap != nil {
		if err := startBootstrap(); err != nil {
			// The node wire's socket is open and nothing is serving it yet, so it
			// is this function's to close.
			_ = ln.Close()

			return nil, err
		}
	}

	// BOUNDED, AND CHARGED ONLY TO CALLERS THAT PROVED WHO THEY ARE. This budget
	// used to be taken in Accept, before the underlying accept and therefore
	// before the handshake — which cannot tell an enrolled node from a stranger,
	// because at that point nobody has presented anything. So the fleet's capacity
	// was spendable by whoever could open a socket, and while it was full Accept
	// blocked before the kernel accept and a node's connection waited in the
	// backlog until its own dial timeout. Moving the unauthenticated routes to
	// their own listener did not fix that half; this does.
	//
	// handshakingListener accepts unconditionally, bounds handshakes separately,
	// and charges this budget only for a connection that verified. What is
	// guaranteed is that anonymous traffic can never displace an admitted node;
	// what is best effort is a handshake slot, which no server can reserve for a
	// caller it has not yet identified.
	if wire.TLS != nil {
		ln = newHandshakingListener(ctx, ln, wire.TLS, limits.operational,
			handshakeBounds{handshakeFor: limits.handshake}, slog.Default())
	} else {
		// A LOOPBACK WIRE HAS NO HANDSHAKE TO WAIT FOR. There are no certificates
		// here at all — the trust boundary is the machine — so there is nothing to
		// charge after, and the simple bound is the honest one.
		ln = newLimitedListener(ln, limits.operational)
	}

	srv := &http.Server{
		Handler: wire.Handler,
		// A command poll is a LONG poll, so there is deliberately NO WriteTimeout:
		// one would cut every cycle. The rest are safe and are what stop a stalled
		// or hostile connection holding a slot.
		//
		// ReadTimeout bounds the REQUEST, never the response, so a long poll is
		// untouched -- it is what stops a body dribbled a byte at a time from
		// holding a connection for days, which ReadHeaderTimeout does not cover.
		//
		// IdleTimeout is 120s BECAUSE THE CLIENT'S OWN IdleConnTimeout IS 90s
		// (internal/nodeclient/client.go). Strictly longer, so it can only ever
		// reap a connection the client has already abandoned or that was never a
		// node; a shorter value would cut healthy keep-alives.
		//
		// The 90s is what the ONLY production construction gets: cmd/billet builds
		// its nodeclient with no HTTP client of its own, so it takes the package's
		// transport. That is the whole claim -- a caller that supplies its own
		// client can choose any idle it likes, and this timeout is sized against
		// the one billet ships rather than against every possible caller.
		//
		// ReadTimeout does not cut a long poll: measured, a handler holding the
		// response for 5s completes under a 2s ReadTimeout, and the connection is
		// still reusable afterwards. Go applies it to reading the request, not to
		// the handler.
		//
		// THE HANDSHAKE IS NOT BOUNDED HERE, and that is a change worth knowing
		// about. Go's Server.tlsHandshakeTimeout is the minimum of the POSITIVE
		// ReadHeaderTimeout, ReadTimeout and WriteTimeout — an emergent number,
		// which on this listener happened to be 10s and would have become
		// unlimited if the other two were ever zeroed. handshakingListener does
		// the handshake itself under an explicit handshakeTimeout before this
		// server ever sees the connection, so by the time http.Server re-checks,
		// the handshake is already complete and its own bound is a no-op.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("the node listener stopped", "error", err)
		}
	}()

	slog.Default().Info("serving the node wire", "addr", ln.Addr().String())

	return &servedWire{
		addr:      ln.Addr().String(),
		bootstrap: bootstrapAddr,
		stop: func() {
			// ENROLLMENT STOPS FIRST. It is the surface that admits callers with
			// nothing to present, so leaving it open while the node wire spends up
			// to five seconds draining would keep admitting machines to a
			// deployment that is going away — and its handlers are the ones
			// holding the ledger this process is about to close.
			stopBootstrap()
			stopServing(ctx, srv, "node listener")
		},
	}, nil
}

// serveBootstrapWire opens the listener a machine with no certificate dials.
//
// SEPARATE FROM THE NODE WIRE BECAUSE IT CANNOT AUTHENTICATE. Reading this
// deployment's authority and asking to join are the two things a machine must be
// able to do before it has anything to prove, so the listener in front of them
// admits strangers — and a listener that admits strangers must not share a
// connection budget with the fleet, or a few requests a second take every node
// offline. Saturating this one delays an enrollment instead.
//
// ABSENT IS THE ORDINARY ANSWER. Without server.bootstrap_listen this control
// plane simply does not enroll over the network, and `billet ca issue <node>`
// remains the way in: an operator mints the bundle here and copies it out of
// band, which is the right shape for a machine being provisioned anyway.
//
// The port is meant to be closable between enrollments — nothing that keeps a
// running fleet working goes through it.
func serveBootstrapWire(
	ctx context.Context,
	cfg *config.Config,
	handler http.Handler,
	bundle wirecert.Bundle,
	limit int,
	handshakeFor time.Duration,
) (func(), string, error) {
	addr := strings.TrimSpace(cfg.Server.BootstrapListen)
	if addr == "" {
		slog.Default().Info("this control plane does not enroll nodes over the network; "+
			"issue a bundle with `billet ca issue <node>` and copy it to the host, or set "+
			"server.bootstrap_listen to serve enrollment on an address of its own",
			"listen", cfg.Server.Listen)

		return func() {}, "", nil
	}

	// NO CLIENT CERTIFICATE IS ASKED FOR. Nobody reaching here has one, and not
	// asking means this listener never parses or verifies a chain a stranger
	// chose. What secures these routes is the fingerprint the operator compared,
	// the join token, and the approval that waits for a human.
	tlsConf, err := wirecert.BootstrapTLS(bundle)
	if err != nil {
		return nil, "", err
	}

	var lc net.ListenConfig

	// FATAL, AND THE ESCAPE HATCH IS NAMED. A control plane that cannot bind this
	// is almost always a config error — the node wire's own port, or a second
	// billet — and starting anyway would leave an operator debugging a handshake
	// failure on the far machine with nothing here saying why. But this is an
	// optional surface on the one process whose loss stops every job, so the
	// error has to say how to start without it.
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf(
			"listen for enrollments on %s: %w\n(this is server.bootstrap_listen; remove it to "+
				"start the control plane without network enrollment and admit machines with "+
				"`billet ca issue <node>` instead)", addr, err)
	}

	// THE SAME ACCEPTOR, for the half of its value that applies here. Nobody
	// reaching this listener has a certificate, so "admitted" is not a trust
	// statement and the budget it guards is an anonymous one by design — but the
	// accept loop still never blocks on a permit, so a saturated enrollment port
	// refuses immediately instead of leaving a machine waiting on a backlog it
	// cannot tell from an absent control plane.
	bounded := newHandshakingListener(ctx, ln, tlsConf, limit,
		handshakeBounds{handshakeFor: handshakeFor}, slog.Default())

	srv := &http.Server{
		// ASSEMBLED IN internal/wiring FROM THE SAME ONE READ the operational wire
		// used, rather than here from pieces handed across: two call sites reading
		// the authority separately is the defect wiring exists to remove, and this
		// listener is its second consumer.
		Handler: handler,
		// TIGHTER THAN THE NODE WIRE'S, ALL OF IT, because nothing here is a long
		// poll: both routes are one small request and one small response. That is
		// why WriteTimeout can be set at all, which the node wire cannot do. The
		// handshake itself is bounded by handshakingListener rather than by any of
		// these, so a connection cannot be held for long whatever it does.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	go func() {
		if err := srv.Serve(bounded); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("the enrollment listener stopped; enrolled nodes are unaffected",
				"error", err)
		}
	}()

	slog.Default().Info("serving enrollment on its own listener; it can be closed between "+
		"enrollments without affecting the fleet", "addr", ln.Addr().String())

	return func() { stopServing(ctx, srv, "enrollment listener") }, ln.Addr().String(), nil
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

		ec2Config := *cfg.Node.EC2
		ec2Config.NodeName = cfg.Node.Name

		return ec2.New(deployment, ec2Config, ec2.WithLogger(slog.Default()))

	case config.ProviderCodeBuild:
		if cfg.Node.CodeBuild == nil {
			return nil, errors.New("billet: node.codebuild is missing; the provider is codebuild")
		}

		// THE DEPLOYMENT IDENTITY IS EVEN MORE LOAD-BEARING HERE THAN ON EC2, because
		// a CodeBuild build CANNOT BE TAGGED: `StartBuild` has no field that becomes
		// one, so the per-instance owner tag the ec2 backend filters List on does not
		// exist. What replaces it is a dedicated project plus this identity carried as
		// an environment-variable marker — and List feeds a loop that STOPS builds, so
		// two installations sharing a project and an identity is a way for one to stop
		// the other's live jobs.
		//
		// THE CREDENTIAL CHAIN IS ADAPTED RATHER THAN DUPLICATED. It lives in
		// internal/provider/ec2 and a compute backend must not import a sibling
		// compute backend, so this is the one place that knows about both — exactly
		// what openArchiveStore already does for internal/archivestore. Extracting the
		// chain into a shared package is a separate change; moving a four-types-deep
		// redaction table is a security refactor that wants its own review and its
		// own mutation run rather than riding along here.
		return codebuild.New(deployment, *cfg.Node.CodeBuild,
			codebuild.WithLogger(slog.Default()),
			codebuild.WithCredentials(awsCredentials()))

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
		// Labelled with the DEPLOYMENT id for the docker backend's reason: two
		// billets on one Mac share a hostname, and the ownership marker is what
		// keeps each from destroying the other's guests as orphans.
		var tartCfg config.TartConfig
		if cfg.Node.Tart != nil {
			tartCfg = *cfg.Node.Tart
		}

		return tart.New(deployment,
			tart.WithLogger(slog.Default()),
			tart.WithConfig(tartCfg))

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
	bootstrapAddr := fs.String("bootstrap-addr", "",
		"the control plane's enrollment address, when it serves one separately; overrides "+
			"node.bootstrap_addr and defaults to node.server_addr (with --enroll)")
	upgradeProbe := fs.Bool("upgrade-probe", false,
		"initialize the candidate provider without dialing or accepting workload")

	if err := parse(fs, args); err != nil {
		return err
	}
	if *enroll && *upgradeProbe {
		return errors.New("billet node: --enroll and --upgrade-probe are mutually exclusive")
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
		return enrollNode(ctx, cfg, bootstrapBase(cfg, *bootstrapAddr), *caFingerprint, *joinToken)
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
	if *upgradeProbe {
		if err := notifyReady(); err != nil {
			return fmt.Errorf("node upgrade-probe readiness: %w", err)
		}
		fmt.Printf("billet node %s: upgrade probe ready; registration and workload polling are disabled\n",
			cfg.Node.Name)
		<-ctx.Done()

		return nil
	}

	cacheService, stopCache, err := startNodeCache(ctx, cfg, p, deployment, client)
	if err != nil {
		return err
	}
	defer stopCache()

	maxCustody, err := cfg.Node.MaxCustodyDuration()
	if err != nil {
		return err
	}

	// THE CLIENT IS BOTH THE LEDGER AND THE MINT.
	//
	// It satisfies node.LeaseStore and node.JITSource, which is the whole reason
	// the runner needs no idea it is remote: the interfaces it already took are
	// the seam the network went through.
	runnerOpts := []node.Option{node.WithMaxCustody(maxCustody)}
	if cacheService != nil {
		runnerOpts = append(runnerOpts, node.WithCacheService(cacheService))
	}
	if cfg.Node.RegistryMirrors != nil {
		runnerOpts = append(runnerOpts, node.WithRegistryMirrors(*cfg.Node.RegistryMirrors))
	}

	// AND THE ABILITY TO REPLACE ITSELF, which is what makes a rollout reach a
	// node at all. It is given here rather than defaulted inside the runner
	// because it is a property of how this process was INSTALLED — a node started
	// out of a working directory has no packaged binary to replace, and the
	// runner refusing the command with a sentence an operator can act on is better
	// than one attempting a transaction against a machine that is not shaped for
	// it.
	runnerOpts = append(runnerOpts, node.WithUpgrader(node.ExecUpgrader{
		ConfigPath: *cfgPath,
	}))

	runner := node.New(client, cfg.Node.Name, client, p, slog.Default(), runnerOpts...)

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
	if err := notifyReady(); err != nil {
		return fmt.Errorf("node readiness: %w", err)
	}

	return nodeclient.Run(ctx, client, runner, nodeclient.LoopOptions{
		Provider:       cfg.Node.Provider,
		Deployment:     deployment,
		Site:           cfg.Node.Site,
		VCPU:           contribution.VCPU,
		Memory:         contribution.Memory,
		GuestOS:        nodeGuestOS(cfg),
		EC2Shapes:      remoteShapes(cfg),
		CodeBuildFleet: codeBuildFleet(cfg),

		CodeBuildJITParameterPath: codeBuildJITPath(cfg),
		CodeBuildRegion:           codeBuildRegion(cfg),
		Log:                       slog.Default(),
		Identity:                  identity,
		SweepEvery:                5 * time.Minute,
		DrainTimeout:              drainTimeout,
		// The second signal, reaching the wait that honours it.
		Hurry: lc.hurry,
	})
}

// nodeWireConnectionLimit caps concurrent AUTHENTICATED connections to the
// control plane's node wire.
//
// Sized so a real fleet never reaches it -- a node holds one command poll plus
// the occasional short request, so this is hundreds of nodes.
//
// WHO CAN SPEND IT IS THE PART THAT CHANGED. It used to be charged in Accept,
// before the handshake, so anyone able to open a socket could spend the fleet's
// capacity; handshakingListener charges it only once a connection has verified
// against this deployment's authority. It still depends on the IdleTimeout above
// -- a connection that never speaks again holds its place until something reaps
// it -- but the callers who can reach that state are now hosts billet issued a
// certificate to.
const nodeWireConnectionLimit = 512

// bootstrapConnectionLimit caps concurrent connections to the enrollment
// listener.
//
// Small on purpose: enrolling is a human-paced operation on a handful of
// machines, and this is the one listener a stranger can complete a handshake
// against -- it asks for no certificate, so "admitted" here is not a trust
// statement and this budget is an anonymous one by design. Saturating it delays
// an enrollment; it cannot reach the node wire, which is the whole reason the two
// are separate.
const bootstrapConnectionLimit = 64

const cacheEvictionAge = 7 * 24 * time.Hour

const cacheConnectionLimit = 128

type limitedListener struct {
	net.Listener
	semaphore chan struct{}

	// CLOSING A SATURATED LISTENER HAS TO WORK. Accept blocks acquiring a permit,
	// and closing the underlying listener does NOT unblock a channel send -- so a
	// full semaphore left Shutdown waiting on the accept goroutine before its
	// context deadline could apply, and the process hung until a connection
	// happened to free a slot.
	//
	// THAT WAIT WAS UNBOUNDED, not "up to the idle timeout": IdleTimeout bounds
	// inactivity BETWEEN requests, so anything sending another request before each
	// deadline holds its permit indefinitely and the shutdown never completes.
	closed    chan struct{}
	closeOnce sync.Once
}

func newLimitedListener(inner net.Listener, limit int) *limitedListener {
	return &limitedListener{
		Listener:  inner,
		semaphore: make(chan struct{}, limit),
		closed:    make(chan struct{}),
	}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	// SELECT, NOT A BARE SEND. Whichever is ready first: a permit, or the listener
	// being closed. Without the second case a saturated listener cannot be shut
	// down at all.
	select {
	case l.semaphore <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}

	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.semaphore

		return nil, err
	}

	return &limitedConn{Conn: conn, release: func() { <-l.semaphore }}, nil
}

func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })

	return l.Listener.Close()
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)

	return err
}

// startNodeCache exposes the site's clone store to its managed guests.
func startNodeCache(
	ctx context.Context,
	cfg *config.Config,
	p provider.Provider,
	deployment string,
	cachePolicy node.ActionsPolicy,
) (*node.CacheService, func(), error) {
	if cfg.Node.Cache == nil {
		return nil, func() {}, nil
	}

	attacher, ok := p.(provider.VolumeAttacher)
	if !ok {
		return nil, nil, fmt.Errorf("billet: provider %s cannot attach node.cache volumes",
			cfg.Node.Provider)
	}
	var storage storecontract.Store
	switch cfg.Node.Provider {
	case config.ProviderFirecracker:
		if cfg.Node.Ceph == nil {
			return nil, nil, errors.New("billet: a Firecracker node.cache needs node.ceph")
		}
		var err error
		storage, err = ceph.New(*cfg.Node.Ceph)
		if err != nil {
			return nil, nil, err
		}
	case config.ProviderEC2:
		if cfg.Node.EBSS3 == nil {
			return nil, nil, errors.New("billet: an EC2 node.cache needs node.ebs_s3")
		}
		var err error
		storage, err = ebss3.New(*cfg.Node.EBSS3,
			cacheNamespace(deployment, cfg.Node.Site), awscreds.Default())
		if err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("billet: provider %s has no cache store", cfg.Node.Provider)
	}

	service, err := node.NewCacheService(cfg.Node.Cache.GuestEndpoint,
		cacheNamespace(deployment, cfg.Node.Site), cfg.Node.StateDir, storage, attacher, slog.Default())
	if err != nil {
		return nil, nil, err
	}
	service.SetActionsPolicy(cachePolicy)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Node.Cache.Listen)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for guest cache requests on %s: %w",
			cfg.Node.Cache.Listen, err)
	}

	ln = newLimitedListener(ln, cacheConnectionLimit)
	srv := &http.Server{
		Handler:           service,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	serveListener := ln
	if cfg.Node.Provider == config.ProviderEC2 {
		certificate, err := tls.LoadX509KeyPair(cfg.Node.Cache.TLSCert, cfg.Node.Cache.TLSKey)
		if err != nil {
			_ = ln.Close()

			return nil, nil, fmt.Errorf("load the EC2 cache listener certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13,
		}
		serveListener = tls.NewListener(ln, srv.TLSConfig)
	}
	go func() {
		if err := srv.Serve(serveListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("the guest cache listener stopped; jobs will continue cold",
				"error", err)
		}
	}()

	// Eviction is intentionally best effort. A cache outage may slow a job, but it
	// must never change that job's result or stop the node from serving compute.
	go func() {
		cleanupTicker := time.NewTicker(5 * time.Minute)
		evictionTicker := time.NewTicker(6 * time.Hour)
		defer cleanupTicker.Stop()
		defer evictionTicker.Stop()

		retryClosed := func() {
			if err := service.RetryClosed(ctx); err != nil && ctx.Err() == nil {
				slog.Default().Warn("could not finish closed cache sessions; will retry",
					"error", err)
			}
		}
		renewActive := func() {
			if err := service.RenewActive(ctx, time.Now().Add(7*time.Hour)); err != nil && ctx.Err() == nil {
				slog.Default().Warn("could not renew active cache generations; will retry",
					"error", err)
			}
		}
		evict := func() {
			if err := storage.Evict(ctx, cacheEvictionAge); err != nil && ctx.Err() == nil {
				slog.Default().Warn("could not evict expired cache generations; will retry",
					"error", err)
			}
		}

		retryClosed()
		renewActive()
		evict()

		for {
			select {
			case <-ctx.Done():
				return
			case <-cleanupTicker.C:
				retryClosed()
				renewActive()
			case <-evictionTicker.C:
				evict()
			}
		}
	}()

	slog.Default().Info("serving guest cache requests", "provider", cfg.Node.Provider,
		"addr", ln.Addr().String())

	return service, func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Default().Warn("the guest cache listener did not shut down cleanly", "error", err)
		}
	}, nil
}

func cacheNamespace(deployment, site string) string {
	if site == "" {
		site = "local"
	}

	return deployment + "/" + site
}

// remoteShapes is the ordered shape catalogue this node registers, whichever
// remote backend it runs.
//
// CLONED, because the registration is carried across every reconnect and the
// caller keeps the config: a slice shared with cfg would let anything holding the
// config widen what this host claims it may buy after the numbers were validated.
//
// A HOST-BACKED PROVIDER REGISTERS NONE, and the allocator refuses one that does —
// its capacity is the machine, so a shape catalogue there is a purchase decision
// about compute nobody buys.
func remoteShapes(cfg *config.Config) []config.RemoteShape {
	switch cfg.Node.Provider {
	case config.ProviderEC2:
		if cfg.Node.EC2 != nil {
			return slices.Clone(cfg.Node.EC2.InstanceTypes)
		}

	case config.ProviderCodeBuild:
		if cfg.Node.CodeBuild != nil {
			return slices.Clone(cfg.Node.CodeBuild.ComputeTypes)
		}

	case config.ProviderDocker, config.ProviderFirecracker, config.ProviderTart:
		return nil
	}

	return nil
}

// awsCredentials adapts billet's one AWS credential chain to a consumer that
// declares its own interface over awssig.Credentials.
//
// ONE CHAIN, ADAPTED AT THE EDGE. The chain — environment variables, then this
// instance's IAM role over IMDSv2, with v1 refused rather than used as a fallback —
// lives in internal/provider/ec2 and carries a redaction table that took several
// rounds to get right. A second copy would be a second security boundary, and a
// compute backend importing a sibling compute backend would make one of them a
// library for the other. So consumers declare a narrow interface and cmd/billet
// converts, which is what openArchiveStore already does for internal/archivestore.
//
// Extracting the chain into internal/awscreds is a separate change.
func awsCredentials() codebuild.CredentialSource {
	resolver := awscreds.Default()

	return credentialSourceFunc(func(ctx context.Context) (awssig.Credentials, error) {
		resolved, err := resolver.Credentials(ctx)
		if err != nil {
			return awssig.Credentials{}, err
		}

		return awssig.Credentials{
			AccessKeyID:     resolved.AccessKeyID,
			SecretAccessKey: resolved.SecretAccessKey,
			SessionToken:    resolved.SessionToken,
		}, nil
	})
}

// credentialSourceFunc adapts a function to the interface.
type credentialSourceFunc func(context.Context) (awssig.Credentials, error)

func (f credentialSourceFunc) Credentials(ctx context.Context) (awssig.Credentials, error) {
	return f(ctx)
}

// nodeGuestOS is what this host tells the control plane it can boot, and it is
// derived ONLY for codebuild.
//
// A CODEBUILD NODE'S ENVIRONMENT TYPE DECIDES ITS GUEST OS OUTRIGHT: a MAC_ARM
// project boots macOS and every other permitted environment boots Linux, and there
// is no configuration that could make one serve the other. Reporting it lets the
// plane refuse an obviously wrong dispatch at pick time instead of at the first
// launch, and `alloc.Bind` remains the durable check a node cannot route around.
//
// EVERY OTHER BACKEND KEEPS REPORTING NOTHING, which the plane reads as
// unconstrained. That is today's behaviour and changing it is not this backend's
// business: a firecracker or tart host's real capability comes from its images and
// its `nodes:` policy, and having the node start asserting a narrower answer would
// silently stop dispatching leases that place correctly now.
func nodeGuestOS(cfg *config.Config) []config.GuestOS {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return nil
	}

	if !cfg.Node.CodeBuild.EnvironmentType.Valid() {
		return nil
	}

	return []config.GuestOS{cfg.Node.CodeBuild.EnvironmentType.GuestOS()}
}

// codeBuildFleet is the reserved-capacity fleet this node draws on, or empty.
//
// Read only for a codebuild node: the allocator refuses a fleet reported by any
// other backend, because a shared-pool claim from a host that draws on no pool
// would keep a legitimate second node out of a fleet its provider never reads.
func codeBuildFleet(cfg *config.Config) string {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return ""
	}

	return cfg.Node.CodeBuild.FleetARN
}

// codeBuildJITPath is where this node stages runner registrations, or empty.
//
// Reported for the same reason the fleet is and read under the same guard: the
// control plane sweeps registrations a dead node left behind, and the path is a
// codebuild fact the allocator refuses from any other backend.
func codeBuildJITPath(cfg *config.Config) string {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return ""
	}

	return cfg.Node.CodeBuild.JITParameterPath
}

// codeBuildRegion is the region those registrations live in, or empty.
func codeBuildRegion(cfg *config.Config) string {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return ""
	}

	return cfg.Node.CodeBuild.Region
}

// newScaleSetClient builds the GitHub client from config, reading the key with
// the same hardened reader `billet check` uses.
//
// Shared so teardown and the server authenticate identically. A second,
// slightly-different construction is how one of them ends up talking to a
// different organization than the other.
func newScaleSetClient(ctx context.Context, cfg *config.Config) (*scaleset.Client, error) {
	if cfg.GitHub == nil {
		return nil, errors.New("no github section in the config")
	}

	key, err := resolveAppKey(ctx, cfg)
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
		Org:            cfg.GitHub.Org,
		AppID:          cfg.GitHub.AppID,
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
	group := fs.String("runner-group", "",
		"the runner group to look in, for a --tier the config no longer declares")
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

	wanted, undeclared, err := teardownTargets(cfg.Tiers, *tier, *group, *force)
	if err != nil {
		return err
	}

	// SAID OUT LOUD, because this is the one path that acts on something the
	// config does not describe. The operator is deleting by name in a group they
	// named, and nothing was cross-checked against a tier definition.
	if undeclared {
		fmt.Printf("%q is not a tier in %s. Deleting it by name from runner group %q.\n\n",
			*tier, *cfgPath, groupOrDefault(*group))
	}

	client, err := newScaleSetClient(ctx, cfg)
	if err != nil {
		return err
	}

	// The ACTUAL objects, fetched before anything is destroyed. An operator
	// confirming a destructive act should be shown what is on GitHub, not the
	// names they typed into their own config.
	fmt.Printf("This deletes the following from %s:\n\n", cfg.GitHub.Org)

	present := make([]config.Tier, 0, len(wanted))

	for i := range wanted {
		t := &wanted[i]

		set, labels, err := client.Describe(ctx, t.Label, t.RunnerGroup)
		if err != nil {
			return err
		}

		if set == nil {
			fmt.Printf("  %-32s not present\n", t.Label)

			if err := forgetScaleSet(ctx, cfg, cfg.GitHub.Org,
				groupOrDefault(t.RunnerGroup), t.Label); err != nil {
				fmt.Printf("  %-32s billet could not forget it (%v); the control plane "+
					"will keep reporting it\n", t.Label, err)
			}

			continue
		}

		present = append(present, *t)

		fmt.Printf("  %-32s id %d, group %s, labels %v\n", t.Label, set.ID, set.Group, labels)
	}

	if len(present) == 0 {
		fmt.Println("\nNothing to do.")

		return nil
	}

	fmt.Println("\nRunners already registered to them are removed by GitHub.")

	if !*yes {
		if err := confirmOrganization(ctx, cfg.GitHub.Org); err != nil {
			return err
		}
	}

	for i := range present {
		t := &present[i]

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

			continue
		}

		if err := forgetScaleSet(ctx, cfg, cfg.GitHub.Org, groupOrDefault(t.RunnerGroup), t.Label); err != nil {
			fmt.Printf("%s: deleted, but billet could not forget it had created it (%v); "+
				"the control plane will keep reporting it until this is cleared\n", t.Label, err)
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
		return cmdCARotate(ctx, args[1:])
	case "retire":
		return cmdCARetire(ctx, args[1:])
	case "show":
		return cmdCAShow(args[1:])
	case "sync":
		return cmdCASync(ctx, args[1:])
	}

	return fmt.Errorf(
		"unknown ca command %q; try issue, token, rotate, retire, sync, revoke, "+
			"revocations or show", args[0])
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
	db, err := openStateAdmin(ctx, cfg)
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

	db, err := openStateAdmin(ctx, cfg)
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
	reissue := fs.Bool("reissue", false,
		"deliberately replace an existing bundle directory (the old one is moved to "+
			"<dir>.replaced; the old certificate stays valid until revoked)")
	lifetime := fs.Duration("lifetime", wirecert.LeafLifetime,
		"how long the certificate is good for (default a year; the node renews it on its "+
			"own once less than a third remains). Shorter for a short-lived host or a "+
			"rotation rehearsal; never below "+wirecert.MinIssuedLifetime.String())

	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if name == "" {
		return errors.New("usage: billet ca issue <node> [--out <dir>] [--lifetime <duration>]")
	}

	// The bounds are wirecert's (IssueNodeFor refuses outside them); checked here
	// too so the refusal arrives before the config is loaded and the authority
	// touched, naming the flag.
	if *lifetime < wirecert.MinIssuedLifetime || *lifetime > wirecert.LeafLifetime {
		return fmt.Errorf("--lifetime %s is outside [%s, %s]: a node renews on a five-minute "+
			"sweep once a third of the life remains, so a shorter leaf expires in place, and a "+
			"longer one is not something an authority issues", *lifetime,
			wirecert.MinIssuedLifetime, wirecert.LeafLifetime)
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

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	authority, err := wirecert.LoadServing(cfg.Server.IdentityDir, deployment)
	if err != nil {
		return err
	}

	bundle, err := authority.Issuing.IssueNodeFor(name, *lifetime)
	if err != nil {
		return err
	}

	// THE WHOLE TRUST BUNDLE, NOT THE AUTHORITY THAT SIGNED THIS LEAF. During an
	// overlap the new authority issues and the OLD one signs what the control
	// plane presents, so a bundle carrying only the issuer hands the operator a
	// node that cannot verify the server it was just enrolled against — the one
	// machine a rotation is supposed not to strand. The wire's own enroll and
	// renew responses have always carried the bundle (nodeplane's trustBundle);
	// this is the out-of-band path, and it did not.
	bundle.CAPEM = authority.Trust

	// The destination is resolved and vetted BEFORE the ledger records the new
	// serial: a refusal (an existing .replaced archive, a bad path) must not
	// leave the ledger claiming a credential no bundle carries.
	dir := *out
	if dir == "" {
		dir = name + "-billet-tls"
	}
	dir = filepath.Clean(dir)

	// FAIL CLOSED ON EVERY VETTING UNCERTAINTY, and vet the plain-issue
	// destination too: a refusal here must come BEFORE the ledger records the
	// new serial and displaces the admitted fingerprint.
	if *reissue {
		if _, err := os.Stat(dir + ".replaced"); err == nil {
			return fmt.Errorf("%s.replaced already exists — it holds the certificate from the "+
				"previous reissue, which stays VALID until revoked, and overwriting it would "+
				"destroy the only copy the revoke command reads. Revoke it first (`billet ca "+
				"revoke %s --cert %s`), then remove the directory and re-run",
				dir, shellArg(name), shellArg(filepath.Join(dir+".replaced", "node.crt")))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check %s.replaced: %w", dir, err)
		}
	} else if _, err := os.Stat(filepath.Join(dir, "node.key")); err == nil {
		return fmt.Errorf("%s already holds a bundle and billet will not overwrite it — that "+
			"node is probably enrolled. Write to a new directory with --out, or replace it "+
			"deliberately with --reissue", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check %s: %w", dir, err)
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

	// --reissue MOVES the old bundle aside rather than deleting it: the node is
	// still holding that key until someone installs the new bundle and restarts
	// it, and the old certificate stays VALID until revoked — both facts the
	// operator acts on, so both survive on disk and are said below. A prior
	// .replaced was refused above, so nothing is ever destroyed here.
	replaced := false
	if *reissue {
		if _, err := os.Stat(filepath.Join(dir, "node.key")); err == nil {
			if err := os.Rename(dir, dir+".replaced"); err != nil {
				return fmt.Errorf("move the old bundle aside: %w", err)
			}
			replaced = true
		}
	}

	if err := bundle.Write(dir); err != nil {
		return err
	}

	if replaced {
		fmt.Printf("billet ca: the previous bundle was moved to %s.replaced. The node keeps "+
			"using its old key until this new bundle is installed and the node restarts, and "+
			"the OLD certificate stays valid until you revoke it:\n\n"+
			"  billet ca revoke %s --cert %s --reason reissued\n\n",
			dir, shellArg(name), shellArg(filepath.Join(dir+".replaced", "node.crt")))
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

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	ca, err := wirecert.LoadOrCreateCA(cfg.Server.IdentityDir, deployment)
	if err != nil {
		return err
	}

	fmt.Printf("deployment  %s\nauthority   %s\nexpires     %s\nfingerprint %s\n",
		deployment, wirecert.CADir(cfg.Server.IdentityDir), ca.NotAfter().Format(time.RFC3339),
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
	fmt.Printf("  billet node --enroll --ca-fingerprint %s%s\n",
		ca.Fingerprint(), enrollAddrFlag(cfg))

	if cfg.Server.BootstrapListen == "" && !nodeplane.LoopbackOnly(cfg.Server.Listen) {
		fmt.Printf("\nThis control plane serves no enrollment address, so that command has\n")
		fmt.Printf("nowhere to ask: its node wire requires a certificate an enrolling machine\n")
		fmt.Printf("does not have yet. Either issue the bundle here and copy it out of band:\n\n")
		fmt.Printf("  billet ca issue <node>\n\n")
		fmt.Printf("or set server.bootstrap_listen and restart.\n")
	}

	return nil
}

// githubAPIBase is the API base cmdCheck verifies the App against — a var
// rather than the constant so tests can point it at a fake instead of the real
// GitHub, which a unit test must never reach. Empty selects the default.
var githubAPIBase = ""

// iamEndpointOverride points the instance-profile probe at a fake for tests —
// production always derives the partition-global IAM endpoint from the region.
var iamEndpointOverride = ""

// printRemoteCost bounds what this node's own declarations can cost per hour.
//
// EVERY REMOTE BACKEND, THROUGH remoteShapes, and it used to read node.ec2 directly.
// A codebuild node declares ordered shapes with a price per hour for the same reason
// an ec2 node does — placement charges the first that fits — so reading one block by
// name meant a check that reported the cost exposure of an ec2 node and stayed silent
// for a codebuild one, which reads as compute that is free.
//
// THE NODE'S PROVIDER NAMES ITSELF in the line, because the two backends bill for
// different things: an ec2 shape is an instance-hour, a codebuild compute type is a
// build-minute rate expressed per hour, and an operator comparing the two numbers
// needs to know which they are looking at.
func printRemoteCost(cfg *config.Config) error {
	shapes := remoteShapes(cfg)
	if len(shapes) == 0 {
		return nil
	}

	maxVCPU := cfg.Node.MaxVCPU
	maxMemory := cfg.Node.MaxMemory
	if cfg.Server != nil {
		maxVCPU = min(maxVCPU, cfg.Server.MaxVCPU)
		maxMemory = min(maxMemory, cfg.Server.MaxMemory)
	}

	peak, err := config.RemotePeakHourlyExposure(maxVCPU, maxMemory, shapes)
	if err != nil {
		return err
	}

	fmt.Printf("%-8s <= %s compute (%s/month at 730h), from declared shape prices\n",
		string(cfg.Node.Provider)+" max", &peak, peak.ForHours(730))

	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("billet status")
	cfgPath := addConfigFlag(fs)
	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	a, db, closeDB, err := controlPlaneStores(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	// FIRST, because it is the answer to "why is nothing running" and every other
	// line below reads normally on a sealed deployment. An operator who has to
	// scroll to find out their fleet is deliberately idle has been told too late.
	admission, err := a.Admission(ctx)
	if err != nil {
		return err
	}
	printAdmission(admission)

	// SECOND, AND FOR THE SAME REASON. A force-destroy is the one thing in billet
	// that ends running work, so an operator who finds builds failing needs to see
	// it before the capacity numbers that will look perfectly healthy underneath.
	printForceDestroy(ctx, a, admission)

	// AND A ROLLOUT IN ONE LINE, because it explains the other half of what an
	// operator is looking at: hosts on two versions, capacity down by one machine,
	// a node reporting nothing. `billet rollout status` is the full picture; this
	// is what says to go and look at it.
	printRollout(ctx, db)

	// AND WHO THE DEPLOYMENT'S CONTROLLER IS, because the epoch beside it is a
	// fence rather than a note. Every write is refused once that number moves, so
	// an operator looking at a control plane that has gone quiet needs to be able
	// to see whether something else took it over — and, on PostgreSQL, which
	// machine to go and look at.
	printController(ctx, db)

	usage, err := a.Usage(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("capacity  %d of %d vCPU, %s of %s, %d open leases\n",
		usage.VCPU, cfg.Server.MaxVCPU, usage.Memory, cfg.Server.MaxMemory, usage.Leases)

	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		headroom, err := a.Headroom(ctx, t.Label)
		if err != nil {
			return err
		}
		fmt.Printf("tier      %-24s %d available\n", t.Label, headroom)
	}

	if err := printRemoteFleetCost(ctx, a, cfg); err != nil {
		return err
	}

	// WHAT THE CONTROL PLANE HAS SWEPT out of Parameter Store, and which codebuild
	// hosts it cannot sweep after. A leaked registration is one nobody sees, which
	// is why the count is durable and printed rather than logged.
	printCredentialSweeps(ctx, a, db)

	printReportedInventory(ctx, a)
	printComputeBarrier(ctx, a)
	printWireWindow(ctx, a)

	held, err := a.Held(ctx)
	if err != nil {
		return err
	}

	// A RUNNING LEASE WHOSE HOLDER WAS REPLACED IS NOT HELD, and is exactly the
	// slot an operator finds taken with nothing below saying why.
	printReplacedHolders(ctx, a)

	if len(held) == 0 {
		fmt.Println("held      none")

		return nil
	}

	fmt.Printf("held      %d lease(s) waiting for compute to be confirmed gone\n", len(held))
	printHeld(held)
	printHolderNote(os.Stdout, held)

	return nil
}

// printController names the process holding this deployment's controller claim,
// and the generation it holds.
//
// THE EPOCH IS THE FENCE, so it is printed rather than hidden behind a
// verbosity flag: every ledger write a control plane makes is refused once that
// number moves, and "the control plane stopped and something else has it" is
// otherwise a fact only the journal carries.
//
// NEVER CLAIMED IS AN ORDINARY STATE and is said in those words. A fresh
// deployment has no row, and reporting that as missing or broken would put a
// scary line in front of somebody setting one up.
//
// IT NEVER FAILS THE COMMAND, for the reason printRollout does not: `billet
// status` is what somebody runs when something is already wrong.
// The label is `claim` rather than `controller` because this report's first
// column is ten characters wide — `admission`, `protocol`, `barrier`, `force`,
// `capacity`, `tier`, `rollout`, `held` — and `controller` fills all ten, so the
// value would start one column right of every other line.
func printController(ctx context.Context, db *state.DB) {
	claim, err := db.ControllerHolder(ctx)
	if err != nil {
		fmt.Printf("claim     unavailable: %v\n", err)

		return
	}

	if claim.Holder == "" {
		fmt.Println("claim     nothing has ever claimed this deployment's controller")

		return
	}

	fmt.Printf("claim     %s holds this deployment's controller, at epoch %d\n",
		claim.Holder, claim.Epoch)
}

// printRemoteFleetCost bounds what every registered remote node can cost per hour.
//
// IT COVERS THE WHOLE REMOTE FLEET rather than the ec2 half of it. The query behind
// it was scoped to `provider = 'ec2'`, so a deployment whose cloud capacity was
// codebuild printed nothing here — and the absence of a cost line is exactly what a
// free fleet looks like. See alloc.RemoteCostNodes.
func printRemoteFleetCost(ctx context.Context, a *alloc.Allocator, cfg *config.Config) error {
	nodes, err := a.RemoteCostNodes(ctx)
	if errors.Is(err, alloc.ErrRemoteCostUnavailable) {
		fmt.Printf("cloud peak  unavailable (%v)\n", err)

		return nil
	}
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}

	peak, err := config.RemoteFleetPeakHourlyExposure(
		cfg.Server.MaxVCPU, cfg.Server.MaxMemory, nodes)
	if err != nil {
		return err
	}
	fmt.Printf("cloud peak <= %s compute (%s/month at 730h), across %d registered remote node(s) from declared shape prices\n",
		&peak, peak.ForHours(730), len(nodes))

	return nil
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
func checkEC2Credentials(
	ctx context.Context, cfg *config.Config, bundle *wirecert.Bundle,
	authorize, maintenanceProbe bool,
) error {
	// FIRST, before credentials resolve or anything dials AWS: during the
	// upgrade transaction's stopped-service window, an AWS or IMDS blip must
	// not read as a broken host and roll the upgrade back. The reachability
	// call used to survive the skip; it is a network call like the rest, and
	// the probe's job is the ledger and the config, not the cloud.
	if maintenanceProbe {
		fmt.Printf("aws      (all AWS checks skipped during maintenance)\n")

		return nil
	}

	ec2cfg := *cfg.Node.EC2
	ec2cfg.NodeName = cfg.Node.Name

	creds, err := awscreds.Default().Credentials(ctx)
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
	if err := ec2.CheckReachable(ctx, ec2cfg,
		ec2.WithCredentials(awscreds.Static(creds))); err != nil {
		// MEASURED TRAP: a region that is not enabled on the account answers
		// AuthFailure with credential-shaped prose, so an operator whose key
		// works elsewhere would rotate credentials that were never wrong.
		if ec2.RegionMayBeDisabled(err) {
			return fmt.Errorf("node.ec2: the api in %s refused with a credential-shaped error, "+
				"which is ALSO exactly what a region that is not enabled on this account "+
				"returns. If these credentials work in another region, enable %s on the "+
				"account (or pick an enabled region) before rotating anything: %w",
				ec2cfg.Region, ec2cfg.Region, err)
		}

		return fmt.Errorf("node.ec2: credentials for %s resolved but could not call the ec2 api "+
			"in %s: %w", creds.AccessKeyID, ec2cfg.Region, err)
	}

	// THE ACCESS KEY ID AND NOTHING ELSE. It is an identifier rather than a
	// secret, and printing it is the difference between "billet is using the wrong
	// role" and an operator staring at a working config. The secret and the
	// session token are never rendered anywhere.
	// THE ACCOUNT'S OWN CEILING, once the credentials are known to work. It is
	// read with the SAME credentials that were just proved, so what it reports is
	// what this node will run as. Advisory; see reportQuotas.
	if p, err := ec2.New(deploymentForCheck, ec2cfg,
		ec2.WithCredentials(awscreds.Static(creds))); err == nil {
		reportQuotas(ctx, cfg, p)
	}

	fmt.Printf("aws      %s in %s, subnet %s, %d instance shape(s), credentials %s (can describe)\n",
		spotLabel(ec2cfg.Spot), ec2cfg.Region, ec2cfg.SubnetID, len(ec2cfg.InstanceTypes),
		creds.AccessKeyID)

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. A read-only call says
	// nothing about permission to LAUNCH, and an operator who reads "ok" and then
	// watches every job fail on an IAM denial has been misled by this line.
	fmt.Printf("         (describe only — launching also needs at least ec2:RunInstances, " +
		"ec2:TerminateInstances, ec2:CreateTags and ec2:DescribeImages, plus iam:PassRole " +
		"if node.ec2.instance_profile is set)\n")
	if ec2cfg.Spot {
		fmt.Printf("         spot warnings are consumed from interruption_queue_url; the role also " +
			"needs sqs:ReceiveMessage, sqs:DeleteMessage and sqs:GetQueueAttributes on that queue\n")
	}

	// SAID OUT LOUD RATHER THAN INFERRED FROM AN ABSENT KEY. A deployment that
	// expected to run fork pull requests on rented machines and finds them queuing
	// forever has no other way to see why.
	if len(ec2cfg.UntrustedSecurityGroupIDs) == 0 {
		fmt.Printf("         untrusted work will be refused: no untrusted_security_group_ids\n")
	}

	return ec2Preflight(ctx, cfg, ec2cfg, awscreds.Static(creds), bundle, authorize)
}

// ec2Preflight proves the subnet, security groups and tier AMIs a launch depends
// on actually exist and fit together, which CheckReachable's single
// DescribeInstances cannot. Read-only describes only: a dry-run launch is a
// write-shaped call a diagnostic should not make by default.
//
// A wrong subnet, a security group in another VPC, or a cache zone that does not
// match the subnet's are FATAL — they are misconfigurations a job would fail on.
// An AMI that does not resolve is a WARNING, because the staged flow deliberately
// writes a placeholder for `billet ami build` to replace, so a not-yet-built image
// is an expected intermediate state rather than a broken config.
func ec2Preflight(
	ctx context.Context, cfg *config.Config, ec2cfg config.EC2Config, creds awscreds.Source,
	bundle *wirecert.Bundle, authorize bool,
) error {
	region, endpoint := ec2cfg.Region, ec2cfg.Endpoint

	subnet, err := ec2.DescribeSubnet(ctx, region, endpoint, creds, ec2cfg.SubnetID)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	fmt.Printf("subnet   %s in vpc %s, zone %s (%s)\n",
		subnet.SubnetID, subnet.VPCID, subnet.AvailabilityZone, subnet.State)

	// AN EBS CACHE VOLUME CANNOT ATTACH ACROSS ZONES, so the cache's zone must be
	// the subnet's. config proves the AZ is IN the region; only the API knows which
	// zone the subnet is actually in.
	if cfg.Node.EBSS3 != nil && subnet.AvailabilityZone != cfg.Node.EBSS3.AvailabilityZone {
		return fmt.Errorf("node.ec2.subnet_id %s is in zone %s, but node.ebs_s3.availability_zone "+
			"is %s; an EBS cache volume cannot attach to an instance in another zone",
			subnet.SubnetID, subnet.AvailabilityZone, cfg.Node.EBSS3.AvailabilityZone)
	}

	groupIDs := slices.Concat(ec2cfg.SecurityGroupIDs, ec2cfg.UntrustedSecurityGroupIDs)
	groups, err := ec2.DescribeSecurityGroups(ctx, region, endpoint, creds, groupIDs)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	for _, g := range groups {
		if g.VPCID != subnet.VPCID {
			return fmt.Errorf("node.ec2 security group %s is in vpc %s, but the subnet is in vpc "+
				"%s; a launch's security groups must be in the subnet's vpc", g.GroupID, g.VPCID,
				subnet.VPCID)
		}
	}

	fmt.Printf("groups   %d security group(s), all in vpc %s\n", len(groups), subnet.VPCID)

	amis := distinctEC2TierAMIs(cfg)

	var images []ec2.ImageInfo
	if len(amis) == 0 {
		// A FLEET NODE FILE HAS NO TIERS — they live on the control plane — so there
		// is no AMI to resolve here. Said, rather than passing silently as if the
		// images had been checked. The authorization dry-runs below still run: a
		// fleet node launches and tears down compute too.
		fmt.Printf("images   no tiers in this file, so no AMI to check " +
			"(a fleet node's tiers live on the control plane)\n")
	} else {
		resolved, err := ec2.DescribeImageStates(ctx, region, endpoint, creds, amis)
		if err != nil {
			return fmt.Errorf("node.ec2: %w", err)
		}
		images = resolved
	}

	for _, img := range images {
		switch {
		case !img.Found:
			reason := img.State
			if reason == "" {
				reason = "not found in this account or region"
			}
			fmt.Printf("image    %s is not resolvable yet (%s) — build it with `billet ami build` "+
				"and paste the id\n", img.ImageID, reason)
		case img.State != "available":
			fmt.Printf("image    %s is %s, not yet available\n", img.ImageID, img.State)
		case img.Contract < ec2.AMIContract:
			// A WARNING, NOT A REFUSAL. An image below the contract still runs jobs
			// correctly; what it loses is the Docker cache, silently — every job
			// re-pulls and nothing errors. Failing closed on a performance property
			// would strand a working fleet over a cold cache, so this names the
			// problem and the remedy and lets the deployment run.
			// QUOTED, BECAUSE THIS IS AN OPERATOR-EDITABLE TAG. EC2 permits newlines
			// and control characters in a tag value, and this line is printed into a
			// report an operator reads as billet's own output — so an unquoted value
			// can forge additional report lines. Provenance, not authentication:
			// quoting stops it lying about the REPORT, and nothing stops a tag
			// lying about itself.
			built := strconv.Quote(img.BuiltBy)
			if img.BuiltBy == "" {
				built = "a billet that did not record itself"
			}

			// WHAT IS MISSING DEPENDS ON HOW FAR BEHIND IT IS, and naming only the
			// oldest gap would send an operator looking for a Docker problem in an
			// image whose Docker is fine and whose toolcache is absent.
			missing := "its Docker image store may be the containerd one, which makes the " +
				"cache publish with no images in it so every job re-pulls"
			if img.Contract >= 1 {
				missing = "it carries no toolcache, so every setup-node, setup-go, " +
					"setup-python and setup-java step downloads a runtime that a " +
					"microVM tier of this deployment already has baked in"
			}

			fmt.Printf("image    %s meets AMI contract %d and this billet wants %d (built by "+
				"%s) — %s; rebuild with `billet ami build`\n",
				img.ImageID, img.Contract, ec2.AMIContract, built, missing)
		default:
			fmt.Printf("image    %s available (AMI contract %d)\n", img.ImageID, img.Contract)
		}
	}

	// THE SPOT QUEUE, READ WITHOUT CONSUMING: a deployment whose interruption
	// queue is missing or unreadable silently loses every two-minute warning,
	// so the probe is fatal — and it is GetQueueAttributes, never
	// ReceiveMessage, which would consume a real warning some node needed.
	if ec2cfg.Spot {
		arn, err := ec2.CheckInterruptionQueue(ctx, region, creds, ec2cfg.InterruptionQueueURL)
		switch {
		case err == nil:
			fmt.Printf("spot     interruption queue answers (%s)\n", arn)
		case ec2.QueueProbeInconclusive(err):
			// A fact about the CHECKING identity: a role provisioned before
			// sqs:GetQueueAttributes joined the generated grant refuses this
			// probe while consuming warnings perfectly well.
			fmt.Printf("spot     queue probe INCONCLUSIVE: %v\n", err)
			fmt.Printf("         (this identity may not read queue attributes — a role from " +
				"before the probe existed lacks sqs:GetQueueAttributes; regenerate it with " +
				"`billet init iam`)\n")
		default:
			return fmt.Errorf("node.ec2: %w", err)
		}
	}

	// The instance profile a trusted job would carry, three-valued: Missing is
	// a misconfiguration the launch WILL fail on; Unknown means this checking
	// identity may not read IAM, which says nothing about the profile and is
	// reported as exactly that.
	if ec2cfg.InstanceProfile != "" {
		verdict, reason, err := ec2.CheckInstanceProfile(ctx, region, iamEndpointOverride, creds,
			ec2cfg.InstanceProfile)
		switch {
		case err != nil:
			fmt.Printf("profile  %s UNVERIFIED: %v\n", ec2cfg.InstanceProfile, err)
		case verdict == ec2.ProfileFound:
			fmt.Printf("profile  %s exists\n", ec2cfg.InstanceProfile)
		case verdict == ec2.ProfileMissing:
			return fmt.Errorf("node.ec2.instance_profile %q does not exist in this account (%s); "+
				"a trusted job's launch will fail on it", ec2cfg.InstanceProfile, reason)
		default:
			fmt.Printf("profile  %s could not be checked (%s) — this says the CHECKING identity "+
				"may not read IAM, not that the profile is wrong. billet's own generated node "+
				"policy deliberately grants no iam:GetInstanceProfile; run check with operator "+
				"credentials to verify the profile\n", ec2cfg.InstanceProfile, reason)
		}
	}

	// The cache bucket, probed under the deployment's own prefix — the grant a
	// job will actually use. Skipped when no identity is minted yet, because
	// the prefix is derived from it.
	if cfg.Node.EBSS3 != nil {
		owner, err := authorizeOwner(cfg, bundle)
		switch {
		case err != nil:
			return fmt.Errorf("node.ebs_s3: resolve the deployment identity: %w", err)
		case owner == "":
			fmt.Printf("cache    bucket probe skipped: no deployment identity minted yet " +
				"(it is minted on the server's first start)\n")
		default:
			// The SAME namespace the runtime and decommission use, or the probe
			// reads a prefix no job ever touches.
			store, err := ebss3.New(*cfg.Node.EBSS3, cacheNamespace(owner, cfg.Node.Site), creds)
			if err != nil {
				return fmt.Errorf("node.ebs_s3: %w", err)
			}
			switch probeErr := store.CheckAccess(ctx); {
			case probeErr == nil:
				fmt.Printf("cache    bucket %s answers under this deployment's prefix\n",
					cfg.Node.EBSS3.Bucket)
			case strings.Contains(probeErr.Error(), "HTTP 403"):
				// NOT PROOF OF A BROKEN BUCKET: S3 answers 404 for a miss only
				// with s3:ListBucket, and billet's minimal grant conditions
				// that on s3:prefix — a context key GetObject does not carry —
				// so under exactly the generated policy a healthy miss can
				// answer 403. Advisory, until live acceptance pins the shape.
				fmt.Printf("cache    bucket probe INCONCLUSIVE: %v\n", probeErr)
				fmt.Printf("         (a 403 here is EITHER a refused identity OR a healthy miss " +
					"under billet's minimal grant, whose prefix-conditioned ListBucket cannot " +
					"match a GetObject; a real job read will settle it)\n")
			default:
				return fmt.Errorf("node.ebs_s3: %w", probeErr)
			}
		}
	}

	if !authorize {
		fmt.Printf("         (launch authority not checked — pass --authorize to dry-run " +
			"RunInstances; a DryRun has no side effect)\n")

		return nil
	}

	return ec2Authorize(ctx, cfg, ec2cfg, creds, bundle, images)
}

// ec2Authorize dry-runs the launch a job needs, to prove the role may RunInstances
// — the one thing the read-only describes cannot. A DryRun has no side effect (AWS
// validates and checks IAM, then refuses and starts nothing), which is why this is
// opt-in behind --authorize rather than run by default: it is the only probe here
// that asks a write-shaped question, and an operator should choose to. Teardown is
// NOT dry-run here: a DryRun TerminateInstances validates the instance id before
// the permission verdict (measured), so ec2:TerminateInstances cannot be proved
// without a real instance and stays advisory.
// authorizeOwner resolves the deployment identity the dry-run must tag as, the way
// the node runtime does (nodeDeploymentID): the certificate outranks the config,
// because the certificate is what the control plane actually checks. It PEEKS only
// — a diagnostic must never mint an identity — so an unenrolled, never-started
// deployment returns "" and the caller skips the probe rather than tagging a
// value a per-deployment policy would reject.
func authorizeOwner(cfg *config.Config, bundle *wirecert.Bundle) (string, error) {
	if bundle != nil {
		return bundle.Deployment()
	}

	for _, dir := range deploymentStateDirs(cfg) {
		id, found, err := state.PeekDeploymentID(dir)
		if err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}

	return "", nil
}

// hasEC2Tier reports whether any tier in this file can run on the ec2 provider — a
// fleet node file has none, because its tiers live on the control plane.
func hasEC2Tier(cfg *config.Config) bool {
	for i := range cfg.Tiers {
		if cfg.Tiers[i].AcceptsProvider(config.ProviderEC2) {
			return true
		}
	}

	return false
}

func ec2Authorize(
	ctx context.Context, cfg *config.Config, ec2cfg config.EC2Config, creds awscreds.Source,
	bundle *wirecert.Bundle, images []ec2.ImageInfo,
) error {
	// THE PROBE MUST TAG AS THIS DEPLOYMENT, or a per-deployment IAM policy — which
	// conditions ec2:CreateTags on the exact sh.billet.owner value — refuses the
	// launch's TagSpecification and the dry-run fails as UnauthorizedOperation
	// against the very policy `billet init iam` generates. The real launch tags with
	// the deployment id, so the probe must too. Peek, never mint: a diagnostic must
	// not create an identity.
	owner, err := authorizeOwner(cfg, bundle)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	if owner == "" {
		fmt.Printf("         (launch authority not checked — this deployment's identity is not " +
			"known here yet, so a dry-run cannot tag as a per-deployment IAM policy requires; " +
			"enroll this node, or run `billet server` once to mint it, then re-run with " +
			"--authorize)\n")

		return nil
	}

	p, err := ec2.New(owner, ec2cfg, ec2.WithCredentials(creds))
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	available := make(map[string]bool)
	for _, img := range images {
		if img.Found && img.State == "available" {
			available[img.ImageID] = true
		}
	}

	fatal := false
	verdicts := 0 // dry-runs that reached a permission answer (authorized or not)
	probed := 0
	skipped := 0      // ec2 tiers deliberately not probed (untrusted with no network)
	unresolvable := 0 // ec2 tiers whose AMI is not resolvable yet
	seen := make(map[string]bool)

	// Dry-run every launchable combination the config expresses: each ec2 tier's
	// AMI, on the network its trust selects (a trusted launch also exercises
	// iam:PassRole when an instance profile is configured), at the tier's disk, for
	// every declared shape that fits the tier — the same fallback set a real launch
	// walks. Identical requests are asked once.
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if !t.AcceptsProvider(config.ProviderEC2) {
			continue
		}

		trust := provider.TrustUntrusted
		if t.Trust.Effective() == config.WorkloadTrusted {
			trust = provider.TrustTrusted
		}

		// BOTH blockers are evaluated independently, because ONE tier can carry both
		// — an unresolvable AMI AND an untrusted trust with no untrusted network. If
		// the AMI check short-circuited first, that tier would count only as an AMI
		// problem and the summary would suppress the network remedy the operator also
		// needs. Each blocker prints its own line and bumps its own counter.
		amiBad := !available[t.ImageFor(config.ProviderEC2)]

		// An untrusted launch the node itself would REFUSE (no untrusted network) is
		// not a launch to prove: probing it would put the request on the VPC default
		// security group, so AWS answers about a launch billet never sends — a
		// misleading authorized, or a fatal false NOT-AUTHORIZED against a policy
		// scoped to the untrusted groups. Mirror ec2.Accepts.
		netBad := trust == provider.TrustUntrusted && len(ec2cfg.UntrustedSecurityGroupIDs) == 0

		if netBad {
			fmt.Printf("authz    %s runs untrusted work but node.ec2.untrusted_security_group_ids "+
				"is empty — the node refuses it, so its launch is not probed\n", t.Label)
			skipped++
		}
		if amiBad {
			unresolvable++ // the image probes above already named it as unresolvable
		}
		if amiBad || netBad {
			continue
		}

		ami := t.ImageFor(config.ProviderEC2)

		for _, shape := range ec2cfg.InstanceTypes {
			if shape.VCPU < t.VCPU || shape.Memory < t.Memory {
				continue // the launch would never pick a shape too small for the tier
			}

			key := ami + "|" + trustName(trust) + "|" + shape.Type + "|" +
				strconv.FormatInt(int64(t.Disk), 10)
			if seen[key] {
				continue
			}
			seen[key] = true

			probed++

			res, err := p.DryRunLaunch(ctx, ami, trust, shape, t.Disk)
			if err != nil {
				return fmt.Errorf("node.ec2: dry-run launch %s on %s: %w", t.Label, shape.Type, err)
			}

			hard, verdict := reportAuthz(
				fmt.Sprintf("launch %s on %s (%s)", t.Label, shape.Type, trustName(trust)), res)
			fatal = hard || fatal
			if verdict {
				verdicts++
			}
		}
	}

	switch {
	case probed == 0 && !hasEC2Tier(cfg):
		// A fleet node file: its tiers and AMIs live on the control plane, so there
		// is nothing here to dry-run. Launch authority is genuinely unproven — said,
		// not misdirected to `billet ami build`.
		fmt.Printf("         (launch authority not checked — this file declares no ec2 tiers, so " +
			"there is no launch to dry-run; a fleet node's tiers live on the control plane)\n")
	case probed == 0 && skipped > 0 && unresolvable == 0:
		// EVERY ec2 tier was SKIPPED (untrusted with no untrusted network), none
		// blocked by an AMI — the skip lines above already said why, so do not
		// misdirect to `billet ami build`.
		fmt.Printf("         (launch authority not checked — every ec2 tier was skipped above; " +
			"give them an untrusted network to probe)\n")
	case probed == 0 && skipped > 0:
		// A MIX: some tiers skipped for a missing network, some for an unresolvable
		// AMI. Name BOTH remedies rather than misattributing one cause to all.
		fmt.Printf("         (launch authority not checked — no ec2 tier could be dry-run: build " +
			"the AMIs named above (`billet ami build`) and give untrusted tiers a network)\n")
	case probed == 0:
		fmt.Printf("authz    no ec2 tier has a resolvable AMI to dry-run a launch with; build one " +
			"with `billet ami build`\n")
	case verdicts == 0:
		// Every dry-run was refused before AWS reached a permission answer (a shape
		// not offered in the zone, say), so launch authority is still unproven — said
		// rather than passing silently as if it had been checked.
		fmt.Printf("         (launch authority still unproven — every dry-run was refused for a " +
			"non-permission reason before AWS reached an authorization verdict)\n")
	}

	fmt.Printf("         (ec2:TerminateInstances cannot be dry-run — it validates the instance id " +
		"before the permission verdict, so it needs a real instance; grant it alongside " +
		"RunInstances)\n")

	if fatal {
		return errors.New("node.ec2: the ec2 role is NOT authorized for a launch it will need, " +
			"so jobs would be admitted and then fail — the runtime IAM policy is incomplete. " +
			"`billet init iam` prints exactly what it needs")
	}

	return nil
}

// trustName is the human word for a trust class, for the authz report lines.
func trustName(trust provider.TrustClass) string {
	if trust == provider.TrustTrusted {
		return "trusted"
	}

	return "untrusted"
}

// reportAuthz prints one dry-run outcome and returns (hard, verdict): whether it is
// a hard failure, and whether it reached a permission verdict at all (authorized or
// not).
func reportAuthz(what string, res ec2.DryRunResult) (bool, bool) {
	switch res.Outcome {
	case ec2.DryRunUnauthorized:
		fmt.Printf("authz    %s: NOT AUTHORIZED (%s)\n", what, res.Code)

		return true, true
	case ec2.DryRunAuthorized:
		fmt.Printf("authz    %s: authorized\n", what)

		return false, true
	default:
		// DryRunInconclusive: AWS refused before it reached the permission answer —
		// a shape not offered in the zone, an invalid parameter. NOT a permission
		// verdict, so it is neither a pass nor a fail, and the caller reports that
		// nothing was proved if every probe landed here.
		fmt.Printf("authz    %s: inconclusive (%s — refused before a permission verdict)\n",
			what, res.Code)

		return false, false
	}
}

// distinctEC2TierAMIs is the set of AMIs the ec2 tiers in this file name, in
// first-seen order.
func distinctEC2TierAMIs(cfg *config.Config) []string {
	seen := make(map[string]bool)

	var amis []string
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if !t.AcceptsProvider(config.ProviderEC2) {
			continue
		}

		ami := t.ImageFor(config.ProviderEC2)
		if ami == "" || seen[ami] {
			continue
		}

		seen[ami] = true
		amis = append(amis, ami)
	}

	return amis
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

	report, err := p.CheckHost(ctx, needsFirecrackerRootResize(cfg))
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

// needsFirecrackerRootResize reports whether this deployment can send a tier
// with an explicit root capacity to this backend. A zero-disk catalogue keeps the
// image default and must not acquire resize2fs as a preflight dependency.
func needsFirecrackerRootResize(cfg *config.Config) bool {
	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]
		if tier.Disk > 0 && tier.AcceptsProvider(config.ProviderFirecracker) {
			return true
		}
	}

	return false
}

// checkTartHost proves this machine can act as a tart node.
//
// FATAL for the firecracker preflight's reason: only a tart node reaches here,
// so a failure describes a machine that is meant to run jobs and cannot, and
// reporting it while exiting zero would make `billet check` say a host is fine
// when nothing on it can launch.
func checkTartHost(ctx context.Context, cfg *config.Config) error {
	var tartCfg config.TartConfig
	if cfg.Node.Tart != nil {
		tartCfg = *cfg.Node.Tart
	}

	// Normalized so what is REPORTED is what a launch would use. Load already
	// did this for a config read from a file; doing it again costs nothing and
	// keeps the report honest for one built any other way.
	tartCfg.Normalize()

	p, err := tart.New(deploymentForCheck, tart.WithConfig(tartCfg))
	if err != nil {
		return err
	}

	report, err := p.CheckHost(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("tart     %s, %d local VMs\n", report.Version, report.VMs)

	// SAID EVERY TIME, like softnet's grant. Which billets serialize against each
	// other is decided by TART_HOME, so two processes that disagree about the
	// store take different locks and exclude nothing — printing the path is what
	// makes that a comparison an operator can make rather than an inference.
	//
	// AND AN UNPROVED LOCK IS NOT REPORTED AS A PROVED ONE. A held lock is what a
	// busy node looks like and also what a wedged store looks like, so the line
	// says which of the two billet established.
	if report.StoreLockProved {
		fmt.Printf("         store    %s serializes every lease-name rename and delete\n",
			report.StoreLock)
	} else {
		fmt.Printf("         store    %s NOT PROVED: %s\n",
			report.StoreLock, report.StoreLockWhy)
	}

	// SAID EVERY TIME, not only when broken. softnet's grant is host
	// provisioning that survives nothing — `brew upgrade softnet` replaces the
	// binary and resets its ownership — so an operator has to be able to see its
	// state on an ordinary check rather than discover it on the first untrusted
	// job of the day.
	switch {
	case report.Softnet.GrantConfigured && report.Softnet.Why == "":
		// NOT "isolation available": the check proves a setuid bit and an owner,
		// which says softnet could start and nothing about what its policy then
		// permits. Only a probe from inside a guest can say that.
		fmt.Printf("         softnet  %s: setuid-root grant configured\n", report.Softnet.Path)
	case report.Softnet.GrantConfigured:
		fmt.Printf("         softnet  %s: grant configured, but it %s\n",
			report.Softnet.Path, report.Softnet.Why)
	case report.Softnet.Path != "":
		fmt.Printf("         softnet  %s %s\n", report.Softnet.Path, report.Softnet.Why)
	default:
		fmt.Printf("         softnet  %s\n", report.Softnet.Why)
	}

	// WHAT THIS NODE WILL DO WITH A FORK'S PULL REQUEST, in one line, because the
	// answer is a decision the operator made in config and not a property of the
	// host — and a node that silently ran untrusted work on the default NAT
	// would look exactly like one that refused it.
	switch {
	case tartCfg.UntrustedIsolation == "":
		fmt.Printf("         untrusted work will be refused: node.tart.untrusted_isolation " +
			"is not set, and tart's default NAT reaches the host\n")

	case !report.Softnet.GrantConfigured:
		// FATAL, and this is the case the whole block exists for: the config
		// says this node accepts untrusted work, and the host cannot confine
		// it. Reporting it and exiting zero is how a deployment believes it has
		// isolation it does not have.
		return fmt.Errorf("node.tart.untrusted_isolation is %q, so this node offers to run "+
			"untrusted work, but softnet %s — every untrusted launch would fail, and the "+
			"promise in the config is one this host cannot keep",
			tartCfg.UntrustedIsolation, report.Softnet.Why)

	default:
		fmt.Printf("         untrusted work runs under %s, resolving through %s\n",
			tartCfg.UntrustedIsolation, strings.Join(tartCfg.UntrustedDNS, ", "))
	}

	// THE IMAGES THIS NODE'S TIERS NAME, BY NAME. A launch REFUSES an image that
	// is not present rather than fetching one — tens of gigabytes must not travel
	// the node's single command queue — so "not pulled" is a tier that cannot run
	// a job, and it is worth saying before the first job rather than as its
	// failure. `billet images pull` is what fetches them.
	var missing []string

	// The SAME selection `billet images pull` makes, identity resolution
	// included — a check that listed different images from the command that
	// fetches them would send an operator in a circle.
	tierImages, err := tartTierImages(cfg)
	if err != nil {
		return err
	}

	for _, image := range tierImages {
		if p.Pulled(ctx, image) {
			fmt.Printf("image    %-56s pulled\n", image)

			continue
		}

		missing = append(missing, image)

		fmt.Printf("image    %-56s NOT pulled; every job on its tier will fail to launch\n", image)
	}

	if len(missing) > 0 {
		fmt.Printf("         fetch them with `billet images pull` (each is tens of GB)\n")
	}

	// SAID, BECAUSE THE CHECK IS STILL NARROWER THAN IT LOOKS: a pulled image is
	// not proof that its guest carries the tart guest agent the registration
	// delivery needs, nor that a macOS guest slot is free under Apple's two-guest
	// licence. Both surface at launch, not here.
	fmt.Printf("         (read only — launching also needs the tart guest agent inside the " +
		"image and a free macOS guest slot under Apple's two-VM licence)\n")

	return nil
}

// deploymentForCheck identifies nothing. The provider requires a deployment because
// it marks the jails it creates with one, and this constructs a provider only to
// ask it questions about the host.
const deploymentForCheck = "00000000000000000000000000000000"

// noRootDisk stands in for the storage a preflight does not use.
//
// The provider refuses a nil one — every guest boots from a clone, so a nil
// interface would panic on the first job — and `billet check` proves the cluster
// separately, through the ceph client, where the diagnostic is about storage rather
// than about microVMs.
type noRootDisk struct{}

func (noRootDisk) ResolveGeneration(_ context.Context, image, _ string) (string, error) {
	return image, nil
}

func (noRootDisk) CloneRoot(context.Context, string, string, config.ByteSize) (string, error) {
	return "", errors.New("billet: the preflight does not clone a root disk")
}

func (noRootDisk) DiscardRoot(context.Context, string) error { return nil }

// KernelFor answers "nothing recorded", which is the truthful answer from a node
// with no cluster to have recorded anything in — and the caller treats it as the
// fallback case rather than an error.
func (noRootDisk) KernelFor(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// GenerationGone is false: a node with no cluster has no generations to lose, and
// answering true would have the launch re-resolve an alias forever.
func (noRootDisk) GenerationGone(error) bool { return false }

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

// printReportedInventory shows what each host last SAID it was running.
//
// IT IS EVIDENCE AND NEVER CLEARANCE, and the shape of this output is the
// defence rather than the wording. A node lists its provider and THEN posts the
// result, so the control plane learns when a report ARRIVED and never when the
// snapshot was taken -- and a launch can be handed to that host immediately
// afterwards. Nothing here is aggregated into a fleet-wide verdict, nothing here
// changes an exit status, and a zero is rendered as one host's stale opinion
// rather than as an idle machine.
//
// It exists because the ledger genuinely cannot answer "is anything running on
// that box", and until now the only thing billet could say was to go and look
// somewhere else -- for a fact the control plane had already been told.
// IT RETURNS NOTHING, and that is the point rather than an oversight. This
// section is telemetry; the sections around it are the ledger's own answers. A
// failure to read one host's last word must not change what `billet status`
// exits with, and must not stop `held` — which IS authoritative — from
// printing. An earlier version returned the error, which made a telemetry read
// decide the command's exit status: the exact thing the rest of this comment
// says it must never do.
func printReportedInventory(ctx context.Context, a *alloc.Allocator) {
	fleet, err := a.NodeInventories(ctx)
	if err != nil {
		fmt.Printf("reported  unavailable: %v\n", err)

		return
	}

	if len(fleet) == 0 {
		return
	}

	fmt.Printf("reported  what each host last said it was running. This is the HOST'S OWN\n")
	fmt.Printf("          last word, not a check billet made, and a snapshot it took before\n")
	fmt.Printf("          it sent it — work can have started on that host since.\n")

	for _, inv := range fleet {
		fmt.Printf("          %-24s ", inv.Node)

		switch {
		case inv.Report == nil:
			// NOT THE SAME AS REPORTING NOTHING, and the distinction is the whole
			// reason the epoch is stored beside the count.
			fmt.Printf("has not reported since it last reconnected")
		case inv.Report.ReportedRunning > 0:
			// THE ONE ANSWER HERE THAT IS WORTH ACTING ON. A host that says it is
			// running something is telling you a fact; a host that says it is
			// running nothing is telling you about a moment that has passed.
			fmt.Printf("SAYS IT IS RUNNING %d", inv.Report.ReportedRunning)
		default:
			fmt.Printf("saw 0 billet instances when it last looked")
		}

		if !inv.Live {
			fmt.Printf(" (this deployment cannot reach it)")
		}

		if inv.Report != nil && inv.Report.ReceivedAt != "" {
			fmt.Printf(", received %s", inv.Report.ReceivedAt)
		}

		fmt.Println()
	}
}

// printComputeBarrier reports a drain's outstanding question to the fleet, and
// every host somebody removed from the set it expects to hear from.
//
// TWO SECTIONS, NEVER ONE VERDICT. The barrier's per-host states are what a
// drain is waiting on, and the exclusions are what it will not wait on — and an
// UNPROVEN exclusion is printed whether or not a barrier is running, because it
// is a standing fact about this deployment rather than a detail of one drain.
//
// IT RETURNS NOTHING, for the reason printReportedInventory gives above: this
// must not decide what `billet status` exits with, and must not stop the
// authoritative sections below it from printing.
func printComputeBarrier(ctx context.Context, a *alloc.Allocator) {
	clearance, err := a.ComputeClear(ctx)
	if err != nil {
		fmt.Printf("barrier   unavailable: %v\n", err)

		return
	}

	if len(clearance.Excluded) > 0 {
		fmt.Printf("excluded  %d host(s) billet no longer expects an answer from\n",
			len(clearance.Excluded))

		for _, e := range clearance.Excluded {
			fmt.Printf("          %-24s ", e.Node)

			if e.Proven {
				fmt.Printf("proved idle before it was removed")
			} else {
				// THE LINE THIS SECTION EXISTS FOR. A forced exclusion is billet
				// saying it does not know what is on that machine, and a report that
				// rendered it the same as a proven one would launder exactly the
				// uncertainty membership is allowed to skip past.
				fmt.Printf("REMOVED WITHOUT PROOF — nothing knows what it is running")
			}

			if e.Actor != "" {
				fmt.Printf(" (%s)", e.Actor)
			}

			fmt.Println()
		}
	}

	if !clearance.Requested {
		return
	}

	if clearance.Clear() {
		fmt.Printf("barrier   every host billet expects an answer from says it is running no\n")
		fmt.Printf("          compute, and has said so continuously\n")

		return
	}

	blocking := clearance.Blocking()

	fmt.Printf("barrier   a drain is asking the fleet what it is running; %d host(s) have not\n",
		len(blocking))
	fmt.Printf("          been proved idle\n")

	for _, n := range blocking {
		fmt.Printf("          %-24s %s", n.Node, n.State)

		switch n.State {
		case alloc.ClearanceSettling:
			// See clearanceSummary: the timestamp is when another empty answer
			// would prove the run, not a moment at which it clears itself.
			fmt.Printf(" (needs another empty answer at or after %s)", n.ClearAt)
		case alloc.ClearanceBelowProtocol:
			fmt.Printf(" (wire %d)", n.WireVersion)
		case alloc.ClearanceUnknown, alloc.ClearanceProved, alloc.ClearanceRunning,
			alloc.ClearanceWaiting, alloc.ClearanceUnreachable:
		}

		fmt.Println()
	}
}

// printWireWindow reports which hosts are still on an older node wire.
//
// THE QUESTION IT ANSWERS IS "MAY THE OLD PROTOCOL BE RETIRED YET". A rollout
// is server-first — this control plane speaks a range, and nodes converge onto
// its newest version one at a time — so the operator needs to see exactly which
// hosts are holding the bottom of that range open, and a later release needs to
// know when nothing is.
//
// A HOST THIS DEPLOYMENT CANNOT REACH STILL COUNTS. It is not gone: its compute
// may be running and it will come back speaking whatever it spoke before, so
// writing it off would retire a protocol a live machine still needs.
func printWireWindow(ctx context.Context, a *alloc.Allocator) {
	fleet, err := a.NodeWireVersions(ctx)
	if err != nil {
		fmt.Printf("protocol  unavailable: %v\n", err)

		return
	}

	if len(fleet) == 0 {
		return
	}

	fmt.Printf("protocol  this control plane speaks %s\n", nodeapi.Self())

	var unrecorded, older, newer int

	for _, n := range fleet {
		spoken := "protocol unrecorded"
		if n.Negotiated > 0 {
			spoken = fmt.Sprintf("protocol %d", n.Negotiated)
		}

		fmt.Printf("          %-24s %-12s %-28s %s", n.Name, spoken, describeRelease(n),
			describeInstalled(n))

		// FOUR STATES, NOT TWO, because three of them are not "older". A row
		// written before this was recorded says nothing about what that host
		// speaks; a row written by a NEWER binary — an operator rolled the control
		// plane back — says something this build cannot serve. Calling either one
		// old asserts what billet does not know, and calling either converged
		// retires a protocol on the strength of a column this binary did not write.
		switch {
		case n.Negotiated == 0:
			unrecorded++

			fmt.Printf("  <- NOT RECORDED, SO IT STILL BLOCKS RETIREMENT")
		case n.Negotiated > nodeapi.Version:
			newer++

			fmt.Printf("  <- NEWER THAN THIS CONTROL PLANE, which cannot serve it")
		case n.Negotiated < nodeapi.Version:
			older++

			fmt.Printf("  <- OLDER THAN THIS CONTROL PLANE")
		}

		if !n.Live {
			fmt.Printf(" (this deployment cannot reach it)")
		}

		if note := describeDowngrade(n); note != "" {
			fmt.Printf("  <- %s", note)
		}

		fmt.Println()
	}

	behind := unrecorded + older + newer
	if behind == 0 {
		fmt.Printf("          every host speaks %d; the older protocols in that range are free "+
			"to drop in a later release\n", nodeapi.Version)

		return
	}

	fmt.Printf("          %d host(s) are not known to be on %d, so nothing below it may be "+
		"dropped.\n", behind, nodeapi.Version)

	// THE REMEDY IS PER STATE, BECAUSE ONE OF THEM POINTS THE OTHER WAY. A host
	// on a NEWER protocol is what a rolled-back control plane leaves behind, and
	// telling an operator to upgrade those hosts is backwards — the control plane
	// is the half that is behind. A blanket "upgrade them" contradicted the row it
	// was summarising.
	if older > 0 {
		fmt.Printf("          %d of them speak an older protocol: upgrade those hosts.\n", older)
	}

	if newer > 0 {
		fmt.Printf("          %d speak a protocol NEWER than this control plane, which cannot "+
			"serve it. Upgrade or restore the control plane; upgrading those hosts "+
			"cannot help.\n", newer)
	}

	if unrecorded > 0 {
		fmt.Printf("          %d have said nothing since this binary began recording it; they "+
			"report their protocol on their next registration.\n", unrecorded)
	}

	// WHAT AN OPERATOR CAN NOW DO, and the reason it is stated here rather than
	// left implicit. A host that is permanently gone keeps this window open — and
	// for a long time nothing could clear it, so this line said so rather than
	// advising a command that did not exist. `billet nodes decommission` is that
	// command; it refuses while the host is reachable or holds any lease, because
	// forgetting a row is only safe once nothing says its compute may still be
	// running.
	fmt.Printf("          A host that is gone for good still counts. Once it is stopped and " +
		"holds no lease,\n          `billet nodes decommission <node>` forgets it and closes " +
		"this window.\n")
}

// describeDowngrade says when a host is running something older than it once
// registered with, and stays silent otherwise.
//
// A NOTE, NOT A VERDICT. A rollout that failed on this host and rolled it back
// produces exactly this shape, and `billet rollout status` says whether one did;
// what this line adds is that somebody's hand producing the same shape is no
// longer invisible. Only a proved order is reported: a host whose current release
// cannot be ordered against its highest says nothing here.
func describeDowngrade(n alloc.NodeWire) string {
	if n.HighestRelease == "" || n.Release == "" {
		return ""
	}

	order, ok := version.Compare(n.Release, n.HighestRelease)
	if !ok || order >= 0 {
		return ""
	}

	return fmt.Sprintf("DOWNGRADED: it once registered on %s (a rollout's rollback does this; "+
		"`billet rollout status` says whether one did)", n.HighestRelease)
}

// describeRelease names a host's build, or says why it has no name.
//
// THE TWO SILENCES ARE DIFFERENT FACTS AND ONLY ONE IS ORDINARY. A host below
// the version from which a registration names its release genuinely has none to
// give — that is the whole installed fleet on the day this ships, and reporting
// it as a problem would bury the report in noise. A host at or above that version
// owes the field, so its silence is a build that is not saying what it is, and an
// operator planning an upgrade needs to know it will not be told.
func describeRelease(n alloc.NodeWire) string {
	if n.Release != "" {
		return n.Release
	}

	switch {
	case n.Negotiated == 0:
		// NEITHER OLD NOR NEW. Nothing is recorded about this host's protocol, so
		// any sentence with an age in it is a claim the ledger cannot support —
		// and this line sits beside one that correctly calls the row unrecorded,
		// so an age here makes the two halves contradict each other.
		return "release unrecorded"
	case n.Negotiated >= nodeapi.VersionNodeRelease:
		return "NAMED NO RELEASE (its protocol requires one)"
	default:
		return "release unknown (its protocol predates " +
			strconv.Itoa(nodeapi.VersionNodeRelease) + ")"
	}
}

// describeInstalled says which release manifest produced a host's binary.
//
// FOUR STATES, AND ONLY ONE OF THEM IS A DIGEST. A version string is the name a
// binary was BUILT with; two builds can share one and a moved tag makes them
// identical, so the manifest is the only thing that says which BYTES a host is
// running. What matters here is that the three ways of not knowing are not the
// same fact and must not print as one: a protocol that cannot carry the answer,
// a host billet did not install, and a row from before any of this existed lead
// an operator to three different places.
func describeInstalled(n alloc.NodeWire) string {
	if n.Digest != "" {
		return "manifest " + n.Digest[:12]
	}

	switch {
	case n.Negotiated == 0:
		// NOTHING IS RECORDED ABOUT THIS ROW AT ALL, so any sentence about what it
		// can or cannot say is a claim the ledger does not support.
		return "manifest unrecorded"
	case n.Negotiated >= nodeapi.VersionNodeDigest:
		return "NAMED NO MANIFEST (nothing there could say)"
	default:
		return "manifest unknown (its protocol predates " +
			strconv.Itoa(nodeapi.VersionNodeDigest) + ")"
	}
}

// printAdmission reports whether the deployment is taking new work.
//
// A SEALED DEPLOYMENT SAYS SO WITH ITS ATTRIBUTION, because the operator reading
// it is usually not the one who sealed it, and the question they actually have
// is "may I clear this" — which needs to know who took it and why.
func printAdmission(a state.Admission) {
	if a.Mode == state.AdmissionOpen {
		fmt.Printf("admission open\n")

		return
	}

	fmt.Printf("admission %s — this deployment is not taking new work\n", a.Mode)

	switch {
	case a.Actor != "" && a.Reason != "":
		fmt.Printf("          sealed by %s: %s\n", a.Actor, a.Reason)
	case a.Actor != "":
		fmt.Printf("          sealed by %s\n", a.Actor)
	case a.Reason != "":
		fmt.Printf("          %s\n", a.Reason)
	}

	if a.ChangedAt != "" {
		fmt.Printf("          since %s\n", a.ChangedAt)
	}

	// WHICH SEAL THIS IS decides who may clear it, and an operator staring at a
	// quiet fleet needs to know whether restarting the services will reopen it.
	switch a.Provenance {
	case state.ProvenanceLocalDown:
		fmt.Printf("          held by a shutdown; `billet local up` clears it\n")
	case state.ProvenanceOperator:
		fmt.Printf("          held deliberately; it survives a restart\n")
	}
}
