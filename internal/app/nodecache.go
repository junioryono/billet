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

// nodeCacheControl is what the node's cache service asks the control plane:
// the kill switch, and what a job may publish. The node client is both.
type nodeCacheControl interface {
	node.ActionsPolicy
	node.CachePolicy
	node.CacheAuthorityReader
}

// startNodeCache exposes the site's clone store to its managed guests.
func startNodeCache(
	ctx context.Context,
	cfg *config.Config,
	p provider.Provider,
	deployment string,
	cachePolicy nodeCacheControl,
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
	var storage storecontract.Store
	switch cfg.Node.Provider {
	case config.ProviderFirecracker:
		if cfg.Node.Ceph == nil {
			return nil, nil, nil, errors.New("billet: a Firecracker node.cache needs node.ceph")
		}
		var err error
		storage, err = ceph.New(*cfg.Node.Ceph, ceph.WithCacheSessions(CacheSessionNames(cfg)))
		if err != nil {
			return nil, nil, nil, err
		}
	case config.ProviderEC2:
		if cfg.Node.EBSS3 == nil {
			return nil, nil, nil, errors.New("billet: an EC2 node.cache needs node.ebs_s3")
		}
		var err error
		storage, err = ebss3.New(*cfg.Node.EBSS3,
			CacheNamespace(deployment, cfg.Node.Site), awscreds.Default())
		if err != nil {
			return nil, nil, nil, err
		}
	default:
		return nil, nil, nil, fmt.Errorf("billet: provider %s has no cache store", cfg.Node.Provider)
	}

	service, err := node.NewCacheService(cfg.Node.Cache.GuestEndpoint,
		CacheNamespace(deployment, cfg.Node.Site), cfg.Node.StateDir, storage, attacher, slog.Default())
	if err != nil {
		return nil, nil, nil, err
	}
	service.SetActionsPolicy(cachePolicy)
	service.SetCachePolicy(cachePolicy)
	service.SetAuthorityReader(cachePolicy)

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
		evict := func() {
			if err := storage.Evict(ctx, cacheEvictionAge); err != nil && ctx.Err() == nil {
				slog.Default().Warn("could not evict expired cache generations; will retry",
					"error", err)
			}
			if err := service.ReapGitMirrors(ctx); err != nil && ctx.Err() == nil {
				slog.Default().Warn("could not reap unused git mirrors; will retry", "error", err)
			}
		}

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
