package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
	"github.com/junioryono/billet/internal/store/ceph"
	"github.com/junioryono/billet/internal/store/ebss3"
)

const cacheEvictionAge = 7 * 24 * time.Hour

const cacheConnectionLimit = 128

// generationMapLockName is the file in node.lock_dir whose flock keeps two
// processes on a host from unmapping each other's read-only generation maps.
const generationMapLockName = "ceph-generation-maps.lock"

// NodeCacheControl is what the node's cache service asks the control plane:
// the kill switch, and what a job may publish. The node client is both.
type NodeCacheControl interface {
	node.ActionsPolicy
	node.CachePolicy
	node.CacheAuthorityReader
}

// NewNodeCacheService is the guest cache service a node.cache block describes,
// over the site's store: what it asks the control plane, and whether it takes
// its guests' own reports (node.monitoring.guest). startNodeCache builds it so,
// and a harness builds it over a store of its own.
func NewNodeCacheService(
	cfg *config.Config,
	deployment string,
	storage storecontract.Store,
	attacher provider.VolumeAttacher,
	control NodeCacheControl,
) (*node.CacheService, error) {
	service, err := node.NewCacheService(cfg.Node.Cache.GuestEndpoint,
		CacheNamespace(deployment, cfg.Node.Site), cfg.Node.StateDir, storage, attacher, slog.Default())
	if err != nil {
		return nil, err
	}
	service.SetActionsPolicy(control)
	service.SetCachePolicy(control)
	service.SetAuthorityReader(control)
	if guestMonitoring(cfg) {
		service.MonitorGuests()
	}

	return service, nil
}

// nodeCacheStore is the site store a node.cache block's provider reads and
// writes: the value the node's cache service is handed, so what it offers
// beyond the contract (the generation check among it) is proved on it.
func nodeCacheStore(cfg *config.Config, deployment string) (storecontract.Store, error) {
	switch cfg.Node.Provider {
	case config.ProviderFirecracker:
		if cfg.Node.Ceph == nil {
			return nil, errors.New("billet: a Firecracker node.cache needs node.ceph")
		}

		options := []ceph.Option{ceph.WithCacheSessions(CacheSessionNames(cfg))}
		// THE NODE'S LOCK DIRECTORY, because only billet's account can write it:
		// a lock file another account could create could be held to stop every
		// generation check on the host.
		if cfg.Node.LockDir != "" {
			options = append(options,
				ceph.WithGenerationMapLock(filepath.Join(cfg.Node.LockDir, generationMapLockName)))
		}

		return ceph.New(*cfg.Node.Ceph, options...)
	case config.ProviderEC2:
		if cfg.Node.EBSS3 == nil {
			return nil, errors.New("billet: an EC2 node.cache needs node.ebs_s3")
		}

		return ebss3.New(*cfg.Node.EBSS3, CacheNamespace(deployment, cfg.Node.Site), awscreds.Default())
	default:
		return nil, fmt.Errorf("billet: provider %s has no cache store", cfg.Node.Provider)
	}
}

