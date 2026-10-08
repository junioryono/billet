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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	opsimages "github.com/junioryono/billet/internal/ops/images"

	"github.com/junioryono/billet/internal/ops/cache"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// errNotImplemented marks a role that is scaffolded but cannot serve yet.
//
// It is returned immediately and non-zero rather than blocking. A process that
// idles until signalled looks healthy to systemd, Docker, and every uptime
// check, so a half-built control plane would be reported as running while no
// job is ever picked up. Failing loudly is the honest behaviour for pre-alpha.
var errNotImplemented = errors.New("not implemented yet")

// commands takes the lifecycle so the two long-running roles can close over it.
//
// Only `server` and `node` can be hurried — the rest either exit on their own or
// have nothing running to wait for — so it is captured by those two rather than
// widening every command's signature or, worse, becoming package state.
func main() {
	env := cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Getenv: os.Getenv}

	os.Exit(cli.Main(os.Args[1:], env, commands, os.Exit))
}

func commands(lc *cli.Lifecycle) []cli.Command {
	return []cli.Command{
		{Name: "server", Summary: "run the control plane (run `billet node` alongside it to run jobs here)",
			Run: func(ctx context.Context, env cli.Env, args []string) error { return cmdServer(ctx, env, lc, args) }},
		{Name: "node", Summary: "run a compute host that dials a control plane",
			Run: func(ctx context.Context, env cli.Env, args []string) error { return cmdNode(ctx, env, lc, args) }},
		{Name: "nodes", Summary: "approve the machines asking to join this deployment",
			Run: cmdNodes},
		{Name: "ca", Summary: "issue the certificates nodes authenticate with",
			Run: cmdCA},
		{Name: "leases", Summary: "show capacity held for compute nobody has accounted for",
			Run: cmdLeases},
		{Name: "jobs", Summary: "show which GitHub job a lease ran and what it did to the host",
			Run: cmdJobs},
		{Name: "cache", Summary: "manage transparent Actions caching and install its conformance gate",
			Run: cache.Run},
		{Name: "check", Summary: "validate the config and state directory, then exit",
			Run: cmdCheck},
		{Name: "init", Summary: "generate a billet.yaml interactively",
			Run: cmdInit},
		{Name: "ami", Summary: "build and verify the machine image the ec2 backend launches",
			Run: opsimages.AMI},
		{Name: "runner", Summary: "report how close the pinned actions/runner is to being refused",
			Run: opsimages.Runner},
		{Name: "images", Summary: "verify the golden image a microVM guest boots from",
			Run: opsimages.Run},
		{Name: "fleet", Summary: "converge a fleet from this machine with the collection of this billet's release",
			Run: cmdFleet},
		{Name: "github-app", Summary: "create and install the GitHub App billet uses",
			Run: cmdGitHubApp},
		{Name: "teardown", Summary: "delete the scale sets billet created on GitHub",
			Run: cmdTeardown},
		{Name: "decommission", Summary: "remove the ec2 instances and cache billet made outside Terraform",
			Run: cmdDecommission},
		{Name: "local", Summary: "run the billet services on this machine, and back up or restore what makes " +
			"them this deployment",
			Run: cmdLocal},
		{Name: "drain", Summary: "stop admitting new work and let what is running finish",
			Run: cmdDrain},
		{Name: "resume", Summary: "start admitting work again after a drain",
			Run: cmdResume},
		{Name: "force-destroy", Summary: "DESTROY compute that is still running a job, failing those builds",
			Run: cmdForceDestroy},
		{Name: "rollout", Summary: "move this whole deployment to one release, and watch it converge",
			Run: cmdRollout},
		{Name: "host-upgrade", Summary: "replace billet on THIS machine transactionally, with rollback",
			Run: cmdHostUpgrade},
		{Name: "converge-guard", Summary: "hold the upgrade root's one claim for a converge, so no transaction " +
			"moves this host under it",
			Run: cmdConvergeGuard},
		{Name: "release", Summary: "record which signed manifest produced the billet installed here",
			Run: cmdRelease},
		{Name: "acceptance", Summary: "stand an ISOLATED deployment up beside this one, run a real job on " +
			"it, and destroy exactly what it made",
			Run: cmdAcceptance},
		{Name: "status", Summary: "show cluster status",
			Run: cmdStatus},
		{Name: "version", Summary: "print version information",
			Run: cmdVersion},
	}
}

