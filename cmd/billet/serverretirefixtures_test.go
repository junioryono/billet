package main

import (
	"os"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// THE CALLER'S ROUTES COME FROM INDEPENDENT HOSTS. A fixture is the command's
// answer over established evidence, never a report assembled for the parser.
func TestTheRetireCallerFixturesClassifyIndependentHosts(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		f := newRetireFixture(t)

		out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
		m := assertRetireRoute(t, out, code, "ordinary", "")
		if m["identity"] != "minted" || m["row_fact"] != string(retirement.RowAbsent) || m["journal"] != nil {
			t.Fatalf("the fixture is not an ordinary commissioned controller: %s", out)
		}
		expectRetire(t, out, code, "dry-run-ordinary", retireOutcomeReported, "")
	})

	t.Run("hold-unreadable-row", func(t *testing.T) {
		f := newRetireFixture(t)
		writeFile(t, state.LedgerPath(f.stateDir), "not a database", 0o600)

		out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
		m := assertRetireRoute(t, out, code, "hold", "row is unreadable")
		if m["identity"] != "minted" || m["row_fact"] != string(retirement.RowUnreadable) || m["journal"] != nil {
			t.Fatalf("the unreadable row did not establish the hold: %s", out)
		}
		expectRetire(t, out, code, "dry-run-hold-unreadable-row", retireOutcomeReported, "")
	})

	t.Run("hold-damaged-identity", func(t *testing.T) {
		f := newRetireFixture(t)
		f.cfg = writeRetirePostgresConfig(t, f.stateDir)
		mustOK(t, os.Remove(state.DeploymentIDPath(f.stateDir)))
		mustOK(t, os.Mkdir(wirecert.CADir(f.stateDir), 0o700))

		out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
		m := assertRetireRoute(t, out, code, "hold", "identity is absent beside authority remnants")
		if m["identity"] != "absent" || m["authority"] != "present" || m["row_fact"] != string(retirement.RowUnreadable) {
			t.Fatalf("the authority remnants did not establish the damaged identity: %s", out)
		}
		expectRetire(t, out, code, "dry-run-hold-damaged-identity", retireOutcomeReported, "")
	})

	t.Run("continue", func(t *testing.T) {
		f := newRetireFixture(t)
		f.journalAt(t, retirement.PhaseIntent, "ci-1")

		out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
		assertRetireRoute(t, out, code, "continue", "readable journal")
		expectRetire(t, out, code, "dry-run-continue", retireOutcomeReported, "")
	})
}

// REQUEST AND CANCELLATION NEED A REAL POSTGRESQL PAIR. A reservation planted
// in SQLite is readable but cannot be adopted under that configuration.
func TestTheRetireCallerFixturesClassifyPostgresPairs(t *testing.T) {
	t.Run("new-request", func(t *testing.T) {
		f := newRequestFixture(t)

		out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring, "--requested")
		m := assertRetireRoute(t, out, code, "new-request", "eligible")
		if m["row_fact"] != string(retirement.RowAbsent) || m["installed_roles"] != "server" {
			t.Fatalf("the fixture did not establish a fresh server-only request: %s", out)
		}
		expectRetire(t, out, code, "dry-run-new-request", retireOutcomeReported, "")
	})

	t.Run("cancel", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)
		m := assertRetireRoute(t, out, code, "cancel", "inventory no longer requests")
		if m["row_fact"] != string(retirement.RowReservedMine) || m["journal"] != nil || m["marker"] != nil {
			t.Fatalf("the fixture did not establish this host's unstarted reservation: %s", out)
		}
		expectRetire(t, out, code, "dry-run-cancel", retireOutcomeReported, "")
	})

	t.Run("unsupported-variant", func(t *testing.T) {
		f := newRequestFixture(t)
		f.retainANode(t)

		out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring, "--requested")
		m := assertRetireRoute(t, out, code, "unsupported-variant", "keeps a node")
		if m["installed_roles"] != "both" || m["journal"] != nil || m["row_fact"] != string(retirement.RowAbsent) {
			t.Fatalf("the installed configuration did not establish a fresh retained-node request: %s", out)
		}
		expectRetire(t, out, code, "dry-run-unsupported-variant", retireOutcomeReported, "")
	})
}

// BOTH CLAIM PROTOCOLS MUST REACH RECOVERY ACROSS THE REAL MAINTENANCE FENCE.
func TestTheRetireCallerFixturesClassifyBinaryRecovery(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name, claim := "dry-run-recovery-guard", string(claimGuard)
		if legacy {
			name, claim = "dry-run-recovery-legacy", string(claimLegacyRole)
		}
		t.Run(name, func(t *testing.T) {
			f, _ := retireBinaryRecoveryFixture(t, legacy)
			created, err := state.WriteMaintenanceFence(f.stateDir, "ansible host upgrade")
			mustOK(t, err)
			if !created {
				t.Fatal("the fixture did not create its maintenance fence")
			}

			args := []string{"--dry-run", "--retiring-host", "control-a"}
			if !legacy {
				args = append(args, "--expected-holder", "ci-1", "--expected-guard", f.guard.record(t).ID)
			}
			out, code := f.run(t, "", args...)
			m := assertRetireRoute(t, out, code, "recovery", "recover that transaction alone, then retry the classifier")
			if m["guard"] != claim || m["identity"] != "minted" || m["authority"] != "present" ||
				m["row_fact"] != string(retirement.RowUnreadable) || m["journal"] != nil {
				t.Fatalf("the claim and the fenced controller did not establish recovery: %s", out)
			}
			expectRetire(t, out, code, name, retireOutcomeReported, "")
		})
	}
}
