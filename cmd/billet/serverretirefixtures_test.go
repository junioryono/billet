package main

import (
	"os"
	"path/filepath"
	"strings"
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

// AN UNEXPLAINED ARTEFACT HOLDS ON ITS OWN. Neither fixture borrows a
// journal, a reservation or a marker to make the classifier refuse.
func TestTheRetireCallerFixturesClassifyUnexplainedArtefacts(t *testing.T) {
	for _, artefact := range []string{"status", "stage"} {
		t.Run(artefact, func(t *testing.T) {
			f := newRetireFixture(t)
			status, stage := "absent", "absent"
			if artefact == "status" {
				mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))
				status = "present"
			} else {
				mustOK(t, retirement.WriteStage([]byte("staged configuration")))
				stage = "present"
			}

			out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
			m := assertRetireRoute(t, out, code, "hold", "a retirement "+artefact)
			assertRetireRoute(t, out, code, "hold", "no journal to explain it")
			if m["journal"] != nil || m["marker"] != nil || m["row_fact"] != string(retirement.RowAbsent) ||
				m["status_presence"] != status || m["stage"] != stage {
				t.Fatalf("the artefact did not stand alone: %s", out)
			}
			expectRetire(t, out, code, "dry-run-hold-"+artefact+"-only", retireOutcomeReported, "")
		})
	}
}

// THE SURVIVOR'S ORDINARY CONVERGE MAY CROSS ANOTHER HOST'S RESERVATION;
// a request to retire that survivor must name the reservation it cannot take.
func TestTheRetireCallerFixturesClassifyAnotherHostsRow(t *testing.T) {
	for _, requested := range []bool{false, true} {
		name, route := "dry-run-other-host-row", "ordinary"
		if requested {
			name, route = "dry-run-other-host-row-requested", "hold"
		}
		t.Run(name, func(t *testing.T) {
			f := newRetireFixture(t)
			f.reserveRow(t, "ci-1")

			args := []string{"--dry-run", "--retiring-host", "control-b"}
			if requested {
				args = append(args, "--requested")
			}
			out, code := f.run(t, "", args...)
			m := assertRetireRoute(t, out, code, route, "another host (control-a)")
			row := asMap(m["row"])
			if m["row_fact"] != string(retirement.RowOtherReserved) || row["retiring"] != "control-a" ||
				row["state"] != state.RetirementReserved || m["journal"] != nil || m["marker"] != nil {
				t.Fatalf("the row was not another host's reservation: %s", out)
			}
			expectRetire(t, out, code, name, retireOutcomeReported, "")
		})
	}
}

// AN UNREADABLE ROW HAS TWO PROVED EXCEPTIONS: an installed node without a
// server, and a controller location with neither identity nor authority.
func TestTheRetireCallerFixturesClassifyUnreadableRowExceptions(t *testing.T) {
	t.Run("node-only", func(t *testing.T) {
		f := newRetireFixture(t)
		writeFile(t, f.cfg, "node:\n  name: node-a\n  server_addr: 127.0.0.1:7717\n  provider: docker\n"+
			"  state_dir: "+filepath.Join(t.TempDir(), "node")+"\n", 0o600)
		writeFile(t, state.LedgerPath(f.stateDir), "not a database", 0o600)

		out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
		m := assertRetireRoute(t, out, code, "ordinary", "")
		if m["config"] != "present" || m["installed_roles"] != "node" || m["identity"] != "unreadable" ||
			m["authority"] != "unreadable" || m["row_fact"] != string(retirement.RowUnreadable) || m["row"] != nil {
			t.Fatalf("the installed node did not establish the skipped controller observation: %s", out)
		}
		if why, ok := m["why"].(string); !ok || !strings.Contains(why, "was not read") {
			t.Fatalf("the skipped row observation was reported as a failed read: %s", out)
		}
		expectRetire(t, out, code, "dry-run-ordinary-node-only-unreadable-row", retireOutcomeReported, "")
	})

	t.Run("never-commissioned", func(t *testing.T) {
		f := newRetireFixture(t)
		missing := filepath.Join(retirement.Root, "server")
		f.cfg = writeRetirePostgresConfig(t, missing)

		out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
		m := assertRetireRoute(t, out, code, "ordinary", "")
		if m["config"] != "present" || m["installed_roles"] != "server" || m["identity"] != "absent" ||
			m["authority"] != "absent" || m["row_fact"] != string(retirement.RowUnreadable) || m["row"] != nil ||
			m["journal"] != nil || m["status_presence"] != "absent" || m["stage"] != "absent" || m["marker"] != nil {
			t.Fatalf("the host did not establish both absences without retirement artefacts: %s", out)
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Fatalf("the classifier commissioned the absent directory: %v", err)
		}
		expectRetire(t, out, code, "dry-run-ordinary-never-commissioned", retireOutcomeReported, "")
	})
}

