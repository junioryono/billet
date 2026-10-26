package images

import (
	"context"
	"errors"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
	"github.com/junioryono/billet/internal/unitpath"
)

// unitPathLockWait bounds an exercise's wait for the kernel lock, which a pull
// waits thirty minutes for: an exercise asks only whether the sandbox lets it be
// taken, and a lock held by another billet answers that.
const unitPathLockWait = 5 * time.Second

// RefreshPaths is every host path `billet images refresh` touches through the
// pull it runs (#174), built by the functions the pull calls: the staging
// directory, the kernel directory and its lock, the jailer's chroot base the
// probe boots under, the probe's lock, and, for a node with no certificate to
// name its identity, the state directory that keeps the one the probe's lock is
// named for. A
// host that boots no firecracker guest needs the configuration alone, and a
// lock named for an identity the host has not minted is Unknown.
func RefreshPaths(env cli.Env, cfg *config.Config, cfgPath string) []unitpath.Need {
	needs := make([]unitpath.Need, 0, 7)
	needs = append(needs, unitpath.Need{Path: cfgPath, Access: unitpath.Read, Why: "the configuration the refresh reads"})

	if cfg.Node == nil || cfg.Node.Provider != config.ProviderFirecracker || cfg.Node.Firecracker == nil {
		return needs
	}

	kernels := nodeKernelDir(cfg)

	needs = append(needs,
		unitpath.Need{Path: DefaultStagingDir, Access: unitpath.Private,
			Why: "where a pull stages and decompresses the image's parts"},
		unitpath.Need{Path: kernels, Access: unitpath.Write, Create: true,
			Why: "where a pull installs the kernel paired with the image"},
		unitpath.Need{Path: kernelLockPath(kernels), Access: unitpath.Lock,
			Why: "the lock around every change to the kernel directory",
			Take: func(ctx context.Context) (func() error, error) {
				ctx, cancel := context.WithTimeout(ctx, unitPathLockWait)
				defer cancel()

				lock, err := takeKernelDirLock(ctx, env, kernels, "exercise this unit's paths")
				if errors.Is(err, errKernelLockBusy) {
					return unitpath.NothingHeld, nil
				}

				if err != nil {
					return nil, err
				}

				return nil, lock.release()
			}},
		unitpath.Need{Path: cfg.Node.Firecracker.ChrootBase, Access: unitpath.Write, Create: true, Mode: 0o700,
			Why: "the jailer's chroot base, where the verification boots its probe"})

	// WHERE THE VERIFICATION FINDS ITS IDENTITY, in verifyDeploymentID's order: a
	// certificate that loads is the answer, its deployment or the error that ends
	// the verification, and nothing is written; past one that does not load, or
	// none, the control plane's directory beside the node, or else the node's own
	// state directory, is where the identity is read or minted.
	switch {
	case certificateLoads(cfg):
	case cfg.Server != nil:
		needs = append(needs, unitpath.Need{Path: cfg.Server.IdentityDir, Access: unitpath.Write, Create: true, Mode: 0o700,
			Why: "where a node beside the control plane, with no certificate, reads or founds the deployment identity"})
	default:
		needs = append(needs, unitpath.Need{Path: cfg.Node.StateDir, Access: unitpath.Write, Create: true, Mode: 0o700,
			Why: "where a node with no certificate keeps its deployment identity"})
	}

	needs = append(needs, probeLockNeed(cfg))

	// THE HOST'S GENERATION-MAP LOCK, which a pull holds while it reads a golden
	// image's generation, as the node's cache store holds it for its own checks.
	// IN node.lock_dir, which it does not make: the node's unit and the probe lock
	// declared above both do.
	if lock := app.GenerationMapLockPath(cfg); lock != "" {
		needs = append(needs, unitpath.Need{Path: lock, Access: unitpath.Lock,
			Why: "the lock every read-only generation map on the host is held under, which a pull " +
				"takes to read a generation's architecture",
			Take: func(ctx context.Context) (func() error, error) {
				ctx, cancel := context.WithTimeout(ctx, unitPathLockWait)
				defer cancel()

				release, err := ceph.TakeGenerationMapLock(ctx, lock)
				if errors.Is(err, context.DeadlineExceeded) {
					return unitpath.NothingHeld, nil
				}

				if err != nil {
					return nil, err
				}

				release()

				return unitpath.NothingHeld, nil
			}})
	}

	return needs
}

// certificateLoads reports whether the node has a certificate bundle the
// verification loads, which answers its identity, or refuses it, without
// writing one: a bundle that does not load is passed over, not refused.
func certificateLoads(cfg *config.Config) bool {
	bundle, err := app.NodeBundle(cfg)

	return err == nil && bundle != nil
}

// probeLockNeed is the verification lock, named for the deployment the probe
// belongs to. Reported from the identity the host already has, never minted;
// exercised through verifyDeploymentID, so an exercise mints one where, and only
// where, the verification itself would.
func probeLockNeed(cfg *config.Config) unitpath.Need {
	need := unitpath.Need{Access: unitpath.Lock,
		Why: "the lock that keeps two verifications from being one microVM",
		Take: func(context.Context) (func() error, error) {
			deployment, err := verifyDeploymentID(cfg)
			if err != nil {
				return nil, err
			}

			lock, err := takeProbeLock(cfg.Node.LockDir, deployment)
			if errors.Is(err, errProbeLockContended) {
				return unitpath.NothingHeld, nil
			}

			if err != nil {
				return nil, err
			}

			return nil, lock.release()
		}}

	deployment, known, err := app.PeekNodeDeploymentID(cfg)

	switch {
	case err != nil:
		need.Path, need.Unknown = probeLockPath(cfg.Node.LockDir, "<deployment>"), err.Error()
	case !known:
		need.Path = probeLockPath(cfg.Node.LockDir, "<deployment>")
		need.Unknown = "this host has no deployment identity yet, and the first verification mints one"
	default:
		need.Path = probeLockPath(cfg.Node.LockDir, deployment)
	}

	return need
}
