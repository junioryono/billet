package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"strings"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/uplink"
)

// Uplink keeps this host's own traffic from filling its site's shared internet
// line: `shape` runs until stopped, `check` says whether this host can be
// shaped, `clear` removes what a run left behind.
func Uplink(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet uplink shape [--interface NAME] [--reflectors A,B,...] " +
			"[--max-up MBIT] [--max-down MBIT], " +
			"billet uplink check [--interface NAME], or billet uplink clear [--interface NAME]")
	}

	switch args[0] {
	case "shape":
		return uplinkShape(ctx, env, args[1:])
	case "check":
		return uplinkCheck(ctx, env, args[1:])
	case "clear":
		return uplinkClear(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown uplink command %q; try shape, check or clear", args[0])
}

// linuxOnly is checked after a command's flags are parsed, so `-h` answers on
// every platform: the shaper is tc and CAKE, which only Linux has.
func linuxOnly() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("billet uplink shapes with Linux's tc and CAKE, which %s does not have", runtime.GOOS)
	}

	return nil
}

func uplinkShape(ctx context.Context, env cli.Env, args []string) error {
	opts, err := uplinkShapeOptions(env, args)
	if err != nil {
		return err
	}

	if err := linuxOnly(); err != nil {
		return err
	}

	return uplink.Run(ctx, opts)
}

// uplinkShapeOptions is what `billet uplink shape`'s flags ask the shaper for.
func uplinkShapeOptions(env cli.Env, args []string) (uplink.Options, error) {
	fs := cli.NewFlagSet("billet uplink shape", env.Stdout)
	iface := fs.String("interface", "", "the interface to shape; empty shapes the one the default route leaves by")
	reflectors := fs.String("reflectors", strings.Join(uplink.DefaultReflectors, ","),
		"IPv4 addresses whose round trip measures the line's queue, comma-separated")
	maxUp := fs.Float64("max-up", 0, "the most this host sends, in Mbit/s; 0 is the interface's speed")
	maxDown := fs.Float64("max-down", 0, "the most this host receives, in Mbit/s; 0 is the interface's speed")
	if err := cli.Parse(fs, args); err != nil {
		return uplink.Options{}, err
	}

	for name, value := range map[string]float64{"--max-up": *maxUp, "--max-down": *maxDown} {
		if err := uplinkCeiling(name, value); err != nil {
			return uplink.Options{}, err
		}
	}

	return uplink.Options{
		Iface:      *iface,
		Reflectors: strings.Split(*reflectors, ","),
		MaxUp:      *maxUp,
		MaxDown:    *maxDown,
		Params:     uplink.DefaultParams(),
		Log:        slog.New(slog.NewTextHandler(env.Stderr, nil)),
	}, nil
}

// uplinkCeiling refuses a ceiling the shaper could not hold: a negative or
// non-finite one, and one under the floor no cut goes below.
func uplinkCeiling(name string, mbit float64) error {
	if mbit == 0 {
		return nil
	}

	if math.IsNaN(mbit) || math.IsInf(mbit, 0) || mbit < uplink.Floor {
		return fmt.Errorf("%s %v: want 0 (the interface's speed) or at least %v Mbit/s", name, mbit, uplink.Floor)
	}

	return nil
}

func uplinkCheck(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet uplink check", env.Stdout)
	iface := fs.String("interface", "", "the interface to check; empty checks the one the default route leaves by")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if err := linuxOnly(); err != nil {
		return err
	}

	name := *iface
	if name == "" {
		found, err := uplink.DefaultInterface()
		if err != nil {
			return err
		}

		name = found
	}

	// WHAT A RUNNING SHAPER INSTALLED IS ITS OWN AND PASSES, which is the state a
	// converge finds; the record decides that, by the interface's index.
	shaper := &uplink.Shaper{Iface: name}
	if r, ok := uplink.ReadRecord(); ok {
		if current, gone, err := r.Resolve(); err == nil && !gone && current == name && r.IFB == shaper.IFB() {
			shaper.Owned = true
		}
	}

	if err := shaper.Check(ctx); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "billet uplink: %s can be shaped\n", name)

	return nil
}

func uplinkClear(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet uplink clear", env.Stdout)
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if err := linuxOnly(); err != nil {
		return err
	}

	// NOT WHILE A SHAPER RUNS: clearing under it would take its qdiscs away while
	// it goes on adjusting them. The unit's ExecStopPost runs after the shaper
	// has exited, when the claim is free.
	release, err := uplink.Lock()
	if err != nil {
		return err
	}
	defer release()

	// BY THE RECORD AND NOTHING ELSE: wherever the recorded interface is now, and
	// nothing at all without a record, because nothing else proves what is on an
	// interface is billet's.
	r, err := uplink.ClearRecorded(ctx)
	if err != nil {
		return err
	}

	if r.Iface == "" {
		fmt.Fprintln(env.Stdout, "billet uplink: no record of shaping on this host; nothing to clear")

		return nil
	}

	fmt.Fprintf(env.Stdout, "billet uplink: cleared what was recorded on %s\n", r.Iface)

	return nil
}
