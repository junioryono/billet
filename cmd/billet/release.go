package main

import (
	"context"
	"fmt"
)

// cmdRelease groups what a host can say about the release it is running.
func cmdRelease(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: billet release record --manifest <path> --archive " +
			"<path> --binary <path>, or billet release inspect [--json] [--config <path>]")
	}

	switch args[0] {
	case "record":
		return cmdReleaseRecord(ctx, args[1:])
	case "inspect":
		return cmdReleaseInspect(ctx, args[1:])
	}

	return fmt.Errorf("unknown release command %q; try record or inspect", args[0])
}
