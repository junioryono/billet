package alloc

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
)

// placedOrder records an order, places it under host, and mints one token for it.
func placedOrder(t *testing.T, a *Allocator, id string) string {
	t.Helper()
	ctx := t.Context()
	if err := a.RecordPoolOrder(ctx, poolOrder(id)); err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	token, err := a.NewPoolJoinToken(ctx, id, time.Hour)
	if err != nil {
		t.Fatalf("token for %s: %v", id, err)
	}
	if err := a.PoolOrderPlaced(ctx, id, "i-"+id); err != nil {
		t.Fatalf("place %s: %v", id, err)
	}

	return token
}

// usesLeft is what the ledger says remains of the tokens minted for order, smallest
// first: the tokens of one test share a mint time, so their listed order says nothing.
func usesLeft(t *testing.T, a *Allocator, order string) []int {
	t.Helper()
	tokens, err := a.JoinTokens(t.Context())
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	var out []int
	for _, tok := range tokens {
		if tok.Note == "pool order "+order {
			out = append(out, tok.Uses)
		}
	}
	slices.Sort(out)

	return out
}

func readOrder(t *testing.T, a *Allocator, id string) PoolOrder {
	t.Helper()
	o, err := a.ReadPoolOrder(t.Context(), id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}

	return o
}

func admission(order, fingerprint, token, serial string) PoolAdmission {
	return PoolAdmission{
		Order: order, Fingerprint: fingerprint, CSRPEM: "csr-" + fingerprint, Token: token,
		CertPEM: "cert-" + serial, Serial: serial, NotAfter: "2027-11-06T00:00:00Z",
	}
}

// A POOL'S TOKEN ENROLLS ONLY ITS ORDER'S NODE: under any other name, or another
// order's, it is refused as unusable and nothing is spent, and under its own it records
// a request that names the order.
func TestAPoolTokenEnrollsOnlyItsOrdersNode(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tokA := placedOrder(t, a, "pa")
	placedOrder(t, a, "pb")

	for _, name := range []string{"epyc-1", PoolNodeName("pb"), "pa"} {
		if _, err := a.RequestEnrollmentWithToken(ctx, name, "SHA256:k", "csr", tokA); !errors.Is(err, ErrBadJoinToken) {
			t.Fatalf("order pa's token under %q answered %v, want ErrBadJoinToken", name, err)
		}
	}
	if left := usesLeft(t, a, "pa"); len(left) != 1 || left[0] != 1 {
		t.Fatalf("a refused request spent the token: %v", left)
	}
	e, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tokA)
	if err != nil || e.State != EnrollPending || e.PoolOrder != "pa" {
		t.Fatalf("the order's own name: %+v, %v", e, err)
	}
	if left := usesLeft(t, a, "pa"); len(left) != 1 || left[0] != 0 {
		t.Fatalf("the recorded request did not spend the token: %v", left)
	}
	if got, _, err := a.LookupEnrollment(ctx, PoolNodeName("pa")); err != nil || got.PoolOrder != "pa" {
		t.Fatalf("the request read back as %+v", got)
	}
}

// A HOST THAT ASKS BEFORE ITS ORDER IS RECORDED AS PLACED is told to ask again, and an
// order that is no longer open admits nothing; neither spends the token.
func TestAPoolTokenWaitsForItsOrderAndIsRefusedOnceTheOrderCloses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	if err := a.RecordPoolOrder(ctx, poolOrder("pa")); err != nil {
		t.Fatal(err)
	}
	tok, err := a.NewPoolJoinToken(ctx, "pa", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tok); !errors.Is(err, ErrPoolOrderNotPlaced) {
		t.Fatalf("an order being ordered answered %v, want ErrPoolOrderNotPlaced", err)
	}
	if _, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:k", tok, "01")); !errors.Is(err, ErrPoolOrderNotPlaced) {
		t.Fatalf("admitting from an order being ordered answered %v", err)
	}
	if err := a.PoolOrderUncertain(ctx, "pa", "lost"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tok); !errors.Is(err, ErrPoolOrderClosed) {
		t.Fatalf("an uncertain order answered %v, want ErrPoolOrderClosed", err)
	}
	if _, err := a.NewPoolJoinToken(ctx, "pa", time.Hour); !errors.Is(err, ErrPoolOrderClosed) {
		t.Fatalf("a token for an uncertain order answered %v", err)
	}
	if left := usesLeft(t, a, "pa"); len(left) != 1 || left[0] != 1 {
		t.Fatalf("a refused join spent the token: %v", left)
	}
}

