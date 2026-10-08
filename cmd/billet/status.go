package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/ops/host"

	"github.com/junioryono/billet/internal/ops/fleetops"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

func cmdStatus(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet status", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	a, db, closeDB, err := fleetops.ControlPlaneStores(ctx, *cfgPath)
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
	fleetops.PrintForceDestroy(ctx, env, a, admission)

	// AND A ROLLOUT IN ONE LINE, because it explains the other half of what an
	// operator is looking at: hosts on two versions, capacity down by one machine,
	// a node reporting nothing. `billet rollout status` is the full picture; this
	// is what says to go and look at it.
	host.PrintRollout(ctx, env, db)

	// AND THE HOST'S OWN GUARD, read from this host's upgrade root and never
	// from the ledger: a converge holding this host is why a rollout is refusing
	// to move it.
	host.PrintGuard(env)

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
			fmt.Fprintf(env.Stdout, "target    %s (%s)\n", target.Name, app.DescribeGitHubTarget(target))
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
			printTierCapacity(env.Stdout, fleetops.TierDisplay(t), report, time.Now())
		}
	}

	if err := printRemoteFleetCost(ctx, env, a, cfg); err != nil {
		return err
	}

	// WHAT THE CONTROL PLANE HAS SWEPT out of Parameter Store, and which codebuild
	// hosts it cannot sweep after. A leaked registration is one nobody sees, which
	// is why the count is durable and printed rather than logged.
	host.PrintCredentialSweeps(ctx, env, a, db)

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
	fleetops.PrintReplacedHolders(ctx, env, a)

	if len(held) == 0 {
		fmt.Fprintln(env.Stdout, "held      none")

		return nil
	}

	fmt.Fprintf(env.Stdout, "held      %d lease(s) waiting for compute to be confirmed gone\n", len(held))
	fleetops.PrintHeld(env, held)
	fleetops.PrintHolderNote(env.Stdout, held)

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
