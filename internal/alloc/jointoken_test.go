package alloc

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

func tokenAllocator(t *testing.T, now func() time.Time) *Allocator {
	t.Helper()

	return newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)}, WithClock(now))
}

// A SINGLE-USE TOKEN IS USED ONCE, and the second caller is refused.
func TestAJoinTokenIsSpentOnce(t *testing.T) {
	a := tokenAllocator(t, time.Now)

	token, err := a.NewJoinToken(t.Context(), time.Hour, 1, "one machine")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if err := a.SpendJoinToken(t.Context(), token); err != nil {
		t.Fatalf("the first use was refused: %v", err)
	}

	if err := a.SpendJoinToken(t.Context(), token); !errors.Is(err, ErrBadJoinToken) {
		t.Errorf("a single-use token was accepted twice: %v", err)
	}
}

// AND TWO MACHINES RACING ON ONE USE CANNOT BOTH WIN.
//
// The check and the decrement are one statement for exactly this: read-then-write
// would let both see a remaining use and both proceed, which is the whole point
// of a single-use credential undone by concurrency.
func TestASingleUseTokenSurvivesARace(t *testing.T) {
	a := tokenAllocator(t, time.Now)

	token, err := a.NewJoinToken(t.Context(), time.Hour, 1, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	const racers = 8

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)

	for range racers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := a.SpendJoinToken(t.Context(), token); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if accepted != 1 {
		t.Errorf("%d of %d racers spent a single-use token; want exactly 1", accepted, racers)
	}
}