// THE ADMISSION IS ONE TRANSACTION: the request is recorded approved under the order's
// name with its certificate, the certificate is recorded for revocation, the order is
// linked to its node with the key and serial, and every other token of the order is
// retired. A retry with the same key is answered the first certificate; a different key
// afterwards admits nothing.
func TestAPoolAdmissionLinksTheOrderAndRetiresItsTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	second, err := a.NewPoolJoinToken(ctx, "pa", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	e, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:k", second, "0a"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if e.State != EnrollApproved || e.Source != CertPool || e.CertPEM != "cert-0a" || e.Name != PoolNodeName("pa") ||
		e.PoolOrder != "pa" {
		t.Fatalf("admitted as %+v", e)
	}
	o := readOrder(t, a, "pa")
	if o.NodeName != PoolNodeName("pa") || o.NodeFingerprint != "SHA256:k" || o.NodeCertSerial != "0a" {
		t.Fatalf("the order after its admission: %+v", o)
	}
	certs, err := a.LiveCertsFor(ctx, PoolNodeName("pa"))
	if err != nil || len(certs) != 1 || certs[0].Serial != "0a" || certs[0].Source != CertPool {
		t.Fatalf("recorded certificates %+v, %v", certs, err)
	}
	if left := usesLeft(t, a, "pa"); len(left) != 2 || left[0] != 0 || left[1] != 0 {
		t.Fatalf("the order's tokens after its admission: %v, want every one retired", left)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:other", "csr", tok); err == nil {
		t.Fatal("a retired token recorded a request")
	}

	again, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:k", "", "0b"))
	if err != nil || again.CertPEM != "cert-0a" {
		t.Fatalf("a retry with the same key answered %+v, %v; want the first certificate", again, err)
	}
	if certs, err := a.LiveCertsFor(ctx, PoolNodeName("pa")); err != nil || len(certs) != 1 {
		t.Fatalf("a retry recorded another certificate: %+v", certs)
	}
	if _, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:other", tok, "0c")); !errors.Is(err, ErrPoolOrderClosed) {
		t.Fatalf("another key after the admission answered %v, want ErrPoolOrderClosed", err)
	}
}

// A REQUEST RECORDED WHILE THE SOURCE COULD NOT SAY is admitted later on its word,
// without spending a second use; a request of another key already holding the name
// holds the order for a person and admits nothing.
func TestAPendingPoolRequestIsAdmittedAndAnotherKeyHoldsTheOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr-SHA256:k", tok); err != nil {
		t.Fatal(err)
	}
	e, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:k", tok, "0a"))
	if err != nil || e.State != EnrollApproved || e.Source != CertPool {
		t.Fatalf("admitting the pending request: %+v, %v", e, err)
	}
	if o := readOrder(t, a, "pa"); o.NodeFingerprint != "SHA256:k" {
		t.Fatalf("order %+v", o)
	}

	tokB := placedOrder(t, a, "pb")
	spare, err := a.NewPoolJoinToken(ctx, "pb", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pb"), "SHA256:first", "csr", tokB); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AdmitPoolEnrollment(ctx, admission("pb", "SHA256:vouched", spare, "0b")); !errors.Is(err, ErrEnrollmentConflict) {
		t.Fatalf("a vouched key against another's request answered %v, want ErrEnrollmentConflict", err)
	}
	o := readOrder(t, a, "pb")
	if o.JoinHold == "" || o.NodeName != "" {
		t.Fatalf("the order after a conflict: %+v, want held and joined by nothing", o)
	}
	if left := usesLeft(t, a, "pb"); !slices.Equal(left, []int{0, 1}) {
		t.Fatalf("the conflict spent the spare token: %v", left)
	}
}