// AN ABNORMAL CLAIM REACHES THE CLASSIFIER, which reports its own hold.
// No preparation command runs first and substitutes a guard refusal for it.
func TestTheRetireCallerFixtureClassifiesAnAbnormalClaim(t *testing.T) {
	f := newRetireFixture(t)
	mustOK(t, os.MkdirAll(f.guard.active(), 0o700))

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	m := assertRetireRoute(t, out, code, "hold", "claim is unpublished-guard")
	if m["guard"] != string(claimUnpublished) || m["journal"] != nil || m["marker"] != nil ||
		m["row_fact"] != string(retirement.RowAbsent) || m["status_presence"] != "absent" || m["stage"] != "absent" {
		t.Fatalf("the abnormal claim did not establish the hold: %s", out)
	}
	expectRetire(t, out, code, "dry-run-hold-abnormal-claim", retireOutcomeReported, "")
}

// A NEW REQUEST WAITS FOR ITS OWN GUARD'S CLEANUP WINDOW TO CLOSE.
func TestTheRetireCallerFixtureClassifiesAPreparingGuard(t *testing.T) {
	f := newRequestFixture(t)
	rec := f.guard.record(t)
	args := []string{"--dry-run", "--retiring-host", requestRetiring, "--requested",
		"--expected-holder", requestRun, "--expected-guard", rec.ID}

	out, code := f.run(t, "", args...)
	assertRetireRoute(t, out, code, "new-request", "eligible")

	rec.Preparing, rec.Token = true, strings.Repeat("a", 32)
	writeGuardRecordForTest(t, f.guard, rec)
	recordPath := filepath.Join(f.guard.active(), guardRecordName)
	before := mustRead(t, recordPath)

	out, code = f.run(t, "", args...)
	m := assertRetireRoute(t, out, code, "hold", "the guard is still preparing; settle it")
	if m["guard"] != string(claimGuard) || m["installed_roles"] != "server" ||
		m["row_fact"] != string(retirement.RowAbsent) || m["journal"] != nil || m["marker"] != nil ||
		m["status_presence"] != "absent" || m["stage"] != "absent" {
		t.Fatalf("the preparing guard did not hold an otherwise fresh request: %s", out)
	}
	if mustRead(t, recordPath) != before || !f.guard.record(t).Preparing {
		t.Fatal("the classifier settled or rewrote the preparing guard")
	}
	expectRetire(t, out, code, "dry-run-hold-preparing", retireOutcomeReported, "")
}

