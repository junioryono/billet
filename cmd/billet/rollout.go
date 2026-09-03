package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// cmdRollout is the operator's view of, and control over, one durable fleet
// decision.
//
// ONE DECISION, NOT A COMMAND PER HOST. Updating billet is meant to be a thing
// somebody does once: a channel is resolved to one immutable target, that target
// is recorded, and every controller and node converges on it without another
// version edit. Everything under this command either creates that decision,
// reports where it has got to, or records an operator's judgement about a
// component the rollout cannot resolve by itself.
func cmdRollout(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "status" {
		rest := args
		if len(args) > 0 {
			rest = args[1:]
		}

		return cmdRolloutStatus(ctx, rest)
	}

	switch args[0] {
	case "start":
		return cmdRolloutStart(ctx, args[1:])
	case "abort":
		return cmdRolloutAbort(ctx, args[1:])
	case "retry":
		return cmdRolloutNodePhase(ctx, args[1:], rollout.PhasePending, "retry")
	case "exempt":
		return cmdRolloutNodePhase(ctx, args[1:], rollout.PhaseExempt, "exempt")
	case "decommission":
		return cmdRolloutNodePhase(ctx, args[1:], rollout.PhaseDecommissioned, "decommission")
	}

	return fmt.Errorf("unknown rollout command %q; try status, start, abort, retry, exempt "+
		"or decommission", args[0])
}

// rolloutStore opens the ledger the way every operator command does, through
// OpenAdmin, so this works WHILE a control plane is running.
func rolloutStore(ctx context.Context, cfgPath string,
) (*rollout.Store, *alloc.Allocator, func(), error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}

	if cfg.Server == nil {
		return nil, nil, nil, errors.New("a rollout is a property of the control plane, " +
			"and this config has no server section")
	}

	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("server state: %w", err)
	}

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		_ = db.Close()

		return nil, nil, nil, fmt.Errorf("capacity allocator: %w", err)
	}

	return rollout.New(db), a, func() { _ = db.Close() }, nil
}

// cmdRolloutStart resolves a target and records the decision.
//
// THE CHANNEL IS RESOLVED ONCE, HERE, AND THE DIGEST IS WHAT IS PERSISTED. A
// channel that advances later must not retarget a rollout already underway —
// which is why nothing downstream ever consults the channel again, and why the
// record carries the manifest's digest rather than only its tag.
func cmdRolloutStart(ctx context.Context, args []string) error {
	fs := newFlagSet("billet rollout start")
	cfgPath := addConfigFlag(fs)
	channel := fs.String("channel", releasesource.ChannelStable,
		"the signed channel to resolve, e.g. stable or candidate")
	pin := fs.String("version", "",
		"an exact release to install instead of following a channel; it never moves")
	cohort := fs.Int("cohort", 0, "how many hosts may be past pending at once (default 1)")
	failureBudget := fs.Int("failure-budget", 1,
		"how many hosts may end blocked or rolled back before this stops starting new ones")
	skipVerify := fs.Bool("skip-signature-verification", false,
		"trust this source by other means; only for an air-gapped mirror")

	if err := parse(fs, args); err != nil {
		return err
	}

	store, a, closeDB, err := rolloutStore(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	// ALREADY RUNNING IS REPORTED, and the store decides that rather than this
	// command: between a read here and the write below another process can start
	// one, and reporting from the snapshot would tell an operator nothing is
	// running while a rollout is midway through draining a host.
	client := &releasesource.Client{}

	policy, err := releasesource.PolicyForRelease(*skipVerify)
	if err != nil {
		return err
	}

	target, digest, err := resolveTarget(ctx, client, policy, *channel, *pin)
	if err != nil {
		return err
	}

	current := releasesource.Host(version.Version(),
		releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
		state.LatestSchemaVersion(), firecracker.GuestContract)

	// THE PREFLIGHT RUNS BEFORE THE DECISION IS RECORDED, not before each step of
	// it. A rollout that persisted an incompatible target would have every
	// component fail the same way in turn, and the operator would read a fleet
	// problem rather than the one refusal that explains all of it.
	warnings, err := releasesource.Compatibility(target, current)
	for _, w := range warnings {
		fmt.Printf("NOTE: %v\n\n", w)
	}

	// A REFUSAL IS A REFUSAL WHATEVER ELSE CAME BACK. See the comment in
	// hostupgrade.go: picking a warning out of a joined error and carrying on is
	// how a candidate that shares no wire version gets installed.
	if err != nil {
		return err
	}

	nodes, err := a.RegisteredNodes(ctx)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].Name)
	}

	recorded, err := store.Start(ctx, rollout.StartRequest{
		Channel:       channelOrPin(*channel, *pin),
		TargetVersion: target.Version,
		TargetDigest:  digest,
		PriorVersion:  version.Version(),
		Policy: rollout.Policy{
			Cohort:        *cohort,
			FailureBudget: *failureBudget,
		},
		CreatedBy: actor(),
		Nodes:     names,
	})
	if err != nil {
		if errors.Is(err, rollout.ErrOpen) {
			return fmt.Errorf("%w. `billet rollout status` says where it has got to", err)
		}

		return err
	}

	fmt.Printf("Rollout %s: %s -> %s\n", recorded.ID, recorded.PriorVersion,
		recorded.TargetVersion)
	fmt.Printf("  manifest %s\n", recorded.TargetDigest)
	fmt.Printf("  %d host(s), %d at a time\n", len(names), recorded.Policy.Cohort)
	fmt.Printf("\nThe control plane picks this up and converges the fleet. Nothing here\n")
	fmt.Printf("drains or installs: a rollout waits for the work already running on a host\n")
	fmt.Printf("for as long as it takes, and no elapsed time ever ends a job.\n")
	fmt.Printf("\nWatch it with `billet rollout status`.\n")

	return nil
}

