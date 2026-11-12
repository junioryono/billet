package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/junioryono/billet/internal/alloc"
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
	PoolHost(ctx context.Context, id string) (ec2.HostRecord, error)
	FindPoolHost(ctx context.Context, order string) (ec2.HostRecord, bool, error)
	TerminatePoolHost(ctx context.Context, id string) error
	PoolHostConsole(ctx context.Context, id string) (string, error)
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
//
// EVERY ORDER THE LEDGER RECORDS IS ACCOUNTED FOR BY ITS OWN HOST, never by the tag
// listing alone: DescribeInstances is eventually consistent, and a host it leaves out of
// one listing is not a host that is gone. An order whose host EC2 cannot name could still
// hold one, and is residue for a person.
//
// It answers every host it saw, for the sweep to ask about again by id: a host one
// listing showed and the next left out is still one to prove gone.
func (p acceptancePool) decommission(ctx context.Context, env cli.Env, orders []acceptancePoolOrder) (
	[]string, error,
) {
	live, err := p.hosts.PoolHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list this run's pool hosts: %w", err)
	}
	ids, err := p.orderHosts(ctx, orders, live)
	if err != nil {
		return ids, err
	}

	ids, err = p.awaitGone(ctx, env, ids)
	if err != nil {
		return ids, err
	}

	if p.cache == nil {
		fmt.Fprintf(env.Stdout, "cache    this run's pool hands its hosts no ebs-s3 store, so there is none to purge\n")

		return ids, nil
	}

	report, err := p.cache.Purge(ctx)
	if err != nil {
		return ids, fmt.Errorf("purge the site's cache: %w", err)
	}
	fmt.Fprintf(env.Stdout, "cache    purged %d snapshot(s), %d volume(s), %d state object(s), %d cache object(s)\n",
		report.Snapshots, report.Volumes, report.StateObjects, report.CacheObjects)

	return ids, nil
}

// orderHosts is every instance the run's pool may hold: each order's host as the ledger
// names it, or as EC2 finds it by the order's tag when the ledger names none, and every
// host the listing found. An order its source refused holds none. An order that names no
// host and whose host EC2 does not find could still hold one EC2 has not yet listed,
// which nothing here can rule out.
func (p acceptancePool) orderHosts(ctx context.Context, orders []acceptancePoolOrder,
	live []ec2.HostRecord,
) ([]string, error) {
	var ids []string
	add := func(id string) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for i := range live {
		add(live[i].ID)
	}
	var unknown []string
	for i := range orders {
		o := &orders[i]
		switch {
		case o.HostID != "":
			add(o.HostID)
		case o.State == alloc.PoolRefused:
		default:
			h, found, err := p.hosts.FindPoolHost(ctx, o.ID)
			switch {
			case err != nil:
				return nil, fmt.Errorf("find order %s's pool host: %w", o.ID, err)
			case found:
				add(h.ID)
			default:
				unknown = append(unknown, o.ID+" ("+o.State+")")
			}
		}
	}
	if len(unknown) > 0 {
		return ids, fmt.Errorf("%w: EC2 names no host for pool order(s) %s, which the ledger does not say were "+
			"refused, so nothing here proves they hold none; a person decides", errAcceptanceResidue,
			strings.Join(unknown, ", "))
	}

	return ids, nil
}

// awaitGone ends every host of the run's pool EC2 describes as live and waits, bounded,
// until it describes every one of ids as terminated and lists no other host of the pool
// that is not. A host is ended the first time EC2 describes it live, on whichever poll
// that is: one EC2 could not find at first may be found running at the next. A host a
// listing shows joins ids for good, so a later listing that leaves it out does not
// excuse it; awaitGone answers ids as they grew.
func (p acceptancePool) awaitGone(ctx context.Context, env cli.Env, ids []string) ([]string, error) {
	deadline := time.Now().Add(p.gone)
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()

	ended := map[string]bool{}
	end := func(h ec2.HostRecord) error {
		if ended[h.ID] || h.State == "shutting-down" {
			return nil
		}
		if err := p.hosts.TerminatePoolHost(ctx, h.ID); err != nil {
			return fmt.Errorf("end pool host %s (order %s): %w", h.ID, h.Order, err)
		}
		ended[h.ID] = true
		fmt.Fprintf(env.Stdout, "pool     ended host %s (order %s), which the pool had not released\n", h.ID, h.Order)

		return nil
	}

	for {
		left, err := p.notTerminated(ctx, &ids, end)
		if err != nil {
			return ids, err
		}
		if len(left) == 0 {
			fmt.Fprintf(env.Stdout, "pool     every host of this run's pool is terminated (%d)\n", len(ids))

			return ids, nil
		}
		if time.Now().After(deadline) {
			return ids, fmt.Errorf("%w: after %s, %d pool host(s) of this run are not described as terminated (%s)",
				errAcceptanceResidue, p.gone, len(left), strings.Join(left, ", "))
		}

		select {
		case <-ctx.Done():
			return ids, ctx.Err()
		case <-ticker.C:
		}
	}
}