// startNodeCache exposes the site's clone store to its managed guests.
func startNodeCache(
	ctx context.Context,
	cfg *config.Config,
	p provider.Provider,
	deployment string,
	cachePolicy NodeCacheControl,
	stopGrace time.Duration,
) (*node.CacheService, func(), func(), error) {
	if cfg.Node.Cache == nil {
		return nil, func() {}, func() {}, nil
	}

	attacher, ok := p.(provider.VolumeAttacher)
	if !ok {
		return nil, nil, nil, fmt.Errorf("billet: provider %s cannot attach node.cache volumes",
			cfg.Node.Provider)
	}
	storage, err := nodeCacheStore(cfg, deployment)
	if err != nil {
		return nil, nil, nil, err
	}

	service, err := NewNodeCacheService(cfg, deployment, storage, attacher, cachePolicy)
	if err != nil {
		return nil, nil, nil, err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Node.Cache.Listen)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listen for guest cache requests on %s: %w",
			cfg.Node.Cache.Listen, err)
	}

	ln = LimitListener(ln, cacheConnectionLimit)
	srv := &http.Server{
		Handler:           service,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	// BAZEL'S AND BUCK2'S REMOTE CACHES SPEAK gRPC, which is HTTP/2, and a guest
	// reaches this listener in the clear on its own bridge, so it takes HTTP/2
	// with prior knowledge beside HTTP/1. The read timeout applies per stream,
	// and a cache transfer extends its own.
	if cfg.Node.Provider != config.ProviderEC2 {
		srv.Protocols = new(http.Protocols)
		srv.Protocols.SetHTTP1(true)
		srv.Protocols.SetUnencryptedHTTP2(true)
	}
	serveListener := ln
	if cfg.Node.Provider == config.ProviderEC2 {
		certificate, err := tls.LoadX509KeyPair(cfg.Node.Cache.TLSCert, cfg.Node.Cache.TLSKey)
		if err != nil {
			_ = ln.Close()

			return nil, nil, nil, fmt.Errorf("load the EC2 cache listener certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13,
		}
		serveListener = tls.NewListener(ln, srv.TLSConfig)
	}
	// BOUND NOW, SERVED ONCE THE NODE IS READY. A failure to bind is a startup
	// error as it always was, but nothing is answered until this process is
	// registered and recovered: a handed-over guest's request asked earlier would
	// be judged by a process the control plane does not know, and the kill
	// switch reads that as disabled. Connections arriving meanwhile wait in the
	// listener's queue.
	//
	// THE MOUNTS A PREVIOUS PROCESS MADE ARE NOT HERE, so they are restored first
	// (#374): the unit has a mount namespace of its own, and a recovered session's
	// paths are empty directories until its volumes are mounted again in this
	// one. In the background, each mount bounded, so storage that stalls delays
	// only the cache, never the registration and renewal of the compute; and not
	// on the node's context, which a stop cancels before a drain serves the
	// guests it waits for.
	var serveOnce sync.Once
	serve := func() {
		serveOnce.Do(func() {
			go func() {
				service.RestoreMounts(context.WithoutCancel(ctx))
				slog.Default().Info("serving guest cache requests", "addr", serveListener.Addr().String())
				if err := srv.Serve(serveListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Default().Error("the guest cache listener stopped; jobs will continue cold",
						"error", err)
				}
			}()
		})
	}

	// Eviction is intentionally best effort. A cache outage may slow a job, but it
	// must never change that job's result or stop the node from serving compute.
	//
	// ITS OWN GOROUTINE, because a pass asks rbd several questions about every old
	// volume and can run for many minutes, and the loop below finishes closed
	// sessions, whose publications are abandoned after publishWindow. It starts
	// after the first renewal, so a restarted node's live clones are renewed before
	// any eviction pass reads their records.
	go func() {
		cleanupTicker := time.NewTicker(5 * time.Minute)
		defer cleanupTicker.Stop()

		retryClosed := func() {
			if err := service.RetryClosed(ctx); err != nil && ctx.Err() == nil {
				slog.Default().Warn("could not finish closed cache sessions; will retry",
					"error", err)
			}
		}
		renewActive := func() {
			if err := service.RenewActive(ctx, time.Now().Add(7*time.Hour)); err != nil && ctx.Err() == nil {
				slog.Default().Warn("could not renew active cache generations; will retry",
					"error", err)
			}
		}
		evict := func() { evictPass(ctx, storage, service.ReapGitMirrors, slog.Default()) }

		retryClosed()
		renewActive()

		go func() {
			evictionTicker := time.NewTicker(6 * time.Hour)
			defer evictionTicker.Stop()

			evict()

			for {
				select {
				case <-ctx.Done():
					return
				case <-evictionTicker.C:
					evict()
				}
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case <-cleanupTicker.C:
				retryClosed()
				renewActive()
			case <-service.ClosedSessions():
				retryClosed()
			}
		}
	}()

	slog.Default().Info("listening for guest cache requests; they are answered once this node "+
		"has registered and recovered", "provider", cfg.Node.Provider, "addr", ln.Addr().String())

	return service, serve, func() {
		// A listener never served is closed here, since Shutdown closes only what
		// Serve was given; and once this has run, serve does nothing.
		serveOnce.Do(func() { _ = serveListener.Close() })
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Default().Warn("the guest cache listener did not shut down cleanly",
				"grace", stopGrace, "error", err)
		}
	}, nil
}