// resolveTarget turns a channel or an exact pin into one immutable manifest.
func resolveTarget(ctx context.Context, client *releasesource.Client,
	policy releasesource.Policy, channel, pin string,
) (*releasesource.Manifest, string, error) {
	// AN EXACT PIN NEVER MOVES, and it is resolved without touching a channel at
	// all. An operator who typed a version is asking for that version, and
	// consulting a pointer would let a channel's opinion reach a decision the
	// operator had already made.
	if pin != "" {
		return client.Manifest(ctx, pin, "", policy)
	}

	statement, err := client.Resolve(ctx, channel, policy)
	if err != nil {
		return nil, "", err
	}

	return client.Manifest(ctx, statement.Tag, statement.ManifestSHA256, policy)
}

func channelOrPin(channel, pin string) string {
	if pin != "" {
		return ""
	}

	return channel
}

// cmdRolloutStatus reports where one fleet decision has got to.
func cmdRolloutStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("billet rollout status")
	cfgPath := addConfigFlag(fs)

	if err := parse(fs, args); err != nil {
		return err
	}

	store, _, closeDB, err := rolloutStore(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	current, err := store.Open(ctx)
	if err != nil {
		if !errors.Is(err, rollout.ErrNoRollout) {
			return err
		}

		return reportLastRollout(ctx, store)
	}

	fmt.Printf("rollout   %s  %s -> %s\n", current.ID, current.PriorVersion,
		current.TargetVersion)

	if current.Channel != "" {
		fmt.Printf("          following the %s channel; the target is pinned to the manifest "+
			"below and does not move if the channel does\n", current.Channel)
	} else {
		fmt.Printf("          started from an exact version pin\n")
	}

	fmt.Printf("          manifest %s\n", current.TargetDigest)
	fmt.Printf("          started by %s at %s\n", current.CreatedBy, current.CreatedAt)
	fmt.Printf("controller %s\n", current.ControllerPhase)

	nodes, err := store.Nodes(ctx, current.ID)
	if err != nil {
		return err
	}

	printRolloutNodes(nodes)

	return nil
}

