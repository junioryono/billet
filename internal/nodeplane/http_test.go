package nodeplane_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/server"
)

const deployment = "0123456789abcdef0123456789abcdef"

// fakeStore is a ledger that answers whatever a test needs.
//
// The point of these tests is the WIRE, not the allocator: what has to be proved
// is that an error raised on one side arrives on the other as the same kind of
// error, and that requires being able to raise each kind on demand.
type fakeStore struct {
	mu sync.Mutex

	bindErr      error
	advanceErr   error
	heartbeatErr error
	releaseErr   error
	leaseErr     error

	lease    *alloc.Lease
	launched map[string]bool

	bound    []string
	advanced []alloc.Phase
	released []alloc.Phase
}

func (f *fakeStore) Bind(_ context.Context, leaseID string, _ int64, node string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.bound = append(f.bound, node+"/"+leaseID)

	return f.bindErr
}

func (f *fakeStore) Advance(_ context.Context, _ string, _ int64, to alloc.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.advanced = append(f.advanced, to)

	return f.advanceErr
}

func (f *fakeStore) Heartbeat(context.Context, string, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.heartbeatErr
}

func (f *fakeStore) Release(_ context.Context, _ string, _ int64, outcome alloc.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.released = append(f.released, outcome)

	return f.releaseErr
}

func (f *fakeStore) Lease(context.Context, string) (*alloc.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lease, f.leaseErr
}

func (f *fakeStore) LaunchedLeaseIDs(context.Context, string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.launched, nil
}

func serve(t *testing.T, store nodeplane.LeaseStore, opts ...nodeplane.Option) (*nodeplane.Plane, string) {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, opts...)
	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil))

	t.Cleanup(srv.Close)

	return p, srv.URL
}

func dial(tb testing.TB, base string) *nodeclient.Client {
	tb.Helper()

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		tb.Fatalf("new client: %v", err)
	}

	if err := c.Register(tb.Context(), config.ProviderDocker,
		[]config.GuestOS{config.GuestLinux}, deployment); err != nil {
		tb.Fatalf("register: %v", err)
	}

	return c
}

// A COMMAND MAKES THE ROUND TRIP AND ITS RESULT COMES BACK.
//
// The whole wire in one test: the server queues a launch, the node's poll picks
// it up over HTTP, the node reports, and the caller of Launch sees the verdict.
// Everything else here tests an edge of this.
func TestACommandAndItsResultCrossTheWire(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{}, nodeplane.WithCommandTimeout(5*time.Second))
	c := dial(t, base)

	reported := make(chan error, 1)

	go func() {
		cmd, ok, err := c.Poll(t.Context())
		if err != nil || !ok {
			reported <- err

			return
		}

		reported <- c.Report(t.Context(), nodeapi.CommandResult{ID: cmd.ID, OK: true})
	}()

	lease := &alloc.Lease{
		ID:        "l1",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}

	if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}); err != nil {
		t.Fatalf("Launch across the wire: %v", err)
	}

	// DRAINED, so the node's side is not still deciding when the test ends. An
	// unjoined goroutine calling t.Errorf after its test finishes panics the whole
	// run, which is exactly what this file did on its first green build.
	if err := <-reported; err != nil {
		t.Errorf("the node could not report: %v", err)
	}
}

// The launch command arrives with everything a launch needs.
func TestTheLaunchArrivesIntact(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{}, nodeplane.WithCommandTimeout(5*time.Second))
	c := dial(t, base)

	got := make(chan nodeapi.Command, 1)

	go func() {
		cmd, ok, err := c.Poll(t.Context())
		if err != nil || !ok {
			close(got)

			return
		}

		got <- cmd

		if err := c.Report(t.Context(), nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			return
		}
	}()

	lease := &alloc.Lease{
		ID:        "l1",
		Tier:      "billet-2vcpu",
		VCPU:      4,
		Memory:    16 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     9,
		RequestID: 7,
	}

	if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7, RunID: 8, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	select {
	case cmd := <-got:
		if cmd.Lease == nil {
			t.Fatal("the command arrived with no lease")
		}

		if cmd.Lease.VCPU != 4 || cmd.Lease.Memory != 16*config.GiB {
			t.Errorf("size did not survive: %d vcpu / %v", cmd.Lease.VCPU, cmd.Lease.Memory)
		}

		if cmd.Lease.Epoch != 9 {
			t.Errorf("epoch did not survive (%d); the node fences its writes with it", cmd.Lease.Epoch)
		}

		if cmd.Job == nil || cmd.Job.Event != "push" {
			t.Errorf("the job's event did not survive: %+v", cmd.Job)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the node never saw the command")
	}
}

// AN IDLE POLL IS A 204 AND NOT AN ERROR.
func TestAnIdlePollIsQuiet(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{})
	p.SetPollWindowForTest(80 * time.Millisecond)

	c := dial(t, base)

	_, ok, err := c.Poll(t.Context())
	if err != nil {
		t.Fatalf("an idle poll failed: %v", err)
	}

	if ok {
		t.Error("an idle poll produced a command")
	}
}

