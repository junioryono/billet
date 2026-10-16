package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/junioryono/billet/internal/lifeops"
)

// checkUnitProperties asks systemd about a unit through the inspector, bounded
// by its timeout. A variable so a caller of runCheck that must not reach the
// host's systemd can answer for it.
var checkUnitProperties = func(ctx context.Context) func(unit string, props ...string) (map[string][]string, error) {
	inspector := lifeops.NewInspector()

	return func(unit string, props ...string) (map[string][]string, error) {
		return inspector.UnitProperties(ctx, unit, props...)
	}
}

// resolveLockDir resolves the lock directory as resolveExisting does; a
// variable so a test can stand a filesystem in.
var resolveLockDir = resolveExisting

// resolveExisting is path with every symlink in its longest existing prefix
// followed and the missing rest joined on: /var/run/billet/locks is
// /run/billet/locks on a host whose /var/run leads to /run, whether or not the
// directory exists yet.
func resolveExisting(path string) (string, error) {
	path = filepath.Clean(path)

	resolved, err := filepath.EvalSymlinks(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}

	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}

	resolvedParent, err := resolveExisting(parent)
	if err != nil {
		return "", err
	}

	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}

// statLockDir examines the lock directory; a variable so a test can stand a
// filesystem in.
var statLockDir = os.Stat

// reportLockDir says whether node.lock_dir outlives billet-node.service's stop.
// systemd removes a unit's runtime directories at its stop, so while the node
// unit declared the lock directory (or a parent of it) a node restarted under a
// running `billet images verify` unlinked the verifier's lock, and the next
// verification locked a fresh file at the same path and destroyed the first
// one's live probe (#175). The units this release ships declare none of it and
// name it in ReadWritePaths, which refuses the node's start when it is missing,
// so a unit that does not declare it needs the directory to exist; a host a
// rollout upgraded keeps the units it had until a package install or a
// converge. ADVISORY: it reports and decides nothing, and anything systemd or
// the filesystem did not answer is said to be could-not-tell.
func reportLockDir(w io.Writer, lockDir string, show func(unit string, props ...string) (map[string][]string, error)) {
	if lockDir == "" {
		return
	}

	unknown := func(why string) {
		_, _ = fmt.Fprintf(w, "locks    could not tell whether %s outlives %s's stop: %s\n", lockDir, nodeUnit, why)
	}

	props, err := show(nodeUnit, "LoadState", "RuntimeDirectory")
	if err != nil {
		unknown(err.Error())

		return
	}

	switch load := firstProp(props, "LoadState"); load {
	case "loaded":
	case "not-found", "masked":
		return
	default:
		unknown(fmt.Sprintf("systemd reports LoadState=%q", load))

		return
	}

	// AN UNANSWERED PROPERTY IS NOT AN EMPTY ONE: only an answer, even an empty
	// one, says what the unit declares.
	if values, ok := props["RuntimeDirectory"]; !ok || len(values) != 1 {
		unknown("systemd did not answer the unit's RuntimeDirectory")

		return
	}

	dir, err := resolveLockDir(lockDir)
	if err != nil {
		unknown("resolve " + lockDir + ": " + err.Error())

		return
	}

	for _, entry := range strings.Fields(firstProp(props, "RuntimeDirectory")) {
		declared := filepath.Clean("/run/" + entry)
		if dir == declared || strings.HasPrefix(dir, declared+"/") {
			_, _ = fmt.Fprintf(w, "locks    WARNING: %s declares %s as a RuntimeDirectory, so every stop of "+
				"the node removes %s under the locks other processes hold in it, and a second image "+
				"verification can share the first one's microVM (#175); install the units this release ships "+
				"(converge the host role, or install the package)\n", nodeUnit, declared, lockDir)

			return
		}
	}

	// NO UNIT MAKES IT, so it has to be there: the shipped unit requires it and
	// refuses to start without it.
	switch info, err := statLockDir(lockDir); {
	case errors.Is(err, fs.ErrNotExist):
		_, _ = fmt.Fprintf(w, "locks    WARNING: %s does not exist, and no unit makes it: billet-node.service "+
			"will not start (226/NAMESPACE) until `systemd-tmpfiles --create` makes it from billet's tmpfiles.d entry "+
			"(the package and the host role install one), or the host reboots\n", lockDir)
	case err != nil:
		unknown("examine " + lockDir + ": " + err.Error())
	case !info.IsDir():
		_, _ = fmt.Fprintf(w, "locks    WARNING: %s is not a directory\n", lockDir)
	default:
		_, _ = fmt.Fprintf(w, "locks    %s outlives %s's stop (it is none of its runtime directories)\n",
			lockDir, nodeUnit)
	}
}
