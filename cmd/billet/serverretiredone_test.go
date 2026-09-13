package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// settleRetirement carries this fixture's host to a settled `done`: the
// transition runs, its tail completes the row, clears the marker and writes
// `settled`, and what is left is the host every later converge meets.
func settleRetirement(t *testing.T, f *requestFixture) retirement.Journal {
	t.Helper()

	out, code := f.request(t, f.input(t, nil))
	retiredAnswer(t, out, code)

	j, presence, err := retirement.ReadJournal()
	mustOK(t, err)

	if presence != retirement.JournalPresent || !j.Settled {
		t.Fatalf("the retirement did not settle: %+v %d", j, presence)
	}

	return j
}

// retiredUnits is what a completed server-only retirement leaves systemd
// holding: both services and both timers stopped and disabled, the backup
// service with no process.
func retiredUnits(t *testing.T, f *requestFixture) {
	t.Helper()

	for _, unit := range []string{serverUnit, nodeUnit, upgradeTimerUnit, backupTimerUnit} {
		writeFile(t, filepath.Join(f.unitsDir, unit), "LoadState=loaded\nActiveState=inactive\nSubState=dead\n"+
			"Result=success\nKillMode=mixed\nMainPID=0\nUnitFileState=disabled\nInvocationID=\n"+
			"ExecMainStartTimestamp=\n", 0o644)
	}

	writeFile(t, filepath.Join(f.unitsDir, backupServiceUnit), "LoadState=not-found\nActiveState=inactive\n"+
		"SubState=dead\nResult=success\nKillMode=control-group\nMainPID=0\nUnitFileState=\nInvocationID=\n"+
		"StateChangeTimestamp=\n", 0o644)
}

// EVERY LATER CONVERGE OF A RETIRED HOST MEETS A SETTLED `done` JOURNAL, and
// must answer from the journal alone: the identity is at the archive, the
// configuration of a server-only host is gone, and there is nothing to take
// and nothing to write.
func TestASettledRetirementIsUnchangedAndTakesNothing(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	settleRetirement(t, f)
	retiredUnits(t, f)

	// A FRESH CONVERGE UNDER A NEW HOLDER: the marker is gone, so nothing ties
	// this journal to the run that wrote it, and a settled journal is read by
	// whoever holds the guard now.
	mustOK(t, guardRun(t, "release", "--holder", requestRun))
	mustHold(t, "ci-2")

	// THE LEDGER GOES OUT OF REACH, which must not matter: a settled journal
	// needs nothing from the database again.
	t.Setenv("BILLET_STATE_DSN", "postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable")

	out, code := f.run(t, f.input(t, nil), "--input", "-", "--run", "ci-2", "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor, "--server-only", "--installed-sha256", strings.Repeat("0", 64))

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeUnchanged || m["settled"] != true {
		t.Fatalf("a converge over a settled retirement: %s", out)
	}

	if m["state"] != string(retirement.PhaseDone) || m["row_done"] != true {
		t.Fatalf("the answer does not describe the host: %s", out)
	}

	held, ok := m["postconditions"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no postconditions: %s", out)
	}

	if held["config"] != retireConfigAbsent || held["archive"] != retireArchivePresent ||
		held["server"] != retireUnitQuiet || held["backup_service"] != retireUnitNotFound {
		t.Fatalf("the postconditions: %v", held)
	}

	// THE COMMITTED FIXTURE FOR THIS ANSWER ARRIVES WITH THE ROLE that reads
	// it: a producer's fixture is written by running its producer, and the
	// role's parser is what gives it a reader.
}

// A POSTCONDITION THAT DOES NOT HOLD IS NAMED, and the converge stops there:
// the retirement is what promised these, and a host that has drifted from them
// is one an operator must look at rather than one to converge over.
func TestASettledRetirementRefusesEachPostconditionThatFails(t *testing.T) {
	cases := map[string]struct {
		stage func(t *testing.T, f *requestFixture)
		names string
	}{
		"the server is running again": {
			stage: func(t *testing.T, f *requestFixture) {
				t.Helper()
				writeFile(t, filepath.Join(f.unitsDir, serverUnit), "LoadState=loaded\nActiveState=active\n"+
					"SubState=running\nResult=success\nKillMode=mixed\nMainPID=4242\nUnitFileState=disabled\n"+
					"InvocationID=\nExecMainStartTimestamp=\n", 0o644)
			},
			names: serverUnit,
		},
		"the upgrade timer was enabled again": {
			stage: func(t *testing.T, f *requestFixture) {
				t.Helper()
				writeFile(t, filepath.Join(f.unitsDir, upgradeTimerUnit), "LoadState=loaded\nActiveState=inactive\n"+
					"SubState=dead\nResult=success\nKillMode=mixed\nMainPID=0\nUnitFileState=enabled\n"+
					"InvocationID=\nExecMainStartTimestamp=\n", 0o644)
			},
			names: upgradeTimerUnit,
		},
		// `masked-runtime` hides persistent enablement and is gone at the next
		// boot, so it is admitted for nothing.
		"the node unit is masked only at runtime": {
			stage: func(t *testing.T, f *requestFixture) {
				t.Helper()
				writeFile(t, filepath.Join(f.unitsDir, nodeUnit), "LoadState=loaded\nActiveState=inactive\n"+
					"SubState=dead\nResult=success\nKillMode=mixed\nMainPID=0\nUnitFileState=masked-runtime\n"+
					"InvocationID=\nExecMainStartTimestamp=\n", 0o644)
			},
			names: nodeUnit,
		},
		"the backup service is inactive with a process": {
			stage: func(t *testing.T, f *requestFixture) {
				t.Helper()
				writeFile(t, filepath.Join(f.unitsDir, backupServiceUnit), "LoadState=loaded\nActiveState=inactive\n"+
					"SubState=dead\nResult=success\nKillMode=control-group\nMainPID=91\nUnitFileState=disabled\n"+
					"InvocationID=\nStateChangeTimestamp=\n", 0o644)
			},
			names: backupServiceUnit,
		},
		"a configuration is installed on a host that kept no node": {
			stage: func(t *testing.T, f *requestFixture) {
				t.Helper()
				writeFile(t, f.cfg, "node:\n  name: node-a\n", 0o600)
			},
			names: "configuration is installed",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)

			settleRetirement(t, f)
			retiredUnits(t, f)
			c.stage(t, f)

			out, code := f.request(t, f.input(t, nil))

			m := retireAnswer(t, out)
			if m["reason"] != retireReasonPostcondition || code != exitRefused {
				t.Fatalf("a drifted postcondition: %s", out)
			}

			if !strings.Contains(whyOf(m), c.names) {
				t.Fatalf("the refusal does not name what failed: %s", out)
			}

			if m["state"] != string(retirement.PhaseDone) {
				t.Fatalf("the refusal does not say what the host holds: %s", out)
			}
		})
	}
}

