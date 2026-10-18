package app

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// ONLY A LEDGER THAT IS SCANNED ASKS FOR TIME: PostgreSQL's open reads no whole
// file, so its start keeps the unit's own bound and a wedged one fails at it.
func TestOnlyAScannedLedgerAsksForTime(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		state *config.StateConfig
		want  bool
	}{
		{nil, true},
		{&config.StateConfig{Backend: config.StateSQLite}, true},
		{&config.StateConfig{Backend: config.StatePostgres}, false},
	} {
		cfg := &config.Config{Server: &config.ServerConfig{State: c.state}}
		if got := scansAtOpen(cfg); got != c.want {
			t.Errorf("a ledger of %+v scans at open = %v, want %v", c.state, got, c.want)
		}
	}
}

// THE SERVICE MANAGER IS ASKED FOR THE OPEN'S BOUND BEFORE THE LEDGER IS OPENED.
//
// The identity access is released as soon as the open returns, so an
// extension asked for between taking the access and releasing it was asked
// for before the open finished; asked for after, a cold scan would already
// have been killed. And a failure to ask is said, never a failed start.
func TestTheControlPlaneAsksForTimeBeforeItOpensTheLedger(t *testing.T) {
	t.Parallel()

	for _, refuse := range []bool{false, true} {
		var (
			mu     sync.Mutex
			events []string
			asked  time.Duration
			out    bytes.Buffer
			opened error
		)

		dir := ledgertest.Dir(t)

		note := func(event string) {
			mu.Lock()
			defer mu.Unlock()

			events = append(events, event)
		}

		access := func(context.Context, string) (func() error, error) {
			note("access")

			return func() error { note("release"); return nil }, nil
		}

		host := Host{
			ServerAccess: access, AuthorityLock: noIdentityAccess,
			Ready: func() error { return nil }, Status: func(string) error { return nil },
			ExtendStart: func(d time.Duration) error {
				note("extend")
				asked = d

				// NOT YET OPENED: the control plane's open takes the state
				// directory's lock, so an open here succeeds only before it.
				db, err := state.Open(t.Context(), dir)
				if err == nil {
					err = db.Close()
				}
				opened = err

				if refuse {
					return errors.New("no notification socket")
				}

				return nil
			},
			Out: &out,
		}

		cfg := &config.Config{Server: &config.ServerConfig{
			Listen: "127.0.0.1:0", IdentityDir: dir, MaxVCPU: 8, MaxMemory: 16 * config.GiB,
		}}

		cp, err := OpenControlPlane(t.Context(), cfg, host, nil, ControlPlaneOptions{})
		if err != nil {
			t.Fatalf("OpenControlPlane (refuse=%v): %v", refuse, err)
		}

		_ = cp.Close()

		mu.Lock()
		got := slices.Clone(events)
		mu.Unlock()

		if !slices.Equal(got[:min(len(got), 3)], []string{"access", "extend", "release"}) {
			t.Errorf("refuse=%v: events %v; want the extension between taking the identity access and "+
				"releasing it after the open", refuse, got)
		}

		if opened != nil {
			t.Errorf("refuse=%v: the ledger was already open when the extension was asked for (%v); "+
				"asked after the open, a cold scan has already been killed", refuse, opened)
		}

		if asked != state.StartupBound() {
			t.Errorf("refuse=%v: asked for %s, want the open's bound %s", refuse, asked, state.StartupBound())
		}

		if refuse && !strings.Contains(out.String(), "could not ask the service manager for time") {
			t.Errorf("a refused extension was not said: %q", out.String())
		}
	}
}
