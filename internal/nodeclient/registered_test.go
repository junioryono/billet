package nodeclient

import (
	"log/slog"
	"testing"
)

// A NEW CONTROL PLANE HEARS WHAT IT HAS MISSED AT EVERY REGISTRATION, or its ledger holds
// nothing about this host until something next moves.
func TestARegistrationIsSaid(t *testing.T) {
	t.Parallel()

	said := 0
	recordRegistration(&Client{}, slog.New(slog.DiscardHandler), LoopOptions{
		Registered: func() { said++ },
	})
	if said != 1 {
		t.Fatalf("a registration was said %d times", said)
	}
}
