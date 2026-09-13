package main

import (
	"encoding/json"
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

// retiredRequest is a converge over a host whose retirement is already
// recorded. It cannot use the fixture's ordinary request, which digests the
// installed configuration: a server-only retirement has removed that file, and
// the digest is not something this path reads anyway.
func retiredRequest(t *testing.T, f *requestFixture, run string) (string, int) {
	t.Helper()

	return f.run(t, f.input(t, nil), "--input", "-", "--run", run, "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor, "--server-only", "--installed-sha256", strings.Repeat("0", 64))
}

// takeOverTheGuard re-labels this converge's guard for a new holder, keeping
// the marker and recording the holder it took it from, which is what the
// guard's own takeover writes.
func takeOverTheGuard(t *testing.T, f *requestFixture, holder, from string) {
	t.Helper()

	rec := f.guard.record(t)
	rec.Holder = holder
	rec.TakenOverFrom = append(append([]string{}, rec.TakenOverFrom...), from)

	body, err := json.Marshal(rec)
	mustOK(t, err)
	mustOK(t, os.WriteFile(filepath.Join(f.guard.active(), guardRecordName), body, 0o600))
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

	out, code := retiredRequest(t, f, "ci-2")

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

	if held["status"] != retireStatusDone {
		t.Fatalf("the status the retirement published: %v", held)
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

			out, code := retiredRequest(t, f, requestRun)

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

	// The ledger is reachable again, and the guard has been TAKEN OVER: the
	// run that left the tail unfinished is gone, and its journal is still this
	// guard's through the chain.
	t.Setenv("BILLET_STATE_DSN", f.dsn)
	takeOverTheGuard(t, f, "ci-2", requestRun)

	out, code = retiredRequest(t, f, "ci-2")

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

	out, code := retiredRequest(t, f, requestRun)

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

	out, code := retiredRequest(t, f, requestRun)

	m := retireAnswer(t, out)
	if m["reason"] != retireReasonIdentity || code != exitUnknown {
		t.Fatalf("a retirement whose archive is gone: %s", out)
	}
}

// THE PUBLISHED STATUS IS WHAT CLOSES THE AUTHORITY TO EVERY ORDINARY WRITER,
// so a retired host whose status went missing is one the next `ca rotate`
// would be admitted on. A converge republishes it from the journal — which is
// the record — and says that it did.
func TestASettledRetirementRepublishesAStatusThatWentMissing(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	settleRetirement(t, f)
	retiredUnits(t, f)

	mustOK(t, os.Remove(retirement.StatusPath()))

	out, code := retiredRequest(t, f, requestRun)

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeUnchanged {
		t.Fatalf("a converge over a retired host whose status went missing: %s", out)
	}

	held, ok := m["postconditions"].(map[string]any)
	if !ok || held["status"] != retireStatusRepublished {
		t.Fatalf("the status was not republished: %s", out)
	}

	st, presence, err := retirement.ReadStatus()
	if err != nil || presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
		t.Fatalf("the status on disk: %+v %d %v", st, presence, err)
	}
}

// A PROPERTY SYSTEMD DID NOT ANSWER IS COULD-NOT-TELL, never a refusal: a
// refusal is a claim about what this host holds, and an empty answer
// establishes nothing. The two are different exit statuses to the role.
func TestASettledRetirementCannotTellFromAnAnswerSystemdDidNotGive(t *testing.T) {
	cases := map[string]string{
		"no active state": "LoadState=loaded\nActiveState=\nSubState=dead\nResult=success\nKillMode=mixed\n" +
			"MainPID=0\nUnitFileState=disabled\nInvocationID=\nExecMainStartTimestamp=\n",
		"no enablement": "LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\nKillMode=mixed\n" +
			"MainPID=0\nUnitFileState=\nInvocationID=\nExecMainStartTimestamp=\n",
		"a state this billet does not know": "LoadState=loaded\nActiveState=refurbishing\nSubState=dead\n" +
			"Result=success\nKillMode=mixed\nMainPID=0\nUnitFileState=disabled\nInvocationID=\n" +
			"ExecMainStartTimestamp=\n",
		"no main process answered": "LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\n" +
			"KillMode=mixed\nMainPID=\nUnitFileState=disabled\nInvocationID=\nExecMainStartTimestamp=\n",

		// A NEGATIVE MAIN PID IS MALFORMED EVIDENCE, NOT PROOF OF NO PROCESS.
		// `strconv.Atoi` reads `-1` happily, so a rule that refused only a
		// POSITIVE answer would read this inactive, disabled unit as quiesced
		// — and quiescence is the clause the archive's safety rests on.
		// systemd answers no unit's main pid that way.
		"a main process that is a negative number": "LoadState=loaded\nActiveState=inactive\nSubState=dead\n" +
			"Result=success\nKillMode=mixed\nMainPID=-1\nUnitFileState=disabled\nInvocationID=\n" +
			"ExecMainStartTimestamp=\n",
		"a main process that is not a number": "LoadState=loaded\nActiveState=inactive\nSubState=dead\n" +
			"Result=success\nKillMode=mixed\nMainPID=none\nUnitFileState=disabled\nInvocationID=\n" +
			"ExecMainStartTimestamp=\n",
	}

	for name, unit := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)

			settleRetirement(t, f)
			retiredUnits(t, f)
			writeFile(t, filepath.Join(f.unitsDir, serverUnit), unit, 0o644)

			out, code := retiredRequest(t, f, requestRun)

			m := retireAnswer(t, out)
			if m["reason"] != retireReasonPostcondition || code != exitUnknown {
				t.Fatalf("an answer systemd did not give: %s", out)
			}
		})
	}
}

