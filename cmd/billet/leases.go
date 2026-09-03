package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// cmdLeases is the operator's view of capacity that has not come back.
func cmdLeases(ctx context.Context, args []string) error {
	// Bare `billet leases` — with or without flags — is the documented form and
	// means `held`; only a word selects another view.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return cmdLeasesHeld(ctx, args)
	}

	switch args[0] {
	case "held":
		return cmdLeasesHeld(ctx, args[1:])
	case "quarantined":
		return cmdLeasesQuarantined(ctx, args[1:])
	case "failures":
		return cmdLeasesFailures(ctx, args[1:])
	case "release":
		return cmdLeasesRelease(ctx, args[1:])
	}

	return fmt.Errorf("unknown leases command %q; try held, quarantined, failures, or release",
		args[0])
}

// cmdLeasesHeld shows every lease whose compute has not been confirmed gone,
// including proof obligations a healthy node is actively tending.
func cmdLeasesHeld(ctx context.Context, args []string) error {
	fs := newFlagSet("billet leases held")
	cfgPath := addConfigFlag(fs)
	if err := parse(fs, args); err != nil {
		return err
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	held, err := a.Held(ctx)
	if err != nil {
		return err
	}

	if len(held) == 0 {
		fmt.Println("Nothing is held: no lease is waiting for compute to be confirmed gone.")

		return nil
	}

	printHeld(held)
	fmt.Printf("\nCustody preserves adopted work; teardown is a live node waiting for its backend\n")
	fmt.Printf("to confirm removal; quarantine has no current holder. When you have independent\n")
	fmt.Printf("proof the compute is gone:\n\n  billet leases release <lease> --force\n")
	printHolderNote(os.Stdout, held)
	fmt.Printf("\nFor jobs that FAILED while billet's own infrastructure was disrupted:\n\n")
	fmt.Printf("  billet leases failures\n")

	return nil
}

func printHeld(held []alloc.HeldLease) {
	printHeldTo(os.Stdout, held)
}

// printHeldTo renders the held table, naming the process holding each lease.
//
// THE HOLDER COLUMN IS WHAT THE STUCK-LEASE REPORT WAS MISSING. A lease's node
// name says which machine; the epoch says which PROCESS on it was given the
// obligation, and a holder the host has registered past is one this deployment
// can no longer reach — dead, or superseded and draining. The column says so
// beside the phase, so an operator can tell a slow holder from an absent one.
func printHeldTo(out io.Writer, held []alloc.HeldLease) {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "LEASE\tTIER\tNODE\tSTATE\tVCPU\tMEMORY\tHELD FOR\tHOLDER\tFORCE")

	for i := range held {
		h := &held[i]
		force := ""
		if h.ForceRequested {
			force = "requested"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", h.ID, h.Tier, h.Node,
			h.State, h.VCPU, h.Memory, heldFor(h.Since), describeHolder(h.Holder), force)
	}

	_ = w.Flush()
}

// describeHolder renders who holds a lease and whether they can still be reached.
//
// THREE ANSWERS, NOT TWO. "Unknown" is a lease an older binary recorded no holder
// for, or a host that presented no incarnation, and it is said in that word
// rather than rendered as a process: "cannot tell" must never read as "the
// holder is gone", because the next thing an operator does with "gone" is force
// the lease.
func describeHolder(h alloc.Holder) string {
	if h.Incarnation == "" {
		return "unknown"
	}

	if !h.Replaced() {
		if h.NodeKnown && !h.NodeLive {
			return fmt.Sprintf("process %s (host unreachable)", shortIncarnation(h.Incarnation))
		}

		return "process " + shortIncarnation(h.Incarnation)
	}

	replaced := fmt.Sprintf("process %s, REPLACED by %s", shortIncarnation(h.Incarnation),
		shortIncarnation(h.NodeIncarnation))
	if h.NodeSeenAt != "" {
		replaced += " " + heldFor(h.NodeSeenAt) + " ago"
	}

	return replaced
}

// shortIncarnation trims a node process's 32-hex-character incarnation to what a
// table column can carry; enough to tell two processes apart on one host, which
// is all the column has to do.
func shortIncarnation(s string) string {
	const shown = 12
	if len(s) <= shown {
		return s
	}

	return s[:shown]
}

// printHolderNote says what a replaced holder means, once, when any lease has
// one.
//
// SAID BESIDE THE FORCE COMMAND, because the two states an operator meets here
// call for opposite things. A holder that is genuinely gone stops renewing, the
// lease is quarantined within the lease TTL, and the force command then
// releases it on the spot; a lease still being renewed after that has a LIVE
// holder — a superseded process draining what it holds — and the force request
// goes through that process. Time is not evidence of either; renewal is.
func printHolderNote(out io.Writer, held []alloc.HeldLease) {
	for i := range held {
		if !held[i].Holder.Replaced() {
			continue
		}

		fmt.Fprintf(out, "\nA holder marked REPLACED is a node process that registered again since it took\n")
		fmt.Fprintf(out, "the lease. If that process is gone nothing renews the lease: it is quarantined\n")
		fmt.Fprintf(out, "within the lease TTL and the command above releases it then. A lease still\n")
		fmt.Fprintf(out, "renewed after that is held by a superseded process draining what it runs, and\n")
		fmt.Fprintf(out, "the force request goes through it.\n")

		return
	}
}