// cacheEvictor is the part of a site store an eviction pass runs.
type cacheEvictor interface {
	Evict(ctx context.Context, olderThan time.Duration) error
}

// kernelMappingSweeper is a store that maps volumes into this host's kernel and
// can release the mappings nothing accounts for: the Ceph store.
type kernelMappingSweeper interface {
	SweepKernelMappings(ctx context.Context) (ceph.MappingReport, error)
}

// evictPass is one of the node's background storage passes: the kernel mappings
// no cache session accounts for, then expired cache generations and volumes,
// then unused git mirrors. Every failure is logged and left for the next pass.
//
// THE MAPPINGS FIRST, because eviction keeps every volume this host maps: a
// leaked mapping released here lets the same pass reclaim its volume once the
// volume is old enough.
func evictPass(ctx context.Context, storage cacheEvictor, reapGitMirrors func(context.Context) error,
	log *slog.Logger,
) {
	if sweeper, ok := storage.(kernelMappingSweeper); ok {
		report, err := sweeper.SweepKernelMappings(ctx)
		if unmapped := mappingNames(report, ceph.MappingUnmapped); len(unmapped) > 0 {
			log.Info("unmapped cache volumes the kernel still mapped and no cache session names",
				"volumes", unmapped)
		}
		if err != nil && ctx.Err() == nil {
			log.Warn("kept kernel mappings of cache volumes no cache session names, never forcing one; "+
				"will retry", "error", err)
		}
	}

	if err := storage.Evict(ctx, cacheEvictionAge); err != nil && ctx.Err() == nil {
		log.Warn("could not evict expired cache generations; will retry", "error", err)
	}
	if err := reapGitMirrors(ctx); err != nil && ctx.Err() == nil {
		log.Warn("could not reap unused git mirrors; will retry", "error", err)
	}
}

// mappingNames are the images of the mappings a report puts in state.
func mappingNames(report ceph.MappingReport, state ceph.MappingState) []string {
	var names []string

	for _, m := range report.Mappings {
		if m.State == state {
			names = append(names, m.Name)
		}
	}

	return names
}

// nodeCacheStopGrace is how long the guest cache listener lets requests already
// in flight finish when the node stops; Shutdown stops accepting at once and
// returns as soon as they have.
//
// LONGER ON A HANDOVER (#374), because the guests are still running and a git
// fetch or a build-cache transfer cut off mid-body fails the job, while a new
// connection only waits: the guest's relay redials until the next process
// listens. A drain stops the listener after its jobs have finished, so nothing
// is left to wait for.
//
// AND NO LONGER THAN A SIXTH OF THE LEASE TTL. Nothing renews the handed-over
// leases from the withdrawal until the next process registers, and the last
// renewal may already be a third of the TTL old (the node renews every TTL/3), so
// this grace comes out of what the restart has left before the reaper
// quarantines running jobs.
func nodeCacheStopGrace(handOver bool) time.Duration {
	if handOver {
		return alloc.DefaultLeaseTTL / 6
	}

	return 5 * time.Second
}

func CacheNamespace(deployment, site string) string {
	if site == "" {
		site = "local"
	}

	return deployment + "/" + site
}

// CacheSessionRecords reads this node's cache custody records. A node with no
// cache listener keeps none, so only there is a missing directory an empty set.
func CacheSessionRecords(cfg *config.Config) (node.CacheSessionRecords, error) {
	records, err := node.ReadCacheSessionRecords(cfg.Node.StateDir)
	if err != nil {
		if cfg.Node.Cache == nil && errors.Is(err, fs.ErrNotExist) {
			return node.CacheSessionRecords{}, nil
		}

		return node.CacheSessionRecords{}, fmt.Errorf("could not tell which cache volumes this node's "+
			"sessions hold, so nothing is judged: %w", err)
	}

	return records, nil
}

// CacheSessionNames reads this node's cache custody records afresh each time
// eviction asks, so a volume a session names is never moved to the trash.
func CacheSessionNames(cfg *config.Config) func() (func(name string) bool, error) {
	return func() (func(name string) bool, error) {
		records, err := CacheSessionRecords(cfg)
		if err != nil {
			return nil, err
		}

		return records.Mentions, nil
	}
}
