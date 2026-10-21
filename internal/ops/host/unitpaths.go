package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	opsimages "github.com/junioryono/billet/internal/ops/images"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/unitpath"
)

// unitPathLockWait bounds an exercise's wait for a lock another billet holds.
const unitPathLockWait = 5 * time.Second

// unitDeclarations are the shipped units whose sandbox is ProtectSystem=strict,
// each with the paths its command touches (#174). A unit added under a strict
// sandbox is added here, or unit-paths cannot be asked about it and the gate in
// scripts/test-unit-sandboxes.sh does not start it.
var unitDeclarations = map[string]func(cli.Env, *config.Config, string) []unitpath.Need{
	deploy.ImagesRefreshUnitName: opsimages.RefreshPaths,
	deploy.NodeUnitName:          nodePaths,
	deploy.ServerUnitName:        serverPaths,
	deploy.BackupUnitName:        backupPaths,
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
		"creating what the command would create; a unit's stub ExecStart runs this")
	marker := fs.String("marker", "", "with --exercise, leave a file of this name in each writable "+
		"directory, so a caller outside the unit's sandbox can see where the write landed")

	units := make([]string, 0, len(unitDeclarations))
	for unit := range unitDeclarations {
		units = append(units, unit)
	}

	sort.Strings(units)

	if err := cli.ParseWithArgs(fs, args, 1); err != nil {
		return err
	}

	// THE SANDBOXES ARE SYSTEMD'S: a launch agent on a Mac has none to prove.
	if cli.HostOS != "linux" {
		return fmt.Errorf("billet local unit-paths describes the systemd units billet ships, and "+
			"this host is %s", cli.HostOS)
	}

	if fs.NArg() != 1 {
		return fmt.Errorf("usage: billet local unit-paths [--config <path>] [--json] "+
			"[--exercise [--marker <name>]] <unit>, where <unit> is one of %s", strings.Join(units, ", "))
	}

	unit := fs.Arg(0)

	declare, ok := unitDeclarations[unit]
	if !ok {
		return fmt.Errorf("billet declares the paths of %s, and %q is not one of them",
			strings.Join(units, ", "), unit)
	}

	if *marker != "" && (!*exercise || *marker != filepath.Base(*marker) || strings.HasPrefix(*marker, ".")) {
		return errors.New("--marker names one plain file, and only with --exercise")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	needs := declare(env, cfg, *cfgPath)

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
	var refused []string

	for _, n := range needs {
		if err := unitpath.Exercise(ctx, n, marker); err != nil {
			fmt.Fprintf(env.Stdout, "REFUSED  %-8s %s: %v\n", n.Access, n.Path, err)
			refused = append(refused, n.Path)

			continue
		}

		fmt.Fprintf(env.Stdout, "ok       %-8s %s\n", n.Access, n.Path)
	}

	if len(refused) > 0 {
		return fmt.Errorf("%s's command could not reach %s here", unit, strings.Join(refused, ", "))
	}

	return nil
}

// nodePaths is what `billet node` touches: its configuration and certificate,
// its state directory (its identity and the record of how each process ended),
// the host-wide lock on its identity, the registration record it publishes, the
// directory certificate renewal installs into, and, on a firecracker node, the
// jailer's chroot base.
func nodePaths(_ cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := []unitpath.Need{{Path: cfgPath, Access: unitpath.Read, Why: "the configuration the node reads"}}

	if cfg.Node == nil {
		return needs
	}

	needs = append(needs, unitpath.Need{Path: cfg.Node.StateDir, Access: unitpath.Write,
		Why: "the node's identity and the record of how each of its processes ended"})

	if tls := cfg.Node.TLS; tls != nil {
		for _, path := range []string{tls.CertPath, tls.KeyPath, tls.CAPath} {
			needs = append(needs, unitpath.Need{Path: path, Access: unitpath.Read,
				Why: "the node's certificate bundle"})
		}

		needs = append(needs, unitpath.Need{Path: filepath.Dir(tls.CertPath), Access: unitpath.Write,
			Why: "where certificate renewal installs a new key, leaf and CA"})
	}

	needs = append(needs,
		nodeLockNeed(cfg),
		unitpath.Need{Path: filepath.Dir(NodeRegistrationRecordPath("linux")), Access: unitpath.Write,
			Why: "where the node publishes its registration record"})

	if cfg.Node.Provider == config.ProviderFirecracker && cfg.Node.Firecracker != nil {
		needs = append(needs, unitpath.Need{Path: cfg.Node.Firecracker.ChrootBase, Access: unitpath.Write,
			Why: "the jailer's chroot base, where every guest runs"})
	}

	return needs
}

