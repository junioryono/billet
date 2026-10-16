package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/ops/fleetops"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

const (
	stopTestTier  = "billet-2vcpu-ubuntu-2404"
	stopTestGroup = "billet"
	stopTestSetID = 7
)

// stopTestConfig is a control plane over a seeded SQLite ledger, a loopback
// wire and one tier, with automatic updates off so nothing asks the public
// release channel.
func stopTestConfig(t *testing.T) *config.Config {
	t.Helper()

	automatic := false

	return &config.Config{
		Server: &config.ServerConfig{
			IdentityDir: ledgertest.Dir(t),
			Listen:      "127.0.0.1:0",
			MaxVCPU:     8,
			MaxMemory:   16 * config.GiB,
		},
		Release: &config.ReleaseConfig{Automatic: &automatic},
		Tiers: []config.Tier{{
			Label:       stopTestTier,
			Provider:    config.ProviderFirecracker,
			GuestOS:     config.GuestLinux,
			VCPU:        2,
			Memory:      4 * config.GiB,
			Image:       "ubuntu-2404-x64",
			RunnerGroup: stopTestGroup,
		}},
	}
}

// stopTestGitHub is the scale-set API as far as a tier's reconciliation: its
// runner group, and a scale set that does not exist until it is created.
func stopTestGitHub(t *testing.T) []app.Target {
	t.Helper()

	var created atomic.Bool

	set := fakeactions.ScaleSetJSON(stopTestSetID, stopTestTier, stopTestGroup, stopTestTier)

	gh := fakeactions.New(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			fakeactions.WriteJSON(t, w, fakeactions.ListJSON(
				map[string]any{"id": 1, "name": stopTestGroup, "isDefaultGroup": false}))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			if !created.Load() {
				fakeactions.WriteJSON(t, w, fakeactions.ListJSON())

				return
			}

			fakeactions.WriteJSON(t, w, fakeactions.ListJSON(set))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "runnerscalesets"):
			created.Store(true)
			fakeactions.WriteJSON(t, w, set)
		default:
			// Nothing in these tests reaches a session: each ends in startup.
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := scaleset.New(scaleset.Config{
		Target:         billetgithub.OrganizationTarget("acme"),
		GitHubURL:      gh.URL,
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     gh.PrivateKeyPEM(),
		AppID:          12345,
		APIURL:         gh.URL + "/api/v3",
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	return []app.Target{{Config: config.GitHubTarget{Name: config.DefaultTargetName, Org: "acme"}, Client: client}}
}

// stopHooks are the moments of a control plane's startup a test can act in,
// each given the run's own stop and the identity directory. A nil hook does
// what the CLI's host does.
type stopHooks struct {
	// access is the n'th taking of the server's identity access (1 is the
	// open's, 2 the node wire's); it returns what the access returns.
	access func(n int, ctx context.Context, dir string) (func() error, error)
	// ready is the service manager being told this process is serving.
	ready func() error
}

// stopTestHost is the CLI's host with the hooks in it: the real identity
// accesses and authority lock on the test's directory, and a service manager that records nothing.
func stopTestHost(hooks stopHooks) app.Host {
	var taken atomic.Int32

	access := func(ctx context.Context, dir string) (func() error, error) {
		n := int(taken.Add(1))
		if hooks.access != nil {
			return hooks.access(n, ctx, dir)
		}

		return hostauthority.ServerWireAccess(ctx, dir)
	}

	ready := func() error { return nil }
	if hooks.ready != nil {
		ready = hooks.ready
	}

	return app.Host{
		ServerAccess:  access,
		AuthorityLock: fleetops.AuthorityLockAccess,
		Ready:         ready,
		Status:        func(string) error { return nil },
		Out:           io.Discard,
	}
}

// accessHeldElsewhere takes the server's identity access as the process is
// stopped while another billet holds it, which is how a stop lands in that
// wait: the wait ends on the stop, and what the real access returns is what
// the step returns.
func accessHeldElsewhere(t *testing.T, stop context.CancelFunc, ctx context.Context, dir string) (func() error, error) {
	t.Helper()

	release, err := hostauthority.ServerWireAccess(t.Context(), dir)
	if err != nil {
		t.Fatalf("hold the identity access: %v", err)
	}

	defer func() {
		if err := release(); err != nil {
			t.Errorf("release the held identity access: %v", err)
		}
	}()

	stop()

	return hostauthority.ServerWireAccess(ctx, dir)
}

// stopRun is how one run of a control plane ended: what the process would exit
// with and print, and what runControlPlane itself returned, which says where
// the run stopped.
type stopRun struct {
	exit   error
	stdout string
	ran    error
	fenced bool
}

// stopTestRun runs a control plane the way runServer does, with the hooks the
// test gives it.
func stopTestRun(t *testing.T, ctx context.Context, cfg *config.Config, hooks stopHooks) stopRun {
	t.Helper()

	var out bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: io.Discard, Getenv: func(string) string { return "" }}

	targets := stopTestGitHub(t)

	fenced, runErr := runControlPlane(ctx, env, cli.NewLifecycle(func() {}, io.Discard), cfg, controlPlaneRun{
		host:    stopTestHost(hooks),
		targets: func(context.Context, *config.Config) ([]app.Target, error) { return targets, nil },
	})

	return stopRun{exit: serverEnded(ctx, env, fenced, runErr), stdout: out.String(), ran: runErr, fenced: fenced}
}

// A CONTROL PLANE STOPPED AT ANY POINT IN ITS STARTUP EXITS 0 (#137). Before
// this, only a stop inside the claim did; one landing in the open, the node
// wire's authority read or the scheduler's own startup came back as the error
// that step returned, the process exited 1, and systemd held a failed unit that
// `billet server retire` refuses to act against. Each case lands the stop in its
// step through the real code: a wait on the identity access another billet
// holds, a claim on a stopped context, and the scheduler's startup meeting the
// stop as the service manager is told the plane is serving.
func TestAControlPlaneStoppedDuringStartupExitsCleanly(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		// landed is what the step's own error says, so a stop that landed in a
		// later step instead does not pass for this one; empty for a step that
		// answers its own stop and returns nothing.
		landed string
		// accesses is how many times the run took the identity access: one for
		// the open, two once the node wire took it too.
		accesses int32
		hooks    func(t *testing.T, stop context.CancelFunc) stopHooks
	}{
		{
			name:     "the ledger open, waiting on the identity access",
			landed:   "the wait for it ended",
			accesses: 1,
			hooks: func(t *testing.T, stop context.CancelFunc) stopHooks {
				t.Helper()

				return stopHooks{access: func(n int, ctx context.Context, dir string) (func() error, error) {
					if n == 1 {
						return accessHeldElsewhere(t, stop, ctx, dir)
					}

					return hostauthority.ServerWireAccess(ctx, dir)
				}}
			},
		},
		{
			// The open's access ends with the open, so a stop as it is released
			// is the first thing the claim meets.
			name:     "the controller claim",
			landed:   "controller claim",
			accesses: 1,
			hooks: func(t *testing.T, stop context.CancelFunc) stopHooks {
				t.Helper()

				return stopHooks{access: func(n int, ctx context.Context, dir string) (func() error, error) {
					release, err := hostauthority.ServerWireAccess(ctx, dir)
					if err != nil || n != 1 {
						return release, err
					}

					return func() error {
						defer stop()

						return release()
					}, nil
				}}
			},
		},
		{
			name:     "the node wire's authority read, waiting on the identity access",
			landed:   "the wait for it ended",
			accesses: 2,
			hooks: func(t *testing.T, stop context.CancelFunc) stopHooks {
				t.Helper()

				return stopHooks{access: func(n int, ctx context.Context, dir string) (func() error, error) {
					if n == 2 {
						return accessHeldElsewhere(t, stop, ctx, dir)
					}

					return hostauthority.ServerWireAccess(ctx, dir)
				}}
			},
		},
		{
			// READY=1 is sent as the scheduler is assembled and before it polls, so
			// a stop there is met by the scheduler's own startup, which returns
			// before any listener exists and answers its own stop with nil: there
			// is nothing for the run to report, and internal/server's tests are
			// where that step is named.
			name:     "the scheduler's startup",
			accesses: 2,
			hooks: func(t *testing.T, stop context.CancelFunc) stopHooks {
				t.Helper()

				return stopHooks{ready: func() error {
					stop()

					return nil
				}}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			ctx, stop := context.WithCancel(t.Context())
			defer stop()

			hooks := c.hooks(t, stop)

			var accesses atomic.Int32

			counted := hooks.access
			hooks.access = func(n int, ctx context.Context, dir string) (func() error, error) {
				accesses.Add(1)

				if counted != nil {
					return counted(n, ctx, dir)
				}

				return hostauthority.ServerWireAccess(ctx, dir)
			}

			run := stopTestRun(t, ctx, stopTestConfig(t), hooks)
			if run.exit != nil {
				t.Fatalf("a control plane stopped during %s exits non-zero with %v, so systemd "+
					"holds a failed unit and `billet server retire` refuses to act against it", c.name, run.exit)
			}

			if !strings.Contains(run.stdout, "billet server: stopped") {
				t.Errorf("a control plane stopped during %s said %q, not that it stopped", c.name, run.stdout)
			}

			landed := run.ran == nil
			if c.landed != "" {
				landed = run.ran != nil && strings.Contains(run.ran.Error(), c.landed)
			}

			if !landed || accesses.Load() != c.accesses {
				t.Errorf("the stop was meant to land in %s and the run ended with %v after %d "+
					"identity accesses", c.name, run.ran, accesses.Load())
			}
		})
	}
}

