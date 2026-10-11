package e2e

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/simulated"
	storecontract "github.com/junioryono/billet/internal/store"
)

// withGuestReports gives a wire stack's node a guest cache service that takes its
// guests' own reports.
func withGuestReports(c *stackConfig) { c.guestReports = true }

// guestCacheService is the node's guest cache service as `billet node` builds it
// from node.cache and node.monitoring.guest (app.NewNodeCacheService), over a
// store that holds no volumes: what is under test is the guest's channel, and a
// site's store would need a Ceph cluster.
func guestCacheService(t *testing.T, host, deployment string, nc *nodeclient.Client) *node.CacheService {
	t.Helper()

	cfg := &config.Config{Node: &config.NodeConfig{
		Name: host, StateDir: t.TempDir(),
		Cache:      &config.NodeCacheConfig{Listen: "127.0.0.1:7718", GuestEndpoint: "http://127.0.0.1:7718"},
		Monitoring: &config.NodeMonitoringConfig{Guest: true},
	}}

	service, err := app.NewNodeCacheService(cfg, deployment, noVolumes{}, noVolumes{}, nc)
	if err != nil {
		t.Fatalf("app.NewNodeCacheService: %v", err)
	}

	return service
}

// errNoVolumes is every answer of a store and an attacher that hold nothing.
var errNoVolumes = errors.New("this harness's store holds no volumes")

// noVolumes is a cache store and a volume attacher with nothing in them.
type noVolumes struct{}

func (noVolumes) Current(context.Context, string) (string, error) { return "", errNoVolumes }

func (noVolumes) Create(context.Context, string, int64) (storecontract.Volume, error) {
	return storecontract.Volume{}, errNoVolumes
}

func (noVolumes) Clone(context.Context, string, string) (storecontract.Volume, error) {
	return storecontract.Volume{}, errNoVolumes
}

func (noVolumes) RenewActive(context.Context, storecontract.Volume, time.Time) error {
	return errNoVolumes
}

func (noVolumes) AcquireWriter(context.Context, string, string, time.Duration,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	return storecontract.WriterLease{}, 0, errNoVolumes
}

func (noVolumes) ReleaseWriter(context.Context, storecontract.WriterLease, storecontract.FencingToken) error {
	return errNoVolumes
}

func (noVolumes) Snapshot(context.Context, storecontract.Volume) (storecontract.Candidate, error) {
	return storecontract.Candidate{}, errNoVolumes
}

func (noVolumes) PublishCAS(context.Context, string, string, storecontract.Candidate,
	storecontract.WriterLease, storecontract.FencingToken,
) error {
	return errNoVolumes
}

func (noVolumes) Discard(context.Context, storecontract.Volume) error { return nil }

func (noVolumes) Evict(context.Context, time.Duration) error { return nil }

func (noVolumes) AttachVolume(context.Context, string, int, string) error { return errNoVolumes }

func (noVolumes) DetachVolume(context.Context, string, int, string) error { return nil }

// specRecorder is a backend that remembers what each guest was given, which is
// the only place the guest's bearer and its instruction to monitor exist.
type specRecorder struct {
	provider.Provider

	mu    sync.Mutex
	specs []provider.Spec
}

func (r *specRecorder) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()

	return r.Provider.Launch(ctx, spec)
}

func (r *specRecorder) only(t *testing.T) provider.Spec {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.specs) != 1 {
		t.Fatalf("the backend launched %d guests, want 1", len(r.specs))
	}

	return r.specs[0]
}

// A GUEST'S REPORT REACHES THE LEDGER ACROSS THE WIRE, assembled as the CLI
// assembles both roles: the guest is told to start its agent, says hello and sends
// its last batch with the bearer it was given, the job completes, and the node
// sends what it kept before the destroy returns, against the lease the bearer's
// session belongs to. The plane finds the allocator as its guest report store.
func TestAGuestsReportReachesTheLedgerAcrossTheWire(t *testing.T) {
	recorder := &specRecorder{}
	backend := simulatedBackend(simulated.NewHost())

	s := newWireStack(t, withGuestReports, withBackend(config.ProviderSimulated,
		func(t *testing.T, deployment string) provider.Provider {
			t.Helper()

			recorder.Provider = backend(t, deployment)

			return recorder
		}))

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 4101, "push", testTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job across the wire")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 4101, "push", testTier))
	s.awaitOneRunning(t)

	spec := recorder.only(t)
	if !spec.GuestMonitor || spec.CacheToken == "" {
		t.Fatalf("the guest was given monitor %t and a bearer %t, want both", spec.GuestMonitor, spec.CacheToken != "")
	}

	hello := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/guest/limits", nil)
	hello.Header.Set("Authorization", "Bearer "+spec.CacheToken)
	limits := httptest.NewRecorder()
	s.guestCache.ServeHTTP(limits, hello)
	if limits.Code != http.StatusOK {
		t.Fatalf("the guest's hello answered %d, want 200", limits.Code)
	}

	data, err := guestreport.EncodeBatch(guestreport.Batch{
		Seq: 1, AgentVersion: "v0.15.0", TicksPerSecond: 100,
		Steps: []guestreport.StepMark{{Name: "Run tests", AtSeconds: time.Now().Unix()}},
	})
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}

	batch := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/guest/reports?final=1",
		bytes.NewReader(data))
	batch.Header.Set("Authorization", "Bearer "+spec.CacheToken)
	batch.Header.Set("Content-Type", "application/vnd.billet.guest-batch")
	accepted := httptest.NewRecorder()
	s.guestCache.ServeHTTP(accepted, batch)
	if accepted.Code != http.StatusNoContent {
		t.Fatalf("the guest's batch answered %d, want 204", accepted.Code)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		fakeactions.JobJSON("JobCompleted", 4101, "push", testTier))
	s.awaitGone(t)

	leaseID, ok := provider.LeaseOf(spec.Name)
	if !ok {
		t.Fatalf("the guest %q names no lease", spec.Name)
	}

	// THE ROW IS WRITTEN BEFORE THE DESTROY RETURNS, and awaitGone sees the
	// backend's half of that destroy, so the row may land a moment after.
	var recorded alloc.RecordedGuestReport

	deadline = time.Now().Add(30 * time.Second)
	for {
		recorded, err = s.alloc.LeaseGuestReport(t.Context(), leaseID)
		if err == nil {
			break
		}
		if !errors.Is(err, alloc.ErrLeaseNotFound) || time.Now().After(deadline) {
			t.Fatalf("LeaseGuestReport: %v", err)
		}

		time.Sleep(50 * time.Millisecond)
	}

	if recorded.Node != s.node || !recorded.Hello || recorded.Accepted != 1 || !recorded.FinalSeen ||
		recorded.NodeRestarted || recorded.AgentVersion != "v0.15.0" {
		t.Errorf("the ledger keeps node %q, hello %t, accepted %d, final %t, restarted %t, agent %q; "+
			"want %q, true, 1, true, false, v0.15.0", recorded.Node, recorded.Hello, recorded.Accepted,
			recorded.FinalSeen, recorded.NodeRestarted, recorded.AgentVersion, s.node)
	}

	merged, err := guestreport.Decode(recorded.Data)
	if err != nil {
		t.Fatalf("the ledger's guest data does not decode: %v", err)
	}
	if merged.Batches != 1 || len(merged.Steps) != 1 || merged.Steps[0].Name != "Run tests" {
		t.Errorf("the ledger's guest data holds %d batches and steps %v, want 1 and Run tests",
			merged.Batches, merged.Steps)
	}
}
