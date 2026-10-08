package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
)

// metricsCloseWait bounds how long a stopping process waits for scrapes in
// flight.
const metricsCloseWait = 5 * time.Second

// Metrics is one role's running metrics endpoint. A nil *Metrics is the
// endpoint a configuration without a metrics block has, and closing it does
// nothing.
type Metrics struct {
	srv *metrics.Server
	// stop is srv.Close, a field so a test can see the context Close hands it.
	stop func(context.Context) error
}

// ServeMetrics starts role's metrics endpoint when the configuration has one,
// and returns nil, with nothing bound, when it has none: there is no default
// address. The bind happens before it returns, so a port already taken stops
// the process at startup.
//
// NOT FOR AN UPGRADE PROBE: the service the probe stands beside holds the port.
func ServeMetrics(ctx context.Context, role string, m *config.MetricsConfig) (*Metrics, error) {
	if m == nil {
		return nil, nil //nolint:nilnil // no block, no endpoint: the nil *Metrics closes to nothing
	}

	reg, err := metrics.New(role)
	if err != nil {
		return nil, err
	}

	srv, err := metrics.Listen(ctx, m.Listen, reg.Handler(m.Pprof))
	if err != nil {
		return nil, err
	}

	slog.Default().Info("serving metrics", "role", role, "address", srv.Addr().String(), "pprof", m.Pprof)

	return &Metrics{srv: srv, stop: srv.Close}, nil
}

// Close stops the endpoint, waiting briefly for scrapes in flight. It is
// called as the process stops, whose context is usually already done, so it
// waits on its own bound rather than on that.
func (m *Metrics) Close(ctx context.Context) {
	if m == nil {
		return
	}

	wait, cancel := context.WithTimeout(context.WithoutCancel(ctx), metricsCloseWait)
	defer cancel()

	if err := m.stop(wait); err != nil {
		slog.Default().Warn("could not stop the metrics endpoint cleanly", "error", err)
	}
}