// AND ONLY A STOP EXITS 0. A step that fails while the process is being
// stopped still failed: telling the service manager the plane is serving is the
// one step that takes no context, so nothing a stop does can be its error.
func TestAFailureDuringAStoppedStartupIsStillAFailure(t *testing.T) {
	t.Parallel()

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	notify := errors.New("sd_notify: the service manager's socket refused the message")

	run := stopTestRun(t, ctx, stopTestConfig(t), stopHooks{ready: func() error {
		stop()

		return notify
	}})
	if !errors.Is(run.exit, notify) {
		t.Fatalf("a readiness failure during a stop exits with %v, want the failure", run.exit)
	}

	if strings.Contains(run.stdout, "stopped") {
		t.Errorf("a failed start reported a clean stop: %q", run.stdout)
	}
}

// A HOST A SUCCESSOR FENCED EXITS NON-ZERO, even with nobody asking it to stop:
// a clean exit would leave a healed deployment with no controller at all. The
// successor's claim is staged as the service manager is told this one is
// serving, so the scheduler's first write, recording the tier's scale set, is
// the refused one.
func TestAControlPlaneFencedDuringStartupExitsNonZero(t *testing.T) {
	t.Parallel()

	cfg := stopTestConfig(t)

	// A FAILURE GUARD, never the condition: a run whose fence never ended it
	// would keep reopening its session, and this ends it as a test failure
	// rather than at the binary's timeout. It ends the run as a stop, which
	// the assertions below refuse.
	guard, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var staged sync.Once

	run := stopTestRun(t, guard, cfg, stopHooks{ready: func() error {
		staged.Do(func() {
			operator, err := state.OpenAdmin(t.Context(), cfg.Server.IdentityDir)
			if err != nil {
				t.Errorf("open an operator handle: %v", err)
				cancel()

				return
			}

			defer func() { _ = operator.Close() }()

			ledgertest.StageSuccessor(t, operator, "successor")
		})

		return nil
	}})
	if !errors.Is(run.exit, state.ErrLeadershipLost) {
		t.Fatalf("a control plane fenced during startup exits with %v, want ErrLeadershipLost", run.exit)
	}

	// AND THE RUN REPORTED IT, read from the ledger after every loop had stopped.
	if !run.fenced {
		t.Error("the run did not report the fence its ledger latched")
	}

	// THE SCHEDULER REPORTED THE FENCE, so the latch reached Scheduler.Run and the
	// answer did not come only from serverEnded's own reading of it.
	if !errors.Is(run.ran, state.ErrLeadershipLost) {
		t.Errorf("the scheduler did not report the fence: %v", run.ran)
	}

	if strings.Contains(run.stdout, "stopped") {
		t.Errorf("a fenced control plane reported a clean stop: %q", run.stdout)
	}
}