// forgetHost removes a host from the fleet, and tolerates one that is already
// gone.
//
// A ROLLOUT NAMES HOSTS FROM A SNAPSHOT TAKEN WHEN IT STARTED, so a name in the
// rollout with no fleet row is a machine something already removed — and that is
// exactly when an operator needs to record the decision, because that host is
// otherwise holding the rollout open with nothing able to resolve it. Passing the
// refusal through would leave a rollout that can never complete.
//
// THE MEMBERSHIP READ HAPPENS ONLY ON THE FAILURE PATH, AND ONLY WIDENS ONE
// ANSWER. `alloc.Decommission` deliberately establishes its own proof inside its
// own transaction rather than trusting a clearance read beforehand, and nothing
// here weakens that: the refusals about outstanding leases and a host that is
// still talking all concern a host that HAS a row, so this read cannot turn any
// of them into a success. What it can do is tell "never registered" apart from
// them, which the error alone does not.
//
// WHAT IT DOES NOT PROVE is that the host is still absent a moment later. A
// machine can register between this read and the record — but the phase being
// written is a decision an operator made to stop expecting that host, not a claim
// that billet proved anything, and `store.Advance` still refuses a name that is
// not part of this rollout.
func forgetHost(ctx context.Context, a *alloc.Allocator, node string, force bool) error {
	proven, err := a.Decommission(ctx, alloc.DecommissionRequest{
		Node: node, Actor: actor(), Force: force,
	})

	switch {
	case err == nil:
		if !proven {
			fmt.Printf("Nothing proved %s is running no compute, so the exclusion is "+
				"recorded as UNPROVEN\nand every later drain will say so.\n\n", node)
		}

		return nil

	case errors.Is(err, alloc.ErrNotDecommissionable):
		known, checkErr := hostIsRegistered(ctx, a, node)
		if checkErr != nil {
			return errors.Join(err, checkErr)
		}

		if known {
			return err
		}

		fmt.Printf("This deployment already has no row for %q; recording the decision so "+
			"the rollout can complete.\n\n", node)

		return nil

	default:
		return err
	}
}

// hostIsRegistered reports whether the fleet still has a row for a name.
func hostIsRegistered(ctx context.Context, a *alloc.Allocator, node string) (bool, error) {
	nodes, err := a.RegisteredNodes(ctx)
	if err != nil {
		return false, err
	}

	for i := range nodes {
		if nodes[i].Name == node {
			return true, nil
		}
	}

	return false, nil
}

