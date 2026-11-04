package nodeplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// THE LEDGER DECIDES WHAT MAY BE CLAIMED, and a store that could not say grants
// nothing. Each case is a superseded process claiming on a controller whose
// ownership is empty, then trying the lease's own route, which is what the claim is
// for: renewing a lease the ledger places on its node, and being refused the rest.
func TestACustodyClaimIsHeldToTheLedger(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		store     *fakeStore
		wantErr   string
		wantOwner bool
		heartbeat error
	}{
		{
			name:      "bound to this node",
			store:     &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}},
			wantOwner: true,
		},
		{
			name:      "chosen for this node and not yet bound",
			store:     &fakeStore{lease: &alloc.Lease{ID: "l1", TargetNode: "n1", RequestID: 7}},
			wantOwner: true,
		},
		{
			name:      "bound to another node",
			store:     &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n2", RequestID: 7}},
			heartbeat: nodeclient.ErrSuperseded,
		},
		{
			name:      "bound to another, chosen for this one",
			store:     &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n2", TargetNode: "n1", RequestID: 7}},
			heartbeat: nodeclient.ErrSuperseded,
		},
		{
			name:      "placed nowhere",
			store:     &fakeStore{lease: &alloc.Lease{ID: "l1", RequestID: 7}},
			heartbeat: nodeclient.ErrSuperseded,
		},
		{
			name:      "ended",
			store:     &fakeStore{leaseErr: alloc.ErrLeaseNotFound},
			heartbeat: alloc.ErrLeaseNotFound,
		},
		{
			name:      "a ledger that could not answer",
			store:     &fakeStore{leaseErr: errors.New("busy")},
			wantErr:   "busy",
			heartbeat: nodeclient.ErrSuperseded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, base := serve(t, tc.store)

			old := dial(t, base)
			current := dial(t, base)

			held, err := old.ClaimCustody(t.Context(), []string{"l1"})

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ClaimCustody: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("ClaimCustody = %v, want %q", err, tc.wantErr)
			}

			if got := len(held) == 1; got != tc.wantOwner {
				t.Fatalf("the answer says it holds %v, want the lease held: %v", held, tc.wantOwner)
			}

			if got := p.OwnsForTest("l1", "n1", old.Incarnation()); got != tc.wantOwner {
				t.Fatalf("the claimant owns the lease: %v, want %v", got, tc.wantOwner)
			}

			if p.OwnsForTest("l1", "n1", current.Incarnation()) {
				t.Fatal("the claim gave the lease to the current process")
			}

			// THE REQUEST IS THE LEDGER'S, as the destroy that ends the job needs it.
			if tc.wantOwner {
				owner, known := p.OwnerOfRequest(7)
				if !known || owner.Incarnation != old.Incarnation() {
					t.Fatalf("the request's owner is %+v (known=%v)", owner, known)
				}
			}

			if err := old.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, tc.heartbeat) {
				t.Fatalf("the claimant's heartbeat = %v, want %v", err, tc.heartbeat)
			}
		})
	}
}

// readingStore counts the ledger's reads of a lease, and says a lease ended once it has
// been read openAnswers times.
type readingStore struct {
	*fakeStore

	reads       atomic.Int32
	openAnswers int32
	// onOpen runs as the last open answer is given, with that read's context.
	onOpen func(ctx context.Context)
}

func (s *readingStore) Lease(ctx context.Context, id string) (*alloc.Lease, error) {
	n := s.reads.Add(1)
	if s.openAnswers > 0 && n > s.openAnswers {
		return nil, alloc.ErrLeaseNotFound
	}

	if s.onOpen != nil && n == s.openAnswers {
		s.onOpen(ctx)
	}

	return s.fakeStore.Lease(ctx, id)
}

// A CLAIM NAMING MORE THAN A FLEET HOLDS IS REFUSED before the ledger is asked about
// any of it.
func TestACustodyClaimIsBounded(t *testing.T) {
	t.Parallel()

	store := &readingStore{fakeStore: &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}}}
	p, base := serve(t, store)
	c := dial(t, base)

	many := make([]string, 4097)
	for i := range many {
		many[i] = fmt.Sprintf("l%d", i)
	}

	reads := store.reads.Load()

	if _, err := c.ClaimCustody(t.Context(), many); !errors.Is(err, nodeclient.ErrRefused) {
		t.Fatalf("a claim of %d leases = %v, want a refusal", len(many), err)
	}

	if got := store.reads.Load(); got != reads {
		t.Fatalf("a refused claim read the ledger %d times", got-reads)
	}

	if p.OwnsForTest("l1", "n1", c.Incarnation()) {
		t.Fatal("a refused claim took a lease")
	}

	if _, err := c.ClaimCustody(t.Context(), many[:4096]); err != nil {
		t.Fatalf("a claim of 4096 leases = %v", err)
	}
}