// notTerminated is each of the run's pool hosts EC2 does not describe as terminated,
// with what it said instead: every host the listing returns, which joins ids, and each
// of ids EC2 describes in another state or cannot find. Each it describes as neither
// terminated nor shutting down is handed to end, when there is one.
func (p acceptancePool) notTerminated(ctx context.Context, ids *[]string, end func(ec2.HostRecord) error) (
	[]string, error,
) {
	live, err := p.hosts.PoolHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list this run's pool hosts: %w", err)
	}
	for _, h := range live {
		if !slices.Contains(*ids, h.ID) {
			*ids = append(*ids, h.ID)
		}
	}
	left := make([]string, 0, len(*ids))
	stillUp := func(h ec2.HostRecord) error {
		left = append(left, h.ID+" "+h.State)
		if end == nil {
			return nil
		}

		return end(h)
	}
	for _, h := range live {
		if err := stillUp(h); err != nil {
			return nil, err
		}
	}
	for _, id := range *ids {
		if slices.ContainsFunc(live, func(l ec2.HostRecord) bool { return l.ID == id }) {
			continue
		}
		h, err := p.hosts.PoolHost(ctx, id)
		switch {
		case errors.Is(err, ec2.ErrHostNotFound):
			// NOT FOUND IS NOT GONE: a terminated instance stays described for about an
			// hour, and one just launched may not be described yet.
			left = append(left, id+" not found")
		case err != nil:
			return nil, fmt.Errorf("describe pool host %s: %w", id, err)
		case h.State != "terminated":
			if err := stillUp(h); err != nil {
				return nil, err
			}
		}
	}

	return left, nil
}

// sweep is the last question: every host the run's pool may hold is described as
// terminated, no other is listed, and nothing is left in the site's cache under the
// run's owner.
//
// known are hosts the teardown already saw, asked about again by id.
func (p acceptancePool) sweep(ctx context.Context, env cli.Env, deployment string,
	orders []acceptancePoolOrder, known []string,
) error {
	live, err := p.hosts.PoolHosts(ctx)
	if err != nil {
		return fmt.Errorf("the sweep could not list this run's pool hosts, so nothing here establishes whether "+
			"it left one running: %w", err)
	}
	ids, err := p.orderHosts(ctx, orders, live)
	if err != nil {
		return err
	}
	for _, id := range known {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	left, err := p.notTerminated(ctx, &ids, nil)
	if err != nil {
		return fmt.Errorf("the sweep could not read this run's pool hosts, so nothing here establishes whether "+
			"it left one running: %w", err)
	}
	if len(left) > 0 {
		return fmt.Errorf("%w: %d pool host(s) of %s are not described as terminated (%s)", errAcceptanceResidue,
			len(left), deployment, strings.Join(left, ", "))
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

	fmt.Fprintf(env.Stdout, "sweep: every pool host of %s is terminated and nothing in the cache carrying it "+
		"remains\n", deployment)

	return nil
}

// preludeFlags are the init_args a pool prelude prepares a host by itself, with the
// values it prepares (the kernel it installs and the Ceph identity it creates), so it is
// handed the rest as PRELUDE_INIT_ARGS. A pool whose init_args name another value would
// join a node to a kernel or an identity its prelude never prepared.
var preludeFlags = map[string]string{
	"kernel-image": "/var/lib/billet/kernels/vmlinux",
	"ceph-user":    "billet",
	"ceph-keyring": "/etc/ceph/ceph.client.billet.keyring",
}

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
		if !ok {
			continue
		}
		if own, prepared := preludeFlags[flag]; prepared {
			if value != own {
				return fmt.Errorf("server.pool.init_args' --%s is %q, and the prelude prepares %q, which the "+
					"host's join would then not use", flag, value, own)
			}

			continue
		}
		// A PRELUDE TAKES THEM AS WORDS (PRELUDE_INIT_ARGS), so a value holding whitespace
		// would reach init as two arguments, and prepare another host than the join's.
		if strings.ContainsFunc(value, unicode.IsSpace) {
			return fmt.Errorf("server.pool.init_args' --%s holds whitespace, which a prelude's words cannot "+
				"carry", flag)
		}
		fmt.Fprintf(env.Stdout, "--%s=%s\n", flag, value)
	}

	return nil
}

