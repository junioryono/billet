package host

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/junioryono/billet/internal/lifeops"
)

// checkUnitProperties asks systemd about a unit through the inspector, bounded
// by its timeout. A variable so a test of runCheck can answer for systemd.
var checkUnitProperties = func(ctx context.Context) func(unit string, props ...string) (map[string][]string, error) {
	inspector := lifeops.NewInspector()

	return func(unit string, props ...string) (map[string][]string, error) {
		return inspector.UnitProperties(ctx, unit, props...)
	}
}

// reportLockDir warns when node.lock_dir is one of billet-node.service's
// runtime directories, or beneath one. systemd removes a unit's runtime
// directories at its stop, so a node restarted under a running `billet images
// verify` unlinks the verifier's lock, and the next verification locks a fresh
// file at the same path and destroys the first one's live probe (#175). The
// units this release ships name the directory in ReadWritePaths and declare
// none of it; a host a rollout upgraded keeps the units it had until a package
// install or a converge. ADVISORY: it reports and decides nothing, and a unit
// systemd could not be asked about is said to be could-not-tell.
func reportLockDir(w io.Writer, lockDir string, show func(unit string, props ...string) (map[string][]string, error)) {
	if lockDir == "" {
		return
	}

	props, err := show(nodeUnit, "LoadState", "RuntimeDirectory")
	if err != nil {
		_, _ = fmt.Fprintf(w, "locks    could not tell whether %s outlives %s's stop: %v\n", lockDir, nodeUnit, err)

		return
	}

	switch load := firstProp(props, "LoadState"); load {
	case "loaded":
	case "not-found":
		return
	default:
		_, _ = fmt.Fprintf(w, "locks    could not tell whether %s outlives %s's stop: systemd reports "+
			"LoadState=%q\n", lockDir, nodeUnit, load)

		return
	}

	dir := filepath.Clean(lockDir)

	for _, entry := range strings.Fields(firstProp(props, "RuntimeDirectory")) {
		declared := filepath.Clean("/run/" + entry)
		if dir == declared || strings.HasPrefix(dir, declared+"/") {
			_, _ = fmt.Fprintf(w, "locks    WARNING: %s declares %s as a RuntimeDirectory, so every stop of "+
				"the node removes %s under the locks other processes hold in it, and a second image "+
				"verification can share the first one's microVM (#175); install the units this release ships "+
				"(converge the host role, or install the package)\n", nodeUnit, declared, dir)

			return
		}
	}

	_, _ = fmt.Fprintf(w, "locks    %s outlives %s's stop (it is none of its runtime directories)\n", dir, nodeUnit)
}
