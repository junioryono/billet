package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/state"
)

// errForceRefused is "I showed you what this would destroy and you did not
// confirm", which is not a billet failure and must not exit the same way as one.
//
// NON-ZERO, because nothing was destroyed. Automation that read an exit status of
// zero would carry on believing the compute is gone and the capacity is back.
var errForceRefused = &exitError{
	code: 2,
	msg:  "nothing was destroyed",
}

// cmdForceDestroy destroys compute that is still running a job.
//
// THE ONE OPERATION IN BILLET THAT FAILS SOMEBODY'S BUILD ON PURPOSE. Everything
// else that tears anything down either acts on a job GitHub has already concluded
// or leaves running work alone: a drain waits for as long as the work takes, a
// second signal ends the waiting rather than the work, and a shutdown leaves the
// guests running for the next control plane to re-adopt. That is why this is a
// separate command with a separate name rather than a flag on any of them.
//
// IT IS AN EMERGENCY, SO IT SAYS WHAT IT WILL COST BEFORE IT ASKS. GitHub does
// not requeue a job whose runner vanished after it started — the reassignment it
// documents is for a job never acquired in time — so each of these is a build
// that fails and stays failed. Naming the run ids is what lets an operator
// recognise whose work they are about to end; "7 leases" tells them nothing.
func cmdForceDestroy(ctx context.Context, args []string) error {
	fs := newFlagSet("billet force-destroy")
	cfgPath := addConfigFlag(fs)
	reason := fs.String("reason", "",
		"why running work is being destroyed, for whoever finds the failed builds")
	tier := fs.String("tier", "", "only destroy compute in this tier")
	node := fs.String("node", "", "only destroy compute on this host")
	yes := fs.Bool("yes", false,
		"actually destroy it (without this, only report what would be destroyed)")

	if err := parse(fs, args); err != nil {
		return err
	}

	db, cfg, err := openLedgerForAdmission(ctx, *cfgPath)
	if err != nil {
		return err
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

	// ALREADY RUNNING IS REPORTED, NOT REPLACED. A second force would enumerate a
	// set the first is midway through destroying, and neither diagnostic would
	// then describe what happened to anything.
	if open, found, err := allocator.OpenForceDestroy(ctx); err != nil {
		return fmt.Errorf("read force-destroy: %w", err)
	} else if found {
		return reportOpenForce(ctx, allocator, open)
	}

	admission, err := db.Admission(ctx)
	if err != nil {
		return fmt.Errorf("read admission: %w", err)
	}

	// SEALED FIRST, AND BY AN OPERATOR. This command enumerates a set, shows it to
	// a person, and acts on their answer — so admission has to be closed across
	// all three or a job admitted in between is destroyed without ever appearing
	// in the list that was approved. A local-down seal will not do: it is cleared
	// by the next `billet local up`, so a routine restart would reopen admission
	// underneath the force.
	//
	// REFUSED RATHER THAN TAKEN HERE. Sealing on the operator's behalf would make
	// a command that only meant to LOOK at what is running change the deployment's
	// state, and the seal outlives this process — an aborted force would leave a
	// deployment nobody meant to quiesce.
	if admission.Mode != state.AdmissionSealed ||
		admission.Provenance != state.ProvenanceOperator {
		return fmt.Errorf("this deployment is %s, and a force-destroy has to be taken "+
			"against one an operator has already sealed. Run `billet drain --reason ...` "+
			"first: sealing stops new work arriving while you decide, and the list this "+
			"command shows you would otherwise be out of date by the time you answer it",
			admission.Mode)
	}

	candidates, err := allocator.ForceDestroyCandidates(ctx, *tier, *node)
	if err != nil {
		return fmt.Errorf("list what is running: %w", err)
	}

	if len(candidates) == 0 {
		fmt.Printf("Nothing is running that this would destroy.\n")

		return reportHeldElsewhere(ctx, allocator)
	}

	printForceCandidates(candidates)

	if err := reportHeldElsewhere(ctx, allocator); err != nil {
		return err
	}

	if !*yes {
		fmt.Printf("\nRe-run with --yes and --reason to destroy these.\n")

		return errForceRefused
	}

	if *reason == "" {
		// REQUIRED ALONGSIDE --yes, because this record is the only answer to
		// "why did my build die". A force nobody can attribute is one nobody can
		// explain afterwards.
		return errors.New("--yes needs --reason: this is the only record of why these " +
			"builds were failed on purpose")
	}

	targets := make([]state.ForceTarget, 0, len(candidates))

	for i := range candidates {
		c := &candidates[i]
		targets = append(targets, state.ForceTarget{
			LeaseID:          c.ID,
			Tier:             c.Tier,
			Node:             c.Node,
			RunID:            c.RunID,
			SchedulerRequest: c.SchedulerRequest,
			Phase:            string(c.Phase),
		})
	}

	// THE ADMISSION GENERATION THE LIST WAS TAKEN AGAINST. If somebody resumed and
	// resealed while this operator was reading, the set above was enumerated on a
	// deployment that has since admitted work, so the ledger refuses it rather
	// than acting on a stale list.
	recorded, err := allocator.RequestForceDestroy(ctx, state.ForceDestroyRequest{
		ExpectAdmission: admission.Generation,
		Reason:          *reason,
		Actor:           actor(),
		Targets:         targets,
	})
	if err != nil {
		if errors.Is(err, state.ErrForceDestroyOpen) {
			return fmt.Errorf("%w; watch it with `billet status`", err)
		}

		return err
	}

	fmt.Printf("\nRecorded force-destroy %d covering %d lease(s).\n",
		recorded.Generation, len(targets))
	fmt.Printf("\nThe listeners act on this on their next poll, which can take most of a\n")
	fmt.Printf("minute. Watch it with `billet status`; the record says what became of every\n")
	fmt.Printf("lease, including any this could not destroy.\n")

	return nil
}

// printForceCandidates names every job about to be ended.
func printForceCandidates(candidates []alloc.ForceCandidate) {
	fmt.Printf("This will DESTROY %d running job(s). GitHub does NOT requeue a job whose\n",
		len(candidates))
	fmt.Printf("runner vanishes after it has started, so every one of these builds fails\n")
	fmt.Printf("and stays failed.\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "LEASE\tTIER\tNODE\tPHASE\tRUN\tRUNNING FOR")

	for i := range candidates {
		c := &candidates[i]

		run := c.RunID
		if run == "" {
			run = "-"
		}

		node := c.Node
		if node == "" {
			node = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Tier, node, c.Phase, run,
			heldFor(c.Since))
	}

	_ = w.Flush()
}

// reportHeldElsewhere names the leases a force does NOT cover.
//
// SAID OUT LOUD, because the alternative is an operator who forced everything and
// still has capacity missing. Custody, teardown and quarantine are held by a NODE
// rather than by a listener, and billet already has the operation for them — one
// that goes THROUGH the holder instead of changing the ledger underneath a process
// that still believes it owns the proof obligation. Silently including them here
// would be a second mechanism for the same situation, and the one that skips the
// handoff; silently omitting them leaves somebody hunting for the difference.
func reportHeldElsewhere(ctx context.Context, a *alloc.Allocator) error {
	held, err := a.Held(ctx)
	if err != nil {
		return fmt.Errorf("list held leases: %w", err)
	}

	if len(held) == 0 {
		return nil
	}

	fmt.Printf("\n%d lease(s) are held by a node rather than by a listener, and this\n",
		len(held))
	fmt.Printf("command does not touch them:\n\n")
	printHeld(held)
	fmt.Printf("\nEach is a proof obligation its holder is still working on. When you know\n")
	fmt.Printf("that compute is gone:\n\n  billet leases release <lease> --force\n")

	return nil
}

// printForceDestroy reports the last operator instruction to destroy running
// compute, for `billet status`.
//
// THE LAST ONE, NOT ONLY AN OPEN ONE. A finished force is the explanation for a
// set of failed builds and for capacity that came back without anything having
// finished, and an operator arriving afterwards has no other way to learn it
// happened — the listener logs scroll away, and every other line in that report
// reads perfectly normally.
func printForceDestroy(ctx context.Context, a *alloc.Allocator, admission state.Admission) {
	last, found, err := a.LatestForceDestroy(ctx)
	if err != nil {
		fmt.Printf("force     unavailable: %v\n", err)

		return
	}

	if !found {
		return
	}

	targets, err := a.ForceTargets(ctx, last.Generation)
	if err != nil {
		fmt.Printf("force     %d: %v\n", last.Generation, err)

		return
	}

	var pending, destroyed, failed int

	for i := range targets {
		switch targets[i].State {
		case state.ForceTargetPending:
			pending++
		case state.ForceTargetDestroyed:
			destroyed++
		case state.ForceTargetFailed:
			failed++
		}
	}

	if last.State == state.ForceRequested {
		fmt.Printf("force     %d IN PROGRESS - destroying running compute on an operator's "+
			"instruction\n", last.Generation)
	} else {
		fmt.Printf("force     %d finished at %s\n", last.Generation, last.CompletedAt)
	}

	if last.Actor != "" {
		fmt.Printf("          requested by %s: %s\n", last.Actor, last.Reason)
	}

	fmt.Printf("          %d lease(s): %d destroyed, %d not destroyed, %d still to act on\n",
		len(targets), destroyed, failed, pending)

	// A DESTROYED JOB DOES NOT COME BACK, said here because this report is where
	// somebody lands when they are working out why a build vanished.
	if destroyed > 0 {
		fmt.Printf("          those builds FAILED; GitHub does not requeue a job whose " +
			"runner vanished after starting\n")
	}

	// NOT DESTROYED IS NOT PROOF IT SURVIVED, and the capacity stays charged
	// either way — which is the thing an operator will otherwise go looking for.
	if failed > 0 {
		fmt.Printf("          %d could not be confirmed destroyed and stay charged; "+
			"`billet leases` says what is holding them\n", failed)
	}

	// THE SEAL IT WAS AUTHORISED AGAINST, so a reader can see whether the
	// deployment has been reopened since. A force taken against generation N on a
	// deployment now at N+2 was authorised in a state that no longer holds, and
	// that belongs beside the result rather than being reconstructed.
	if admission.Generation != last.AdmissionGeneration {
		fmt.Printf("          authorised against admission generation %d; the ledger is now "+
			"at %d\n", last.AdmissionGeneration, admission.Generation)
	}
}

// reportOpenForce describes a force-destroy that has not finished.
func reportOpenForce(ctx context.Context, a *alloc.Allocator, open state.ForceDestroy) error {
	targets, err := a.ForceTargets(ctx, open.Generation)
	if err != nil {
		return fmt.Errorf("read force-destroy targets: %w", err)
	}

	fmt.Printf("Force-destroy %d is still running, requested by %s: %s\n",
		open.Generation, open.Actor, open.Reason)
	fmt.Printf("\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "LEASE\tTIER\tNODE\tRUN\tSTATE\tDETAIL")

	for i := range targets {
		t := &targets[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.LeaseID, t.Tier, t.Node, t.RunID,
			t.State, t.Detail)
	}

	_ = w.Flush()

	fmt.Printf("\nA listener acts on its own tier's leases on its next poll. Only one\n")
	fmt.Printf("force-destroy runs at a time, so this one finishes before another can\n")
	fmt.Printf("be taken.\n")

	// NON-ZERO, because this request was not taken. A script that read zero would
	// believe its own force had been recorded.
	return errForceRefused
}
