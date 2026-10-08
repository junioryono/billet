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
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/provider/tart"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/store/ceph"
	"github.com/junioryono/billet/internal/store/ebss3"
	"github.com/junioryono/billet/internal/version"
	"github.com/junioryono/billet/internal/wirecert"
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
			Run: cmdCache},
		{Name: "check", Summary: "validate the config and state directory, then exit",
			Run: cmdCheck},
		{Name: "init", Summary: "generate a billet.yaml interactively",
			Run: cmdInit},
		{Name: "ami", Summary: "build and verify the machine image the ec2 backend launches",
			Run: cmdAMI},
		{Name: "runner", Summary: "report how close the pinned actions/runner is to being refused",
			Run: cmdRunner},
		{Name: "images", Summary: "verify the golden image a microVM guest boots from",
			Run: cmdImages},
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

func cmdServer(ctx context.Context, env cli.Env, lc *cli.Lifecycle, args []string) error {
	// `billet server retire` is a controller's retirement, an operator command
	// that runs under a converge guard; it never starts the plane.
	if len(args) > 0 && args[0] == "retire" {
		return cmdServerRetire(ctx, env, args[1:])
	}

	fs := cli.NewFlagSet("billet server", env.Stdout)
	cfgPath := addConfigFlag(fs)
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
		ServerAccess:  serverWireAccess,
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
func cmdTeardown(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet teardown", env.Stdout)
	cfgPath := addConfigFlag(fs)
	tier := fs.String("tier", "", "delete the scale set with this name (a tier's runs_on, which defaults to its label)")
	all := fs.Bool("all", false, "delete every tier's scale set")
	force := fs.Bool("force", false,
		"delete even if the scale set's labels are not this tier's (requires --tier)")
	group := fs.String("runner-group", "",
		"the runner group to look in, for a --tier the config no longer declares")
	targetName := fs.String("target", "",
		"the one target to act on for --tier (default: every target declaring it, or the only one)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// Checked HERE rather than inside the client. config.Load accepts a node-only
	// config with no github section, and everything below resolves a target — so
	// without this a node config fails obscurely instead of explaining itself.
	if len(cfg.GitHubTargets()) == 0 {
		return fmt.Errorf("%s has no github section and no targets, so it names no "+
			"organization or repository to delete anything from", *cfgPath)
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

	if *all && *targetName != "" {
		return errors.New("--target scopes one --tier; --all walks every target")
	}

	// --target SCOPES THE NAME TO ONE TARGET. Several targets may declare one
	// scale-set name, and one may have stopped declaring it while another still
	// does; unscoped, the name matches the live set and the leftover stays.
	candidates := cfg.Tiers
	if *targetName != "" {
		if candidates, err = tiersOnTarget(cfg, *targetName); err != nil {
			return err
		}
	}

	wanted, undeclared, err := teardownTargets(candidates, *tier, *group, *force)
	if err != nil {
		return err
	}

	// SAID OUT LOUD, because this is the one path that acts on something the
	// config does not describe. The operator is deleting by name in a group they
	// named, and nothing was cross-checked against a tier definition.
	if undeclared {
		fmt.Fprintf(env.Stdout, "%q is not a tier in %s. Deleting it by name from runner group %q.\n\n",
			*tier, *cfgPath, groupOrDefault(*group))
	}

	// EVERY TARGET IN CONFIG ORDER, each with its own client and its own
	// confirmation: a scale set lives on exactly one owner, and the credential
	// that deletes it is that owner's App.
	for _, target := range cfg.GitHubTargets() {
		mine := make([]config.Tier, 0, len(wanted))

		for i := range wanted {
			t := &wanted[i]

			switch {
			case undeclared:
				// An undeclared tier names its target with --target, or has the
				// only target there is.
				resolved, err := targetByName(cfg, *targetName)
				if err != nil {
					return err
				}

				if resolved.Name == target.Name {
					mine = append(mine, *t)
				}
			default:
				resolved, ok := cfg.TierTarget(t)
				if ok && resolved.Name == target.Name {
					mine = append(mine, *t)
				}
			}
		}

		if len(mine) == 0 {
			continue
		}

		if err := teardownOnTarget(ctx, env, cfg, target, mine, *force, *yes); err != nil {
			return err
		}
	}

	fmt.Fprintln(env.Stdout, "Done.")

	return nil
}

// teardownOnTarget removes the wanted scale sets from one target.
func teardownOnTarget(
	ctx context.Context, env cli.Env, cfg *config.Config, target config.GitHubTarget,
	wanted []config.Tier, force, yes bool,
) error {
	client, err := newScaleSetClientFor(ctx, cfg, target)
	if err != nil {
		return err
	}

	path := target.Path()

	// The ACTUAL objects, fetched before anything is destroyed. An operator
	// confirming a destructive act should be shown what is on GitHub, not the
	// names they typed into their own config.
	fmt.Fprintf(env.Stdout, "This deletes the following from %s (target %s):\n\n", describeGitHubTarget(target), target.Name)

	present := make([]config.Tier, 0, len(wanted))

	for i := range wanted {
		t := &wanted[i]

		set, labels, err := client.Describe(ctx, t.ScaleSetName(), t.RunnerGroup)
		if err != nil {
			return err
		}

		if set == nil {
			fmt.Fprintf(env.Stdout, "  %-32s not present\n", t.ScaleSetName())

			if err := forgetScaleSet(ctx, cfg, path, groupOrDefault(t.RunnerGroup), t.ScaleSetName()); err != nil {
				fmt.Fprintf(env.Stdout, "  %-32s billet could not forget it (%v); the control plane "+
					"will keep reporting it\n", t.ScaleSetName(), err)
			}

			continue
		}

		present = append(present, *t)

		fmt.Fprintf(env.Stdout, "  %-32s id %d, group %s, labels %v\n", t.ScaleSetName(), set.ID, set.Group, labels)
	}

	if len(present) == 0 {
		fmt.Fprintln(env.Stdout, "\nNothing to do here.")

		return nil
	}

	fmt.Fprintln(env.Stdout, "\nRunners already registered to them are removed by GitHub.")

	if !yes {
		if err := confirmTarget(ctx, env, path); err != nil {
			return err
		}
	}

	for i := range present {
		t := &present[i]

		deleted, err := client.DeleteScaleSet(ctx, t.ScaleSetName(), t.RunnerGroup, []string{t.ScaleSetName()}, force)
		if err != nil {
			return err
		}

		// Reported distinctly. Absence is scoped to the runner group being asked
		// about, so a tier created under a different group reports "not present"
		// here while the original survives — and an operator who reads that as
		// "deleted" walks away from an object that is still there.
		if !deleted {
			fmt.Fprintf(env.Stdout, "%s: nothing in runner group %q; if it was created under a different "+
				"group it is still there\n", t.ScaleSetName(), groupOrDefault(t.RunnerGroup))

			continue
		}

		if err := forgetScaleSet(ctx, cfg, path, groupOrDefault(t.RunnerGroup), t.ScaleSetName()); err != nil {
			fmt.Fprintf(env.Stdout, "%s: deleted, but billet could not forget it had created it (%v); "+
				"the control plane will keep reporting it until this is cleared\n", t.ScaleSetName(), err)
		}
	}

	return nil
}

// groupOrDefault names the runner group a tier resolves to, for messages.
func groupOrDefault(group string) string {
	if group == "" {
		return scaleset.DefaultRunnerGroup
	}

	return group
}

// cmdCA issues the certificates the node wire authenticates with.
//
// RUN ON THE CONTROL PLANE, because that is where the authority's private key
// is and where it stays. The bundle it writes is copied to the node — the key
// travels once, by an operator, rather than over a wire that does not yet trust
// anybody.
func cmdCA(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet ca issue <node> [--out <dir>] | billet ca token | " +
			"billet ca rotate | billet ca retire | billet ca revoke <node> | " +
			"billet ca revocations | billet ca show")
	}

	switch args[0] {
	case "issue":
		return cmdCAIssue(ctx, env, args[1:])
	case "revoke":
		return cmdCARevoke(ctx, env, args[1:])
	case "revocations":
		return cmdCARevocations(ctx, env, args[1:])
	case "token":
		return cmdCAToken(ctx, env, args[1:])
	case "rotate":
		return cmdCARotate(ctx, env, args[1:])
	case "retire":
		return cmdCARetire(ctx, env, args[1:])
	case "show":
		return cmdCAShow(ctx, env, args[1:])
	case "sync":
		return cmdCASync(ctx, env, args[1:])
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
func cmdCARevoke(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet ca revoke", env.Stdout)
	cfgPath := addConfigFlag(fs)
	certPath := fs.String("cert", "", "the certificate to revoke (default <node>-billet-tls/node.crt)")
	reason := fs.String("reason", "", "why, recorded alongside it")

	name, err := cli.ParseWithName(fs, args)
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
		Shares:    cfg.TargetShares(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	if err := allocator.RevokeCert(ctx, serial, name, *reason); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "Revoked %s (node %s)\n", serial, name)
	fmt.Fprintf(env.Stdout, "\nIt is refused on the next request that certificate makes. Issue a replacement\n")
	fmt.Fprintf(env.Stdout, "with `billet ca issue %s` if the machine is coming back.\n", name)

	return nil
}

// cmdCARevocations lists what has been withdrawn.
func cmdCARevocations(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet ca revocations", env.Stdout)
	cfgPath := addConfigFlag(fs)

	if err := cli.Parse(fs, args); err != nil {
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
		Shares:    cfg.TargetShares(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	revoked, err := allocator.RevokedCerts(ctx)
	if err != nil {
		return err
	}

	if len(revoked) == 0 {
		fmt.Fprintln(env.Stdout, "No certificates have been revoked.")

		return nil
	}

	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
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

func cmdCAIssue(ctx context.Context, env cli.Env, args []string) (err error) {
	fs := cli.NewFlagSet("billet ca issue", env.Stdout)
	cfgPath := addConfigFlag(fs)
	out := fs.String("out", "", "directory to write the bundle to (default ./<node>-billet-tls)")
	reissue := fs.Bool("reissue", false,
		"deliberately replace an existing bundle directory (the old one is moved to "+
			"<dir>.replaced; the old certificate stays valid until revoked)")
	lifetime := fs.Duration("lifetime", wirecert.LeafLifetime,
		"how long the certificate is good for (default a year; the node renews it on its "+
			"own once less than a third remains). Shorter for a short-lived host or a "+
			"rotation rehearsal; never below "+wirecert.MinIssuedLifetime.String())

	name, err := cli.ParseWithName(fs, args)
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

	// HELD THROUGH THE LEDGER RECORD BELOW, not released after the authority
	// load: `recordIssued` opens the ledger, which creates the directory and its
	// lock on first use, and an issue interrupted by a closure between the load
	// and the record would otherwise open a directory a retirement had moved.
	acc, err := openIdentityAccess(ctx, cfg.Server.IdentityDir, identityIntent{create: true, wait: identityAccessWait})
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, acc.Release()) }()

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
	if err := recordIssued(ctx, env, *cfgPath, name, bundle); err != nil {
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
		fmt.Fprintf(env.Stdout, "billet ca: the previous bundle was moved to %s.replaced. The node keeps "+
			"using its old key until this new bundle is installed and the node restarts, and "+
			"the OLD certificate stays valid until you revoke it:\n\n"+
			"  billet ca revoke %s --cert %s --reason reissued\n\n",
			dir, shellArg(name), shellArg(filepath.Join(dir+".replaced", "node.crt")))
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}

	fmt.Fprintf(env.Stdout, "billet ca: wrote a bundle for node %q to %s\n\n", name, abs)
	fmt.Fprintf(env.Stdout, "  fingerprint  %s\n\n", wirecert.Fingerprint(mustSPKI(bundle)))
	fmt.Fprint(env.Stdout, "Copy that directory to the node, then point its config at the files:\n\n")
	fmt.Fprintf(env.Stdout, "  node:\n    tls:\n      cert: /etc/billet/tls/node.crt\n"+
		"      key:  /etc/billet/tls/node.key\n      ca:   /etc/billet/tls/ca.crt\n\n")
	fmt.Fprint(env.Stdout, "node.name comes from the certificate, so it does not have to be written.\n")
	fmt.Fprint(env.Stdout, "node.key is a private key: keep it 0600 and do not copy it anywhere else.\n")

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
func recordIssued(ctx context.Context, env cli.Env, cfgPath, name string, bundle wirecert.Bundle) error {
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
		fmt.Fprintf(env.Stdout, "\nNOTE: %s was already admitted as %s.\nThat key can no longer be used "+
			"under this name; revoke its certificate if the machine still holds it.\n",
			name, displaced)
	}

	return nil
}

func cmdCAShow(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet ca show", env.Stdout)
	cfgPath := addConfigFlag(fs)

	if err := cli.Parse(fs, args); err != nil {
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

	acc, err := openIdentityAccess(ctx, cfg.Server.IdentityDir, identityIntent{create: true, wait: identityAccessWait})
	if err != nil {
		return err
	}

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return errors.Join(err, acc.Release())
	}

	ca, err := wirecert.LoadOrCreateCA(cfg.Server.IdentityDir, deployment)
	if err := errors.Join(err, acc.Release()); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "deployment  %s\nauthority   %s\nexpires     %s\nfingerprint %s\n",
		deployment, wirecert.CADir(cfg.Server.IdentityDir), ca.NotAfter().Format(time.RFC3339),
		ca.Fingerprint())

	if left, capping := ca.Capping(); capping {
		fmt.Fprintf(env.Stdout, "\nWARNING: this authority expires in %s, which is less than a certificate's\n",
			left.Round(24*time.Hour))
		fmt.Fprintf(env.Stdout, "full life, so every certificate it issues from now on is SHORTER than the\n")
		fmt.Fprintf(env.Stdout, "last — and when it expires, every node stops at once. Nothing will error\n")
		fmt.Fprintf(env.Stdout, "before that. Plan a rotation: issue a new authority, let nodes pick it up\n")
		fmt.Fprintf(env.Stdout, "through renewal while both are trusted, then retire the old one.\n")
	}

	fmt.Fprintf(env.Stdout, "\nGive the fingerprint to a node that is enrolling, so it can tell this control\n")
	fmt.Fprintf(env.Stdout, "plane from anything else that answers:\n\n")
	fmt.Fprintf(env.Stdout, "  billet node --enroll --ca-fingerprint %s%s\n",
		ca.Fingerprint(), enrollAddrFlag(cfg))

	if cfg.Server.BootstrapListen == "" && !nodeplane.LoopbackOnly(cfg.Server.Listen) {
		fmt.Fprintf(env.Stdout, "\nThis control plane serves no enrollment address, so that command has\n")
		fmt.Fprintf(env.Stdout, "nowhere to ask: its node wire requires a certificate an enrolling machine\n")
		fmt.Fprintf(env.Stdout, "does not have yet. Either issue the bundle here and copy it out of band:\n\n")
		fmt.Fprintf(env.Stdout, "  billet ca issue <node>\n\n")
		fmt.Fprintf(env.Stdout, "or set server.bootstrap_listen and restart.\n")
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
// EVERY REMOTE BACKEND, THROUGH app.RemoteShapes, and it used to read node.ec2 directly.
// A codebuild node declares ordered shapes with a price per hour for the same reason
// an ec2 node does — placement charges the first that fits — so reading one block by
// name meant a check that reported the cost exposure of an ec2 node and stayed silent
// for a codebuild one, which reads as compute that is free.
//
// THE NODE'S PROVIDER NAMES ITSELF in the line, because the two backends bill for
// different things: an ec2 shape is an instance-hour, a codebuild compute type is a
// build-minute rate expressed per hour, and an operator comparing the two numbers
// needs to know which they are looking at.
func printRemoteCost(env cli.Env, cfg *config.Config) error {
	shapes := app.RemoteShapes(cfg)
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

	fmt.Fprintf(env.Stdout, "%-8s <= %s compute (%s/month at 730h), from declared shape prices\n",
		string(cfg.Node.Provider)+" max", &peak, peak.ForHours(730))

	return nil
}

func cmdStatus(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet status", env.Stdout)
	cfgPath := addConfigFlag(fs)
	if err := cli.Parse(fs, args); err != nil {
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
	printAdmission(env, admission)

	// SECOND, AND FOR THE SAME REASON. A force-destroy is the one thing in billet
	// that ends running work, so an operator who finds builds failing needs to see
	// it before the capacity numbers that will look perfectly healthy underneath.
	printForceDestroy(ctx, env, a, admission)

	// AND A ROLLOUT IN ONE LINE, because it explains the other half of what an
	// operator is looking at: hosts on two versions, capacity down by one machine,
	// a node reporting nothing. `billet rollout status` is the full picture; this
	// is what says to go and look at it.
	printRollout(ctx, env, db)

	// AND THE HOST'S OWN GUARD, read from this host's upgrade root and never
	// from the ledger: a converge holding this host is why a rollout is refusing
	// to move it.
	printGuard(env)

	// AND WHO THE DEPLOYMENT'S CONTROLLER IS, because the epoch beside it is a
	// fence rather than a note. Every write is refused once that number moves, so
	// an operator looking at a control plane that has gone quiet needs to be able
	// to see whether something else took it over — and, on PostgreSQL, which
	// machine to go and look at.
	printController(ctx, env, db)

	usage, err := a.Usage(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "capacity  %d of %d vCPU, %s of %s, %d open leases\n",
		usage.VCPU, cfg.Server.MaxVCPU, usage.Memory, cfg.Server.MaxMemory, usage.Leases)

	// A CEILING BELOW THE HOSTS IS SILENT OTHERWISE. The deployment ceiling caps
	// every node, so a host added without raising it registers and advertises and
	// never gets its own room; said here because nothing else ever says it.
	hostVCPU, hostMemory, hosts, err := a.PlaceableContribution(ctx)
	if err != nil {
		return err
	}

	if hostVCPU > cfg.Server.MaxVCPU || hostMemory > cfg.Server.MaxMemory {
		fmt.Fprintf(env.Stdout, "ceiling   BELOW THE HOSTS: the %d live hosts contribute %d vCPU and %s, "+
			"and server.max_vcpu / max_memory allow %d and %s, so the ceiling, not the hosts, "+
			"decides what runs; raise the ceiling to the hosts' sum, or cap a host with "+
			"node.max_vcpu / max_memory\n",
			hosts, hostVCPU, hostMemory, cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	// GROUPED BY TARGET WHEN THERE ARE SEVERAL, because a tier's scale set lives
	// on exactly one owner and an operator reading capacity per label needs to
	// know which owner's jobs it serves.
	targets := cfg.GitHubTargets()

	for _, target := range targets {
		if len(targets) > 1 {
			fmt.Fprintf(env.Stdout, "target    %s (%s)\n", target.Name, describeGitHubTarget(target))
		}

		for i := range cfg.Tiers {
			t := &cfg.Tiers[i]
			if len(targets) > 1 && t.Target != target.Name {
				continue
			}

			report, err := a.CapacityReport(ctx, t.Label)
			if err != nil {
				return err
			}
			printTierCapacity(env.Stdout, tierDisplay(t), report, time.Now())
		}
	}

	if err := printRemoteFleetCost(ctx, env, a, cfg); err != nil {
		return err
	}

	// WHAT THE CONTROL PLANE HAS SWEPT out of Parameter Store, and which codebuild
	// hosts it cannot sweep after. A leaked registration is one nobody sees, which
	// is why the count is durable and printed rather than logged.
	printCredentialSweeps(ctx, env, a, db)

	printReportedInventory(ctx, env, a)
	printComputeBarrier(ctx, env, a)
	printWireWindow(ctx, env, a)
	printCacheAwareWaits(ctx, env, a, cfg.Tiers)

	held, err := a.Held(ctx)
	if err != nil {
		return err
	}

	// A RUNNING LEASE WHOSE HOLDER WAS REPLACED IS NOT HELD, and is exactly the
	// slot an operator finds taken with nothing below saying why.
	printReplacedHolders(ctx, env, a)

	if len(held) == 0 {
		fmt.Fprintln(env.Stdout, "held      none")

		return nil
	}

	fmt.Fprintf(env.Stdout, "held      %d lease(s) waiting for compute to be confirmed gone\n", len(held))
	printHeld(env, held)
	printHolderNote(env.Stdout, held)

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
func printController(ctx context.Context, env cli.Env, db *state.DB) {
	claim, err := db.ControllerHolder(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "claim     unavailable: %v\n", err)

		return
	}

	if claim.Holder == "" {
		fmt.Fprintln(env.Stdout, "claim     nothing has ever claimed this deployment's controller")

		return
	}

	fmt.Fprintf(env.Stdout, "claim     %s holds this deployment's controller, at epoch %d\n",
		claim.Holder, claim.Epoch)
}

// printRemoteFleetCost bounds what every registered remote node can cost per hour.
//
// IT COVERS THE WHOLE REMOTE FLEET rather than the ec2 half of it. The query behind
// it was scoped to `provider = 'ec2'`, so a deployment whose cloud capacity was
// codebuild printed nothing here — and the absence of a cost line is exactly what a
// free fleet looks like. See alloc.RemoteCostNodes.
func printRemoteFleetCost(ctx context.Context, env cli.Env, a *alloc.Allocator, cfg *config.Config) error {
	nodes, err := a.RemoteCostNodes(ctx)
	if errors.Is(err, alloc.ErrRemoteCostUnavailable) {
		fmt.Fprintf(env.Stdout, "cloud peak  unavailable (%v)\n", err)

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
	fmt.Fprintf(env.Stdout, "cloud peak <= %s compute (%s/month at 730h), across %d registered remote node(s) from declared shape prices\n",
		&peak, peak.ForHours(730), len(nodes))

	return nil
}

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

// checkEC2Credentials proves this machine can act on its ec2 configuration.
//
// THE SAME DISTINCTION github.ReadPrivateKeyFile MAKES, one credential over: config
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
	ctx context.Context, env cli.Env, cfg *config.Config, bundle *wirecert.Bundle,
	authorize, maintenanceProbe bool,
) error {
	// FIRST, before credentials resolve or anything dials AWS: during the
	// upgrade transaction's stopped-service window, an AWS or IMDS blip must
	// not read as a broken host and roll the upgrade back. The reachability
	// call used to survive the skip; it is a network call like the rest, and
	// the probe's job is the ledger and the config, not the cloud.
	if maintenanceProbe {
		fmt.Fprintf(env.Stdout, "aws      (all AWS checks skipped during maintenance)\n")

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
	// github.ReadPrivateKeyFile makes by parsing the key rather than stat-ing it.
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
		reportQuotas(ctx, env, cfg, p)
	}

	fmt.Fprintf(env.Stdout, "aws      %s in %s, subnet %s, %d instance shape(s), credentials %s (can describe)\n",
		spotLabel(ec2cfg.Spot), ec2cfg.Region, ec2cfg.SubnetID, len(ec2cfg.InstanceTypes),
		creds.AccessKeyID)

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. A read-only call says
	// nothing about permission to LAUNCH, and an operator who reads "ok" and then
	// watches every job fail on an IAM denial has been misled by this line.
	fmt.Fprintf(env.Stdout, "         (describe only — launching also needs at least ec2:RunInstances, "+
		"ec2:TerminateInstances, ec2:CreateTags and ec2:DescribeImages, plus iam:PassRole "+
		"if node.ec2.instance_profile is set)\n")
	if ec2cfg.Spot {
		fmt.Fprintf(env.Stdout, "         spot warnings are consumed from interruption_queue_url; the role also "+
			"needs sqs:ReceiveMessage, sqs:DeleteMessage and sqs:GetQueueAttributes on that queue\n")
	}

	// SAID OUT LOUD RATHER THAN INFERRED FROM AN ABSENT KEY. A deployment that
	// expected to run fork pull requests on rented machines and finds them queuing
	// forever has no other way to see why.
	if len(ec2cfg.UntrustedSecurityGroupIDs) == 0 {
		fmt.Fprintf(env.Stdout, "         untrusted work will be refused: no untrusted_security_group_ids\n")
	}

	return ec2Preflight(ctx, env, cfg, ec2cfg, awscreds.Static(creds), bundle, authorize)
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
	ctx context.Context, env cli.Env, cfg *config.Config, ec2cfg config.EC2Config, creds awscreds.Source,
	bundle *wirecert.Bundle, authorize bool,
) error {
	region, endpoint := ec2cfg.Region, ec2cfg.Endpoint

	subnet, err := ec2.DescribeSubnet(ctx, region, endpoint, creds, ec2cfg.SubnetID)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	fmt.Fprintf(env.Stdout, "subnet   %s in vpc %s, zone %s (%s)\n",
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

	fmt.Fprintf(env.Stdout, "groups   %d security group(s), all in vpc %s\n", len(groups), subnet.VPCID)

	amis := distinctEC2TierAMIs(cfg)

	var images []ec2.ImageInfo
	if len(amis) == 0 {
		// A FLEET NODE FILE HAS NO TIERS — they live on the control plane — so there
		// is no AMI to resolve here. Said, rather than passing silently as if the
		// images had been checked. The authorization dry-runs below still run: a
		// fleet node launches and tears down compute too.
		fmt.Fprintf(env.Stdout, "images   no tiers in this file, so no AMI to check "+
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
			fmt.Fprintf(env.Stdout, "image    %s is not resolvable yet (%s) — build it with `billet ami build` "+
				"and paste the id\n", img.ImageID, reason)
		case img.State != "available":
			fmt.Fprintf(env.Stdout, "image    %s is %s, not yet available\n", img.ImageID, img.State)
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

			// EVERY GAP IT HAS, BECAUSE THEY ARE DIFFERENT PROBLEMS. Naming only the
			// oldest would send an operator looking for a Docker problem in an image
			// whose Docker is fine and whose toolcache is absent, and naming only
			// the newest would present a credential exposure as a performance note.
			var gaps []string
			if img.Contract < 1 {
				gaps = append(gaps, "its Docker image store may be the containerd one, "+
					"which makes the cache publish with no images in it so every job re-pulls")
			}
			if img.Contract < 2 {
				gaps = append(gaps, "it carries no toolcache, so every setup-node, setup-go, "+
					"setup-python and setup-java step downloads a runtime that a microVM "+
					"tier of this deployment already has baked in")
			}
			if img.Contract < 3 {
				gaps = append(gaps, "it starts the runner with the registration and the "+
					"cache token in an argument list, which any process in the instance "+
					"can read until the runner starts")
			}
			missing := strings.Join(gaps, "; ")

			fmt.Fprintf(env.Stdout, "image    %s meets AMI contract %d and this billet wants %d (built by "+
				"%s) — %s; rebuild with `billet ami build`\n",
				img.ImageID, img.Contract, ec2.AMIContract, built, missing)
		default:
			fmt.Fprintf(env.Stdout, "image    %s available (AMI contract %d)\n", img.ImageID, img.Contract)
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
			fmt.Fprintf(env.Stdout, "spot     interruption queue answers (%s)\n", arn)
		case ec2.QueueProbeInconclusive(err):
			// A fact about the CHECKING identity: a role provisioned before
			// sqs:GetQueueAttributes joined the generated grant refuses this
			// probe while consuming warnings perfectly well.
			fmt.Fprintf(env.Stdout, "spot     queue probe INCONCLUSIVE: %v\n", err)
			fmt.Fprintf(env.Stdout, "         (this identity may not read queue attributes — a role from "+
				"before the probe existed lacks sqs:GetQueueAttributes; regenerate it with "+
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
			fmt.Fprintf(env.Stdout, "profile  %s UNVERIFIED: %v\n", ec2cfg.InstanceProfile, err)
		case verdict == ec2.ProfileFound:
			fmt.Fprintf(env.Stdout, "profile  %s exists\n", ec2cfg.InstanceProfile)
		case verdict == ec2.ProfileMissing:
			return fmt.Errorf("node.ec2.instance_profile %q does not exist in this account (%s); "+
				"a trusted job's launch will fail on it", ec2cfg.InstanceProfile, reason)
		default:
			fmt.Fprintf(env.Stdout, "profile  %s could not be checked (%s) — this says the CHECKING identity "+
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
			fmt.Fprintf(env.Stdout, "cache    bucket probe skipped: no deployment identity minted yet "+
				"(it is minted on the server's first start)\n")
		default:
			// The SAME namespace the runtime and decommission use, or the probe
			// reads a prefix no job ever touches.
			store, err := ebss3.New(*cfg.Node.EBSS3, app.CacheNamespace(owner, cfg.Node.Site), creds)
			if err != nil {
				return fmt.Errorf("node.ebs_s3: %w", err)
			}
			// THE VERDICT IS judgeCacheProbe's, not this switch's. What each
			// answer means is in cacheprobe.go, where a test can reach it.
			probeErr := store.CheckAccess(ctx)
			switch judgeCacheProbe(probeErr) {
			case cacheProbeAnswered:
				// A BUCKET THAT ANSWERS IS NOT A CACHE. Without a node.cache
				// listener nothing on this host ever reads or writes that prefix,
				// and this line read as though the cache were working — which is
				// most of why the whole shape was silent. The refusal is above;
				// this stops the report contradicting it three lines later.
				reachable := ""
				if cfg.Node.Cache == nil {
					reachable = " (but nothing on this node serves it — see the cache " +
						"line above)"
				}

				fmt.Fprintf(env.Stdout, "cache    bucket %s answers under this deployment's prefix%s\n",
					cfg.Node.EBSS3.Bucket, reachable)
			case cacheProbeInconclusive:
				fmt.Fprintf(env.Stdout, "cache    bucket probe INCONCLUSIVE: %v\n", probeErr)
				fmt.Fprintf(env.Stdout, "         (a 403 here is EITHER a refused identity OR a healthy miss "+
					"under billet's minimal grant, whose prefix-conditioned ListBucket cannot "+
					"match a GetObject; a real job read will settle it)\n")
			case cacheProbeFailed:
				return fmt.Errorf("node.ebs_s3: %w", probeErr)
			}
		}
	}

	if !authorize {
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — pass --authorize to dry-run "+
			"RunInstances; a DryRun has no side effect)\n")

		return nil
	}

	return ec2Authorize(ctx, env, cfg, ec2cfg, creds, bundle, images)
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
	ctx context.Context, env cli.Env, cfg *config.Config, ec2cfg config.EC2Config, creds awscreds.Source,
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
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — this deployment's identity is not "+
			"known here yet, so a dry-run cannot tag as a per-deployment IAM policy requires; "+
			"enroll this node, or run `billet server` once to mint it, then re-run with "+
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
			fmt.Fprintf(env.Stdout, "authz    %s runs untrusted work but node.ec2.untrusted_security_group_ids "+
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

			hard, verdict := reportAuthz(env,
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
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — this file declares no ec2 tiers, so "+
			"there is no launch to dry-run; a fleet node's tiers live on the control plane)\n")
	case probed == 0 && skipped > 0 && unresolvable == 0:
		// EVERY ec2 tier was SKIPPED (untrusted with no untrusted network), none
		// blocked by an AMI — the skip lines above already said why, so do not
		// misdirect to `billet ami build`.
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — every ec2 tier was skipped above; "+
			"give them an untrusted network to probe)\n")
	case probed == 0 && skipped > 0:
		// A MIX: some tiers skipped for a missing network, some for an unresolvable
		// AMI. Name BOTH remedies rather than misattributing one cause to all.
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — no ec2 tier could be dry-run: build "+
			"the AMIs named above (`billet ami build`) and give untrusted tiers a network)\n")
	case probed == 0:
		fmt.Fprintf(env.Stdout, "authz    no ec2 tier has a resolvable AMI to dry-run a launch with; build one "+
			"with `billet ami build`\n")
	case verdicts == 0:
		// Every dry-run was refused before AWS reached a permission answer (a shape
		// not offered in the zone, say), so launch authority is still unproven — said
		// rather than passing silently as if it had been checked.
		fmt.Fprintf(env.Stdout, "         (launch authority still unproven — every dry-run was refused for a "+
			"non-permission reason before AWS reached an authorization verdict)\n")
	}

	fmt.Fprintf(env.Stdout, "         (ec2:TerminateInstances cannot be dry-run — it validates the instance id "+
		"before the permission verdict, so it needs a real instance; grant it alongside "+
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
func reportAuthz(env cli.Env, what string, res ec2.DryRunResult) (bool, bool) {
	switch res.Outcome {
	case ec2.DryRunUnauthorized:
		fmt.Fprintf(env.Stdout, "authz    %s: NOT AUTHORIZED (%s)\n", what, res.Code)

		return true, true
	case ec2.DryRunAuthorized:
		fmt.Fprintf(env.Stdout, "authz    %s: authorized\n", what)

		return false, true
	default:
		// DryRunInconclusive: AWS refused before it reached the permission answer —
		// a shape not offered in the zone, an invalid parameter. NOT a permission
		// verdict, so it is neither a pass nor a fail, and the caller reports that
		// nothing was proved if every probe landed here.
		fmt.Fprintf(env.Stdout, "authz    %s: inconclusive (%s — refused before a permission verdict)\n",
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
// THE SAME DISTINCTION github.ReadPrivateKeyFile AND checkEC2Credentials MAKE. Config
// validation proves the block is coherent; it cannot prove firecracker is
// installed, that /dev/kvm can be opened, that the jail account exists or that the
// bridge does. A node that is wrong about any of those validates perfectly and
// then fails on the first job of the day.
//
// FATAL, because only a firecracker node reaches here, so this file describes a
// machine that is meant to run jobs and cannot. Reporting it and exiting zero would
// make `billet check` say a host is fine when nothing on it can launch.
func checkFirecrackerHost(ctx context.Context, env cli.Env, cfg *config.Config) error {
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

	fmt.Fprintf(env.Stdout, "microvm  %s, %s\n", report.Firecracker, report.Jailer)
	fmt.Fprintf(env.Stdout, "         jails in %s, one uid per guest from %d (%d available)\n",
		report.JailDir, report.JailUIDMin, report.JailUIDCount)

	untrusted := "untrusted work will be refused: no untrusted_bridge"
	if report.UntrustedBridge != "" {
		untrusted = "untrusted work runs on " + report.UntrustedBridge
	}

	fmt.Fprintf(env.Stdout, "         guests on %s; %s\n", report.Bridge, untrusted)
	fmt.Fprintf(env.Stdout, "         %s\n", report.Accounting.Summary())

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. Opening /dev/kvm says
	// nothing about the jailer's ability to chroot, mknod or place a cgroup, all of
	// which need root — and an operator who reads "ok" and then watches every
	// launch fail has been misled by this line.
	fmt.Fprintf(env.Stdout, "         (read only — launching also needs root, to chroot, to create the root "+
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
func checkTartHost(ctx context.Context, env cli.Env, cfg *config.Config) error {
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

	fmt.Fprintf(env.Stdout, "tart     %s, %d local VMs\n", report.Version, report.VMs)

	// SAID EVERY TIME, like softnet's grant. Which billets serialize against each
	// other is decided by TART_HOME, so two processes that disagree about the
	// store take different locks and exclude nothing — printing the path is what
	// makes that a comparison an operator can make rather than an inference.
	//
	// AND AN UNPROVED LOCK IS NOT REPORTED AS A PROVED ONE. A held lock is what a
	// busy node looks like and also what a wedged store looks like, so the line
	// says which of the two billet established.
	if report.StoreLockProved {
		fmt.Fprintf(env.Stdout, "         store    %s serializes every lease-name rename and delete\n",
			report.StoreLock)
	} else {
		fmt.Fprintf(env.Stdout, "         store    %s NOT PROVED: %s\n",
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
		fmt.Fprintf(env.Stdout, "         softnet  %s: setuid-root grant configured\n", report.Softnet.Path)
	case report.Softnet.GrantConfigured:
		fmt.Fprintf(env.Stdout, "         softnet  %s: grant configured, but it %s\n",
			report.Softnet.Path, report.Softnet.Why)
	case report.Softnet.Path != "":
		fmt.Fprintf(env.Stdout, "         softnet  %s %s\n", report.Softnet.Path, report.Softnet.Why)
	default:
		fmt.Fprintf(env.Stdout, "         softnet  %s\n", report.Softnet.Why)
	}

	if report.Softnet.Path != "" && !report.Softnet.HostBlockSupported {
		fmt.Fprintf(env.Stdout, "         softnet  %s\n", report.Softnet.HostBlockWhy)
	}

	// WHAT THIS NODE WILL DO WITH A FORK'S PULL REQUEST, in one line, because the
	// answer is a decision the operator made in config and not a property of the
	// host — and a node that silently ran untrusted work on the default NAT
	// would look exactly like one that refused it.
	switch {
	case tartCfg.UntrustedIsolation == "":
		fmt.Fprintf(env.Stdout, "         untrusted work will be refused: node.tart.untrusted_isolation "+
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

	case !report.Softnet.HostBlockSupported:
		// FATAL FOR THE SAME REASON: every untrusted launch passes
		// --net-softnet-block=@host, so a softnet that refuses the alias fails
		// each one, and a check that passed would be a promise the host breaks.
		return fmt.Errorf("node.tart.untrusted_isolation is %q, but softnet %s",
			tartCfg.UntrustedIsolation, report.Softnet.HostBlockWhy)

	default:
		fmt.Fprintf(env.Stdout, "         untrusted work runs under %s, resolving through %s\n",
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
			fmt.Fprintf(env.Stdout, "image    %-56s pulled\n", image)

			continue
		}

		missing = append(missing, image)

		fmt.Fprintf(env.Stdout, "image    %-56s NOT pulled; every job on its tier will fail to launch\n", image)
	}

	if len(missing) > 0 {
		fmt.Fprintf(env.Stdout, "         fetch them with `billet images pull` (each is tens of GB)\n")
	}

	// SAID, BECAUSE THE CHECK IS STILL NARROWER THAN IT LOOKS: a pulled image is
	// not proof that its guest carries the tart guest agent the registration
	// delivery needs, nor that a macOS guest slot is free under Apple's two-guest
	// licence. Both surface at launch, not here.
	fmt.Fprintf(env.Stdout, "         (read only — launching also needs the tart guest agent inside the "+
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
// THE SAME DISTINCTION github.ReadPrivateKeyFile AND checkEC2Credentials MAKE, one backend
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
func checkCephCluster(ctx context.Context, env cli.Env, cfg *config.CephConfig) error {
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
		printCephReport(env, cfg, report)
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
func printCephReport(env cli.Env, cfg *config.CephConfig, report ceph.Report) {
	fmt.Fprintf(env.Stdout, "ceph     client.%s -> %s\n", report.User, cfg.ConfPathOrDefault())

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

		fmt.Fprintf(env.Stdout, "         %-16s %3d image(s)  %-24s %s%s\n",
			p.Name, p.Images, replication, p.Purpose, override)
	}

	for _, p := range report.Pools {
		if p.Size == 1 {
			fmt.Fprintf(env.Stdout, "         %s keeps ONE copy: a single drive failure loses everything in "+
				"it\n", p.Name)
		}
	}

	if report.CloneV2 {
		// BOTH SETTINGS, because either can be the one making it true. A cluster on
		// luminous with rbd_default_clone_format forced to 2 clones the new way, and
		// printing only the release would have an operator reading "luminous" beside
		// "clone v2" with no way to see why they agree.
		fmt.Fprintf(env.Stdout, "         clone v2 (require-min-compat-client %s), so a cache generation can "+
			"be reclaimed while a job still holds a clone of it\n", report.MinCompatClient)
	}

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. Listing a pool proves the
	// monitors answered and the keyring authenticated; it proves nothing about
	// permission to create, clone or remove an image, which is what a launch does.
	fmt.Fprintf(env.Stdout, "         (read only — launching also needs create, clone, snapshot and remove in "+
		"both pools; `ceph auth get-or-create client.<user> mon 'profile rbd' osd 'profile rbd "+
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
func printReportedInventory(ctx context.Context, env cli.Env, a *alloc.Allocator) {
	fleet, err := a.NodeInventories(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "reported  unavailable: %v\n", err)

		return
	}

	if len(fleet) == 0 {
		return
	}

	fmt.Fprintf(env.Stdout, "reported  what each host last said it was running. This is the HOST'S OWN\n")
	fmt.Fprintf(env.Stdout, "          last word, not a check billet made, and a snapshot it took before\n")
	fmt.Fprintf(env.Stdout, "          it sent it — work can have started on that host since.\n")

	for _, inv := range fleet {
		fmt.Fprintf(env.Stdout, "          %-24s ", inv.Node)

		switch {
		case inv.Report == nil:
			// NOT THE SAME AS REPORTING NOTHING, and the distinction is the whole
			// reason the epoch is stored beside the count.
			fmt.Fprintf(env.Stdout, "has not reported since it last reconnected")
		case inv.Report.ReportedRunning > 0:
			// THE ONE ANSWER HERE THAT IS WORTH ACTING ON. A host that says it is
			// running something is telling you a fact; a host that says it is
			// running nothing is telling you about a moment that has passed.
			fmt.Fprintf(env.Stdout, "SAYS IT IS RUNNING %d", inv.Report.ReportedRunning)
		default:
			fmt.Fprintf(env.Stdout, "saw 0 billet instances when it last looked")
		}

		if !inv.Live {
			fmt.Fprintf(env.Stdout, " (this deployment cannot reach it)")
		}

		if inv.Report != nil && inv.Report.ReceivedAt != "" {
			fmt.Fprintf(env.Stdout, ", received %s", inv.Report.ReceivedAt)
		}

		fmt.Fprintln(env.Stdout)
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
func printComputeBarrier(ctx context.Context, env cli.Env, a *alloc.Allocator) {
	clearance, err := a.ComputeClear(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "barrier   unavailable: %v\n", err)

		return
	}

	if len(clearance.Excluded) > 0 {
		fmt.Fprintf(env.Stdout, "excluded  %d host(s) billet no longer expects an answer from\n",
			len(clearance.Excluded))

		for _, e := range clearance.Excluded {
			fmt.Fprintf(env.Stdout, "          %-24s ", e.Node)

			if e.Proven {
				fmt.Fprintf(env.Stdout, "proved idle before it was removed")
			} else {
				// THE LINE THIS SECTION EXISTS FOR. A forced exclusion is billet
				// saying it does not know what is on that machine, and a report that
				// rendered it the same as a proven one would launder exactly the
				// uncertainty membership is allowed to skip past.
				fmt.Fprintf(env.Stdout, "REMOVED WITHOUT PROOF — nothing knows what it is running")
			}

			if e.Actor != "" {
				fmt.Fprintf(env.Stdout, " (%s)", e.Actor)
			}

			fmt.Fprintln(env.Stdout)
		}
	}

	if !clearance.Requested {
		return
	}

	if clearance.Clear() {
		fmt.Fprintf(env.Stdout, "barrier   every host billet expects an answer from says it is running no\n")
		fmt.Fprintf(env.Stdout, "          compute, and has said so continuously\n")

		return
	}

	blocking := clearance.Blocking()

	fmt.Fprintf(env.Stdout, "barrier   a drain is asking the fleet what it is running; %d host(s) have not\n",
		len(blocking))
	fmt.Fprintf(env.Stdout, "          been proved idle\n")

	for _, n := range blocking {
		fmt.Fprintf(env.Stdout, "          %-24s %s", n.Node, n.State)

		switch n.State {
		case alloc.ClearanceSettling:
			// See clearanceSummary: the timestamp is when another empty answer
			// would prove the run, not a moment at which it clears itself.
			fmt.Fprintf(env.Stdout, " (needs another empty answer at or after %s)", n.ClearAt)
		case alloc.ClearanceBelowProtocol:
			fmt.Fprintf(env.Stdout, " (wire %d)", n.WireVersion)
		case alloc.ClearanceUnknown, alloc.ClearanceProved, alloc.ClearanceRunning,
			alloc.ClearanceWaiting, alloc.ClearanceUnreachable:
		}

		fmt.Fprintln(env.Stdout)
	}
}

// printCacheAwareWaits names every tier that waits for a host new enough to
// read its cache block, so a rollout's wait reads as a wait and not a stall.
func printCacheAwareWaits(ctx context.Context, env cli.Env, a *alloc.Allocator, tiers []config.Tier) {
	lines, err := cacheAwareWaits(tiers, func(t config.Tier) (bool, error) {
		return a.WaitsForCacheAwareHost(ctx, t)
	})
	if err != nil {
		fmt.Fprintf(env.Stdout, "cache     unavailable: %v\n", err)
	}
	for _, line := range lines {
		fmt.Fprintln(env.Stdout, line)
	}
}

// cacheAwareWaits is a line for each tier placed nowhere only because every
// host it could otherwise use is too old to read its cache block: in a rollout,
// until the first of ITS hosts upgrades, whatever other hosts have.
func cacheAwareWaits(tiers []config.Tier, waits func(config.Tier) (bool, error)) ([]string, error) {
	var lines []string
	for i := range tiers {
		waiting, err := waits(tiers[i])
		if err != nil {
			return lines, err
		}
		if !waiting {
			continue
		}
		label := "cache"
		if len(lines) > 0 {
			label = ""
		}
		lines = append(lines, fmt.Sprintf("%-9s tier %s WAITS FOR A HOST ON PROTOCOL %d: an older host "+
			"would ignore its cache block and read, publish or keep more than it allows, and "+
			"none of the hosts it could run on speaks %d yet", label, tiers[i].Label,
			nodeapi.VersionCacheAuthority, nodeapi.VersionCacheAuthority))
	}

	return lines, nil
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
func printWireWindow(ctx context.Context, env cli.Env, a *alloc.Allocator) {
	fleet, err := a.NodeWireVersions(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "protocol  unavailable: %v\n", err)

		return
	}

	if len(fleet) == 0 {
		return
	}

	fmt.Fprintf(env.Stdout, "protocol  this control plane speaks %s\n", nodeapi.Self())

	var unrecorded, older, newer int

	for _, n := range fleet {
		spoken := "protocol unrecorded"
		if n.Negotiated > 0 {
			spoken = fmt.Sprintf("protocol %d", n.Negotiated)
		}

		fmt.Fprintf(env.Stdout, "          %-24s %-12s %-28s %s", n.Name, spoken, describeRelease(n),
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

			fmt.Fprintf(env.Stdout, "  <- NOT RECORDED, SO IT STILL BLOCKS RETIREMENT")
		case n.Negotiated > nodeapi.Version:
			newer++

			fmt.Fprintf(env.Stdout, "  <- NEWER THAN THIS CONTROL PLANE, which cannot serve it")
		case n.Negotiated < nodeapi.Version:
			older++

			fmt.Fprintf(env.Stdout, "  <- OLDER THAN THIS CONTROL PLANE")
		}

		if !n.Live {
			fmt.Fprintf(env.Stdout, " (this deployment cannot reach it)")
		}

		if note := describeDowngrade(n); note != "" {
			fmt.Fprintf(env.Stdout, "  <- %s", note)
		}

		fmt.Fprintln(env.Stdout)
	}

	behind := unrecorded + older + newer
	if behind == 0 {
		fmt.Fprintf(env.Stdout, "          every host speaks %d; the older protocols in that range are free "+
			"to drop in a later release\n", nodeapi.Version)

		return
	}

	fmt.Fprintf(env.Stdout, "          %d host(s) are not known to be on %d, so nothing below it may be "+
		"dropped.\n", behind, nodeapi.Version)

	// THE REMEDY IS PER STATE, BECAUSE ONE OF THEM POINTS THE OTHER WAY. A host
	// on a NEWER protocol is what a rolled-back control plane leaves behind, and
	// telling an operator to upgrade those hosts is backwards — the control plane
	// is the half that is behind. A blanket "upgrade them" contradicted the row it
	// was summarising.
	if older > 0 {
		fmt.Fprintf(env.Stdout, "          %d of them speak an older protocol: upgrade those hosts.\n", older)
	}

	if newer > 0 {
		fmt.Fprintf(env.Stdout, "          %d speak a protocol NEWER than this control plane, which cannot "+
			"serve it. Upgrade or restore the control plane; upgrading those hosts "+
			"cannot help.\n", newer)
	}

	if unrecorded > 0 {
		fmt.Fprintf(env.Stdout, "          %d have said nothing since this binary began recording it; they "+
			"report their protocol on their next registration.\n", unrecorded)
	}

	// WHAT AN OPERATOR CAN NOW DO, and the reason it is stated here rather than
	// left implicit. A host that is permanently gone keeps this window open — and
	// for a long time nothing could clear it, so this line said so rather than
	// advising a command that did not exist. `billet nodes decommission` is that
	// command; it refuses while the host is reachable or holds any lease, because
	// forgetting a row is only safe once nothing says its compute may still be
	// running.
	fmt.Fprintf(env.Stdout, "          A host that is gone for good still counts. Once it is stopped and "+
		"holds no lease,\n          `billet nodes decommission <node>` forgets it and closes "+
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
func printAdmission(env cli.Env, a state.Admission) {
	if a.Mode == state.AdmissionOpen {
		fmt.Fprintf(env.Stdout, "admission open\n")

		return
	}

	fmt.Fprintf(env.Stdout, "admission %s — this deployment is not taking new work\n", a.Mode)

	switch {
	case a.Actor != "" && a.Reason != "":
		fmt.Fprintf(env.Stdout, "          sealed by %s: %s\n", a.Actor, a.Reason)
	case a.Actor != "":
		fmt.Fprintf(env.Stdout, "          sealed by %s\n", a.Actor)
	case a.Reason != "":
		fmt.Fprintf(env.Stdout, "          %s\n", a.Reason)
	}

	if a.ChangedAt != "" {
		fmt.Fprintf(env.Stdout, "          since %s\n", a.ChangedAt)
	}

	// WHICH SEAL THIS IS decides who may clear it, and an operator staring at a
	// quiet fleet needs to know whether restarting the services will reopen it.
	switch a.Provenance {
	case state.ProvenanceLocalDown:
		fmt.Fprintf(env.Stdout, "          held by a shutdown; `billet local up` clears it\n")
	case state.ProvenanceOperator:
		fmt.Fprintf(env.Stdout, "          held deliberately; it survives a restart\n")
	}
}
