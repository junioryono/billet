package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

// readCacheReap is how often the node's read cache is checked against its fill.
const readCacheReap = time.Minute

// generationMapLockName is the file in node.lock_dir whose flock keeps two
// processes on a host from unmapping each other's read-only generation maps.
const generationMapLockName = "ceph-generation-maps.lock"

// GenerationMapLock is the option that holds a Ceph client's read-only generation
// maps under this host's generation-map lock, or none when the node names no
// lock_dir: the node's cache store and a pull reading a golden image's generations
// take the same lock, so neither unmaps what the other is reading.
//
// THE NODE'S LOCK DIRECTORY, because only billet's account can write it: a lock
// file another account could create could be held to stop every generation check
// on the host.
func GenerationMapLock(cfg *config.Config) []ceph.Option {
	path := GenerationMapLockPath(cfg)
	if path == "" {
		return nil
	}

	return []ceph.Option{ceph.WithGenerationMapLock(path)}
}

// GenerationMapLockPath is the file GenerationMapLock holds a client's maps under,
// empty when the node names no lock_dir.
func GenerationMapLockPath(cfg *config.Config) string {
	if cfg.Node == nil || cfg.Node.LockDir == "" {
		return ""
	}

	return filepath.Join(cfg.Node.LockDir, generationMapLockName)
}

// NodeCacheControl is what the node's cache service asks the control plane:
// the kill switch, and what a job may publish. The node client is both.
type NodeCacheControl interface {
	node.ActionsPolicy
	node.CachePolicy
	node.CacheAuthorityReader
	node.CacheStoreReporter
}

// storeDeadline is how long a job waits for the site's store to hand over a volume,
// per store, past which it goes on without it. See docs/reference/records/cache-breaker.md
// for what each was chosen from.
const (
	cephStoreDeadline  = 60 * time.Second
	ebsS3StoreDeadline = 30 * time.Second
)

// guardNodeStore puts the store a node's provider uses behind a breaker. Only a store
// that stands between a job and its start has one to guard.
func guardNodeStore(cfg *config.Config, service *node.CacheService) {
	if name, deadline := nodeStoreGuard(cfg); name != "" {
		service.GuardStore(name, deadline)
	}
}

// nodeStoreGuard is the store a node's breaker names and how long a job waits for it,
// or no name for a node whose jobs wait on no store.
func nodeStoreGuard(cfg *config.Config) (string, time.Duration) {
	switch {
	case cfg.Node.Provider == config.ProviderFirecracker && cfg.Node.EBSS3 != nil:
		return string(config.SiteStoreEBSS3), ebsS3StoreDeadline
	case cfg.Node.Provider == config.ProviderFirecracker:
		return string(config.SiteStoreCeph), cephStoreDeadline
	case cfg.Node.Provider == config.ProviderEC2:
		return string(config.SiteStoreEBSS3), ebsS3StoreDeadline
	}

	return "", 0
}

// NodeSiteStore is the store a node's registration names, when its caches use one its
// provider has not always used: ebs-s3 for a Firecracker node on AWS metal, whose guests'
// root disks stay on its own Ceph (ADR-030). Empty for every other node, which then
// sends nothing a plane has not always read.
func NodeSiteStore(cfg *config.Config) config.SiteStoreKind {
	if cfg.Node != nil && cfg.Node.Provider == config.ProviderFirecracker && cfg.Node.EBSS3 != nil {
		return config.SiteStoreEBSS3
	}

	return ""
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

		// A NODE THAT CANNOT KEEP LOGS STILL RUNS JOBS: the store decides nothing, and an index
		// holds nothing a job needs.
		if err := startJobLogs(cfg, service); err != nil {
			slog.Warn("this node will keep no job logs", "error", err)
		}
	}
	guardNodeStore(cfg, service)

	return service, nil
}