// A HELD ORDER ADMITS NOTHING ON A SOURCE'S WORD OR A NEW REQUEST, and spends nothing;
// a person's approval of a request already recorded is what the hold waits for, and it
// links the order as the admission would. A person's denial of a key stands against
// the source's word.
func TestAHeldOrderWaitsForAPerson(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	spare, err := a.NewPoolJoinToken(ctx, "pa", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tok); err != nil {
		t.Fatal(err)
	}
	if err := a.HoldPoolJoin(ctx, "pa", "the console named another key", readOrder(t, a, "pa").Updated); err != nil {
		t.Fatal(err)
	}
	if err := a.HoldPoolJoin(ctx, "pa", "a second reason", readOrder(t, a, "pa").Updated); err != nil {
		t.Fatalf("a second hold: %v", err)
	}
	if o := readOrder(t, a, "pa"); o.JoinHold != "the console named another key" {
		t.Fatalf("hold = %q, want the first reason kept", o.JoinHold)
	}
	if _, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:k", spare, "0a")); !errors.Is(err, ErrPoolJoinHeld) {
		t.Fatalf("admitting a held order answered %v", err)
	}
	if left := usesLeft(t, a, "pa"); !slices.Equal(left, []int{0, 1}) {
		t.Fatalf("a refused admission spent the spare token: %v", left)
	}
	if err := a.DecideEnrollment(ctx, PoolNodeName("pa"), "SHA256:k", EnrollApproved, "cert", "0f"); err != nil {
		t.Fatalf("a person's approval of a held order: %v", err)
	}
	o := readOrder(t, a, "pa")
	if o.NodeName != PoolNodeName("pa") || o.NodeFingerprint != "SHA256:k" || o.NodeCertSerial != "0f" {
		t.Fatalf("the order after a person's approval: %+v", o)
	}
	if left := usesLeft(t, a, "pa"); !slices.Equal(left, []int{0, 0}) {
		t.Fatalf("the approval left the spare token live: %v", left)
	}

	tokB := placedOrder(t, a, "pb")
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pb"), "SHA256:denied", "csr", tokB); err != nil {
		t.Fatal(err)
	}
	if err := a.DecideEnrollment(ctx, PoolNodeName("pb"), "SHA256:denied", EnrollDenied, "", ""); err != nil {
		t.Fatal(err)
	}
	e, err := a.AdmitPoolEnrollment(ctx, admission("pb", "SHA256:denied", "", "0b"))
	if err != nil || e.State != EnrollDenied {
		t.Fatalf("the source vouching for a denied key answered %+v, %v; want the denial", e, err)
	}
	if o := readOrder(t, a, "pb"); o.NodeName != "" {
		t.Fatalf("a denied key joined: %+v", o)
	}
}

