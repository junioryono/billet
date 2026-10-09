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
// line: `shape` runs until stopped, `clear` removes what a run left behind.
func Uplink(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet uplink shape [--interface NAME] [--reflectors A,B,...], " +
			"or billet uplink clear [--interface NAME]")
	}

	// LINUX ONLY: the shaper is tc and CAKE. A Mac has neither, and refusing
	// here says so rather than failing on the first command it cannot find.
	if runtime.GOOS != "linux" {
		return fmt.Errorf("billet uplink shapes with Linux's tc and CAKE, which %s does not have", runtime.GOOS)
	}

	switch args[0] {
	case "shape":
		return uplinkShape(ctx, env, args[1:])
	case "clear":
		return uplinkClear(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown uplink command %q; try shape or clear", args[0])
}

func uplinkShape(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet uplink shape", env.Stdout)
	iface := fs.String("interface", "", "the interface to shape; empty shapes the one the default route leaves by")
	reflectors := fs.String("reflectors", strings.Join(uplink.DefaultReflectors, ","),
		"IPv4 addresses whose round trip measures the line's queue, comma-separated")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	return uplink.Run(ctx, uplink.Options{
		Iface:      *iface,
		Reflectors: strings.Split(*reflectors, ","),
		Params:     uplink.DefaultParams(),
		Log:        slog.New(slog.NewTextHandler(env.Stderr, nil)),
	})
}

func uplinkClear(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet uplink clear", env.Stdout)
	iface := fs.String("interface", "", "the interface to clear; empty clears the one the default route leaves by")
	if err := cli.Parse(fs, args); err != nil {
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

	(&uplink.Shaper{Iface: name}).Clear(ctx)
	fmt.Fprintf(env.Stdout, "billet uplink: no shaping on %s\n", name)

	return nil
}
