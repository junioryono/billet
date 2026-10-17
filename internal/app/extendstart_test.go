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
		)

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

				if refuse {
					return errors.New("no notification socket")
				}

				return nil
			},
			Out: &out,
		}

		cfg := &config.Config{Server: &config.ServerConfig{
			Listen: "127.0.0.1:0", IdentityDir: ledgertest.Dir(t), MaxVCPU: 8, MaxMemory: 16 * config.GiB,
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

		if asked != state.OpenBound() {
			t.Errorf("refuse=%v: asked for %s, want the open's bound %s", refuse, asked, state.OpenBound())
		}

		if refuse && !strings.Contains(out.String(), "could not ask the service manager for time") {
			t.Errorf("a refused extension was not said: %q", out.String())
		}
	}
}
