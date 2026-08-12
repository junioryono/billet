package main

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/state"
)

// THE COMMANDS THEMSELVES MUST WORK WHILE THE CONTROL PLANE IS RUNNING.
//
// state.OpenAdmin having the right behaviour proves nothing about whether the
// commands were rewired onto it — and the wiring is the whole defect. Every
// command below reached the ledger through Open, which takes the exclusive
// directory lock the server holds for its entire life, so all of them failed
// against a live deployment with "another billet process holds this state
// directory".
//
// Driven through the cmd functions rather than through controlPlaneAllocator,
// because calling the helper directly would stay green with the production call
// sites reverted to Open, which is exactly the regression this guards.
func TestOperatorCommandsRunWhileTheControlPlaneIsRunning(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	// The control plane, holding the directory exactly as `billet server` does
	// for its whole life.
	server, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	// The guard that made this a defect rather than a preference: a second
	// control plane really is refused, and must stay refused.
	if _, err := state.Open(ctx, stateDir); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("a second control plane must still be refused, got: %v", err)
	}

	// A READ and a WRITE, because they fail for the same reason but a fix could
	// plausibly cover only one. `ca token` inserts a join token; `nodes pending`
	// only queries.
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"nodes pending", func() error { return cmdNodesPending(ctx, []string{"--config", cfg}) }},
		{"ca token", func() error { return cmdCAToken(ctx, []string{"--config", cfg}) }},
		{"ca revocations", func() error { return cmdCARevocations(ctx, []string{"--config", cfg}) }},
		{"leases quarantined", func() error { return cmdLeasesQuarantined(ctx, []string{"--config", cfg}) }},
	} {
		if err := tc.run(); err != nil {
			t.Errorf("billet %s against a running control plane: %v", tc.name, err)
		}
	}

	// AND THE WRITE LANDED WHERE THE SERVER CAN SEE IT. A command that opens,
	// reports success and writes somewhere else would satisfy every assertion
	// above.
	var tokens int

	if err := server.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM join_tokens`).Scan(&tokens); err != nil {
		t.Fatalf("count the join tokens the server can see: %v", err)
	}

	if tokens != 1 {
		t.Errorf("join_tokens = %d, want 1 minted by `billet ca token`", tokens)
	}
}
