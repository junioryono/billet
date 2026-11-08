package nodeplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// afterOpen is what a read past the open answers says; "ended" when nil.
	afterOpen error
}

func (s *readingStore) Lease(ctx context.Context, id string) (*alloc.Lease, error) {
	n := s.reads.Add(1)
	if s.openAnswers > 0 && n > s.openAnswers {
		if s.afterOpen != nil {
			return nil, s.afterOpen
		}

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

	many := make([]string, nodeapi.MaxCustodyClaim+1)
	for i := range many {
		many[i] = fmt.Sprintf("l%d", i)
	}

	reads := store.reads.Load()

	if status, _ := claimRaw(t, base, c.Incarnation(), many...); status != http.StatusForbidden {
		t.Fatalf("a claim of %d leases answered %d, want a refusal", len(many), status)
	}

	if got := store.reads.Load(); got != reads {
		t.Fatalf("a refused claim read the ledger %d times", got-reads)
	}

	if p.OwnsForTest("l1", "n1", c.Incarnation()) {
		t.Fatal("a refused claim took a lease")
	}

	if status, _ := claimRaw(t, base, c.Incarnation(), many[:nodeapi.MaxCustodyClaim]...); status != http.StatusOK {
		t.Fatalf("a claim of as many as one may answered %d", status)
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

// A STORE THAT COULD NOT ANSWER THE READ BACK KEEPS THE RECORD: could-not-tell is not an
// end, and the process may still hold the compute it just claimed.
func TestAClaimWhoseReadBackCouldNotBeAnsweredStands(t *testing.T) {
	t.Parallel()

	store := &readingStore{
		fakeStore:   &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}},
		openAnswers: 1,
		afterOpen:   errors.New("busy"),
	}
	p, base := serve(t, store)
	c := dial(t, base)

	held, err := c.ClaimCustody(t.Context(), []string{"l1"})
	if err != nil || len(held) != 1 {
		t.Fatalf("ClaimCustody = %v, %v, want the lease held", held, err)
	}

	if !p.OwnsForTest("l1", "n1", c.Incarnation()) {
		t.Fatal("a read that could not answer took the lease from the process that holds it")
	}
}

// A SUPERSEDED PROCESS HEARS A LEASE IS OVER WHETHER OR NOT ANYONE IS REGISTERED. "Register
// again" is not something it may do, so that answer would hold its wait for as long as
// nothing registered, over a lease already over. Only an end the ledger states gets it.
func TestAProcessIsToldALeaseIsOverWithNoProcessRegistered(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		store *fakeStore
		want  error
	}{
		{"ended", &fakeStore{leaseErr: alloc.ErrLeaseNotFound}, alloc.ErrLeaseNotFound},
		{"a ledger that could not answer", &fakeStore{leaseErr: errors.New("busy")}, nodeclient.ErrUnregistered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, base := serve(t, tc.store)
			c := dial(t, base)
			p.ForgetForTest("n1")

			if err := c.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, tc.want) {
				t.Fatalf("Heartbeat = %v, want %v", err, tc.want)
			}
		})
	}
}

// claimRaw is a claim the client would not send, and its status and body.
func claimRaw(t *testing.T, base, incarnation string, leases ...string) (int, []byte) {
	t.Helper()

	if len(leases) == 0 {
		leases = []string{"l1"}
	}

	body, err := json.Marshal(nodeapi.ClaimCustodyRequest{LeaseIDs: leases})
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

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, answer
}

// A CLAIM NAMING NO PROCESS GRANTS NOTHING TO NOBODY: the lease would have an owner
// nothing could be, and the process that has the compute would be refused it.
func TestACustodyClaimNamingNoProcessIsRefused(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}})

	if status, _ := claimRaw(t, base, ""); status != http.StatusForbidden {
		t.Fatalf("a claim naming no process answered %d", status)
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

	_, answer := claimRaw(t, base, "p1")

	var res nodeapi.ClaimCustodyResponse
	if err := json.Unmarshal(answer, &res); err != nil {
		t.Fatal(err)
	}

	if res.Version != nodeapi.Version {
		t.Fatalf("the answer says version %d, want %d", res.Version, nodeapi.Version)
	}
}
