package wiring

import (
	"io"
	"log/slog"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/version"
)

// ConfigPath is where billet.yaml is. Its own type so a container cannot hand
// a constructor some other string in its place.
type ConfigPath string

// RunningRelease is the release this binary is, as the ledger's watermark
// compares it. Its own type so a test can inject one; production registers
// version.Version().
type RunningRelease string

// Console is where a role prints what an operator reads. cmd/billet is the
// only package that owns the process's streams, so it passes its stdout in and
// wiring writes through this rather than printing.
type Console interface{ io.Writer }

// Readiness is the service manager's notification channel: READY=1 and the
// STATUS line. cmd/billet passes its systemd notifier in; a test passes a fake
// that records what was said.
type Readiness interface {
	// Ready tells the service manager initialization completed. Sent BEFORE a
	// standby's wait, because the packaged unit is Type=notify with a start
	// timeout, and withholding it would have systemd kill a standby forever.
	Ready() error
	// Status replaces the one line `systemctl status` shows.
	Status(text string) error
}

// NoReadiness is a Readiness that tells nobody, for an interactive run or a
// test.
type NoReadiness struct{}

// Ready does nothing.
func (NoReadiness) Ready() error { return nil }

// Status does nothing.
func (NoReadiness) Status(string) error { return nil }

// Signals is the operator's second signal: the one that ends a drain's wait
// without destroying what is running. Closed by cmd/billet's lifecycle.
type Signals struct {
	Hurry <-chan struct{}
}

// Core is what every role's container starts from: where the config is, where
// output goes, who is told about readiness, and which release this is.
type Core struct {
	ConfigPath ConfigPath
	// Console defaults to io.Discard: an operator command prints from
	// cmd/billet, and only the two roles' runtimes print from wiring.
	Console Console
	// Readiness defaults to NoReadiness.
	Readiness Readiness
	Signals   Signals
	// Release defaults to version.Version().
	Release RunningRelease
}

// CoreModule registers the config, the logger, the release and the process's
// three edges (console, readiness, signals) as singletons.
func CoreModule(c Core) godi.ModuleOption {
	console := c.Console
	if console == nil {
		console = io.Discard
	}

	readiness := c.Readiness
	if readiness == nil {
		readiness = NoReadiness{}
	}

	release := c.Release
	if release == "" {
		release = RunningRelease(version.Version())
	}

	return godi.NewModule("core",
		godi.AddSingleton(func() ConfigPath { return c.ConfigPath }),
		godi.AddSingleton(func() Console { return console }),
		godi.AddSingleton(func() Readiness { return readiness }),
		godi.AddSingleton(func() Signals { return c.Signals }),
		godi.AddSingleton(func() RunningRelease { return release }),
		godi.AddSingleton(newConfig),
		godi.AddSingleton(newLogger),
	)
}

// newConfig loads and validates billet.yaml once for the whole container.
func newConfig(path ConfigPath) (*config.Config, error) {
	return config.Load(string(path))
}

// newLogger is the process logger. slog.Default, because cmd/billet configures
// the default handler and every package logs through whatever it is handed.
func newLogger() *slog.Logger {
	return slog.Default()
}