// A PROBE THAT CANNOT SAY IT IS READY SAYS WHAT ITS OPEN MIGRATED. A probe
// opens the candidate's ledger the way a control plane does, and on a ledger
// nothing has migrated the open creates the schema; the probe's failure is its
// verdict, and it still has to say what it left behind.
func TestAProbeThatCannotReportReadinessSaysWhatItsOpenMigrated(t *testing.T) {
	t.Parallel()

	cfg := stopTestConfig(t)
	cfg.Server.IdentityDir = t.TempDir()

	socket := filepath.Join(t.TempDir(), "no-such-notify.sock")
	env := cli.Env{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(name string) string {
		if name == "NOTIFY_SOCKET" {
			return socket
		}

		return ""
	}}

	targets := stopTestGitHub(t)

	_, err := runControlPlane(t.Context(), env, cli.NewLifecycle(func() {}, io.Discard), cfg, controlPlaneRun{
		host:    stopTestHost(stopHooks{}),
		targets: func(context.Context, *config.Config) ([]app.Target, error) { return targets, nil },
		probe:   true,
	})
	if err == nil || !strings.Contains(err.Error(), "upgrade-probe readiness") {
		t.Fatalf("a probe that could not report readiness returned %v", err)
	}

	moved, ok := errors.AsType[*state.SchemaMoved](err)
	if !ok || moved.From != 0 || moved.To != state.LatestSchemaVersion() {
		t.Errorf("a probe that could not report readiness does not say what its open migrated: %v", err)
	}
}

// A FAILURE BETWEEN THE OPEN AND THE CLAIM SAYS WHAT THE OPEN MIGRATED. The
// metrics endpoint is bound between the two, and on a ledger nothing has
// migrated yet the open has just created the whole schema: a process that then
// cannot bind its port has left that behind, and the exit has to say so.
func TestAFailureBeforeTheClaimSaysWhatTheOpenMigrated(t *testing.T) {
	t.Parallel()

	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("take a port: %v", err)
	}

	t.Cleanup(func() { _ = taken.Close() })

	cfg := stopTestConfig(t)
	cfg.Server.IdentityDir = t.TempDir()
	cfg.Server.Metrics = &config.MetricsConfig{Listen: taken.Addr().String()}

	run := stopTestRun(t, t.Context(), cfg, stopHooks{})
	if run.exit == nil {
		t.Fatal("a control plane whose metrics port was taken started")
	}

	moved, ok := errors.AsType[*state.SchemaMoved](run.exit)
	if !ok || moved.From != 0 || moved.To != state.LatestSchemaVersion() {
		t.Errorf("a failure between the open and the claim does not say what the open migrated: %v", run.exit)
	}
}