// A PERSON'S APPROVAL OF A POOL REQUEST WHOSE ORDER CLOSED is refused whole: the
// decision is rolled back with the link, so nothing is admitted from a closed order.
func TestApprovingARequestFromAClosedOrderIsRefusedWhole(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	if err := a.RecordPoolOrder(ctx, poolOrder("pa")); err != nil {
		t.Fatal(err)
	}
	tok, err := a.NewPoolJoinToken(ctx, "pa", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PoolOrderPlaced(ctx, "pa", "i-pa"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tok); err != nil {
		t.Fatal(err)
	}
	// The order closes behind the request: a node joined from it under another key.
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		return a.linkPoolNodeTx(ctx, state.WriteQueries(tx), "pa", PoolNodeName("pa"), "SHA256:other", "09", false)
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.DecideEnrollment(ctx, PoolNodeName("pa"), "SHA256:k", EnrollApproved, "cert", "0a"); !errors.Is(err, ErrPoolOrderClosed) {
		t.Fatalf("approving a request from a closed order answered %v", err)
	}
	if e, _, err := a.LookupEnrollment(ctx, PoolNodeName("pa")); err != nil || e.State != EnrollPending {
		t.Fatalf("the refused approval left the request %s", e.State)
	}
}

// AN ORDER NOTHING CAN JOIN FROM ANY MORE IS HELD: placed, nothing joined, no live
// token and no request waiting. One with a live token, a waiting request or a node is
// not, and the hold is counted once.
func TestALapsedJoinIsHeld(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "lapsed")
	placedOrder(t, a, "live")
	waiting := placedOrder(t, a, "waiting")
	joined := placedOrder(t, a, "joined")
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("waiting"), "SHA256:w", "csr", waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AdmitPoolEnrollment(ctx, admission("joined", "SHA256:j", joined, "0j")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := a.NewPoolJoinToken(ctx, "live", time.Hour); err != nil {
		t.Fatal(err)
	}
	held, err := a.HoldLapsedPoolJoins(ctx)
	if err != nil || held != 1 {
		t.Fatalf("held %d, %v; want 1", held, err)
	}
	for id, want := range map[string]bool{"lapsed": true, "live": false, "waiting": false, "joined": false} {
		if got := readOrder(t, a, id).JoinHold != ""; got != want {
			t.Errorf("%s held = %v, want %v", id, got, want)
		}
	}
	if held, err := a.HoldLapsedPoolJoins(ctx); err != nil || held != 0 {
		t.Fatalf("a second pass held %d, %v", held, err)
	}
}

// A HOLD IS ONLY ON AN ORDER A NODE MAY STILL JOIN FROM: holding a joined or closed
// order changes nothing, holding none is an error, and a hold needs its reason.
func TestAHoldIsOnlyOnAnOpenOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "joined")
	if _, err := a.AdmitPoolEnrollment(ctx, admission("joined", "SHA256:j", tok, "0j")); err != nil {
		t.Fatal(err)
	}
	if err := a.HoldPoolJoin(ctx, "joined", "too late", readOrder(t, a, "joined").Updated); err != nil {
		t.Fatalf("holding a joined order: %v", err)
	}
	if o := readOrder(t, a, "joined"); o.JoinHold != "" {
		t.Fatalf("a joined order was held: %+v", o)
	}
	if err := a.HoldPoolJoin(ctx, "missing", "why", time.Time{}); !errors.Is(err, ErrPoolOrderClosed) {
		t.Fatalf("holding no order answered %v", err)
	}
	if err := a.HoldPoolJoin(ctx, "joined", "", readOrder(t, a, "joined").Updated); err == nil {
		t.Fatal("a hold with no reason was accepted")
	}
}

// A PERSON RELEASES A HELD JOIN, and the host's next request is admitted on its
// source's word again; an order with no hold, a closed order or none at all has
// nothing to release. A person's approval of a held order clears its hold.
func TestAHeldJoinIsReleasedOnAPersonsWord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	if err := a.ReleasePoolJoin(ctx, "pa"); !errors.Is(err, ErrPoolJoinNotHeld) {
		t.Fatalf("releasing an order with no hold answered %v", err)
	}
	if err := a.HoldPoolJoin(ctx, "pa", "a key nobody can account for", readOrder(t, a, "pa").Updated); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:k", tok, "0a")); !errors.Is(err, ErrPoolJoinHeld) {
		t.Fatalf("admitting a held order answered %v", err)
	}
	if err := a.ReleasePoolJoin(ctx, "pa"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := a.AdmitPoolEnrollment(ctx, admission("pa", "SHA256:k", tok, "0a")); err != nil {
		t.Fatalf("admitting once the hold was released: %v", err)
	}
	if err := a.ReleasePoolJoin(ctx, "pa"); !errors.Is(err, ErrPoolOrderClosed) && !errors.Is(err, ErrPoolJoinNotHeld) {
		t.Fatalf("releasing a joined order answered %v", err)
	}
	if err := a.ReleasePoolJoin(ctx, "missing"); !errors.Is(err, ErrPoolOrderClosed) {
		t.Fatalf("releasing no order answered %v", err)
	}

	tokB := placedOrder(t, a, "pb")
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pb"), "SHA256:b", "csr", tokB); err != nil {
		t.Fatal(err)
	}
	if err := a.HoldPoolJoin(ctx, "pb", "held", readOrder(t, a, "pb").Updated); err != nil {
		t.Fatal(err)
	}
	if err := a.DecideEnrollment(ctx, PoolNodeName("pb"), "SHA256:b", EnrollApproved, "cert", "0b"); err != nil {
		t.Fatal(err)
	}
	if o := readOrder(t, a, "pb"); o.JoinHold != "" || o.NodeName == "" {
		t.Fatalf("a person's approval left %+v", o)
	}
}