// nodeCacheStore is the site store a node.cache block's provider reads and
// writes: the value the node's cache service is handed, so what it offers
// beyond the contract (the generation check among it) is proved on it.
//
// instance is how a node on AWS metal learns its own instance: ThisInstance, or a test's.
func nodeCacheStore(
	ctx context.Context, cfg *config.Config, deployment string,
	instance func(context.Context) (ebss3.Host, error),
) (storecontract.Store, error) {
	switch cfg.Node.Provider {
	case config.ProviderFirecracker:
		// ON AWS METAL, THE SITE'S STORE IS EBS AND S3 (ADR-030): the guests' root disks
		// stay on the host's own Ceph, and the caches go where a shrinking pool cannot
		// take them, on volumes this node attaches to its own instance.
		if cfg.Node.EBSS3 != nil {
			host, err := instance(ctx)
			if err != nil {
				return nil, fmt.Errorf("billet: a Firecracker node.ebs_s3 attaches its cache volumes to "+
					"its own instance, which it could not identify: %w", err)
			}

			return ebss3.NewAttached(*cfg.Node.EBSS3, CacheNamespace(deployment, cfg.Node.Site),
				awscreds.Default(), host, CacheSessionNames(cfg))
		}
		if cfg.Node.Ceph == nil {
			return nil, errors.New("billet: a Firecracker node.cache needs node.ceph")
		}

		// THE NODE IT MAPS FOR, recorded on every writable volume it maps: the
		// mapping table is the host's, so only that record proves a leftover
		// mapping this node's rather than another identity's on the same host.
		options := []ceph.Option{ceph.WithCacheSessions(CacheSessionNames(cfg)),
			ceph.WithMapOwner(deployment + "/" + cfg.Node.Name)}
		if path := GenerationMapLockPath(cfg); path != "" {
			options = append(options, ceph.WithGenerationMapLock(path))
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
	sweepWait func() time.Duration,
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
	storage, err := nodeCacheStore(ctx, cfg, deployment, ThisInstance)
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
	// reaches this listener in the clear on its own bridge, or through its own
	// relay on an EC2 guest, so it takes HTTP/2 with prior knowledge beside HTTP/1.
	// The read timeout applies per stream, and a cache transfer extends its own.
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	serveListener := ln
	if cfg.Node.Provider == config.ProviderEC2 {
		certificate, err := tls.LoadX509KeyPair(cfg.Node.Cache.TLSCert, cfg.Node.Cache.TLSKey)
		if err != nil {
			_ = ln.Close()

			return nil, nil, nil, fmt.Errorf("load the EC2 cache listener certificate: %w", err)
		}
		// NO TICKETS, because the relay resumes nothing, and a ticket is a record that
		// makes its socket readable with nothing for it to read.
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13,
			SessionTicketsDisabled: true,
		}
		// TLS IS THE TRANSPORT HERE AND NOTHING ABOVE IT KNOWS: what crosses it is what
		// crosses a Firecracker guest's bridge, HTTP/2 with prior knowledge included,
		// which http.Server takes only from a connection that is not a *tls.Conn.
		serveListener = opaqueTLSListener{tls.NewListener(ln, srv.TLSConfig)}
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

	// THE STORE'S BREAKER PROBES AND REPORTS UNTIL THE NODE STOPS, off every job's path.
	go service.RunStoreGuard(ctx, cachePolicy)

	// Eviction is intentionally best effort. A cache outage may slow a job, but it
	// must never change that job's result or stop the node from serving compute.
	//
	// ITS OWN GOROUTINE, because a pass asks rbd several questions about every old
	// volume and can run for many minutes, and the loop below finishes closed
	// sessions, whose publications are abandoned after publishWindow. It starts
	// after the first renewal, so a restarted node's live clones are renewed before
	// any eviction pass reads their records.
	//
	// ON A CONTEXT THE STOP CANCELS, and the stop waits for a kernel-mapping sweep
	// in flight (sweeping): an unmap the process exits under is killed with the
	// unit's cgroup, and one cut short leaves the image's lock with the host's one
	// kernel client.
	evictCtx, stopEviction := context.WithCancel(ctx)
	sweeping := make(chan struct{}, 1)

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
			evictPass(evictCtx, storage, service.ReapGitMirrors, sweeping, slog.Default())
		}

		// BEFORE THE FIRST PASS, which can run for a publication's whole window after a
		// restart, with the disk filling meanwhile.
		go reapReadCache(evictCtx, service)

		retryClosed()
		renewActive()

		go runEviction(evictCtx, evict, func() {
			sweepSuperseded(evictCtx, storage, slog.Default())
		})

		for {
			select {
			case <-ctx.Done():
				return
			case <-cleanupTicker.C:
				retryClosed()
				renewActive()
			case <-service.ClosedSessions():
				retryClosed()
				// A PASS CAN TAKE HOURS ON A STORE OF OBJECTS, and a chain of them chosen over
				// a tick that was also ready must not outlast what a lease has left.
				select {
				case <-cleanupTicker.C:
					renewActive()
				default:
				}
			}
		}
	}()

	slog.Default().Info("listening for guest cache requests; they are answered once this node "+
		"has registered and recovered", "provider", cfg.Node.Provider, "addr", ln.Addr().String())

	return service, serve, func() {
		// THE SWEEP IS STOPPED FIRST AND WAITED FOR BESIDE THE LISTENER, not after
		// it: on a handover both waits come out of what the leases can spare.
		stopEviction()
		wait := sweepWait()
		swept := make(chan bool, 1)
		go func() { swept <- awaitSweep(sweeping, wait) }()

		// A listener never served is closed here, since Shutdown closes only what
		// Serve was given; and once this has run, serve does nothing.
		serveOnce.Do(func() { _ = serveListener.Close() })
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Default().Warn("the guest cache listener did not shut down cleanly",
				"grace", stopGrace, "error", err)
		}

		if !<-swept {
			slog.Default().Warn("stopping while a kernel-mapping sweep is still unmapping a volume",
				"waited", wait)
		}
	}, nil
}