// AN EXPIRED TOKEN IS REFUSED, so a value that leaked out of a terminal history
// stops being useful on its own schedule rather than when somebody remembers.
func TestAnExpiredJoinTokenIsRefused(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	a := tokenAllocator(t, func() time.Time { return clock() })

	token, err := a.NewJoinToken(t.Context(), time.Hour, 5, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	now = now.Add(2 * time.Hour)

	if err := a.SpendJoinToken(t.Context(), token); !errors.Is(err, ErrBadJoinToken) {
		t.Errorf("an expired token was accepted: %v", err)
	}
}

// THE TOKEN ITSELF IS NOT STORED, for the same reason a password is not: the
// ledger needs to RECOGNISE one, not to be able to reproduce it. Somebody with a
// copy of the database should not walk away with credentials that still work.
func TestAJoinTokenIsNotStored(t *testing.T) {
	a := tokenAllocator(t, time.Now)

	token, err := a.NewJoinToken(t.Context(), time.Hour, 1, "a note")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	listed, err := a.JoinTokens(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(listed) != 1 {
		t.Fatalf("listed %d tokens, want 1", len(listed))
	}

	// The listing carries what an operator needs to manage it and nothing that
	// could be replayed.
	if listed[0].Note != "a note" || listed[0].Uses != 1 {
		t.Errorf("the listing lost the token's own details: %+v", listed[0])
	}

	// AND THE SECRET IS NOWHERE IN THE ROW. Spending it works, which proves the
	// hash matches; reading the table back gives nothing that could be replayed.
	if err := a.SpendJoinToken(t.Context(), token); err != nil {
		t.Fatalf("the minted token does not work: %v", err)
	}

	stored, err := a.storedJoinTokenKeys(t.Context())
	if err != nil {
		t.Fatalf("read the table: %v", err)
	}

	for _, k := range stored {
		if k == token || strings.Contains(k, token) {
			t.Fatal("the join token is stored verbatim; a copy of the database is a copy of " +
				"every credential that still works")
		}

		// NOT MERELY DIFFERENT — NOT REVERSIBLE. An encoding rather than a hash
		// passes an inequality check and hands the token back to anyone who
		// decodes it, which is the same failure wearing a different alphabet.
		if raw, err := hex.DecodeString(k); err == nil && strings.Contains(string(raw), token) {
			t.Fatal("the stored value decodes back to the join token; it is an encoding, not a hash")
		}

		if len(k) != sha256.Size*2 {
			t.Errorf("the stored value is %d characters; a SHA-256 hex digest is %d",
				len(k), sha256.Size*2)
		}
	}
}

// A REQUEST THAT DOES NOT LAND DOES NOT COST A USE.
//
// The decrement is atomic on its own, and that was not enough: it committed in
// its own transaction, and if recording the request it authorised then failed —
// a crash, a busy ledger — the credential was gone with nothing to show for it.
// The machine retries, is treated as new because no row exists, and finds its
// token spent. It is stranded until an operator mints another and works out why
// the first one evaporated.
//
// The failure is staged with a name already claimed by a different key, which is
// the one way the insert refuses after the token has been checked.
func TestAnEnrollmentThatIsRefusedDoesNotSpendTheToken(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

	token, err := a.NewJoinToken(t.Context(), time.Hour, 1, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// The name is taken by somebody else, so the request cannot be recorded.
	if _, err := a.RequestEnrollment(t.Context(), "epyc-1", "SHA256:first", "csr-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := a.RequestEnrollmentWithToken(
		t.Context(), "epyc-1", "SHA256:second", "csr-2", token,
	); !errors.Is(err, ErrEnrollmentConflict) {
		t.Fatalf("expected the name to be held, got: %v", err)
	}

	// THE USE SURVIVES, so the operator's credential is still good for the
	// machine it was minted for.
	if _, err := a.RequestEnrollmentWithToken(
		t.Context(), "mac-mini-1", "SHA256:other", "csr-3", token,
	); err != nil {
		t.Fatalf("the token was consumed by a request that was refused: %v", err)
	}
}

// AND A POLL IS FREE. A node asks until a human decides, so charging every call
// would spend a single-use token on the second one and strand the machine it was
// minted for.
func TestPollingAnEnrollmentDoesNotSpendTheToken(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

	token, err := a.NewJoinToken(t.Context(), time.Hour, 1, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	for i := range 3 {
		if _, err := a.RequestEnrollmentWithToken(
			t.Context(), "epyc-1", "SHA256:same", "csr-1", token,
		); err != nil {
			t.Fatalf("poll %d was refused, so a waiting node gives up before an operator "+
				"answers: %v", i, err)
		}
	}
}

// A NODE LEARNING IT WAS DENIED DOES NOT PAY AGAIN.
//
// "Denied" is a decision, and it is the one that stops a node retrying — so it
// has to be able to read it. Treating the same key asking again as a NEW request
// spent another use of the token that already paid for the request being
// answered: a single-use token returned 401, and the operator saw a credential
// problem where there was a verdict.
func TestPollingAfterADecisionDoesNotSpendTheToken(t *testing.T) {
	for _, decision := range []string{EnrollApproved, EnrollDenied} {
		t.Run(decision, func(t *testing.T) {
			a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

			token, err := a.NewJoinToken(t.Context(), time.Hour, 1, "")
			if err != nil {
				t.Fatalf("mint: %v", err)
			}

			if _, err := a.RequestEnrollmentWithToken(
				t.Context(), "epyc-1", "SHA256:same", "csr-1", token,
			); err != nil {
				t.Fatalf("first request: %v", err)
			}

			if err := a.DecideEnrollment(t.Context(), "epyc-1", "SHA256:same", decision, "cert"); err != nil {
				t.Fatalf("decide: %v", err)
			}

			rec, err := a.RequestEnrollmentWithToken(
				t.Context(), "epyc-1", "SHA256:same", "csr-1", token)
			if err != nil {
				t.Fatalf("the node could not read its own %s decision because polling for it "+
					"was charged as a new request: %v", decision, err)
			}

			if rec.State != decision {
				t.Errorf("the poll reported %q rather than the recorded %q", rec.State, decision)
			}
		})
	}
}

// AN ANONYMOUS ENROLLMENT NEVER REACHES THE WRITER. /v1/enroll is reachable
// without a client certificate, deliberately, so a machine that has not enrolled
// can ask. Every write transaction is BEGIN IMMEDIATE, so before this check the
// request took SQLite's single writer connection before the token was examined --
// unauthenticated contention against the one process whose loss stops every job.
//
// THE ASSERTION IS THAT THE WRITER WAS NEVER TAKEN, not merely that the call
// failed. It failed before too; it failed after paying the cost this closes. The
// writer is observed by holding it: a long transaction runs on another goroutine
// and the refusal must return while that is still open. Before the pre-check this
// blocked until the holder committed.
func TestAnAnonymousEnrollmentIsRefusedWithoutTakingTheWriter(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	// Hold the single writer slot for the duration of the refusal below.
	go func() {
		done <- a.db.Tx(t.Context(), func(*sql.Tx) error {
			close(held)
			<-release

			return nil
		})
	}()

	<-held

	refused := make(chan error, 1)
	go func() {
		_, err := a.RequestEnrollmentWithToken(
			t.Context(), "stranger", "SHA256:stranger", "csr", "not-a-real-token")
		refused <- err
	}()

	select {
	case err := <-refused:
		if !errors.Is(err, ErrBadJoinToken) {
			t.Errorf("expected ErrBadJoinToken, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("the refusal blocked on the writer slot, so an anonymous caller can still contend for it")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the holding transaction failed: %v", err)
	}
}
