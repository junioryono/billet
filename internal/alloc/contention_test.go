package alloc

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// EVERY READ-ONLY OPERATION MUST STAY OFF THE WRITER SLOT.
//
// Write transactions begin IMMEDIATE, so anything routed through DB.Tx takes
// SQLite's single writer slot for its whole duration. That is correct for a
// decision and wrong for a question: a read on Tx queues behind the control
// plane, and holds the slot against it while scanning.
//
// This matters well beyond the operator CLI. CertRevokedFor runs on EVERY
// authenticated node request and Headroom on every escrow refill, so leaving
// them on Tx made ordinary node traffic contend with scheduling.
//
// DRIVEN THROUGH THE PUBLIC METHODS, not through DB.View. A test that called
// View directly would pass with every one of these reverted to Tx, which is
// exactly the regression it exists to catch — and there are eighteen of them, so
// "someone would notice" is not a guard.
func TestEveryReadOnlyOperationRunsWhileTheWriterSlotIsHeld(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	// The control plane owns the directory, as it does for its whole life.
	server, err := state.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	adminDB, err := state.OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("state.OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = adminDB.Close() })

	tiers := []config.Tier{tier("small", 4, 16*config.GiB)}

	a, err := New(adminDB, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Seeded BEFORE the writer slot is taken, so the reads below have rows to
	// find and this test is not quietly asserting things about an empty database.
	if _, err := a.RegisterNode(ctx, testRegistration("epyc-1", config.ProviderFirecracker)); err != nil {
		t.Fatalf("register a host: %v", err)
	}

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("reserve a lease: %v", err)
	}

	// The control plane is now mid-decision and holds the writer slot until this
	// test lets go.
	holding := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)

	go func() {
		held <- server.Tx(ctx, func(*sql.Tx) error {
			close(holding)
			<-release

			return nil
		})
	}()

	select {
	case <-holding:
	case err := <-held:
		t.Fatalf("the server's transaction ended before it took the slot: %v", err)
	}

	defer func() {
		close(release)

		if err := <-held; err != nil {
			t.Errorf("the server's transaction: %v", err)
		}
	}()

	// BOUNDED, so an operation that DOES need the writer slot fails here rather
	// than hanging the suite. Generous, so a slow machine cannot fail it for the
	// wrong reason.
	read, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"Headroom", func() error { _, err := a.Headroom(read, "small"); return err }},
		{"Usage", func() error { _, err := a.Usage(read); return err }},
		{"Lease", func() error { _, err := a.Lease(read, lease.ID); return err }},
		{"LaunchedLeaseIDs", func() error { _, err := a.LaunchedLeaseIDs(read, "epyc-1"); return err }},
		{"QuarantinedLeaseIDs", func() error { _, err := a.QuarantinedLeaseIDs(read, "epyc-1"); return err }},
		{"Quarantined", func() error { _, err := a.Quarantined(read); return err }},
		{"Stranded", func() error { _, err := a.Stranded(read, []string{lease.ID}); return err }},
		{"CertRevoked", func() error { _, err := a.CertRevoked(read, "serial"); return err }},
		{"CertRevokedFor", func() error {
			_, err := a.CertRevokedFor(read, "epyc-1", "serial", time.Now())

			return err
		}},
		{"RevokedCerts", func() error { _, err := a.RevokedCerts(read); return err }},
		{"LiveCertsFor", func() error { _, err := a.LiveCertsFor(read, "epyc-1"); return err }},
		{"Enrollments", func() error { _, err := a.Enrollments(read, EnrollPending); return err }},
		{"LookupEnrollment", func() error { _, _, err := a.LookupEnrollment(read, "epyc-1"); return err }},
		{"JoinTokens", func() error { _, err := a.JoinTokens(read); return err }},
		{"HistoryOutcomesForRequest", func() error { _, err := a.HistoryOutcomesForRequest(read, 1); return err }},
		{"HistoryOutcome", func() error { _, err := a.HistoryOutcome(read, lease.ID); return err }},
		// A NODE THE LEDGER HAS NEVER HEARD OF, so Reconcile returns straight
		// after its epoch READ and never reaches the write behind it. That is
		// what makes it a fair test of the read: with the lookup reverted to a
		// write transaction it contends before answering.
		{"Reconcile", func() error { _, err := a.Reconcile(read, "no-such-host", nil); return err }},
	} {
		err := tc.run()

		// A DOMAIN ANSWER IS FINE — HistoryOutcome for an unarchived lease says
		// so, and that is not what is being measured. What must never happen is
		// waiting for a slot this operation has no business needing.
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			t.Errorf("%s waited for the write lock and timed out", tc.name)
		case err != nil && contended(err):
			t.Errorf("%s needed the write lock: %v", tc.name, err)
		}
	}
}

// contended reports whether an error is SQLite refusing for want of a lock,
// rather than an ordinary domain answer.
func contended(err error) bool {
	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "busy") ||
		strings.Contains(msg, "run it again")
}