// A FENCED LEASE ARRIVES AS alloc.ErrFenced, not as prose.
//
// This is the one the runner branches on: fenced means stop, something else owns
// this. It has to survive the wire as the same error value the in-process path
// produces, or every caller needs a second code path for remote nodes.
func TestLedgerErrorsKeepTheirKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		store *fakeStore
		want  error
		call  func(ctx context.Context, c *nodeclient.Client) error
	}{
		{
			name:  "fenced heartbeat",
			store: &fakeStore{heartbeatErr: alloc.ErrFenced},
			want:  alloc.ErrFenced,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				return c.Heartbeat(ctx, "l1", 1)
			},
		},
		{
			name:  "missing lease",
			store: &fakeStore{leaseErr: alloc.ErrLeaseNotFound},
			want:  alloc.ErrLeaseNotFound,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				_, err := c.Lease(ctx, "l1")

				return err
			},
		},
		{
			name:  "fenced release",
			store: &fakeStore{releaseErr: alloc.ErrFenced},
			want:  alloc.ErrFenced,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				return c.Release(ctx, "l1", 1, alloc.PhaseDone)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, base := serve(t, tc.store)
			c := dial(t, base)

			if err := tc.call(t.Context(), c); !errors.Is(err, tc.want) {
				t.Fatalf("want %v across the wire, got %v", tc.want, err)
			}
		})
	}
}

// AN UNREGISTERED NODE IS TOLD TO REGISTER, which is what a restarted control
// plane looks like from a node that is still running.
func TestARestartedControlPlaneTellsTheNodeToRegister(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Never registered — the same state as a node whose server restarted.
	if err := c.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, nodeclient.ErrUnregistered) {
		t.Fatalf("want ErrUnregistered, got %v", err)
	}
}

// THE PATH DECIDES WHICH NODE, NOT THE BODY.
//
// They can disagree, and a node binding a lease under another node's name would
// place compute where the ledger does not expect it.
func TestTheBodyCannotRenameTheNode(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	// The client sends its own name in the body; pass a different one to prove
	// the server ignores it.
	if err := c.Bind(t.Context(), "l1", 1, "somebody-else"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.bound) != 1 || store.bound[0] != "n1/l1" {
		t.Fatalf("the ledger was told %v; the node's identity must come from the "+
			"authenticated path, not from a field it fills in", store.bound)
	}
}

// A phase billet does not know is refused, and the refusal names the value.
func TestAnUnknownPhaseIsRefusedOverTheWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	if err := c.Advance(t.Context(), "l1", 1, alloc.Phase("lauching")); err == nil {
		t.Fatal("a misspelled phase was accepted")
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.advanced) != 0 {
		t.Errorf("the ledger was asked to advance to %v", store.advanced)
	}
}

// Every phase the ledger has can actually be sent.
func TestEveryPhaseCanBeAdvancedTo(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	want := []alloc.Phase{
		alloc.PhaseAssigned, alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy,
	}

	for _, p := range want {
		if err := c.Advance(t.Context(), "l1", 1, p); err != nil {
			t.Errorf("phase %q could not be sent: %v", p, err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.advanced) != len(want) {
		t.Errorf("the ledger saw %v, want %v", store.advanced, want)
	}
}

// LaunchedLeaseIDs survives as a set, including when it is empty.
func TestLaunchedLeasesSurviveEmpty(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})
	c := dial(t, base)

	ids, err := c.LaunchedLeaseIDs(t.Context(), "n1")
	if err != nil {
		t.Fatalf("LaunchedLeaseIDs: %v", err)
	}

	if ids == nil {
		t.Error("an empty set arrived as nil, which a caller would range over as absent " +
			"rather than as empty")
	}
}

// The node learns the TTL from the server rather than inventing one.
func TestTheNodeTakesItsTimingsFromTheServer(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, 90*time.Second)
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)

	if c.LeaseTTL() != 90*time.Second {
		t.Errorf("lease TTL = %v, want the server's 90s; a node that guesses is guessing at "+
			"somebody else's deadline", c.LeaseTTL())
	}
}

// A node from another deployment is refused at registration.
func TestAForeignNodeIsRefusedOverTheWire(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	err = c.Register(t.Context(), config.ProviderDocker, nil, "ffffffffffffffffffffffffffffffff")
	if err == nil {
		t.Fatal("a node from another deployment registered")
	}
}

// The wire refuses a body carrying fields it does not understand, rather than
// silently dropping them.
func TestUnknownFieldsAreRefused(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/v1/register", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	req.Body = http.NoBody

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 400 {
		t.Errorf("an empty registration was accepted with %s", resp.Status)
	}
}

// Loopback is the only address the wire may serve on without TLS.
//
// THIS TEST USED TO ASSERT THE VULNERABILITY. It listed ":7717" as safe, which is
// the wildcard — net.Listen binds every interface for an empty host — so the
// unauthenticated wire, JIT credential endpoint included, was served to the whole
// network by a guard that reported it had not. The hole was pinned by its own
// test rather than caught by it.
func TestOnlyLoopbackIsSafeWithoutTLS(t *testing.T) {
	t.Parallel()

	for addr, want := range map[string]bool{
		"127.0.0.1:7717":  true,
		"localhost:7717":  true,
		"[::1]:7717":      true,
		"127.0.0.53:7717": true,

		// AN IPv4-MAPPED LOOPBACK, which is loopback and which no amount of
		// prefix-matching on "127." recognises. Included because without it the
		// whole test passes against a string-comparison implementation, and this
		// function's first version was exactly that.
		"[::ffff:127.0.0.1]:7717": true,

		// THE WILDCARD, in every spelling. An empty host is not "unspecified so
		// probably local"; it is every interface this machine has.
		":7717":         false,
		"0.0.0.0:7717":  false,
		"[::]:7717":     false,
		"10.0.0.5:7717": false,

		// A name that might resolve to loopback is still not loopback: what it
		// resolves to is not knowable here and can change under the process.
		"example.com:443":       false,
		"localhost.evil.com:80": false,

		// Not an address at all. Refusing beats guessing.
		"127.0.0.1":   false,
		"":            false,
		"garbage":     false,
		"1.2.3.4.5:1": false,
	} {
		if got := nodeplane.LoopbackOnly(addr); got != want {
			t.Errorf("LoopbackOnly(%q) = %v, want %v", addr, got, want)
		}
	}
}
