package state

import (
	"os"
	"testing"
	"time"
)

// TestMain gives every open in this package's tests a startup budget no loaded
// machine reaches.
//
// THE BUDGET GUARDS A SICK DISK, NOT A SLOW ONE, and its thirty seconds went to
// migrating a fresh ledger under a loaded local gate: "apply migration 49 ... did
// not finish inside the 30s startup budget" failed tests here and in every package
// that migrates its own ledgers (2026-09 to 2026-10, #188). Nearly every test in
// this package opens a fresh ledger and none of them is about how long the budget
// is. The ones that exercise the budget set it themselves
// (TestAnOpenThatTimesOutOnItsOwnBudgetSaysSo) or hand the open a shorter deadline
// of their own, which the open keeps, since WithTimeout never extends a parent.
// Set before any test runs, so no parallel test can read it mid-write.
func TestMain(m *testing.M) {
	startupTimeout = 10 * time.Minute

	os.Exit(m.Run())
}