func printRolloutNodes(nodes []rollout.Node) {
	if len(nodes) == 0 {
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NODE\tPHASE\tPROVED BY\tATTEMPTS\tNEXT TRY\tDETAIL")

	var converged, decided, unproved int

	for i := range nodes {
		n := &nodes[i]

		detail := n.Blocker
		switch {
		case detail == "" && n.ExemptReason != "":
			detail = n.ExemptReason
		case detail == "" && n.RollbackResult != "":
			detail = n.RollbackResult
		}

		next := n.NextAttemptAt
		if next == "" {
			next = "-"
		} else if when, err := time.Parse(time.RFC3339Nano, next); err == nil {
			next = when.Format(time.RFC3339)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			n.Node, n.Phase, describeProof(n), n.Attempts, next, detail)

		switch {
		case n.Phase.Converged():
			converged++

			if n.ConvergedDigest == "" {
				unproved++
			}
		case n.Phase.Terminal():
			decided++
		}
	}

	_ = w.Flush()

	fmt.Printf("\n%d of %d host(s) converged", converged, len(nodes))

	if unproved > 0 {
		// SAID SEPARATELY, BECAUSE IT IS A WEAKER CLAIM THAN THE LINE ABOVE. Those
		// hosts reached the target VERSION and could not say which bytes they
		// installed — which is every host in the field until one billet-driven
		// upgrade has run, and is exactly the thing that used to be invisible.
		fmt.Printf("; %d on their version alone, having named no release manifest", unproved)
	}

	if decided > 0 {
		// SAID SEPARATELY, because it is not the same fact. An exempted or
		// decommissioned host lets the rollout complete and is still running the
		// old release; folding it into "converged" is how a protocol gets retired
		// while a live machine still needs it.
		fmt.Printf("; %d decided by an operator and still on the old release", decided)
	}

	fmt.Println()
}

// describeProof says what a rollout accepted as evidence for one host.
//
// READ BESIDE THE PHASE, NEVER INSTEAD OF IT. A committed host with no digest is
// one that reached the target version and could not name the manifest that
// produced it; a host that is not committed has proved nothing yet, and saying
// "version only" about it would read as a weaker success rather than as no
// success at all.
func describeProof(n *rollout.Node) string {
	switch {
	case !n.Phase.Converged():
		return "-"
	case n.ConvergedDigest == "":
		return "version only"
	default:
		return "manifest " + n.ConvergedDigest[:12]
	}
}

func reportLastRollout(ctx context.Context, store *rollout.Store) error {
	history, err := store.History(ctx, 1)
	if err != nil {
		return err
	}

	if len(history) == 0 {
		fmt.Printf("No rollout has ever run on this deployment.\n")
		fmt.Printf("\n`billet rollout start` resolves the stable channel to one immutable\n")
		fmt.Printf("release and records it as the fleet's target.\n")

		return nil
	}

	last := history[0]
	fmt.Printf("No rollout is running. The last one was %s: %s -> %s, %s at %s\n",
		last.ID, last.PriorVersion, last.TargetVersion, last.State, last.FinishedAt)

	if last.TerminalReason != "" {
		fmt.Printf("  %s\n", last.TerminalReason)
	}

	return nil
}

// printRollout reports a rollout in one line, for `billet status`.
//
// A POINTER, NOT THE WHOLE PICTURE. `billet rollout status` is where the per-host
// detail lives; what belongs in the single-glance command is the fact that a
// rollout explains what the operator is looking at — hosts on two versions,
// capacity down by one machine, a node reporting nothing.
//
// IT NEVER FAILS THE COMMAND. `billet status` is what somebody runs when
// something is already wrong, and a ledger read that failed must not take the
// rest of the report with it.
func printRollout(ctx context.Context, db *state.DB) {
	store := rollout.New(db)

	current, err := store.Open(ctx)
	if err != nil {
		if !errors.Is(err, rollout.ErrNoRollout) {
			fmt.Printf("rollout   unavailable: %v\n", err)
		}

		return
	}

	nodes, err := store.Nodes(ctx, current.ID)
	if err != nil {
		fmt.Printf("rollout   %s -> %s (could not read its hosts: %v)\n",
			current.PriorVersion, current.TargetVersion, err)

		return
	}

	var converged, decided, blocked int

	for i := range nodes {
		switch {
		case nodes[i].Phase.Converged():
			converged++
		case nodes[i].Phase.Terminal():
			decided++
		case nodes[i].Phase == rollout.PhaseBlocked:
			blocked++
		}
	}

	fmt.Printf("rollout   %s IN PROGRESS: %s -> %s, %d of %d host(s) converged\n",
		current.ID, current.PriorVersion, current.TargetVersion, converged, len(nodes))

	// THE TWO NUMBERS THAT CHANGE WHAT AN OPERATOR DOES NEXT. A blocked host needs
	// somebody to look at a machine; a decided one is a host that will stay on the
	// old release, which is why the fleet is mixed and why a protocol is still
	// open.
	if blocked > 0 {
		fmt.Printf("          %d host(s) BLOCKED and advertising nothing; "+
			"`billet rollout status` says why\n", blocked)
	}

	if decided > 0 {
		fmt.Printf("          %d exempted or decommissioned by an operator and staying on "+
			"the old release\n", decided)
	}

	if current.ControllerPhase != rollout.PhaseCommitted {
		fmt.Printf("          the control plane itself is %s\n", current.ControllerPhase)
	}
}

// cmdRolloutAbort abandons a rollout.
//
// IT STOPS THE DECISION, NOT THE MACHINES. Components already converged stay
// converged and components midway through an install finish or roll back on their
// own; what ends is billet's intent to move anything else. A command that also
// reverted hosts would be a second, undeclared rollout in the opposite direction.
func cmdRolloutAbort(ctx context.Context, args []string) error {
	fs := newFlagSet("billet rollout abort")
	cfgPath := addConfigFlag(fs)
	reason := fs.String("reason", "", "why this rollout is being abandoned")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *reason == "" {
		return errors.New("--reason is required: an abandoned rollout with nothing recorded " +
			"is one nobody can explain to whoever finds the fleet on two versions")
	}

	store, _, closeDB, err := rolloutStore(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	current, err := store.Open(ctx)
	if err != nil {
		return err
	}

	if err := store.Finish(ctx, current.ID, rollout.StateAborted, *reason); err != nil {
		return err
	}

	fmt.Printf("Abandoned rollout %s.\n\n", current.ID)
	fmt.Printf("Hosts that already converged are on %s and stay there; the rest are on\n",
		current.TargetVersion)
	fmt.Printf("%s. `billet status` under `protocol` says which is which. Nothing was\n",
		current.PriorVersion)
	fmt.Printf("reverted: an abort ends the decision, not the machines.\n")

	return nil
}

// cmdRolloutNodePhase records an operator's judgement about one host.
func cmdRolloutNodePhase(ctx context.Context, args []string, to rollout.Phase, verb string,
) error {
	fs := newFlagSet("billet rollout " + verb)
	cfgPath := addConfigFlag(fs)
	reason := fs.String("reason", "", "the operator's reason, recorded against this host")

	// ONLY MEANINGFUL FOR A DECOMMISSION, and offered on both so the flag set does
	// not change shape between two subcommands that share this function. An
	// exemption asserts nothing about compute, so it ignores this.
	force := fs.Bool("force", false,
		"record the decommission even though nothing has proved the host is running no "+
			"compute; the exclusion is recorded as UNPROVEN and every later drain says so")

	node, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if node == "" {
		return fmt.Errorf("usage: billet rollout %s <node>", verb)
	}

	store, a, closeDB, err := rolloutStore(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	current, err := store.Open(ctx)
	if err != nil {
		return err
	}

	// THE NODE IS FORGOTTEN BEFORE THE ROLLOUT RECORDS IT, and the reverse order
	// was a defect a review caught. These are two transactions in two packages and
	// cannot be one, so the question is which failure is survivable: recording the
	// decision first and then failing to delete leaves a rollout that COMPLETES
	// while the node row survives, still holding its protocol window open with
	// nothing saying so. Deleting first and then failing to record leaves a
	// forgotten host and an unrecorded decision, which the same command fixes on
	// its next run — Decommission is idempotent about a host that is already gone.
	//
	// THE PROOF IS THE LEDGER'S, INSIDE ITS OWN TRANSACTION, and this deliberately
	// does not read a clearance first. A preflight here — which this command used
	// to do — is a statement about an incarnation that can be superseded before it
	// is acted on: the host re-registers, and the exclusion is recorded as proved
	// about a machine that has just come back. The compute barrier settled that,
	// and a rollout is not a reason to answer the question differently.
	//
	// UNPROVEN IS RECORDED, NOT REFUSED. A host in a rollout is usually one nothing
	// can reach — that is why an operator is deciding about it at all — so refusing
	// without a current proof would leave the rollout open with no way to resolve
	// it. What `--force` buys is a DURABLE note that nothing proved the machine
	// idle, which every later drain and `billet status` repeat.
	if to == rollout.PhaseDecommissioned {
		if err := forgetHost(ctx, a, node, *force); err != nil {
			return err
		}
	}

	if err := store.Advance(ctx, rollout.AdvanceRequest{
		RolloutID:    current.ID,
		Node:         node,
		To:           to,
		ExemptReason: *reason,
	}); err != nil {
		return err
	}

	fmt.Printf("Recorded %s as %s in rollout %s.\n", node, to, current.ID)

	return nil
}
