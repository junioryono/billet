package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	opsimages "github.com/junioryono/billet/internal/ops/images"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/unitpath"
)

// unitPathLockWait bounds an exercise's wait for a lock another billet holds.
const unitPathLockWait = 5 * time.Second

// unitDeclaration is one shipped unit and the paths its command touches.
type unitDeclaration struct {
	unit string
	// root says the unit runs its command as root. An exercise of a unit that
	// does not is refused as root: what it creates, a deployment identity or a
	// lock beside the ledger, would be root's, and the service account could not
	// open it at the unit's next start.
	root  bool
	paths func(cli.Env, *config.Config, string) []unitpath.Need
}

// unitDeclarations are the shipped units whose sandbox is ProtectSystem=strict
// (#174), IN THE ORDER `billet local up` STARTS THEM: a node beside a control
// plane claims the identity the server founded, which the node's own sandbox
// cannot write. A unit added under a strict sandbox is added here, or unit-paths
// cannot be asked about it and the gate in scripts/test-unit-sandboxes.sh, which
// asks `--units`, does not start it.
var unitDeclarations = []unitDeclaration{
	{unit: deploy.ServerUnitName, paths: serverPaths},
	{unit: deploy.BackupUnitName, paths: backupPaths},
	{unit: deploy.ImagesRefreshUnitName, root: true, paths: opsimages.RefreshPaths},
	{unit: deploy.NodeUnitName, root: true, paths: nodePaths},
}

