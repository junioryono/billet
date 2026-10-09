package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
		return errors.New("usage: billet uplink shape [--interface NAME] [--reflectors A,B,...], " +
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
	fs := cli.NewFlagSet("billet uplink shape", env.Stdout)
	iface := fs.String("interface", "", "the interface to shape; empty shapes the one the default route leaves by")
	reflectors := fs.String("reflectors", strings.Join(uplink.DefaultReflectors, ","),
		"IPv4 addresses whose round trip measures the line's queue, comma-separated")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if err := linuxOnly(); err != nil {
		return err
	}

	return uplink.Run(ctx, uplink.Options{
		Iface:      *iface,
		Reflectors: strings.Split(*reflectors, ","),
		Params:     uplink.DefaultParams(),
		Log:        slog.New(slog.NewTextHandler(env.Stderr, nil)),
	})
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

	name, err := pickInterface(*iface, "")
	if err != nil {
		return err
	}

	if err := (&uplink.Shaper{Iface: name, Owned: uplink.RecordedInterface() == name}).Check(ctx); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "billet uplink: %s can be shaped\n", name)

	return nil
}

func uplinkClear(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet uplink clear", env.Stdout)
	iface := fs.String("interface", "", "the interface to clear; empty clears the one the last run recorded, "+
		"or else the default route's")
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

	// THE RECORDED INTERFACE FIRST: after a crash the default route may have
	// moved, and the shaping is on the interface the shaper chose.
	name, err := pickInterface(*iface, uplink.RecordedInterface())
	if err != nil {
		return err
	}

	// BILLET'S ONLY BY ITS RECORD, whichever interface is named: without it the
	// cleanup touches nothing, because nothing proves what is there is billet's.
	owned := uplink.RecordedInterface() == name
	if err := (&uplink.Shaper{Iface: name, Owned: owned}).Clear(ctx); err != nil {
		return fmt.Errorf("remove the shaping from %s: %w", name, err)
	}

	if err := uplink.Forget(name); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "billet uplink: no shaping of billet's on %s\n", name)

	return nil
}

func pickInterface(named, recorded string) (string, error) {
	switch {
	case named != "":
		return named, nil
	case recorded != "":
		return recorded, nil
	}

	return uplink.DefaultInterface()
}
