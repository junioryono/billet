package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/ops/setup"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// The teardown of a pool's acceptance run (#829): a control plane with no node of its
// own, whose pool rents AWS metal hosts at about $4.46 an hour each. Its hosts are the
// pool's, released by drain on proof while the control plane runs; what a teardown finds
// still there afterwards is ended here, because a host nothing watches is a bill nothing
// stops (two hosts ran unattended for about seven hours on 2026-11-08). Then the site's
// cache, which the pool's init_args name, is purged, and the sweep asks EC2 again.

const (
	// acceptancePoolGoneWait bounds how long the teardown waits for EC2 to say a pool
	// host it ended is terminated. A metal host stayed shutting-down for most of the
	// 20 minutes before its source read it as gone (the bare-metal hosts record).
	acceptancePoolGoneWait = 40 * time.Minute
	// acceptancePoolGonePoll is how often it asks.
	acceptancePoolGonePoll = 30 * time.Second
)

// acceptancePoolHosts is what the teardown asks of EC2 about the run's pool hosts:
// ec2.Reader, or a test's.
type acceptancePoolHosts interface {
	PoolHosts(ctx context.Context) ([]ec2.HostRecord, error)
	TerminatePoolHost(ctx context.Context, id string) error
}

// acceptancePoolCache is the site's cache a pool's run used: ebss3.Store, or a test's.
type acceptancePoolCache interface {
	Purge(ctx context.Context) (ebss3.PurgeReport, error)
	Residue(ctx context.Context) (ebss3.Residue, error)
}

// acceptancePool is a pool's run's teardown, with what it asks of AWS.
type acceptancePool struct {
	hosts acceptancePoolHosts
	// cache is nil for a pool whose init_args name no store.
	cache acceptancePoolCache
	// gone and poll bound the wait for an ended host to be terminated.
	gone, poll time.Duration
}

// poolOnly says whether a config is a pool's acceptance run's: a control plane with a
// pool and no node of its own.
func poolOnly(cfg *config.Config) bool {
	return cfg.Node == nil && cfg.Server != nil && cfg.Server.Pool != nil
}

// newAcceptancePool is the teardown of a pool's run, asking AWS as this run's deployment.
func newAcceptancePool(cfg *config.Config, deployment string) (acceptancePool, error) {
	metal := cfg.Server.Pool.AWSMetal
	if metal == nil {
		return acceptancePool{}, errors.New("this run's pool orders from no AWS metal source, and a pool " +
			"acceptance run's teardown can sweep AWS metal alone")
	}

	reader, err := ec2.NewReader(deployment, metal.Region, "", ec2.WithCredentials(awscreds.Default()))
	if err != nil {
		return acceptancePool{}, fmt.Errorf("server.pool.aws_metal: %w", err)
	}

	p := acceptancePool{hosts: reader, gone: acceptancePoolGoneWait, poll: acceptancePoolGonePoll}

	site, store := cfg.Server.Pool.SiteStore()
	if store != nil {
		cache, err := ebss3.New(*store, app.CacheNamespace(deployment, site), awscreds.Default())
		if err != nil {
			return acceptancePool{}, fmt.Errorf("server.pool.init_args' ebs-s3 store: %w", err)
		}
		p.cache = cache
	}

	return p, nil
}

// decommission ends every host of the run's pool EC2 has not terminated, waits until
// it says each is, then purges the site's cache. The cache only after the hosts: a host
// still up may hold a session whose objects no volume lease names.
func (p acceptancePool) decommission(ctx context.Context, env cli.Env) error {
	live, err := p.hosts.PoolHosts(ctx)
	if err != nil {
		return fmt.Errorf("list this run's pool hosts: %w", err)
	}

	for _, h := range live {
		if h.State == "shutting-down" {
			continue
		}
		if err := p.hosts.TerminatePoolHost(ctx, h.ID); err != nil {
			return fmt.Errorf("end pool host %s (order %s): %w", h.ID, h.Order, err)
		}
		fmt.Fprintf(env.Stdout, "pool     ended host %s (order %s), which the pool had not released\n", h.ID, h.Order)
	}

	if err := p.awaitGone(ctx, env); err != nil {
		return err
	}

	if p.cache == nil {
		fmt.Fprintf(env.Stdout, "cache    this run's pool hands its hosts no ebs-s3 store, so there is none to purge\n")

		return nil
	}

	report, err := p.cache.Purge(ctx)
	if err != nil {
		return fmt.Errorf("purge the site's cache: %w", err)
	}
	fmt.Fprintf(env.Stdout, "cache    purged %d snapshot(s), %d volume(s), %d state object(s), %d cache object(s)\n",
		report.Snapshots, report.Volumes, report.StateObjects, report.CacheObjects)

	return nil
}

