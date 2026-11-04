package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

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
		wantErr   bool
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
			wantErr:   true,
			heartbeat: nodeclient.ErrSuperseded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, base := serve(t, tc.store)

			old := dial(t, base)
			current := dial(t, base)

			adopted, err := old.ClaimCustody(t.Context(), []string{"l1"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("ClaimCustody = %v, want an error: %v", err, tc.wantErr)
			}

			if len(adopted) != 0 {
				t.Fatalf("a lease %v was called adopted by a process that holds nothing", adopted)
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

// A CLAIM NAMING MORE THAN A FLEET HOLDS IS REFUSED before the ledger is asked about
// any of it.
func TestACustodyClaimIsBounded(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}})
	c := dial(t, base)

	many := make([]string, 4097)
	for i := range many {
		many[i] = fmt.Sprintf("l%d", i)
	}

	if _, err := c.ClaimCustody(t.Context(), many); !errors.Is(err, nodeclient.ErrRefused) {
		t.Fatalf("a claim of %d leases = %v, want a refusal", len(many), err)
	}

	if p.OwnsForTest("l1", "n1", c.Incarnation()) {
		t.Fatal("a refused claim took a lease")
	}

	if _, err := c.ClaimCustody(t.Context(), many[:4096]); err != nil {
		t.Fatalf("a claim of 4096 leases = %v", err)
	}
}

// THE ANSWER SAYS WHAT THIS CONTROLLER SPEAKS, since the process reached it without
// registering and has nothing else to go on.
func TestACustodyClaimReportsTheControllersVersion(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{leaseErr: alloc.ErrLeaseNotFound})

	body, err := json.Marshal(nodeapi.ClaimCustodyRequest{LeaseIDs: []string{"l1"}})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/nodes/n1/custody", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set(nodeapi.HeaderIncarnation, "p1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	var res nodeapi.ClaimCustodyResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	if res.Version != nodeapi.Version {
		t.Fatalf("the answer says version %d, want %d", res.Version, nodeapi.Version)
	}
}

// A CLAIM NAMING NO PROCESS GRANTS NOTHING TO NOBODY: the lease would have an owner
// nothing could be, and the process that has the compute would be refused it.
func TestACustodyClaimNamingNoProcessIsRefused(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}})

	body, err := json.Marshal(nodeapi.ClaimCustodyRequest{LeaseIDs: []string{"l1"}})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/nodes/n1/custody", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a claim naming no process answered %s", resp.Status)
	}

	if p.OwnsForTest("l1", "n1", "") {
		t.Fatal("a claim naming no process took a lease")
	}
}

// THE PROCESS THAT HAS THE COMPUTE LETS GO when the current one reported it: the
// answer names it, and the lease stays with the current process.
func TestACustodyClaimSaysWhatTheCurrentProcessAdopted(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1", RequestID: 7}})

	old := dial(t, base)
	current := dial(t, base)

	p.AdoptOwnershipWithInventory("n1", current.Incarnation(), []string{"l1"}, true)

	adopted, err := old.ClaimCustody(t.Context(), []string{"l1"})
	if err != nil {
		t.Fatalf("ClaimCustody: %v", err)
	}

	if len(adopted) != 1 || adopted[0] != "l1" {
		t.Fatalf("adopted = %v, want the lease", adopted)
	}

	if !p.OwnsForTest("l1", "n1", current.Incarnation()) {
		t.Fatal("the claim moved a lease the current process has the compute of")
	}
}
