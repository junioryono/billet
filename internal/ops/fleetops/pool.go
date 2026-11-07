package fleetops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
)

const poolUsage = "usage: billet pool [--config PATH]\n" +
	"       billet pool abandon [--config PATH] --reason TEXT <order>\n" +
	"       billet pool unhold [--config PATH] <order>\n" +
	"       billet pool release [--config PATH] --reason TEXT <order>\n" +
	"       billet pool release [--config PATH] <order>   (seal a release already asked for)\n" +
	"       billet pool close [--config PATH] --reason TEXT <order>"

// Pool is `billet pool`: what the pool of rented bare metal ordered and what became of
// each order (ADR-030), closing an uncertain order, releasing a held join, and
// releasing a host, each on a person's word.
func Pool(ctx context.Context, env cli.Env, args []string) error {
	if len(args) > 0 && args[0] == "abandon" {
		return poolAbandon(ctx, env, args[1:])
	}
	if len(args) > 0 && args[0] == "unhold" {
		return poolUnhold(ctx, env, args[1:])
	}
	if len(args) > 0 && args[0] == "release" {
		return poolRelease(ctx, env, args[1:])
	}
	if len(args) > 0 && args[0] == "close" {
		return poolClose(ctx, env, args[1:])
	}
	fs := cli.NewFlagSet("billet pool", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(poolUsage)
	}
	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	orders, err := a.PoolOrders(ctx)
	if err != nil {
		return err
	}
	if len(orders) == 0 {
		fmt.Fprintln(env.Stdout, "The pool has placed no order.")

		return nil
	}

	return printPoolOrders(env, orders)
}

func printPoolOrders(env cli.Env, orders []alloc.PoolOrder) error {
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ORDER\tARCH\tSOURCE\tSCOPE\tSHAPE\tLOCATION\tSTATE\tHOST\tNODE\tFIRST ATTEMPT\tREASON")
	uncertain := 0
	for i := range orders {
		o := &orders[i]
		if o.State == alloc.PoolUncertain {
			uncertain++
		}
		st := o.State
		if o.State == alloc.PoolPlaced && !o.ReleaseRequested.IsZero() {
			st = "draining"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.Arch, o.Source, o.Scope, o.Shape,
			dash(o.Location), st, dash(o.HostID), dash(o.NodeName),
			o.FirstAttempt.UTC().Format(time.RFC3339), dash(oneLine(o.Reason)))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// WHY A HOST WAS RELEASED OUTLIVES ITS DRAINING: the REASON column is the order's own
	// ending (refused, gone, closed by a person), so the person's release reason is listed
	// for every order it was asked of, whatever the order has become since.
	releases := false
	for i := range orders {
		if o := &orders[i]; !o.ReleaseRequested.IsZero() {
			if !releases {
				fmt.Fprintf(env.Stdout, "\nReleased by a person (ADR-030). A draining host is out of placement, and is "+
					"sealed, revoked and released once nothing holds it and its node is decommissioned on proof "+
					"(docs/operating/bare-metal-pool.md).\n")
				releases = true
			}
			st := o.State
			switch {
			case o.State == alloc.PoolPlaced:
				st = "draining"
			case o.State == alloc.PoolSealed && o.ReleaseContradicted:
				// ITS NODE SAID IT IS BACK AFTER THE SEAL, so its source is never asked.
				st = "sealed, contradicted since: take it back by hand and `billet pool close` it"
			}
			fmt.Fprintf(env.Stdout, "  %s (%s) since %s: %s\n", o.ID, st, o.ReleaseRequested.UTC().Format(time.RFC3339),
				oneLine(o.ReleaseReason))
		}
	}
	if uncertain > 0 {
		fmt.Fprintf(env.Stdout, "\n%d uncertain: the source's answer was lost and a resend could place a second host. "+
			"Look for the order where its SCOPE says it was placed, in the source's own console (an EC2 instance tagged "+
			"sh.billet.pool-order, a phoenixNAP server named billet-<order>); if there is none, close it with `billet pool abandon --reason ... <order>`.\n",
			uncertain)
	}

	// A HELD JOIN IS A HOST BILLED AND NOT SERVING, which only a person moves.
	held := false
	for i := range orders {
		if o := &orders[i]; o.JoinHold != "" {
			if !held {
				fmt.Fprintf(env.Stdout, "\nJoins held for a person (ADR-030): the order admits no node on its source's word. "+
					"Look at the host: approve a request it recorded (`billet nodes pending`, `billet nodes approve`), release "+
					"the hold so its next ask is judged again (`billet pool unhold <order>`), or release the host.\n")
				held = true
			}
			fmt.Fprintf(env.Stdout, "  held %s: %s\n", o.ID, oneLine(o.JoinHold))
		}
	}

	return nil
}

func poolAbandon(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet pool abandon", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	reason := fs.String("reason", "", "what you checked in the source's console, recorded on the order")
	order, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}
	if order == "" || strings.TrimSpace(*reason) == "" {
		return errors.New(poolUsage)
	}
	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	if err := a.PoolOrderAbandoned(ctx, order, strings.TrimSpace(*reason)); err != nil {
		if errors.Is(err, alloc.ErrPoolOrderMoved) {
			return fmt.Errorf("billet pool abandon: %s is not uncertain; only an order whose answer was lost is "+
				"closed by hand", order)
		}

		return err
	}
	fmt.Fprintf(env.Stdout, "Abandoned %s.\n", order)

	return nil
}