// A NEW REQUEST CANNOT REPAIR AN INTERRUPTED REWRITE OF ITS OWN GUARD.
func TestTheRetireCallerFixtureClassifiesAnInterruptedGuardRewrite(t *testing.T) {
	f := newRequestFixture(t)
	rec := f.guard.record(t)
	args := []string{"--dry-run", "--retiring-host", requestRetiring, "--requested",
		"--expected-holder", requestRun, "--expected-guard", rec.ID}

	out, code := f.run(t, "", args...)
	assertRetireRoute(t, out, code, "new-request", "eligible")

	temporary := filepath.Join(f.guard.active(), guardTmpName)
	writeFile(t, temporary, "interrupted guard record", 0o600)
	recordPath := filepath.Join(f.guard.active(), guardRecordName)
	before := mustRead(t, recordPath)

	out, code = f.run(t, "", args...)
	m := assertRetireRoute(t, out, code, "hold", "the guard carries an interrupted rewrite (guard.json.tmp)")
	if m["guard"] != string(claimGuard) || m["installed_roles"] != "server" ||
		m["row_fact"] != string(retirement.RowAbsent) || m["journal"] != nil || m["marker"] != nil ||
		m["status_presence"] != "absent" || m["stage"] != "absent" {
		t.Fatalf("the interrupted guard rewrite did not hold an otherwise fresh request: %s", out)
	}
	if mustRead(t, recordPath) != before || f.guard.record(t).Preparing ||
		mustRead(t, temporary) != "interrupted guard record" {
		t.Fatal("the classifier changed the guard or its interrupted rewrite")
	}
	expectRetire(t, out, code, "dry-run-hold-interrupted-rewrite", retireOutcomeReported, "")
}

// LATER PHASES ARE INDEPENDENT INTERRUPTIONS, with the row, marker and status
// the resume helpers establish. The archived host has actually moved its
// identity; the classifier reads that state through the command's own entry.
func TestTheRetireCallerFixturesClassifyLaterPhases(t *testing.T) {
	for _, phase := range []retirement.Phase{retirement.PhaseStopped, retirement.PhaseArchived} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)
			j := plantResumedRetirement(t, f, phase, retirement.VariantServerOnly)
			retiredUnits(t, f)
			if phase == retirement.PhaseArchived {
				mustOK(t, os.Rename(f.stateDir, j.Archive))
			}

			out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)
			m := assertRetireRoute(t, out, code, "continue", "readable journal")
			journal := asMap(m["journal"])
			if m["state"] != string(phase) || journal["phase"] != string(phase) ||
				journal["variant"] != string(retirement.VariantServerOnly) || journal["settled"] != false ||
				m["status_presence"] != "present" || asMap(m["status"])["phase"] != string(phase) {
				t.Fatalf("the host did not establish the later server-only phase: %s", out)
			}
			rowFact := retirement.RowIntentMine
			if phase == retirement.PhaseArchived {
				rowFact = retirement.RowUnreadable
			}
			if m["row_fact"] != string(rowFact) || asMap(m["marker"])["id"] != retireTestID {
				t.Fatalf("the interruption did not keep its row observation and marker: %s", out)
			}
			expectRetire(t, out, code, "dry-run-continue-"+string(phase), retireOutcomeReported, "")
		})
	}
}

// A RETAINED-NODE JOURNAL SELECTS CONTINUATION at every phase,
// including done after the real request has installed a node-only config.
func TestTheRetireCallerFixturesClassifyRetainedNodeJournals(t *testing.T) {
	for _, phase := range []retirement.Phase{retirement.PhaseIntent, retirement.PhaseArchived, retirement.PhaseDone} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			if phase == retirement.PhaseDone {
				out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
				retiredAnswer(t, out, code)
			} else {
				j := plantResumedRetirement(t, f, phase, retirement.VariantRetainedNode)
				if phase == retirement.PhaseArchived {
					mustOK(t, os.Rename(f.stateDir, j.Archive))
				}
			}

			out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)
			m := assertRetireRoute(t, out, code, "continue", "continues the retained-node retirement it records")
			journal := asMap(m["journal"])
			roles := "both"
			if phase == retirement.PhaseDone {
				roles = "node"
			}
			if m["state"] != string(phase) || journal["phase"] != string(phase) ||
				journal["variant"] != string(retirement.VariantRetainedNode) || m["installed_roles"] != roles ||
				journal["settled"] != (phase == retirement.PhaseDone) {
				t.Fatalf("the retained-node journal did not establish its phase and variant: %s", out)
			}
			expectRetire(t, out, code, "dry-run-continue-retained-"+string(phase), retireOutcomeReported, "")
		})
	}
}

