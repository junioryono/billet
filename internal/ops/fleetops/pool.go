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
	"       billet pool abandon [--config PATH] --reason TEXT <order>"

// Pool is `billet pool`: what the pool of rented bare metal ordered and what became of
// each order (ADR-028), and closing an uncertain order on a person's word.
func Pool(ctx context.Context, env cli.Env, args []string) error {
	if len(args) > 0 && args[0] == "abandon" {
		return poolAbandon(ctx, env, args[1:])
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
	fmt.Fprintln(w, "ORDER\tARCH\tSOURCE\tSHAPE\tLOCATION\tSTATE\tHOST\tNODE\tFIRST ATTEMPT\tREASON")
	uncertain := 0
	for i := range orders {
		o := &orders[i]
		if o.State == alloc.PoolUncertain {
			uncertain++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.Arch, o.Source, o.Shape,
			dash(o.Location), o.State, dash(o.HostID), dash(o.NodeName),
			o.FirstAttempt.UTC().Format(time.RFC3339), dash(oneLine(o.Reason)))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if uncertain > 0 {
		fmt.Fprintf(env.Stdout, "\n%d uncertain: the source's answer was lost and a resend could place a second host. "+
			"Look for the order in the source's own console (an EC2 instance tagged sh.billet.pool-order, a phoenixNAP "+
			"server named billet-<order>); if there is none, close it with `billet pool abandon --reason ... <order>`.\n",
			uncertain)
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

func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