// AN UNFINISHED TAIL IS NOT AN EXEMPTION FROM THE POSTCONDITIONS. Every phase
// of the transition has run by the time a journal reads `done`, so a host that
// has drifted from what the retirement left — a server started again, a timer
// re-enabled, a systemd that cannot be asked — is one an operator must look
// at, and settling the retirement over it would record as finished a
// retirement whose host no longer holds what it promised.
func TestAnUnsettledRetirementIsNotFinishedOverADriftedHost(t *testing.T) {
	cases := map[string]struct {
		unit string
		code int
	}{
		"the server is running again": {
			unit: "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nKillMode=mixed\n" +
				"MainPID=4242\nUnitFileState=disabled\nInvocationID=\nExecMainStartTimestamp=\n",
			code: exitRefused,
		},
		"systemd answered nothing for its state": {
			unit: "LoadState=loaded\nActiveState=\nSubState=dead\nResult=success\nKillMode=mixed\n" +
				"MainPID=0\nUnitFileState=disabled\nInvocationID=\nExecMainStartTimestamp=\n",
			code: exitUnknown,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)

			// The first run reaches `done` owing the ledger a row.
			saved := retireBeforeRename
			retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }

			t.Cleanup(func() { retireBeforeRename = saved })

			out, code := f.request(t, f.input(t, nil))
			if m := retireAnswer(t, out); code != 0 || m["settled"] != false {
				t.Fatalf("the first run: %s", out)
			}

			retiredUnits(t, f)
			writeFile(t, filepath.Join(f.unitsDir, serverUnit), c.unit, 0o644)

			// The ledger is reachable again, so nothing but the host's own
			// state stands in the way of settling.
			t.Setenv("BILLET_STATE_DSN", f.dsn)

			out, code = retiredRequest(t, f, requestRun)

			m := retireAnswer(t, out)
			if m["reason"] != retireReasonPostcondition || code != c.code {
				t.Fatalf("a drifted host with an unfinished tail: %s", out)
			}

			// AND THE TAIL WAS NOT FINISHED: the marker is kept, the journal
			// is not settled, and the ledger's row still waits.
			j, _, err := retirement.ReadJournal()
			mustOK(t, err)

			if j.Settled || j.RowDone {
				t.Fatalf("the tail was finished over a drifted host: %+v", j)
			}

			if f.guard.record(t).Transition == nil {
				t.Fatal("the marker was cleared over a drifted host")
			}

			f.pgLedger(t, func(db *state.DB) {
				r, present, err := db.ReadRetirement(t.Context(), f.identity)
				mustOK(t, err)

				if !present || r.State != state.RetirementIntent {
					t.Fatalf("the row moved: %+v (present %v)", r, present)
				}
			})
		})
	}
}

