package app

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// A CONTROL PLANE REFUSED AFTER ITS OPEN MIGRATED SAYS WHAT THE OPEN MOVED. The
// allocator is built after the ledger opens, so a config it refuses is found
// once the open has already created or moved the schema; the refusal alone
// would leave an operator going back to an older binary with no reason to
// expect that binary to be refused by the ledger.
func TestAControlPlaneRefusedAfterItsOpenSaysWhatTheOpenMigrated(t *testing.T) {
	t.Parallel()

	// A DIRECTORY NOTHING HAS OPENED, so the open migrates the whole schema, and no
	// vCPU ceiling, which the allocator refuses.
	cfg := &config.Config{Server: &config.ServerConfig{
		Listen: "127.0.0.1:0", IdentityDir: t.TempDir(), MaxMemory: 16 * config.GiB,
	}}

	host := Host{
		ServerAccess: noIdentityAccess, AuthorityLock: noIdentityAccess,
		Ready: func() error { return nil }, Status: func(string) error { return nil }, Out: io.Discard,
	}

	cp, err := OpenControlPlane(t.Context(), cfg, host, nil, ControlPlaneOptions{})
	if err == nil {
		_ = cp.Close()

		t.Fatal("a control plane with no vCPU ceiling opened, so this test stages nothing")
	}

	if !strings.Contains(err.Error(), "capacity allocator:") {
		t.Fatalf("the open was meant to be refused by the allocator and returned %v", err)
	}

	moved, ok := errors.AsType[*state.SchemaMoved](err)
	if !ok || moved.From != 0 || moved.To != state.LatestSchemaVersion() {
		t.Errorf("a control plane refused after its open does not say what the open migrated: %v", err)
	}
}