// A LEASE THAT ENDS WHILE ITS CLAIM IS IN HAND IS NOT LEFT WITH AN OWNER. The ledger said
// it was open, and the release and its forgetting came before the claim was written: the
// process has let go of it, so nothing would ever name the record again, and it would
// answer for its request for as long as the plane ran.
func TestALeaseThatEndsDuringItsClaimIsForgotten(t *testing.T) {
	t.Parallel()

	store := &readingStore{
		fakeStore:   &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}},
		openAnswers: 1,
	}
	p, base := serve(t, store)
	c := dial(t, base)

	held, err := c.ClaimCustody(t.Context(), []string{"l1"})
	if err != nil {
		t.Fatalf("ClaimCustody: %v", err)
	}

	if len(held) != 0 {
		t.Fatalf("the answer says it holds %v, of a lease that ended", held)
	}

	if p.OwnsForTest("l1", "n1", c.Incarnation()) {
		t.Fatal("the claim left an owner for a lease that ended")
	}

	if _, known := p.OwnerOfRequest(7); known {
		t.Fatal("a lease that ended still answers for its request")
	}
}

// cancellingStore refuses a read on a context that has ended, as a real store does.
type cancellingStore struct {
	*readingStore
}

func (s *cancellingStore) Lease(ctx context.Context, id string) (*alloc.Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return s.readingStore.Lease(ctx, id)
}

// A CLAIM WHOSE REQUEST IS CANCELLED STILL READS BACK WHAT IT ADDED. The process's last
// release is what ends its wait and cancels its claim, which is exactly when the lease
// has just ended, and nothing would ever name the record left behind.
func TestALeaseThatEndsDuringACancelledClaimIsForgotten(t *testing.T) {
	t.Parallel()

	store := &cancellingStore{readingStore: &readingStore{
		fakeStore:   &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}},
		openAnswers: 1,
	}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// THE REQUEST HAS ENDED ON THE SERVER'S SIDE BEFORE THE CLAIM IS WRITTEN, so only a read
	// back on another context can see that the lease ended.
	store.onOpen = func(served context.Context) {
		cancel()

		select {
		case <-served.Done():
		case <-time.After(10 * time.Second):
			t.Error("the server never saw the request end")
		}
	}

	p, base := serve(t, store)
	c := dial(t, base)

	if _, err := c.ClaimCustody(ctx, []string{"l1"}); err == nil {
		t.Log("the answer arrived before the cancellation")
	}

	deadline := time.Now().Add(10 * time.Second)
	for store.reads.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the claim never read back what it added")
		}

		time.Sleep(time.Millisecond)
	}

	waitForForgotten := time.Now().Add(10 * time.Second)
	for p.OwnsForTest("l1", "n1", c.Incarnation()) {
		if time.Now().After(waitForForgotten) {
			t.Fatal("the claim left an owner for a lease that ended")
		}

		time.Sleep(time.Millisecond)
	}
}

// claimRaw is a claim the client would not send.
func claimRaw(t *testing.T, base, incarnation string) *http.Response {
	t.Helper()

	body, err := json.Marshal(nodeapi.ClaimCustodyRequest{LeaseIDs: []string{"l1"}})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/nodes/n1/custody", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	if incarnation != "" {
		req.Header.Set(nodeapi.HeaderIncarnation, incarnation)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

// A CLAIM NAMING NO PROCESS GRANTS NOTHING TO NOBODY: the lease would have an owner
// nothing could be, and the process that has the compute would be refused it.
func TestACustodyClaimNamingNoProcessIsRefused(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}})

	if resp := claimRaw(t, base, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a claim naming no process answered %s", resp.Status)
	}

	if p.OwnsForTest("l1", "n1", "") {
		t.Fatal("a claim naming no process took a lease")
	}
}

// THE ANSWER SAYS WHAT THIS CONTROLLER SPEAKS, since the process reached it without
// registering and has nothing else to go on.
func TestACustodyClaimReportsTheControllersVersion(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{leaseErr: alloc.ErrLeaseNotFound})

	var res nodeapi.ClaimCustodyResponse
	if err := json.NewDecoder(claimRaw(t, base, "p1").Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	if res.Version != nodeapi.Version {
		t.Fatalf("the answer says version %d, want %d", res.Version, nodeapi.Version)
	}
}