func cmdServer(ctx context.Context, env cli.Env, lc *cli.Lifecycle, args []string) error {
	// `billet server retire` is a controller's retirement, an operator command
	// that runs under a converge guard; it never starts the plane.
	if len(args) > 0 && args[0] == "retire" {
		return cmdServerRetire(ctx, env, args[1:])
	}

	fs := cli.NewFlagSet("billet server", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	dryRun := fs.Bool("dry-run", false,
		"connect to GitHub and advertise ZERO capacity: proves the whole path without accepting a job")
	upgradeProbe := fs.Bool("upgrade-probe", false,
		"open candidate state without polling, dispatching, or accepting workload")
	holdProbeFlag := fs.Bool("upgrade-probe-hold", false,
		"with --upgrade-probe, stay up until stopped instead of exiting once ready; the "+
			"Ansible role's Type=notify probe unit passes this")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	if *dryRun && *upgradeProbe {
		return errors.New("billet server: --dry-run and --upgrade-probe are mutually exclusive")
	}
	if *holdProbeFlag && !*upgradeProbe {
		return errors.New("billet server: --upgrade-probe-hold needs --upgrade-probe")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Server == nil {
		return fmt.Errorf("%s has no server section", *cfgPath)
	}
	if len(cfg.GitHubTargets()) == 0 {
		return fmt.Errorf("%s has no github section and no targets; run `billet github-app create` first", *cfgPath)
	}

	// THE CONTROL PLANE RUNS NO COMPUTE. A single machine runs `billet server` and
	// `billet node` as two processes over loopback.
	//
	// A control plane with no nodes advertises nothing, so an empty fleet is harmless:
	// GitHub is told zero and assigns nothing.
	//
	// --dry-run remains for proving the GitHub path while advertising zero.

	return runServer(ctx, env, lc, cfg, *dryRun, *upgradeProbe, *holdProbeFlag)
}

// runServer starts the control plane and blocks until it is told to stop.
//
// THE ORDER IS THE TYPES': internal/app's control plane hands out a Controller
// only from BecomeController, and the node wire only to a controller that has
// forgotten the fleet and adopted the authority, so a step moved above the claim
// does not compile. What stays here is how the process was invoked: the
// scale-set clients, the probe's hold, the signals, and what exit status a stop
// earns.
func runServer(
	ctx context.Context, env cli.Env,
	lc *cli.Lifecycle,
	cfg *config.Config,
	dryRun, upgradeProbe, holdProbeFlag bool,
) error {
	// Built by the SHARED constructor, one client per target, so the server and
	// teardown authenticate identically. Two near-identical constructions is how
	// one of them ends up pointed at a different owner than the other.
	targets, err := newScaleSetClients(ctx, cfg)
	if err != nil {
		return err
	}

	cp, err := app.OpenControlPlane(ctx, cfg, serverHost(env), targets,
		app.ControlPlaneOptions{Probe: upgradeProbe})
	if err != nil {
		return err
	}

	defer cp.Close()

	if upgradeProbe {
		if err := notifyReady(env); err != nil {
			return fmt.Errorf("server upgrade-probe readiness: %w", err)
		}
		holdProbe(ctx, env, holdProbeFlag, serverProbeReadyLine)

		return nil
	}

	// Ctrl-C and SIGTERM stop the listeners through the context, which is what
	// releases escrowed capacity — see the listener's deferred release. A hard
	// kill skips that and leaves the reaper to expire it.
	//
	// AND IT IS INSTALLED BEFORE THE CLAIM, because a standby may wait there for
	// days and an operator stopping one must not have to kill it.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// THE CONTROLLER CLAIM, BEFORE ANYTHING POLLS GITHUB OR DISPATCHES A NODE.
	//
	// A STOP WHILE THIS HOST IS STILL TRYING IS A STOP, NOT A FAILURE. See
	// stoppedBeforeTheClaim: exiting non-zero here left systemd holding a failed
	// unit for a standby that was asked to stop, and `billet server retire`
	// refuses to act against one.
	ctl, err := cp.BecomeController(ctx, stop)
	if err != nil {
		// THE FENCE IS READ AFTER THE ATTEMPT, not beside it: the claim's own
		// write is one a successor can refuse.
		return stoppedBeforeTheClaim(ctx, env, cp.LeadershipLost(), err)
	}

	// THE LOOPS ARE JOINED BEFORE THE LEDGER CLOSES: this runs ahead of cp.Close.
	defer ctl.Close()

	adopted, err := ctl.AdoptAuthority(ctx)
	if err != nil {
		return err
	}

	fleet, err := ctl.ForgetFleet(ctx)
	if err != nil {
		return err
	}

	wire, err := ctl.ServeWire(ctx, fleet, adopted)
	if err != nil {
		return err
	}

	defer wire.Stop()

	published, err := ctl.PublishAuthority(ctx, wire)
	if err != nil {
		return err
	}

	scheduler, err := ctl.Schedule(wire, published, app.ScheduleOptions{Hurry: lc.Hurry(), DryRun: dryRun})
	if err != nil {
		return err
	}

	err = scheduler.Run(ctx)

	// A FENCED CONTROLLER'S ERROR IS ITS OWN, never a GitHub access problem to
	// explain: Run already says what happened and what a restart does. ASKED OF
	// THE ERROR, not of the ledger again: Run read the fence once, after the
	// plane stopped, and a second read here could see a fence latched since and
	// still return the nil Run answered before it.
	if errors.Is(err, state.ErrLeadershipLost) {
		return err
	}

	if err != nil {
		return explainGitHubAccess(ctx, cfg, err)
	}

	fmt.Fprintln(env.Stdout, "billet server: stopped")

	return nil
}

// serverHost is what the control plane takes from this process and machine:
// the identity exclusions the host authority provides, the service manager's
// notifications, and stdout for the lines an operator reads.
func serverHost(env cli.Env) app.Host {
	return app.Host{
		ServerAccess:  hostauthority.ServerWireAccess,
		AuthorityLock: authorityLockAccess,
		Ready:         func() error { return notifyReady(env) },
		Status:        func(text string) error { return notifyStatus(env, text) },
		Out:           env.Stdout,
	}
}

func cmdNode(ctx context.Context, env cli.Env, lc *cli.Lifecycle, args []string) error {
	// THE NODE'S OWN SUBCOMMANDS, before the role's flags: the endpoint
	// migration and the receipt are commands about the node this host runs,
	// invoked by the role and never by the service.
	if len(args) > 0 {
		switch args[0] {
		case "migrate-endpoint":
			return cmdNodeMigrate(ctx, env, args[1:])
		case "receipt":
			return cmdNodeReceipt(ctx, env, args[1:])
		}
	}

	fs := cli.NewFlagSet("billet node", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
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
	holdProbeFlag := fs.Bool("upgrade-probe-hold", false,
		"with --upgrade-probe, stay up until stopped instead of exiting once ready; the "+
			"Ansible role's Type=notify probe unit passes this")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	if *enroll && *upgradeProbe {
		return errors.New("billet node: --enroll and --upgrade-probe are mutually exclusive")
	}
	if *holdProbeFlag && !*upgradeProbe {
		return errors.New("billet node: --upgrade-probe-hold needs --upgrade-probe")
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
		return enrollNode(ctx, env, cfg, bootstrapBase(cfg, *bootstrapAddr), *caFingerprint, *joinToken)
	}

	upgrader, err := nodeUpgrader(cfg, *cfgPath)
	if err != nil {
		return err
	}

	// THE IDENTITY, ITS LOCK AND THE PROVIDER, IN THAT ORDER: see app.OpenNode.
	n, err := app.OpenNode(cfg, app.NodeOptions{Upgrader: upgrader})
	if err != nil {
		return err
	}

	defer func() {
		if err := n.Close(); err != nil {
			slog.Default().Warn("could not release the deployment lock", "error", err)
		}
	}()

	if *upgradeProbe {
		if err := notifyReady(env); err != nil {
			return fmt.Errorf("node upgrade-probe readiness: %w", err)
		}
		holdProbe(ctx, env, *holdProbeFlag, fmt.Sprintf(nodeProbeReadyFormat, n.Name()))

		return nil
	}

	// THE HANDLER BEFORE THE REPORT that says this process has one, and neither
	// for a probe, which is not the node a stop is asking.
	stopDrainRequests := lc.HandleDrainRequests()
	defer stopDrainRequests()
	publishNodeDrainReport(hostOS)

	return n.Run(ctx, nodeHost(env, lc, hostOS))
}

// nodeHost is what the node takes from this process and machine: the service
// manager's notification, the drain request a stop may carry, the second
// signal, stdout, and where the registration record is published on platform.
func nodeHost(env cli.Env, lc *cli.Lifecycle, platform string) app.NodeHost {
	return app.NodeHost{
		Ready:                  func() error { return notifyReady(env) },
		DrainRequested:         nodeDrainRequested,
		Hurry:                  lc.Hurry(),
		Out:                    env.Stdout,
		RegistrationRecordPath: nodeRegistrationRecordPath(platform),
	}
}

// githubAPIBase is the API base cmdCheck verifies the App against — a var
// rather than the constant so tests can point it at a fake instead of the real
// GitHub, which a unit test must never reach. Empty selects the default.
var githubAPIBase = ""

// iamEndpointOverride points the instance-profile probe at a fake for tests —
// production always derives the partition-global IAM endpoint from the region.
var iamEndpointOverride = ""

func cmdVersion(_ context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet version", env.Stdout)
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	// The release version, not just the revision. This printed a bare commit sha,
	// which is true and unhelpful: an operator comparing what is installed against
	// what was released has to go and look the sha up.
	fmt.Fprintf(env.Stdout, "billet %s\n", version.String())

	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Fprintf(env.Stdout, "  go %s\n", info.GoVersion)
	}

	return nil
}