// THE TAIL CLEARS THE MARKER AND THEN WRITES `settled`, so a crash between the
// two leaves an acknowledged row, an unsettled journal and NO marker — which
// is the one window that ordering was chosen for, and the one a marker
// requirement would make unrecoverable. What still ties it to this converge is
// the journal's own ownership.
func TestARetirementInterruptedBetweenTheMarkerAndSettledIsFinished(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// THE JOURNAL'S LAST WRITE FAILS, after the marker has gone: `settled` is
	// the write the tail makes last, and this is the host it leaves.
	saved := retirement.Publishing
	failed := false

	retirement.Publishing = func(path string) error {
		if failed || filepath.Base(path) != "journal.json" {
			return nil
		}

		j, presence, err := retirement.ReadJournal()
		mustOK(t, err)

		// The settled write is the one made over an acknowledged row.
		if presence != retirement.JournalPresent || !j.RowDone || j.Settled {
			return nil
		}

		failed = true

		return os.ErrPermission
	}

	t.Cleanup(func() { retirement.Publishing = saved })

	out, code := f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); code != exitUnknown || m["reason"] != retireReasonJournal {
		t.Fatalf("the run that could not write `settled`: %s", out)
	}

	if !failed {
		t.Fatal("the settled write was never attempted, so this case proved nothing")
	}

	// THE HOST IS IN THE WINDOW: the row is acknowledged, the journal is not
	// settled, and the marker is gone.
	j, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if !j.RowDone || j.Settled {
		t.Fatalf("the journal is not in the window: %+v", j)
	}

	if f.guard.record(t).Transition != nil {
		t.Fatal("the marker was not cleared, so this is not the window")
	}

	retiredUnits(t, f)

	// AND THE NEXT CONVERGE FINISHES IT.
	out, code = retiredRequest(t, f, requestRun)

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeRetired || m["settled"] != true {
		t.Fatalf("the converge that found the window: %s", out)
	}

	j, _, err = retirement.ReadJournal()
	mustOK(t, err)

	if !j.Settled {
		t.Fatalf("the journal was not settled: %+v", j)
	}
}

// THE OTHER HALF OF THE SAME RULE, on the node a retirement KEPT: there the
// postcondition requires a process, so a negative pid read as a number would
// say the node is running when nothing established that. Zero is the one
// answer that means no process, and on this host it is a refusal rather than
// could-not-tell: the unit is active and systemd told us it has none.
func TestARetainedNodesMainProcessIsJudgedAsAProcessId(t *testing.T) {
	for name, c := range map[string]struct {
		pid  string
		code int
	}{
		"a negative number":   {pid: "-1", code: exitUnknown},
		"not a number at all": {pid: "none", code: exitUnknown},
		"no process":          {pid: "0", code: exitRefused},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)

			record := useRegistrationRecord(t)

			f.svc.onStart = func(unit string) {
				if unit == nodeUnit {
					restartedNode(t, f, record, retainedEndpoint)
				}
			}

			// THE RETIREMENT COMPLETES FIRST, with the node running under a
			// real pid: that is the baseline each case below is a drift FROM,
			// and a case that could not reach it would prove nothing.
			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			retiredAnswer(t, out, code)

			// THE STOPPED UNITS ARE WHAT THE FAKE SERVICE MANAGER DOES NOT
			// WRITE: it moves its own maps and leaves the property files a
			// fixture put there, so the server and the timers must be given
			// the answers a completed retirement leaves. The node keeps the
			// one the restart left it, which `retiredUnits` would flatten.
			node := mustRead(t, filepath.Join(f.unitsDir, nodeUnit))

			retiredUnits(t, f)
			writeFile(t, filepath.Join(f.unitsDir, nodeUnit), node, 0o644)

			// AND THE WHOLE HOST PASSES BEFORE THE DRIFT, with the node
			// reported running: without this the cases below could be refused
			// by some other unit and prove nothing about the pid.
			out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

			m := retireAnswer(t, out)
			if code != 0 || m["outcome"] != retireOutcomeUnchanged {
				t.Fatalf("the settled retirement did not pass before the drift: %s", out)
			}

			if asMap(m["postconditions"])["node"] != retireUnitRunning {
				t.Fatalf("the node was not reported running: %s", out)
			}

			setMainPID(t, filepath.Join(f.unitsDir, nodeUnit), c.pid)

			out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

			m = retireAnswer(t, out)
			if m["reason"] != retireReasonPostcondition || code != c.code {
				t.Fatalf("a retained node whose main pid is %q: %s", c.pid, out)
			}

			if !strings.Contains(whyOf(m), nodeUnit) {
				t.Fatalf("the answer does not name the unit: %s", out)
			}
		})
	}
}

// setMainPID rewrites a unit's answer for its main process, whatever it says
// now: the value a fixture left there is the fake service manager's business,
// and a case that matched a literal would pass vacuously the day it changed.
func setMainPID(t *testing.T, path, pid string) {
	t.Helper()

	lines := strings.Split(mustRead(t, path), "\n")
	found := false

	for i, line := range lines {
		if strings.HasPrefix(line, "MainPID=") {
			lines[i], found = "MainPID="+pid, true
		}
	}

	if !found {
		t.Fatalf("%s answers no main process to rewrite", path)
	}

	writeFile(t, path, strings.Join(lines, "\n"), 0o644)
}