// The answers come from resuming a retained intent through the real transition
// and tail, including a ledger that becomes unreachable after the archive.
func TestTheRetireCallerFixturesContinueRetainedNodeJournals(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "retired-retained-settled"
		if pending {
			name = "retired-retained-pending"
		}
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
			if pending {
				saved := retireBeforeRename
				retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }
				t.Cleanup(func() { retireBeforeRename = saved })
			}

			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			m := retireAnswer(t, out)
			row, marker := retireRowDone, retireMarkerCleared
			if pending {
				row, marker = retireRowPending, retireMarkerKept
			}
			if code != 0 || m["outcome"] != retireOutcomeRetired || m["state"] != string(retirement.PhaseDone) ||
				m["variant"] != string(retirement.VariantRetainedNode) || m["receipt"] != retireReceiptWritten ||
				m["row"] != row || m["marker"] != marker || m["settled"] != !pending {
				t.Fatalf("the retained continuation did not establish its tail outcome: %s", out)
			}
			j, presence, err := retirement.ReadJournal()
			mustOK(t, err)
			if presence != retirement.JournalPresent || j.Phase != retirement.PhaseDone ||
				j.RowDone != !pending || j.Settled != !pending || (f.guard.record(t).Transition != nil) != pending {
				t.Fatalf("the retained continuation's durable tail differs: %+v", j)
			}
			expectRetire(t, out, code, name, retireOutcomeRetired, "")
		})
	}
}

// DONE IS CLASSIFIED WHETHER ITS TAIL FINISHED OR STILL OWES THE ROW. The
// settled cases pair an absent publication with the closed one an incapable
// answerer's caller must veto before it can ask the classifier.
func TestTheRetireCallerFixturesClassifyDoneJournals(t *testing.T) {
	for _, name := range []string{"dry-run-continue-done-settled", "dry-run-continue-done-unsettled",
		"dry-run-settled-closed-status"} {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)
			settled := name != "dry-run-continue-done-unsettled"
			if settled {
				settleRetirement(t, f)
			} else {
				// THE RENAME'S HOOK REMOVES THE LOCATOR'S DSN, so the real
				// request reaches done with an unacknowledged row and marker.
				saved := retireBeforeRename
				retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }
				t.Cleanup(func() { retireBeforeRename = saved })
				out, code := f.request(t, f.input(t, nil))
				m := retireAnswer(t, out)
				if code != 0 || m["outcome"] != retireOutcomeRetired || m["row"] != retireRowPending || m["settled"] != false {
					t.Fatalf("the request did not leave an unfinished tail: %s", out)
				}
			}
			retiredUnits(t, f)
			status := "present"
			if name == "dry-run-continue-done-settled" {
				mustOK(t, os.Remove(retirement.StatusPath()))
				status = "absent"
			}

			out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)
			m := assertRetireRoute(t, out, code, "continue", "readable journal")
			journal := asMap(m["journal"])
			if m["state"] != string(retirement.PhaseDone) || journal["phase"] != string(retirement.PhaseDone) ||
				journal["variant"] != string(retirement.VariantServerOnly) || journal["settled"] != settled ||
				journal["row_done"] != settled || m["config"] != "absent" || m["status_presence"] != status {
				t.Fatalf("the host did not establish the done journal and its tail: %s", out)
			}
			if status == "present" && asMap(m["status"])["phase"] != string(retirement.PhaseDone) {
				t.Fatalf("the published status did not close the authority: %s", out)
			}
			if settled {
				if m["marker"] != nil || m["row_fact"] != string(retirement.RowDoneMine) {
					t.Fatalf("the settled tail did not complete its row and clear its marker: %s", out)
				}
			} else if asMap(m["marker"])["id"] != retireTestID || m["row_fact"] != string(retirement.RowUnreadable) {
				t.Fatalf("the unfinished tail did not keep its marker beside the unreadable row: %s", out)
			}
			expectRetire(t, out, code, name, retireOutcomeReported, "")
		})
	}
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