// AN UNSETTLED `done` JOURNAL IS A TAIL SOMEBODY LEFT, and the converge that
// finds it finishes it — including one under a holder that took the guard
// over, which is the crash window between the marker's clearing and `settled`
// seen from the other side.
func TestAnUnsettledRetirementIsFinishedByTheNextConverge(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// THE ROW GOES OUT OF REACH FOR THE FIRST RUN ONLY, so it reaches `done`
	// owing the ledger a row, with its marker kept.
	saved := retireBeforeRename
	retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }

	t.Cleanup(func() { retireBeforeRename = saved })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != 0 || m["row"] != retireRowPending || m["settled"] != false {
		t.Fatalf("the first run: %s", out)
	}

	retiredUnits(t, f)

	// The ledger is reachable again, and the guard is this converge's.
	t.Setenv("BILLET_STATE_DSN", f.dsn)

	out, code = f.request(t, f.input(t, nil))

	m = retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeRetired || m["row"] != retireRowDone {
		t.Fatalf("the converge that finished the tail: %s", out)
	}

	if m["marker"] != retireMarkerCleared || m["settled"] != true {
		t.Fatalf("the tail was not finished: %s", out)
	}

	// AND THE ROW THE SURVIVOR READS IS DONE.
	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementDone {
			t.Fatalf("the row: %+v (present %v)", r, present)
		}
	})
}

// A `done` JOURNAL THIS CONVERGE'S GUARD DOES NOT CARRY THE MARKER FOR is not
// this run's tail to finish: the marker is what keeps a transition's guard from
// being taken away, and a run without it could be a second converge racing the
// one that owns the retirement.
func TestAnUnsettledRetirementRefusesAHolderThatIsNotItsOwn(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	saved := retireBeforeRename
	retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }

	t.Cleanup(func() { retireBeforeRename = saved })

	out, _ := f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["settled"] != false {
		t.Fatalf("the first run settled: %s", out)
	}

	t.Setenv("BILLET_STATE_DSN", f.dsn)

	// THE MARKER GOES, which is the one thing that ties the journal to this
	// converge's guard.
	markGuard(t, f.guard, nil, nil)

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if m["reason"] != retireReasonJournal || code != exitUnknown {
		t.Fatalf("a tail under a guard that does not carry its marker: %s", out)
	}

	if m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("the refusal does not say what the host holds: %s", out)
	}

	// AND NOTHING WAS FINISHED.
	j, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if j.Settled || j.RowDone {
		t.Fatalf("the refused run finished the tail anyway: %+v", j)
	}
}

// A `done` JOURNAL WHOSE ARCHIVE IS GONE is could-not-tell: the identity it
// names is what says the journal describes this deployment, and without it
// there is nothing to hold the answer to.
func TestARetirementWhoseArchiveIsGoneIsUnknown(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	j := settleRetirement(t, f)
	retiredUnits(t, f)

	mustOK(t, os.RemoveAll(j.Archive))

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if m["reason"] != retireReasonIdentity || code != exitUnknown {
		t.Fatalf("a retirement whose archive is gone: %s", out)
	}
}
