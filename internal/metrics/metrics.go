// Package metrics is billet's one Prometheus registry and the endpoint that
// serves it. It is the only package that imports the Prometheus client and
// net/http/pprof (depguard's metrics rule); every other package reports through
// what this one declares, so the ledger writers never import a metrics library.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/junioryono/billet/internal/version"
)

// Registry is one process's metrics: the Go runtime's and the process's own,
// billet_build_info, and whatever the role registers.
type Registry struct {
	reg *prometheus.Registry
}

// New returns the registry for one role ("server" or "node").
func New(role string) (*Registry, error) {
	reg := prometheus.NewRegistry()

	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "billet_build_info",
		Help: "Always 1, labelled with the running binary's version and revision and the role it runs as.",
	}, []string{"version", "revision", "role"})
	build.WithLabelValues(version.Version(), version.Revision(), role).Set(1)

	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		build,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
	}

	return &Registry{reg: reg}, nil
}

// Handler serves /metrics, and /debug/pprof/ only when pprof is true.
//
// PPROF IS A SEPARATE SWITCH because a heap or goroutine profile carries
// memory: whatever a buffer held when it was taken, a credential included.
// The configuration refuses it on any listener that is not loopback.
func (r *Registry) Handler(pprofOn bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))

	if pprofOn {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	return mux
}

// Server is the metrics endpoint, listening from Listen until Close.
type Server struct {
	srv  *http.Server
	ln   net.Listener
	done chan error
}

// readHeaderTimeout bounds a scraper that connects and sends nothing.
const readHeaderTimeout = 10 * time.Second

// Listen binds addr and serves h on it. The bind happens before Listen returns,
// so a port already taken stops the process at startup rather than leaving it
// running without the endpoint an operator configured.
func Listen(ctx context.Context, addr string, h http.Handler) (*Server, error) {
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: listen on %s: %w", addr, err)
	}

	s := &Server{
		srv:  &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout},
		ln:   ln,
		done: make(chan error, 1),
	}

	go func() { s.done <- s.srv.Serve(ln) }()

	return s, nil
}

// Addr is the address the endpoint is bound to.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops the endpoint, waiting up to the context for scrapes in flight,
// and returns once the serving goroutine has.
func (s *Server) Close(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, s.srv.Close())
	}

	if served := <-s.done; !errors.Is(served, http.ErrServerClosed) {
		err = errors.Join(err, served)
	}

	return err
}
