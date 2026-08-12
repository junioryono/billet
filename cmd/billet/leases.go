package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
)

// cmdLeases is the operator's view of capacity that has not come back.
func cmdLeases(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet leases quarantined | billet leases release <lease> --force")
	}

	switch args[0] {
	case "quarantined":
		return cmdLeasesQuarantined(ctx, args[1:])
	case "release":
		return cmdLeasesRelease(ctx, args[1:])
	}

	return fmt.Errorf("unknown leases command %q; try quarantined or release", args[0])
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
		held, err := a.Quarantined(ctx)
		if err != nil {
			return err
		}

		for i := range held {
			if held[i].ID != leaseID {
				continue
			}

			fmt.Printf("Lease %s has been holding %d vCPU and %s on node %q since %s.\n\n",
				leaseID, held[i].VCPU, held[i].Memory, held[i].Node, held[i].Since)
			fmt.Printf("Nothing has confirmed that its container is gone. If that machine is\n")
			fmt.Printf("coming back it will free this by itself, and releasing it now means the\n")
			fmt.Printf("capacity can be sold to a second job while the first is still running.\n\n")
			fmt.Printf("Re-run with --force when you know the compute is gone.\n")

			return nil
		}

		return fmt.Errorf("lease %s is not quarantined; `billet leases quarantined` lists what is",
			leaseID)
	}

	if err := a.ResolveQuarantine(ctx, leaseID); err != nil {
		return err
	}

	fmt.Printf("Released %s. Its capacity is available again.\n", leaseID)

	return nil
}