// nodeLockNeed is the host-wide lock on the node's identity. Reported from the
// identity the host already has; exercised through app.ClaimNodeDeployment,
// which the node itself calls, so it resolves (and where the node would, mints)
// the identity exactly as the node does.
func nodeLockNeed(cfg *config.Config) unitpath.Need {
	need := unitpath.Need{Access: unitpath.Lock,
		Why: "the host-wide lock that keeps two billets with one identity off the same compute",
		Take: func(context.Context) error {
			bundle, err := app.NodeBundle(cfg)
			if err != nil {
				bundle = nil
			}

			_, lock, err := app.ClaimNodeDeployment(cfg, bundle)
			if errors.Is(err, state.ErrDeploymentLocked) {
				return nil
			}

			if err != nil {
				return err
			}

			return lock.Release()
		}}

	dir := cfg.Node.LockDir

	deployment, known, err := app.PeekNodeDeploymentID(cfg)

	switch {
	case err != nil:
		need.Path, need.Unknown = filepath.Join(dir, state.DeploymentLockName("<deployment>")), err.Error()
	case !known:
		need.Path = filepath.Join(dir, state.DeploymentLockName("<deployment>"))
		need.Unknown = "this host has no deployment identity yet; the node's first start mints or adopts one"
	default:
		need.Path = filepath.Join(dir, state.DeploymentLockName(deployment))
	}

	return need
}

// serverPaths is what `billet server` touches: its configuration, the App keys
// it signs with, and its identity directory, which holds the ledger, the
// node-wire authority, the deployment identity the server founds on its first
// open (and a node on the same host claims), and the directory's exclusive lock.
func serverPaths(env cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := identityPaths(env, cfg, cfgPath)

	if cfg.Server != nil {
		dir := cfg.Server.IdentityDir

		needs = append(needs, unitpath.Need{Path: state.DirectoryLockPath(dir), Access: unitpath.Lock,
			Why: "the exclusive lock the control plane holds on its directory, beside the identity it founds",
			Take: func(context.Context) error {
				if _, err := state.DeploymentID(dir); err != nil {
					return err
				}

				lock, err := state.LockStateDir(dir)
				if errors.Is(err, state.ErrLocked) {
					return nil
				}

				if err != nil {
					return err
				}

				return lock.Release()
			}})
	}

	return needs
}

// identityPaths is what the server and the backup both read and write: the
// configuration, the App keys, and the identity directory.
func identityPaths(_ cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := []unitpath.Need{{Path: cfgPath, Access: unitpath.Read, Why: "the configuration"}}

	for _, key := range app.KeyFilePaths(cfg) {
		needs = append(needs, unitpath.Need{Path: key, Access: unitpath.Read, Why: "a GitHub App private key"})
	}

	if cfg.Server != nil {
		needs = append(needs, unitpath.Need{Path: cfg.Server.IdentityDir, Access: unitpath.Write,
			Why: "the ledger and the node-wire authority"})
	}

	return needs
}

// backupPaths is what the packaged `billet local backup` run touches: the
// identity directory it copies, the directory its ExecStart writes each
// archive into, and the authority exclusion it holds while it copies
// (hostauthority.Open, which takes the global lock and the identity
// directory's own). Not the control plane's exclusive lock: a backup runs
// beside a live one.
func backupPaths(env cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := identityPaths(env, cfg, cfgPath)

	needs = append(needs, unitpath.Need{Path: localBackupDir, Access: unitpath.Write,
		Why: "where the packaged unit writes each archive"})

	if cfg.Server != nil {
		dir := cfg.Server.IdentityDir

		needs = append(needs, unitpath.Need{Path: dir, Access: unitpath.Lock,
			Why: "the authority exclusion a backup holds while it copies",
			Take: func(ctx context.Context) error {
				acc, err := hostauthority.Open(ctx, dir, hostauthority.Intent{Wait: unitPathLockWait})
				if err != nil {
					return err
				}

				return acc.ReleaseWithoutHandBack()
			}})
	}

	return needs
}
