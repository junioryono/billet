package host

import (
	"context"

	"github.com/junioryono/billet/internal/cli"
)

// Join is `billet join`: a node joining an existing deployment; as `billet
// join accept`, the control plane taking the joining node's half; and as
// `billet join prerequisites`, the privileged half a Mac's join runs under
// sudo.
func Join(ctx context.Context, env cli.Env, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "accept":
			return joinAccept(ctx, env, args[1:])
		case "prerequisites":
			return joinPrerequisitesRoot(ctx, env, args[1:])
		}
	}

	return joinNode(ctx, env, args)
}
