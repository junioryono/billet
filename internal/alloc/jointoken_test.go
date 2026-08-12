package alloc

import (
	"crypto/sha256"
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