// poolUnhold releases an order's held join on a person's word: its host, which keeps
// asking while it is held, is judged again on its next ask.
func poolUnhold(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet pool unhold", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	order, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}
	if order == "" {
		return errors.New(poolUsage)
	}
	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	if err := a.ReleasePoolJoin(ctx, order); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Released the held join of %s; its host is judged again on its next ask.\n", order)

	return nil
}

// poolRelease asks for a placed host's release on a person's word (ADR-030). A host a
// node joined from leaves placement now and is released once the ledger shows it holds
// nothing; one nothing joined from is sealed now, on the proof that nothing was ever
// admitted for it. The pool's next step asks the source in either case.
func poolRelease(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet pool release", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	reason := fs.String("reason", "", "why the host is released, recorded on the order")
	order, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}
	if order == "" {
		return errors.New(poolUsage)
	}
	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	// NO REASON IS THE SECOND ASK: a release asked for already is sealed on its own reason.
	if strings.TrimSpace(*reason) == "" {
		err := sealPoolRelease(ctx, env, a, order)
		if errors.Is(err, alloc.ErrPoolOrderMoved) {
			return fmt.Errorf("billet pool release: %s has no release of a joined host waiting to be sealed; "+
				"`billet pool release --reason TEXT %s` asks for one: %w", order, order, err)
		}

		return err
	}
	sealed, err := a.RequestPoolRelease(ctx, order, oneLine(*reason))
	if errors.Is(err, alloc.ErrPoolReleaseAsked) {
		return sealPoolRelease(ctx, env, a, order)
	}
	if err != nil {
		return err
	}
	if sealed {
		fmt.Fprintf(env.Stdout, "Sealed %s: nothing ever joined from it, and nothing will. The pool's next step asks "+
			"its source to take the host back.\n", order)

		return nil
	}
	fmt.Fprintf(env.Stdout, "Released %s from placement. Once nothing holds its host and its node is decommissioned "+
		"on proof (`billet nodes decommission %s`), the pool seals it, revokes its node's certificates and asks its "+
		"source to take it back, or `billet pool release %s` (no reason) seals it; `billet pool` shows where it is.\n",
		order, alloc.PoolNodeName(order), order)

	return nil
}

// sealPoolRelease seals a release asked for already, on the ledger's proof that its host
// runs nothing: the same transaction the pool's step seals with, so a deployment with no
// pool configured, or one whose pool is not running, can still finish a release.
func sealPoolRelease(ctx context.Context, env cli.Env, a *alloc.Allocator, order string) error {
	if _, err := a.SealPoolRelease(ctx, order); err != nil {
		if errors.Is(err, alloc.ErrPoolReleaseWaits) {
			return fmt.Errorf("billet pool release: %s is out of placement and cannot be sealed yet: %w. Until "+
				"nothing holds it and `billet nodes decommission %s` has said it was proved, it is not; "+
				"docs/operating/bare-metal-pool.md has the procedure", order, err, alloc.PoolNodeName(order))
		}

		return err
	}
	fmt.Fprintf(env.Stdout, "Sealed %s: its host was proved to run nothing and its node's certificates are revoked. "+
		"The pool's next step asks its source to take the host back; with no pool running, take it back in the "+
		"source's console and `billet pool close` it.\n", order)

	return nil
}

// poolClose closes a sealed or released order on a person's word, after they have looked
// at the source themselves: how a release closes on a source that never says gone of a
// host it took back (phoenixNAP), on one no longer configured or ordering elsewhere, and
// on a sealed host a person took back by hand. The order stops counting against the
// maximum, and its reason records that a person closed it.
func poolClose(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet pool close", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	reason := fs.String("reason", "", "what you checked in the source's console, recorded on the order")
	order, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}
	if order == "" || strings.TrimSpace(*reason) == "" {
		return errors.New(poolUsage)
	}
	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	if err := a.ClosePoolOrder(ctx, order, oneLine(*reason)); err != nil {
		if errors.Is(err, alloc.ErrPoolOrderMoved) {
			return fmt.Errorf("billet pool close: %s is neither sealed nor released; only a host whose release is "+
				"proved is closed by hand", order)
		}

		return err
	}
	fmt.Fprintf(env.Stdout, "Closed %s.\n", order)

	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
