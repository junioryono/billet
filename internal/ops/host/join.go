package host

import (
	"context"

	"github.com/junioryono/billet/internal/cli"
)

// Join is `billet join`: a node joining an existing deployment, and, as
// `billet join accept`, the control plane taking the joining node's half.
func Join(ctx context.Context, env cli.Env, args []string) error {
	if len(args) > 0 && args[0] == "accept" {
		return joinAccept(ctx, env, args[1:])
	}

	return joinNode(ctx, env, args)
}
