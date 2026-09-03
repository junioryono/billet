package state

import (
	"database/sql"
	"errors"
	"testing"
)

// THE PROBE PROVES WHAT A STANDBY PROVES, CROSSES THE FENCE, AND CHANGES NOTHING.
//
// A candidate probing an external ledger may verify the schema is one it knows
// and not ahead of it, and may be refused as a downgrade; it may not write, may
// not claim, may not migrate and may not move the release watermark, because
// the migration is the controller claim's right and happens when the candidate
// serves. Each of those is asserted against a real server, since every one of
// them is a fact about what the handle does rather than what it says.
func TestThePostgresProbeVerifiesAndChangesNothing(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := t.Context()

	// A CONTROL PLANE SERVED THIS LEDGER ON v0.5.0, which migrated it and
	// recorded the watermark.
	plane, err := OpenPostgres(ctx, t.TempDir(), dsn, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := plane.ClaimController(ctx, "controller", "deployment-a"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// THE TRANSACTION MAY HAVE FENCED THIS HOST'S IDENTITY DIRECTORY, and the
	// probe is the one handle entitled to cross that.
	dir := t.TempDir()

	if _, err := WriteMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatalf("WriteMaintenanceFence: %v", err)
	}

	if _, err := OpenPostgresAdmin(ctx, dir, dsn, WithRunningRelease("v0.6.0")); !errors.Is(err,
		ErrMaintenance) {
		t.Fatalf("an operator command crossed the fence: err = %v", err)
	}

	probe, err := OpenPostgresProbe(ctx, dir, dsn, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("OpenPostgresProbe as a newer candidate: %v", err)
	}

	t.Cleanup(func() { _ = probe.Close() })

	// IT CANNOT WRITE, STRUCTURALLY.
	if err := probe.Tx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, ErrStandby) {
		t.Errorf("the probe was allowed a write transaction: err = %v", err)
	}

	// AND IT RECORDED NOTHING: the watermark is the one the control plane left.
	release, _, err := probe.ReleaseWatermark(ctx)
	if err != nil {
		t.Fatalf("ReleaseWatermark: %v", err)
	}

	if release != "v0.5.0" {
		t.Errorf("the probe moved the watermark to %q; only a serving control plane may", release)
	}

	// AN OLDER CANDIDATE IS REFUSED AS A DOWNGRADE, fence or no fence.
	if _, err := OpenPostgresProbe(ctx, dir, dsn, WithRunningRelease("v0.4.9")); !errors.Is(err,
		ErrReleaseBehind) {
		t.Errorf("an older candidate probed a ledger a newer release served: err = %v", err)
	}
}

// A PROBE ON A LEDGER NOBODY HAS MIGRATED APPLIES NOTHING. A fresh deployment's
// first upgrade, before its first controller has run, must not have the
// candidate's probe create the schema: that is the claim's right, and a probe
// that migrated would be a second writer of the schema on a shared database.
func TestThePostgresProbeMigratesNothing(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := t.Context()

	probe, err := OpenPostgresProbe(ctx, t.TempDir(), dsn, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("OpenPostgresProbe on an unmigrated ledger: %v", err)
	}

	t.Cleanup(func() { _ = probe.Close() })

	exists, err := probe.backend.bookkeepingTableExists(ctx, probe.Reader())
	if err != nil {
		t.Fatalf("bookkeepingTableExists: %v", err)
	}

	if exists {
		t.Fatal("the probe created the schema on a ledger no controller had migrated")
	}
}

// AN OPERATOR HANDLE LOWERS THE MARK ON AN EXTERNAL LEDGER, because the
// maintenance open there is a read-only probe and the host transaction has no
// snapshot to lower it after; and it does so beside a controller that still
// holds the exclusion, which is the active-passive shape.
func TestAnAdminHandleLowersTheWatermarkOnAnExternalLedger(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := t.Context()

	plane, err := OpenPostgres(ctx, t.TempDir(), dsn, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })

	if _, err := plane.ClaimController(ctx, "controller", "deployment-a"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	admin, err := OpenPostgresAdmin(ctx, t.TempDir(), dsn, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenPostgresAdmin beside the controller: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if err := admin.SetReleaseWatermark(ctx, "v0.4.0"); err != nil {
		t.Fatalf("an operator handle could not lower the mark on an external ledger: %v", err)
	}

	release, _, err := admin.ReleaseWatermark(ctx)
	if err != nil {
		t.Fatalf("ReleaseWatermark: %v", err)
	}

	if release != "v0.4.0" {
		t.Fatalf("the mark is %q after lowering, want v0.4.0", release)
	}

	// AND THE OLDER CANDIDATE'S PROBE NOW PASSES.
	probe, err := OpenPostgresProbe(ctx, t.TempDir(), dsn, WithRunningRelease("v0.4.0"))
	if err != nil {
		t.Fatalf("the older candidate was still refused after the mark was lowered: %v", err)
	}

	_ = probe.Close()
}
