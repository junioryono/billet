package state

import (
	"errors"
	"testing"
	"time"
)

// A STANDBY ASKS ON ONE SESSION, for as long as it waits. A connection per attempt would
// start a server backend every two seconds and register a pgx configuration that is never
// let go, and that is what a waiting standby costs a packed server, forever.
func TestAWaitingStandbyKeepsOneSession(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)

	holder, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	standby, err := OpenPostgresStandby(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgresStandby: %v", err)
	}
	t.Cleanup(func() { _ = standby.Close() })

	backend, ok := standby.backend.(*postgresBackend)
	if !ok {
		t.Fatalf("a standby's backend is %T", standby.backend)
	}

	session := int64(0)

	for attempt := range 3 {
		if err := backend.claimController(t.Context(), standby); !errors.Is(err, ErrControllerHeld) {
			t.Fatalf("attempt %d: %v, want the claim held", attempt, err)
		}

		var pid int64
		if err := backend.claimAttempt.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("attempt %d: read the session: %v", attempt, err)
		}

		if session != 0 && pid != session {
			t.Fatalf("attempt %d asked on session %d, and the last on %d", attempt, pid, session)
		}

		session = pid
	}

	if err := holder.Close(); err != nil {
		t.Fatalf("close the holder: %v", err)
	}

	// THE SESSION THAT WAITED IS THE ONE THAT TAKES IT, asking as a standby asks: the server
	// ends the holder's session a moment after its client closes.
	deadline := time.Now().Add(30 * time.Second)

	for {
		err := backend.claimController(t.Context(), standby)
		if err == nil {
			break
		}

		if !errors.Is(err, ErrControllerHeld) || time.Now().After(deadline) {
			t.Fatalf("take the claim the holder let go: %v", err)
		}

		time.Sleep(20 * time.Millisecond)
	}

	if backend.claim == nil || backend.claimAttempt != nil {
		t.Fatalf("after taking the claim: claim %v, attempt %v", backend.claim, backend.claimAttempt)
	}

	var pid int64
	if err := backend.claim.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the claim's session: %v", err)
	}

	if pid != session {
		t.Errorf("the claim is on session %d, and the wait was on %d", pid, session)
	}
}
