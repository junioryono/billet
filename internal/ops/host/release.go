package host

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/cli"
)

// cmdRelease groups what a host can say about the release it is running.
func Release(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: billet release record --manifest <path> --archive " +
			"<path> --binary <path>, or billet release inspect [--json] [--config <path>]")
	}

	switch args[0] {
	case "record":
		return cmdReleaseRecord(ctx, env, args[1:])
	case "inspect":
		return cmdReleaseInspect(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown release command %q; try record or inspect", args[0])
}