// awaitGone waits, bounded, until EC2 lists no host of the run's pool that is not
// terminated.
func (p acceptancePool) awaitGone(ctx context.Context, env cli.Env) error {
	deadline := time.Now().Add(p.gone)
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()

	for {
		live, err := p.hosts.PoolHosts(ctx)
		if err != nil {
			return fmt.Errorf("list this run's pool hosts: %w", err)
		}
		if len(live) == 0 {
			fmt.Fprintf(env.Stdout, "pool     no host of this run's pool is left\n")

			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: after %s, %d pool host(s) of this run are not terminated (%s)",
				errAcceptanceResidue, p.gone, len(live), describeHosts(live))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// sweep is the last question: no host of the run's pool EC2 has not terminated, and
// nothing in the site's cache under the run's owner.
func (p acceptancePool) sweep(ctx context.Context, env cli.Env, deployment string) error {
	live, err := p.hosts.PoolHosts(ctx)
	if err != nil {
		return fmt.Errorf("the sweep could not list this run's pool hosts, so nothing here establishes whether "+
			"it left one running: %w", err)
	}
	if len(live) > 0 {
		return fmt.Errorf("%w: %d pool host(s) of %s are not terminated (%s)", errAcceptanceResidue, len(live),
			deployment, describeHosts(live))
	}

	if p.cache != nil {
		residue, err := p.cache.Residue(ctx)
		if err != nil {
			return fmt.Errorf("the sweep could not read the cache, so nothing here establishes whether this "+
				"run left anything behind: %w", err)
		}
		if !residue.Empty() {
			return fmt.Errorf("%w: the ebs-s3 cache still holds %d snapshot(s), %d volume(s), %d state "+
				"object(s) and %d cache object(s) of %s", errAcceptanceResidue, residue.Snapshots,
				residue.Volumes, residue.StateObjects, residue.CacheObjects, deployment)
		}
	}

	fmt.Fprintf(env.Stdout, "sweep: no pool host and nothing in the cache carrying %s remains\n", deployment)

	return nil
}

func describeHosts(hosts []ec2.HostRecord) string {
	out := ""
	for i, h := range hosts {
		if i > 0 {
			out += ", "
		}
		out += h.ID + " " + h.State
	}

	return out
}

// preludeFlags are the init_args a pool prelude prepares a host by itself (the kernel it
// installs and the Ceph identity it creates), so it is handed the rest as
// PRELUDE_INIT_ARGS.
var preludeFlags = []string{"kernel-image", "ceph-user", "ceph-keyring"}

// cmdMatrixPoolInitArgs prints, one per line as --flag=value, the server.pool.init_args a
// pool's prelude hands the config it converges the host from, in init's own order: the
// same words the join hands init, so the bridges and the cache the role prepares are the
// ones the node's config names.
func cmdMatrixPoolInitArgs(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix pool-init-args", env.Stdout)
	base := fs.String("config", "", "a config with a server.pool")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*base)
	if err != nil {
		return err
	}
	if cfg.Server == nil || cfg.Server.Pool == nil {
		return fmt.Errorf("%s has no server.pool", *base)
	}
	values := cfg.Server.Pool.InitArgValues()
	for _, flag := range config.PoolInitFlags {
		value, ok := values[flag]
		if !ok || slices.Contains(preludeFlags, flag) {
			continue
		}
		fmt.Fprintf(env.Stdout, "--%s=%s\n", flag, value)
	}

	return nil
}

// cmdMatrixPoolHostIAM prints the policy a pool's hosts need for the run's deployment:
// what `billet init iam` prints for a Firecracker node on AWS metal with the site and the
// store the pool's init_args hand them (ADR-030), from a node config written for that
// beside the workspace's own.
func cmdMatrixPoolHostIAM(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix pool-host-iam", env.Stdout)
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		return err
	}
	if !poolOnly(cfg) {
		return errors.New("this workspace is not a pool's run")
	}
	site, store := cfg.Server.Pool.SiteStore()
	if store == nil {
		return errors.New("this run's pool hands its hosts no ebs-s3 store, so they call no AWS API")
	}

	dir := filepath.Dir(ws.ConfigPath)
	path := filepath.Join(dir, "pool-host-node.yaml")
	if err := os.WriteFile(path, poolHostNodeConfig(cfg, site, store, dir), 0o600); err != nil {
		return fmt.Errorf("write the pool host's node config: %w", err)
	}

	return setup.Init(ctx, env, []string{"iam", "--config", path, "--deployment", ws.DeploymentID})
}

// poolHostNodeConfig is the node config a pool host's join writes, as far as its role
// needs it: a Firecracker node at the site, with the store and the cache listener the
// pool's init_args name.
func poolHostNodeConfig(cfg *config.Config, site string, store *config.EBSS3Config, dir string) []byte {
	values := cfg.Server.Pool.InitArgValues()
	kms := ""
	if store.KMSKeyID != "" {
		kms = "    kms_key_id: " + store.KMSKeyID + "\n"
	}

	return []byte(`node:
  server_addr: 127.0.0.1:7717
  name: pool-host
  provider: firecracker
  state_dir: ` + filepath.Join(dir, "pool-host-node") + `
  site: ` + site + `
  ebs_s3:
    region: ` + store.Region + `
    availability_zone: ` + store.AvailabilityZone + `
    bucket: ` + store.Bucket + `
    prefix: ` + store.Prefix + `
` + kms + `  firecracker:
    kernel_image: /var/lib/billet/kernels/vmlinux
    bridge: billet0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
  cache:
    listen: ` + values["cache-listen"] + `
    guest_endpoint: ` + values["cache-guest-endpoint"] + `
`)
}