// opaqueTLSListener hands http.Server its connections with their TLS hidden, so the
// server treats what they carry as it treats a cleartext connection. The handshake
// still runs on the first read, under the server's own deadlines.
type opaqueTLSListener struct{ net.Listener }

func (l opaqueTLSListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return opaqueConn{conn}, nil
}

// opaqueConn is a net.Conn that is no longer a *tls.Conn to a type assertion.
type opaqueConn struct{ net.Conn }

// nodeCacheSweepWait is how long a stop waits for a kernel-mapping sweep in
// flight. Once its context is cancelled a sweep starts no unmap, so it ends
// within the one it has started, which MapTimeout bounds.
//
// ON A HANDOVER, NO LONGER THAN THE LISTENER'S GRACE, alongside which it runs:
// nothing renews the handed-over leases until the next process registers
// (nodeCacheStopGrace), and an unmap cut short there is the lesser harm than
// the running jobs quarantined. A drain has no running job left to lose.
func nodeCacheSweepWait(handOver bool) time.Duration {
	if handOver {
		return nodeCacheStopGrace(true)
	}

	return ceph.MapTimeout + 5*time.Second
}

// nodeCacheSweepWaitFor is the stop's wait, decided when the stop runs: a stop
// asked to drain overrides node.stop's handover, as the node loop's own
// DrainRequested does, and then has no lease to spare the wait for. Run hands
// both the same latched answer (latchedDrainRequest), so the two never disagree.
// A handover whose withdrawal did not land drains too, which the cache cannot
// see; it keeps the handover's wait.
func nodeCacheSweepWaitFor(handOver bool, drainRequested func() bool) func() time.Duration {
	return func() time.Duration {
		return nodeCacheSweepWait(handOver && (drainRequested == nil || !drainRequested()))
	}
}