// printReplacedHolders reports running-phase leases whose holder the deployment
// can no longer reach, for `billet status`.
//
// THE STATE THE STUCK-LEASE REPORT FOUND NOTHING TO READ ABOUT: a job's lease
// renewed by the control plane while its completion waits on a node process that
// has been replaced. Such a lease is listed as held by nobody, so without this
// line the only visible fact was a slot in use. It never fails the command, for
// the reason printReportedInventory gives: `billet status` is what somebody runs
// when something is already wrong.
func printReplacedHolders(ctx context.Context, a *alloc.Allocator) {
	orphaned, err := a.RunningWithReplacedHolder(ctx)
	if err != nil {
		fmt.Printf("bound     unavailable: %v\n", err)

		return
	}

	printReplacedHoldersTo(os.Stdout, orphaned)
}

func printReplacedHoldersTo(out io.Writer, orphaned []alloc.ReplacedHolderLease) {
	if len(orphaned) == 0 {
		return
	}

	fmt.Fprintf(out, "bound     %d running lease(s) whose node process was replaced. Each is still\n",
		len(orphaned))
	fmt.Fprintf(out, "          charged; once nothing renews it the reaper quarantines it and its host's\n")
	fmt.Fprintf(out, "          inventory settles it. `billet leases` lists it once that happens.\n")

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "          LEASE\tTIER\tNODE\tSTATE\tHOLDER\tLAST RENEWED")

	for i := range orphaned {
		o := &orphaned[i]
		fmt.Fprintf(w, "          %s\t%s\t%s\t%s\t%s\t%s ago\n", o.ID, o.Tier, o.Node, o.State,
			describeHolder(o.Holder), heldFor(o.LastRenewed))
	}

	_ = w.Flush()
}

func heldFor(since string) string {
	t, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return since
	}

	d := time.Since(t)
	if d < time.Minute {
		return "<1m"
	}

	return d.Round(time.Minute).String()
}

// cmdLeasesQuarantined shows capacity held for compute nobody has accounted for.
//
// THE ANSWER TO "WHERE DID MY CAPACITY GO". A quarantined lease is the one thing
// that shrinks a fleet without anything having failed: its holder stopped
// heartbeating while a container may still be running, so billet keeps charging
// the host until somebody who can see that machine says otherwise. Without this
// the number is simply smaller than it was, with nothing to read.
func cmdLeasesQuarantined(ctx context.Context, args []string) error {
	fs := newFlagSet("billet leases quarantined")
	cfgPath := addConfigFlag(fs)

	if err := parse(fs, args); err != nil {
		return err
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	held, err := a.Quarantined(ctx)
	if err != nil {
		return err
	}

	if len(held) == 0 {
		fmt.Println("Nothing is quarantined: every lease is either running or finished.")

		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)

	fmt.Fprintln(w, "LEASE\tTIER\tNODE\tVCPU\tMEMORY\tSINCE")

	for i := range held {
		q := &held[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", q.ID, q.Tier, q.Node, q.VCPU, q.Memory, q.Since)
	}

	_ = w.Flush()

	fmt.Printf("\nEach is holding its host's capacity because the container behind it has not\n")
	fmt.Printf("been confirmed gone. A host that comes back frees them by itself. For one that\n")
	fmt.Printf("never will:\n\n  billet leases release <lease> --force\n")

	return nil
}

// cmdLeasesRelease hands back capacity for compute an operator knows is gone.
//
// --force IS REQUIRED AND MEANS SOMETHING. Everything else that ends a
// quarantine is evidence: a node destroyed the container and said so, or came
// back reporting an inventory without it. There is no evidence here — the
// operator is asserting it, usually about a machine that is never coming back —
// and if they are wrong, the capacity is sold to a second job while the first is
// still running on it.
func cmdLeasesRelease(ctx context.Context, args []string) error {
	fs := newFlagSet("billet leases release")
	cfgPath := addConfigFlag(fs)
	force := fs.Bool("force", false, "release it even though nothing has confirmed the compute is gone")

	leaseID, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if leaseID == "" {
		return errors.New("usage: billet leases release <lease> --force")
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	if !*force {
		held, err := a.Held(ctx)
		if err != nil {
			return err
		}

		for i := range held {
			if held[i].ID != leaseID {
				continue
			}

			fmt.Printf("Lease %s has been holding %d vCPU and %s on node %q as %s for %s,\n",
				leaseID, held[i].VCPU, held[i].Memory, held[i].Node, held[i].State,
				heldFor(held[i].Since))
			fmt.Printf("held by %s.\n\n", describeHolder(held[i].Holder))
			fmt.Printf("Nothing has confirmed that its container is gone. If that machine is\n")
			fmt.Printf("coming back it will free this by itself, and releasing it now means the\n")
			fmt.Printf("capacity can be sold to a second job while the first is still running.\n\n")
			fmt.Printf("Re-run with --force when you know the compute is gone.\n")

			// NON-ZERO, because nothing was released. Automation that reads an exit
			// status would otherwise carry on believing the capacity came back.
			return errors.New("refusing to release without --force")
		}

		return fmt.Errorf("lease %s is not held; `billet leases` lists what is",
			leaseID)
	}

	result, err := a.ForceRelease(ctx, leaseID)
	if err != nil {
		return err
	}

	if result.Pending {
		fmt.Printf("Asked node %q to release %s. The node will drop its local custody record and "+
			"return the capacity on its next tend.\n", result.Node, leaseID)
		fmt.Printf("\nOnly the process holding the lease can observe that request. If it is gone,\n")
		fmt.Printf("nothing renews the lease: it is quarantined within the lease TTL, and re-running\n")
		fmt.Printf("this command then releases it on the spot. `billet leases` names the holder.\n")
	} else {
		fmt.Printf("Released %s. Its capacity is available again.\n", leaseID)
	}

	return nil
}