// poolConsolesLog is the file in a pool's workspace its hosts' consoles are kept in, a
// name matrix-supervise.py's keep-logs moves out of the workspace with the services'.
const poolConsolesLog = "pool-consoles.log"

// cmdMatrixPoolConsoles writes what each host the run's pool ordered last wrote to its
// serial console into the workspace's pool-consoles.log (#861): the node's warnings
// and errors reach it, as the collection's prelude forwards the journal's there, and
// they would otherwise end with the host. A run with no pool has none and writes
// nothing. A console it could not read is said in the file and fails the command; the
// others are kept.
func cmdMatrixPoolConsoles(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix pool-consoles", env.Stdout)
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}
	pool, orders, isPool, err := acceptancePoolFor(ctx, ws)
	switch {
	case err != nil:
		return err
	case !isPool:
		fmt.Fprintln(env.Stdout, "this run has no pool, so no host's console to keep")

		return nil
	}

	path := filepath.Join(filepath.Dir(ws.ConfigPath), poolConsolesLog)
	kept, failed, err := pool.writeConsoles(ctx, path, orders)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "kept %d pool host console(s) in %s\n", kept, path)
	if failed > 0 {
		return fmt.Errorf("%d pool host console(s) could not be read; %s says which", failed, path)
	}

	return nil
}

// writeConsoles writes each ordered host's console to path, one section a host, and
// answers how many it kept and how many it could not read. An order that names no host
// is asked of EC2 by its tag, as the teardown asks, since a host it holds would be ended
// with no console kept.
//
// INSTALLED BY A RENAME OF A FILE IT CREATED, never written through path: an entry
// already there, a link into out/ or to a stream, or a FIFO, is replaced, never followed.
func (p acceptancePool) writeConsoles(ctx context.Context, path string, orders []acceptancePoolOrder) (
	int, int, error,
) {
	var b strings.Builder
	kept, failed := 0, 0
	for i := range orders {
		o := &orders[i]
		id := o.HostID
		if id == "" && o.State != alloc.PoolRefused {
			h, found, err := p.hosts.FindPoolHost(ctx, o.ID)
			switch {
			case err != nil:
				fmt.Fprintf(&b, "=== pool order %s (%s)\ncould not find its host: %v\n\n", o.ID, o.State, err)
				failed++

				continue
			case found:
				id = h.ID
			}
		}
		if id == "" {
			continue
		}
		fmt.Fprintf(&b, "=== pool host %s (order %s, %s)\n", id, o.ID, o.State)
		text, err := p.hosts.PoolHostConsole(ctx, id)
		if err != nil {
			fmt.Fprintf(&b, "could not read its console: %v\n\n", err)
			failed++

			continue
		}
		b.WriteString(strings.TrimRight(text, "\n") + "\n\n")
		kept++
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return kept, failed, fmt.Errorf("write the pool hosts' consoles: %w", err)
	}
	_, werr := tmp.WriteString(b.String())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())

		return kept, failed, fmt.Errorf("write the pool hosts' consoles: %w", werr)
	}

	return kept, failed, nil
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
	// EVERY VALUE QUOTED, as JSON, which YAML reads as written: an unquoted value YAML
	// reads otherwise (a prefix `cache #v2` is `cache`) would grant another namespace
	// than the hosts' own config names.
	q := func(v string) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err) // a string always marshals
		}

		return string(b)
	}
	kms := ""
	if store.KMSKeyID != "" {
		kms = "    kms_key_id: " + q(store.KMSKeyID) + "\n"
	}

	return []byte(`node:
  server_addr: 127.0.0.1:7717
  name: pool-host
  provider: firecracker
  state_dir: ` + q(filepath.Join(dir, "pool-host-node")) + `
  site: ` + q(site) + `
  ebs_s3:
    region: ` + q(store.Region) + `
    availability_zone: ` + q(store.AvailabilityZone) + `
    bucket: ` + q(store.Bucket) + `
    prefix: ` + q(store.Prefix) + `
` + kms + `  firecracker:
    kernel_image: /var/lib/billet/kernels/vmlinux
    bridge: billet0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
  cache:
    listen: ` + q(values["cache-listen"]) + `
    guest_endpoint: ` + q(values["cache-guest-endpoint"]) + `
`)
}
