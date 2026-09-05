package hostupgrade

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/junioryono/billet/internal/state"
)

// fencingHost is a fakeHost whose fence is the real one.
//
// THE FAKE IGNORED THE REASON, and that is how a fence written as "host upgrade"
// and cleared as "host upgrade committed" passed every test in this package while
// cordoning every real host at commit (the rollout rehearsal, 2026-09-05). The
// ledger's own rule is that a fence is cleared only under the reason it was
// written with, so the only test that can catch a mismatch is one that lets the
// ledger judge it.
type fencingHost struct {
	*fakeHost
	dir string
}

func (h *fencingHost) Fence(ctx context.Context, reason string) error {
	if _, err := state.WriteMaintenanceFence(h.dir, reason); err != nil {
		return err
	}

	return h.fakeHost.Fence(ctx, reason)
}

func (h *fencingHost) ClearFence(ctx context.Context, reason string) error {
	if err := state.ClearMaintenanceFence(h.dir, reason); err != nil {
		return err
	}

	return h.fakeHost.ClearFence(ctx, reason)
}

func fenceStands(t *testing.T, dir string) bool {
	t.Helper()

	_, err := os.Stat(state.MaintenanceFencePath(dir))

	switch {
	case err == nil:
		return true
	case os.IsNotExist(err):
		return false
	default:
		t.Fatalf("could not tell whether the fence stands: %v", err)

		return false
	}
}

// A COMMITTED UPGRADE LEAVES NO FENCE. The ledger it fenced is the ledger the new
// services open, and a fence the commit could not clear is a control plane that
// starts refusing every write.
func TestACommittedUpgradeClearsTheFenceItRaised(t *testing.T) {
	dir := t.TempDir()
	h := &fencingHost{fakeHost: &fakeHost{}, dir: dir}

	if err := Run(t.Context(), Request{Journal: newJournal(t), Host: h, Log: quiet()}); err != nil {
		t.Fatalf("a healthy upgrade failed: %v", err)
	}

	if fenceStands(t, dir) {
		t.Fatal("the upgrade committed and left the ledger fenced")
	}
}

// A ROLLED-BACK UPGRADE LEAVES NO FENCE EITHER, and a rollback that cannot clear
// it cordons the host with its services stopped, which is the state the rehearsal
// controller was found in.
func TestARolledBackUpgradeClearsTheFenceItRaised(t *testing.T) {
	dir := t.TempDir()
	h := &fencingHost{fakeHost: &fakeHost{failAt: "probe"}, dir: dir}

	err := Run(t.Context(), Request{Journal: newJournal(t), Host: h, Log: quiet()})
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a failed probe did not roll back cleanly: %v", err)
	}

	if fenceStands(t, dir) {
		t.Fatal("the upgrade rolled back and left the ledger fenced")
	}
}