// awaitSweep waits, at most bound, until no kernel-mapping sweep holds the
// slot, and reports whether none did by then.
func awaitSweep(sweeping chan struct{}, bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()

	select {
	case sweeping <- struct{}{}:
		<-sweeping

		return true
	case <-timer.C:
		return false
	}
}

// reapReadCache keeps the node's read cache below its fill. Far oftener than an
// eviction pass, because a build fills it in minutes, and a disk past the line writes
// refuse at is what the fill exists to keep a job from meeting. A pass under the fill
// reads one statfs.
func reapReadCache(ctx context.Context, service *node.CacheService) {
	ticker := time.NewTicker(readCacheReap)
	defer ticker.Stop()

	for {
		if err := service.ReapCASCache(ctx); err != nil && ctx.Err() == nil {
			slog.Default().Warn("could not make room in the node's read cache; will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// cacheEvictor is the part of a site store an eviction pass runs.
type cacheEvictor interface {
	Evict(ctx context.Context, olderThan time.Duration) error
}

// supersededEvictor is a store that can sweep the generations a key has moved past
// on their own, cheaply and often: the Ceph store.
type supersededEvictor interface {
	EvictSuperseded(ctx context.Context) error
}

// cacheEvictionEvery is how often the node runs a full eviction pass.
const cacheEvictionEvery = 6 * time.Hour

// supersededSweepEvery is how often the node sweeps superseded generations between
// full passes. A key published from every job published about thirteen an hour on
// the reference deployment on 2026-11-08, and the full pass reached them about
// once a day, while the pool grew by 1.2 TiB in twelve hours.
const supersededSweepEvery = 10 * time.Minute

// runEviction runs evict at once and every cacheEvictionEvery, and sweep every
// supersededSweepEvery, one at a time, until ctx ends. A sweep due while a pass runs
// waits for it, and ticks missed meanwhile collapse into one, as a ticker's do.
func runEviction(ctx context.Context, evict, sweep func()) {
	full := time.NewTicker(cacheEvictionEvery)
	defer full.Stop()

	quick := time.NewTicker(supersededSweepEvery)
	defer quick.Stop()

	evict()

	for {
		select {
		case <-ctx.Done():
			return
		case <-full.C:
		case <-quick.C:
			// A TICK THAT CAME DUE DURING A SWEEP IS READY BESIDE A STOP, and select
			// picks between ready cases at random, so a stop is asked first.
			if ctx.Err() == nil {
				sweep()
			}

			continue
		}

		if ctx.Err() == nil {
			evict()
		}
	}
}

// pressureEvictor is a store that can take generations ahead of their retention
// while its pool is short of room: the Ceph store.
type pressureEvictor interface {
	EvictUnderPressure(ctx context.Context) (ceph.PressureReport, error)
}

// sweepSuperseded runs the store's superseded sweep, when it has one, then its
// pressure sweep, when it has one, and logs a failure for the next sweep to retry.
//
// PRESSURE ON EVERY SWEEP, AFTER THE SUPERSEDED ONES GO, because retention alone
// let one CI build cache keyed per commit fill the reference deployment's pool past
// Ceph's nearfull (2026-11-12): what the pool can hold is the bound, read every
// ten minutes, and what nothing reads any more is taken first.
func sweepSuperseded(ctx context.Context, storage cacheEvictor, log *slog.Logger) {
	if sweeper, ok := storage.(supersededEvictor); ok {
		if err := sweeper.EvictSuperseded(ctx); err != nil && ctx.Err() == nil {
			log.Warn("could not sweep superseded cache generations; will retry", "error", err)
		}
	}

	pressed, ok := storage.(pressureEvictor)
	if !ok || ctx.Err() != nil {
		return
	}

	report, err := pressed.EvictUnderPressure(ctx)
	if report.Pressed() {
		log.Warn("the site's cache pool is past its fill threshold, so the least recently used cache "+
			"generations are evicted ahead of their retention", "fill", report.Fill,
			"threshold", report.Threshold, "evicted", report.Evicted)
	}
	if err != nil && ctx.Err() == nil {
		log.Warn("could not evict cache generations while the site's cache pool is short of room; "+
			"will retry", "error", err)
	}
}

// attachmentSweeper is a store that attaches volumes to this node's own instance and
// can take off the ones no cache session names: the ebs-s3 store on AWS metal.
type attachmentSweeper interface {
	SweepAttachments(ctx context.Context) ([]string, error)
}

// ThisInstance is the EC2 instance this node runs on, from its own identity document.
func ThisInstance(ctx context.Context) (ebss3.Host, error) {
	doc, _, err := awscreds.InstanceIdentity(ctx, "")
	if err != nil {
		return ebss3.Host{}, err
	}

	var identity struct {
		InstanceID       string `json:"instanceId"`
		AvailabilityZone string `json:"availabilityZone"`
	}
	if err := json.Unmarshal(doc, &identity); err != nil {
		return ebss3.Host{}, fmt.Errorf("read this instance's identity document: %w", err)
	}

	return ebss3.Host{InstanceID: identity.InstanceID, AvailabilityZone: identity.AvailabilityZone}, nil
}

// kernelMappingSweeper is a store that maps volumes into this host's kernel and
// can release the mappings nothing accounts for: the Ceph store.
type kernelMappingSweeper interface {
	SweepKernelMappings(ctx context.Context) (ceph.MappingReport, error)
}

// evictPass is one of the node's background storage passes: the kernel mappings
// no cache session accounts for, then expired cache generations and volumes,
// then unused git mirrors. Every failure is logged and left for the next pass.
// The sweep holds the one-token sweeping for its whole run, which a stop waits
// on (awaitSweep).
//
// THE MAPPINGS FIRST, because eviction keeps every volume this host maps: a
// leaked mapping released here lets the same pass reclaim its volume once the
// volume is old enough.
func evictPass(ctx context.Context, storage cacheEvictor, reapGitMirrors func(context.Context) error,
	sweeping chan struct{}, log *slog.Logger,
) {
	if sweeper, ok := storage.(kernelMappingSweeper); ok {
		sweeping <- struct{}{}
		report, err := sweeper.SweepKernelMappings(ctx)
		<-sweeping

		if unmapped := mappingNames(report, ceph.MappingUnmapped); len(unmapped) > 0 {
			log.Info("unmapped cache volumes the kernel still mapped and no cache session names",
				"volumes", unmapped)
		}
		if err != nil && ctx.Err() == nil {
			log.Warn("kept kernel mappings of cache volumes no cache session names, never forcing one; "+
				"will retry", "error", err)
		}
	}

	// THE ATTACHMENTS NEXT, for the same reason: eviction keeps every volume attached
	// to an instance, so one released here is the next pass's to reclaim.
	if sweeper, ok := storage.(attachmentSweeper); ok && ctx.Err() == nil {
		sweeping <- struct{}{}
		removed, err := sweeper.SweepAttachments(ctx)
		<-sweeping

		if len(removed) > 0 {
			log.Info("detached and deleted cache volumes attached to this instance that no cache "+
				"session names", "volumes", removed)
		}
		if err != nil && ctx.Err() == nil {
			log.Warn("kept cache volumes attached to this instance that no cache session names, "+
				"never detaching one that is held; will retry", "error", err)
		}
	}

	// NOTHING MORE ONCE A STOP HAS CANCELLED THE PASS: eviction's own generation-map
	// sweep unmaps outside the slot the stop waits on.
	if ctx.Err() != nil {
		return
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

// ForgetGitMirrors marks every mirror of owner/repo this node keeps so nothing
// more is served from it and the node removes it, and names those it marked.
func ForgetGitMirrors(cfg *config.Config, owner, repo string) ([]string, error) {
	return node.ForgetGitMirrors(cfg.Node.StateDir, owner, repo)
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