// A POOL'S NAMES ARE ITS TOKENS' ALONE: an operator's token under a `pool-` name is
// refused and not spent, so it cannot claim the name of a host the pool ordered.
func TestAnOperatorsTokenCannotClaimAPoolName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok, err := a.NewJoinToken(ctx, time.Hour, 1, "an operator's")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tok); !errors.Is(err, ErrBadJoinToken) {
		t.Fatalf("an operator's token under a pool name answered %v", err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, "epyc-1", "SHA256:k", "csr", tok); err != nil {
		t.Fatalf("the unspent token under an ordinary name: %v", err)
	}
	if !IsPoolNodeName(PoolNodeName("x")) || IsPoolNodeName("epyc-1") {
		t.Fatal("IsPoolNodeName disagrees with PoolNodeName")
	}
}

// A TOKEN'S ORDER IS READ WHETHER IT IS SPENT OR NOT, WITH WHETHER IT IS LIVE: an
// unknown token and an operator's name no order.
func TestATokensOrderIsReadWithWhetherItIsLive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	if order, live, err := a.PoolJoinOrder(ctx, tok); err != nil || order != "pa" || !live {
		t.Fatalf("a live token: %q %v %v", order, live, err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tok); err != nil {
		t.Fatal(err)
	}
	if order, live, err := a.PoolJoinOrder(ctx, tok); err != nil || order != "pa" || live {
		t.Fatalf("a spent token: %q %v %v", order, live, err)
	}
	if order, live, err := a.PoolJoinOrder(ctx, "never-minted"); err != nil || order != "" || live {
		t.Fatalf("an unknown token: %q %v %v", order, live, err)
	}
	if _, err := a.NewPoolJoinToken(ctx, "pa", 0); err == nil {
		t.Fatal("a token with no lifetime was minted")
	}
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		return a.linkPoolNodeTx(ctx, state.WriteQueries(tx), "pa", PoolNodeName("pa"), "SHA256:k", "", false)
	}); err == nil {
		t.Fatal("an order was linked without its certificate's serial")
	}
}