// cmdLocalUnitPaths reports, or exercises, the host paths a shipped unit's
// command needs.
//
// THE GATE ASKS BILLET, NEVER A SECOND LIST. Each path is built by the function
// the command itself calls, so a command that starts writing somewhere new names
// it here by the same change. --exercise is a unit's stub ExecStart: run inside
// the unit's real sandbox, it performs each access the way the command does and
// fails on the first the sandbox refuses, after trying them all.
func cmdLocalUnitPaths(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet local unit-paths", env.Stdout)
	cfgPath := addServiceConfigFlag(fs)
	asJSON := fs.Bool("json", false, "print the paths as JSON")
	exercise := fs.Bool("exercise", false, "perform each access the way the unit's command does, "+
		"creating what the command would create, as the unit's own account; a unit's stub ExecStart runs this")
	marker := fs.String("marker", "", "with --exercise, leave a file of this name (beginning "+
		unitpath.MarkerPrefix+") in each writable directory, so a caller outside the unit's sandbox "+
		"can see where the write landed")
	listUnits := fs.Bool("units", false, "print the units billet declares, in the order "+
		"`billet local up` starts them, and nothing else")

	units := make([]string, 0, len(unitDeclarations))
	for _, d := range unitDeclarations {
		units = append(units, d.unit)
	}

	if err := cli.ParseWithArgs(fs, args, 1); err != nil {
		return err
	}

	// THE SANDBOXES ARE SYSTEMD'S: a launch agent on a Mac has none to prove.
	if cli.HostOS != "linux" {
		return fmt.Errorf("billet local unit-paths describes the systemd units billet ships, and "+
			"this host is %s", cli.HostOS)
	}

	if *listUnits {
		if fs.NArg() != 0 || *exercise || *asJSON || *marker != "" {
			return errors.New("--units takes no unit and no other flag")
		}

		for _, unit := range units {
			fmt.Fprintln(env.Stdout, unit)
		}

		return nil
	}

	if fs.NArg() != 1 {
		return fmt.Errorf("usage: billet local unit-paths [--config <path>] [--json] "+
			"[--exercise [--marker <name>]] <unit>, where <unit> is one of %s", strings.Join(units, ", "))
	}

	unit := fs.Arg(0)

	i := slices.IndexFunc(unitDeclarations, func(d unitDeclaration) bool { return d.unit == unit })
	if i < 0 {
		return fmt.Errorf("billet declares the paths of %s, and %q is not one of them",
			strings.Join(units, ", "), unit)
	}

	declared := unitDeclarations[i]

	if *marker != "" && (!*exercise || !unitpath.ValidMarker(*marker)) {
		return fmt.Errorf("--marker names one plain file beginning %s, and only with --exercise",
			unitpath.MarkerPrefix)
	}

	// AN EXERCISE RUNS AS THE UNIT'S OWN ACCOUNT, before anything is touched:
	// what it creates in a directory the service account owns would otherwise be
	// root's, and the unit's next start could not open it.
	if *exercise && !declared.root && geteuid() == 0 {
		return fmt.Errorf("%s runs its command as its service account, not root; exercise it as that "+
			"account (for example `sudo -u billet billet local unit-paths --exercise %s`), or what it "+
			"creates would be root's", unit, unit)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	needs := unitpath.Unique(declared.paths(env, cfg, *cfgPath))

	if *exercise {
		return exerciseUnitPaths(ctx, env, unit, needs, *marker)
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(struct {
			Unit  string          `json:"unit"`
			Paths []unitpath.Need `json:"paths"`
		}{unit, needs})
	}

	for _, n := range needs {
		if n.Unknown != "" {
			fmt.Fprintf(env.Stdout, "%-8s %s  (%s; could not tell: %s)\n", n.Access, n.Path, n.Why, n.Unknown)

			continue
		}

		fmt.Fprintf(env.Stdout, "%-8s %s  (%s)\n", n.Access, n.Path, n.Why)
	}

	return nil
}


// exerciseUnitPaths performs every need and refuses if the sandbox refused any.
func exerciseUnitPaths(ctx context.Context, env cli.Env, unit string, needs []unitpath.Need, marker string) error {
	err := unitpath.ExerciseAll(ctx, needs, marker, func(n unitpath.Need, err error) {
		if err != nil {
			fmt.Fprintf(env.Stdout, "REFUSED  %-8s %s: %v\n", n.Access, n.Path, err)

			return
		}

		fmt.Fprintf(env.Stdout, "ok       %-8s %s\n", n.Access, n.Path)
	})
	if err != nil {
		return fmt.Errorf("%s's command could not reach what it needs here: %w", unit, err)
	}

	return nil
}

// nodePaths is what `billet node` touches: its configuration and certificate
// bundle, its state directory (its identity and the record of how each process
// ended), the host-wide lock on its identity, the registration record it
// publishes, every directory certificate renewal installs into, and, on a
// firecracker node, the jailer's chroot base.
func nodePaths(_ cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := []unitpath.Need{{Path: cfgPath, Access: unitpath.Read, Why: "the configuration the node reads"}}

	if cfg.Node == nil {
		return needs
	}

	needs = append(needs, unitpath.Need{Path: cfg.Node.StateDir, Access: unitpath.Write, Create: true,
		Why: "the node's identity and the record of how each of its processes ended"})

	if tls := cfg.Node.TLS; tls != nil {
		for _, path := range []string{tls.CertPath, tls.KeyPath, tls.CAPath} {
			needs = append(needs, unitpath.Need{Path: path, Access: unitpath.Read,
				Why: "the node's certificate bundle"})
		}

		// RENEWAL REPLACES EACH FILE BESIDE ITSELF, so every directory the bundle
		// is in is written, however the configuration spreads it; one the state
		// directory already is collapses into it (unitpath.Unique).
		for _, path := range []string{tls.CertPath, tls.KeyPath, tls.CAPath} {
			needs = append(needs, unitpath.Need{Path: filepath.Dir(path), Access: unitpath.Write,
				Why: "where certificate renewal installs a new key, leaf or CA"})
		}
	}

	needs = append(needs,
		nodeLockNeed(cfg),
		unitpath.Need{Path: filepath.Dir(NodeRegistrationRecordPath("linux")), Access: unitpath.Write,
			Why: "where the node publishes its registration record"})

	if cfg.Node.Provider == config.ProviderFirecracker && cfg.Node.Firecracker != nil {
		needs = append(needs, unitpath.Need{Path: cfg.Node.Firecracker.ChrootBase, Access: unitpath.Write,
			Why: "the jailer's chroot base, where every guest runs"})
	}

	return unitpath.Unique(needs)
}

// nodeLockNeed is the host-wide lock on the node's identity, where
// state.LockDeployment puts it (the configured directory, or the per-user
// default). Reported from the identity the host already has; exercised through
// app.ClaimNodeDeployment, the node's own claim, with the bundle the node would
// load, so it resolves (and where the node would, mints or adopts) the identity
// exactly as the node does. A bundle the node could not load refuses, as it
// refuses the node, and a lock the claim could not place (which
// allow_unlocked_deployment turns into a degraded one) is a refusal too.
func nodeLockNeed(cfg *config.Config) unitpath.Need {
	need := unitpath.Need{Access: unitpath.Lock,
		Why: "the host-wide lock that keeps two billets with one identity off the same compute",
		Take: func(context.Context) (func() error, error) {
			bundle, err := app.NodeBundle(cfg)
			if err != nil {
				return nil, fmt.Errorf("the node would not load its certificate bundle: %w", err)
			}

			_, lock, err := app.ClaimNodeDeployment(cfg, bundle)
			if errors.Is(err, state.ErrDeploymentLocked) {
				return nil, nil
			}

			if err != nil {
				return nil, err
			}

			if why := lock.Degraded(); why != "" {
				return nil, errors.Join(fmt.Errorf("the lock could not be placed: %s", why), lock.Release())
			}

			return nil, lock.Release()
		}}

	deployment, known, err := app.PeekNodeDeploymentID(cfg)
	if err == nil && !known {
		deployment = "<deployment>"
	}

	path, pathErr := state.DeploymentLockPath(cfg.Node.LockDir, deployment)

	switch {
	case pathErr != nil:
		need.Path, need.Unknown = cfg.Node.LockDir, pathErr.Error()
	case err != nil:
		need.Path, need.Unknown = filepath.Dir(path), err.Error()
	case !known:
		need.Path = path
		need.Unknown = "this host has no deployment identity yet; the node's first start mints or adopts one"
	default:
		need.Path = path
	}

	return need
}

// serverPaths is what `billet server` touches, in the order it touches it: its
// configuration and App keys; the authority exclusion it takes before any
// identity access (hostauthority.ServerAccess: the global lock beside the
// directory and the directory's own), HELD while the rest is exercised, and
// admitting it, as the server holds it across its first open; then the
// identity directory (the ledger and the node-wire authority), and the
// directory's exclusive lock beside the deployment identity the server founds
// on its first open and a node on the same host claims. Refused, the exclusion
// leaves the identity directory untouched.
func serverPaths(env cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := configPaths(cfg, cfgPath)

	if cfg.Server == nil {
		return needs
	}

	dir := cfg.Server.IdentityDir

	return append(needs,
		unitpath.Need{Path: retirement.GlobalLockPath(), Access: unitpath.Lock, Admits: true,
			Why: "the authority exclusion the control plane takes before it touches its identity",
			Take: func(ctx context.Context) (func() error, error) {
				ctx, cancel := context.WithTimeout(ctx, unitPathLockWait)
				defer cancel()

				acc, err := hostauthority.ServerAccess(ctx, dir)
				if err != nil {
					return nil, err
				}

				return acc.Release, nil
			}},
		unitpath.Need{Path: dir, Access: unitpath.Write,
			Why: "the ledger and the node-wire authority"},
		unitpath.Need{Path: state.DirectoryLockPath(dir), Access: unitpath.Lock,
			Why: "the exclusive lock the control plane holds on its directory, beside the identity it founds",
			Take: func(context.Context) (func() error, error) {
				if _, err := state.DeploymentID(dir); err != nil {
					return nil, err
				}

				lock, err := state.LockStateDir(dir)
				if errors.Is(err, state.ErrLocked) {
					return nil, nil
				}

				if err != nil {
					return nil, err
				}

				return nil, lock.Release()
			}})
}

// configPaths is what the server and the backup both read: the configuration
// and the App keys.
func configPaths(cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := []unitpath.Need{{Path: cfgPath, Access: unitpath.Read, Why: "the configuration"}}

	for _, key := range app.KeyFilePaths(cfg) {
		needs = append(needs, unitpath.Need{Path: key, Access: unitpath.Read, Why: "a GitHub App private key"})
	}

	return needs
}

// backupPaths is what the packaged `billet local backup` run touches, in its
// order: the configuration and App keys, the authority exclusion it holds
// while it copies (hostauthority.Open: the global lock beside the directory and
// the directory's own), held and admitting the rest, then the identity
// directory it copies and the directory its ExecStart writes each archive
// into. Not the control plane's exclusive lock: a backup runs beside a live one.
func backupPaths(_ cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := configPaths(cfg, cfgPath)

	if cfg.Server != nil {
		dir := cfg.Server.IdentityDir

		needs = append(needs,
			unitpath.Need{Path: retirement.GlobalLockPath(), Access: unitpath.Lock, Admits: true,
				Why: "the authority exclusion a backup holds while it copies",
				Take: func(ctx context.Context) (func() error, error) {
					acc, err := hostauthority.Open(ctx, dir, hostauthority.Intent{Wait: unitPathLockWait})
					if err != nil {
						return nil, err
					}

					return acc.ReleaseWithoutHandBack, nil
				}},
			unitpath.Need{Path: dir, Access: unitpath.Write, Why: "the ledger and the node-wire authority it copies"})
	}

	return append(needs, unitpath.Need{Path: localBackupDir, Access: unitpath.Write,
		Why: "where the packaged unit writes each archive"})
}
