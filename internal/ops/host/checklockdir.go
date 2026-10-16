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

	"github.com/junioryono/billet/deploy"
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

// resolveLockDir resolves the lock directory through every symlink on its
// way, a dangling one included, so /var/run/billet/locks is /run/billet/locks
// on a host whose /var/run leads to /run, and a link to a directory a stopped
// unit removed still names it; a variable so a test can stand a filesystem in.
var resolveLockDir = lifeops.ResolveOperationPath

// statLockDir examines the lock directory; a variable so a test can stand a
// filesystem in.
var statLockDir = os.Stat

// reportLockDir says whether node.lock_dir outlives billet-node.service's stop.
// systemd removes a unit's runtime directories at its stop, so while the node
// unit declared the lock directory (or a parent of it) a node restarted under a
// running `billet images verify` unlinked the verifier's lock, and the next
// verification locked a fresh file at the same path and destroyed the first
// one's live probe (#175). The node unit this release ships declares none of it
// and names it in ReadWritePaths, which refuses the node's start when it is
// missing, so a unit that does not declare it needs the directory to exist; a
// host a rollout upgraded keeps the units it had until a package install or a
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

	dir, err := resolveLockDir(filepath.Clean(lockDir))
	if err != nil {
		unknown("resolve " + lockDir + ": " + err.Error())

		return
	}

	within := func(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }

	for _, entry := range strings.Fields(firstProp(props, "RuntimeDirectory")) {
		declared := filepath.Clean("/run/" + entry)
		if !within(dir, declared) {
			continue
		}

		// A UNIT FROM BEFORE #175 declares the shipped lock directory; any
		// other runtime directory is the configuration's to move away from.
		if within(deploy.NodeLockDir, declared) {
			_, _ = fmt.Fprintf(w, "locks    WARNING: %s declares %s as a RuntimeDirectory, so every stop of "+
				"the node removes %s under the locks other processes hold in it, and a second image "+
				"verification can share the first one's microVM (#175); install the units this release ships "+
				"(converge the host role, or install the package)\n", nodeUnit, declared, lockDir)
		} else {
			_, _ = fmt.Fprintf(w, "locks    WARNING: %s is under %s's RuntimeDirectory %s, which every stop of the "+
				"node removes under the locks other processes hold in it (#175); set node.lock_dir to %s\n",
				lockDir, nodeUnit, declared, deploy.NodeLockDir)
		}

		return
	}

	// THE SHIPPED UNIT MAKES ONE DIRECTORY WRITABLE FOR A LOCK, and refuses to
	// start without it: anywhere else is read-only under ProtectSystem=strict.
	// Judged as SPELLED, as `billet local up` judges it, which refuses any other
	// spelling of the same directory.
	if filepath.Clean(lockDir) != deploy.NodeLockDir {
		_, _ = fmt.Fprintf(w, "locks    WARNING: %s is not %s, the one directory %s makes writable for its lock "+
			"and the one spelling `billet local up` accepts; set node.lock_dir to %s\n", lockDir, deploy.NodeLockDir,
			nodeUnit, deploy.NodeLockDir)

		return
	}

	if dir != deploy.NodeLockDir {
		_, _ = fmt.Fprintf(w, "locks    WARNING: %s leads to %s, so the lock records billet-node.service and an "+
			"image verification take are wherever that link points; make %s a directory, as billet's "+
			"tmpfiles.d entry does at boot\n", lockDir, dir, deploy.NodeLockDir)

		return
	}

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