// A HELD ORDER'S REQUEST IS RECORDED FOR A PERSON WITHOUT SPENDING A TOKEN, even once
// every token lapsed, so a person can approve the key the host presents: the name is
// still the first key's, a denied key hears its denial, and only a held order records
// this way. The approval then links the order as any approval does.
func TestAHeldOrdersRequestIsRecordedForAPerson(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "pa")
	if _, err := a.RecordHeldPoolRequest(ctx, "pa", "SHA256:k", "csr"); !errors.Is(err, ErrPoolJoinNotHeld) {
		t.Fatalf("recording for an order that is not held answered %v", err)
	}
	if err := a.HoldPoolJoin(ctx, "pa", "the console names another key", readOrder(t, a, "pa").Updated); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	e, err := a.RecordHeldPoolRequest(ctx, "pa", "SHA256:k", "csr")
	if err != nil || e.State != EnrollPending || e.PoolOrder != "pa" || e.Name != PoolNodeName("pa") {
		t.Fatalf("recording a held order's request: %+v, %v", e, err)
	}
	if left := usesLeft(t, a, "pa"); !slices.Equal(left, []int{1}) {
		t.Fatalf("recording spent a token: %v", left)
	}
	if again, err := a.RecordHeldPoolRequest(ctx, "pa", "SHA256:k", "csr"); err != nil || again.State != EnrollPending {
		t.Fatalf("the same key again: %+v, %v", again, err)
	}
	if _, err := a.RecordHeldPoolRequest(ctx, "pa", "SHA256:other", "csr"); !errors.Is(err, ErrEnrollmentConflict) {
		t.Fatalf("another key: %v", err)
	}
	if err := a.DecideEnrollment(ctx, PoolNodeName("pa"), "SHA256:k", EnrollApproved, "cert", "0a"); err != nil {
		t.Fatalf("a person's approval of the held request: %v", err)
	}
	if o := readOrder(t, a, "pa"); o.NodeName != PoolNodeName("pa") || o.JoinHold != "" {
		t.Fatalf("the order after the approval: %+v", o)
	}

	placedOrder(t, a, "pb")
	if err := a.HoldPoolJoin(ctx, "pb", "held", readOrder(t, a, "pb").Updated); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecordHeldPoolRequest(ctx, "pb", "SHA256:denied", "csr"); err != nil {
		t.Fatal(err)
	}
	if err := a.DecideEnrollment(ctx, PoolNodeName("pb"), "SHA256:denied", EnrollDenied, "", ""); err != nil {
		t.Fatal(err)
	}
	if e, err := a.RecordHeldPoolRequest(ctx, "pb", "SHA256:denied", "csr"); err != nil || e.State != EnrollDenied {
		t.Fatalf("a denied key again: %+v, %v", e, err)
	}
	if e, err := a.RecordHeldPoolRequest(ctx, "pb", "SHA256:next", "csr"); err != nil || e.State != EnrollPending ||
		e.Fingerprint != "SHA256:next" {
		t.Fatalf("another key once the first was denied: %+v, %v", e, err)
	}
}

// A VERDICT REACHED BEFORE THE ORDER MOVED HOLDS NOTHING: a person released a hold
// between a request's judgment and its hold, and the release stands.
func TestAHoldOnAStaleVerdictHoldsNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "pa")
	judged := readOrder(t, a, "pa").Updated
	now = now.Add(time.Second)
	if err := a.HoldPoolJoin(ctx, "pa", "first", judged); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := a.ReleasePoolJoin(ctx, "pa"); err != nil {
		t.Fatal(err)
	}
	if err := a.HoldPoolJoin(ctx, "pa", "a verdict from before the release", judged); !errors.Is(err, ErrPoolOrderMoved) {
		t.Fatalf("holding on a stale verdict answered %v", err)
	}
	if o := readOrder(t, a, "pa"); o.JoinHold != "" {
		t.Fatalf("a stale verdict held the order: %+v", o)
	}
}

// ONE ORDER'S LAPSE IS HELD ON ITS OWN, by the same proof the pool's step uses: placed,
// joined by no node, no live token and no request waiting. An order something can still
// join from is not held, and one already held answers held.
func TestOneOrdersLapseIsHeldOnItsOwn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "pa")
	placedOrder(t, a, "pb")
	if held, err := a.HoldLapsedPoolJoin(ctx, "pa"); err != nil || held {
		t.Fatalf("an order with a live token: held %v, %v", held, err)
	}
	now = now.Add(2 * time.Hour)
	if held, err := a.HoldLapsedPoolJoin(ctx, "pa"); err != nil || !held {
		t.Fatalf("an order nothing can join from: held %v, %v", held, err)
	}
	if o := readOrder(t, a, "pb"); o.JoinHold != "" {
		t.Fatalf("holding one order held another: %+v", o)
	}
	if held, err := a.HoldLapsedPoolJoin(ctx, "pa"); err != nil || !held {
		t.Fatalf("an order already held: held %v, %v", held, err)
	}
	if _, err := a.HoldLapsedPoolJoin(ctx, "missing"); !errors.Is(err, ErrPoolOrderClosed) {
		t.Fatalf("no such order: %v", err)
	}
}
