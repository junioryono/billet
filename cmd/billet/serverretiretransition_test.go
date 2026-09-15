package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// THE TRANSITION AFTER INTENT, driven by the phase table: what each phase does
// on the host, what it writes, what it does under which lock, and what a resume
// makes of every state an interruption can leave.

// transitionArchive is where a journal written now says the identity goes,
// spelled as the request spells it.
func transitionArchive(t *testing.T) string {
	t.Helper()

	return filepath.Join(retirement.RetiredDir(), "identity-"+retireNow().UTC().Format("2006-01-02T15:04:05Z"))
}

// archivedIdentity answers the one archive under the retirement directory.
func archivedIdentity(t *testing.T) string {
	t.Helper()

	found, err := filepath.Glob(filepath.Join(retirement.RetiredDir(), "identity-*"))
	mustOK(t, err)

	if len(found) != 1 {
		t.Fatalf("the retirement directory holds %d archives: %v", len(found), found)
	}

	return found[0]
}

// plantJournal writes a journal for this fixture at the given phase, with the
// members the variant requires, and answers it.
func (f *requestFixture) plantJournal(t *testing.T, phase retirement.Phase, variant retirement.Variant) retirement.Journal {
	t.Helper()

	j := retirement.Journal{
		Phase: phase, Variant: variant, Deployment: f.identity, Retiring: requestRetiring,
		Survivor:    retirement.JournalSurvivor{Host: requestSurvivor, Deployment: f.identity},
		Backend:     "postgres",
		Controllers: "active-passive",
		IdentityDir: f.stateDir, Archive: transitionArchive(t),
		InstalledSHA256: f.installedSHA(t), Config: "absent",
		Locator: retirement.JournalLocator{Backend: "postgres", DSNEnv: "BILLET_STATE_DSN",
			IdentityDir: f.stateDir, Archive: transitionArchive(t)},
		Provenance: retirement.Provenance{ReservingHolder: requestRun, TransitionID: retireTestID,
			Reservation: retireNow().UTC().Format(time.RFC3339Nano), Deployment: f.identity, Retiring: requestRetiring,
			Survivor: requestSurvivor},
		Ownership: retirement.Ownership{Owner: requestRun},
	}

	if variant == retirement.VariantRetainedNode {
		body := []byte(f.rendering(t))
		mustOK(t, retirement.WriteStage(body))

		j.StagedSHA256, j.Config = retirement.Digest(body), "present"
		j.RetainedInvocation = f.originalNode
	}

	// THE TIMERS' STOP IS RECORDED FROM INTENT ON, which is where the window a
	// refused backup lands in begins.
	j.TimerStoppedAt = retireNow().UTC().Format(time.RFC3339Nano)

	f.pgLedger(t, func(db *state.DB) {
		row, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if present {
			j.Provenance.Reservation = row.ReservedAt
		}
	})

	if phase == retirement.PhaseDone {
		j.DoneAt = retireNow().UTC().Format(time.RFC3339Nano)
	}

	mustOK(t, j.Write(retireNow()))

	return j
}

// drive runs the transition over the fixture's host, the way the request runs
// it once its intent is recorded.
func (f *requestFixture) drive(t *testing.T, j retirement.Journal) (retirement.Journal, []retireStep, *retireRefusal) {
	t.Helper()

	// PAST THE REWRITE THERE IS NO CONTROLLER'S CONFIGURATION to observe: a
	// server-only host has no file at all and a retained-node host has one with
	// no server section. The driver is the thing that must go on working there,
	// and it is handed exactly what the command could hand it.
	obs, _ := observeRetireConfig(f.cfg)

	return retireTransition(t.Context(), retireMode{configPath: f.cfg, run: requestRun,
		retiringHost: requestRetiring}, obs, j)
}

// retiredAnswer asserts the answer of a run that carried a retirement to
// `done` AND finished its tail: the row completed from this host, the marker
// gone, the journal settled. Every test that drives a whole transition ends
// here, so a tail that stopped owing something cannot pass as a completed one.
func retiredAnswer(t *testing.T, out string, code int) map[string]any {
	t.Helper()

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeRetired || m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("the transition did not end retired: %s", out)
	}

	if m["row"] != retireRowDone || m["settled"] != true {
		t.Fatalf("the tail left the retirement owing something: %s", out)
	}

	return m
}

// actionsOf renders the steps a run performed, for one assertion per case.
func actionsOf(steps []retireStep) string {
	words := make([]string, 0, len(steps))
	for _, s := range steps {
		words = append(words, s.Phase+":"+s.Action)
	}

	return strings.Join(words, " ")
}

// A SERVER-ONLY TRANSITION RUNS ITS PHASES IN ORDER: the timers stop before
// their stop is recorded, the server stops before the status closes, the
// identity is archived before the configuration is removed, and the journal
// advances only behind each of them.
func TestARetirementRunsItsPhasesInOrder(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	var atArchive []string

	// THE RENAME HAPPENS UNDER A STOPPED SERVER AND A CLOSED STATUS: what the
	// archive finds is what the stop left.
	savedRename := retireBeforeRename
	retireBeforeRename = func() {
		st, _, err := retirement.ReadStatus()
		mustOK(t, err)

		j, _, err := retirement.ReadJournal()
		mustOK(t, err)

		atArchive = append(atArchive, string(st.Phase), string(j.Phase), strings.Join(f.svc.trace, ","),
			j.TimerStoppedAt)
	}

	t.Cleanup(func() { retireBeforeRename = savedRename })

	out, code := f.request(t, f.input(t, nil))

	retiredAnswer(t, out, code)

	want := "stop billet-upgrade.timer,disable billet-upgrade.timer,stop billet-backup.timer," +
		"disable billet-backup.timer,stop billet-server.service,disable billet-server.service"
	if strings.Join(f.svc.trace, ",") != want {
		t.Fatalf("the units this transition stopped, in order:\n%v", f.svc.trace)
	}

	if len(atArchive) != 4 || atArchive[0] != string(retirement.PhaseStopped) ||
		atArchive[1] != string(retirement.PhaseStopped) || atArchive[2] != want || atArchive[3] == "" {
		t.Fatalf("the rename ran with the stop unfinished: %v", atArchive)
	}

	// THE IDENTITY IS AT THE ARCHIVE AND NOWHERE ELSE, and the configuration a
	// serverless host cannot hold is gone.
	if _, err := os.Stat(f.stateDir); !os.IsNotExist(err) {
		t.Fatalf("the identity directory is still at its configured path: %v", err)
	}

	if _, err := os.Stat(filepath.Join(archivedIdentity(t), "deployment-id")); err != nil {
		t.Fatalf("the archive does not hold the identity: %v", err)
	}

	if _, err := os.Stat(f.cfg); !os.IsNotExist(err) {
		t.Fatalf("a server-only host kept its configuration: %v", err)
	}

	j, presence, err := retirement.ReadJournal()
	if err != nil || presence != retirement.JournalPresent || j.Phase != retirement.PhaseDone || j.DoneAt == "" {
		t.Fatalf("the journal: %+v %d %v", j, presence, err)
	}

	// AND THE TAIL RAN: the journal acknowledges the row it wrote and is
	// settled, which is the state the guard's release requires.
	if !j.RowDone || j.CompletedBy != requestRetiring || !j.Settled {
		t.Fatalf("the tail did not finish in the journal: %+v", j)
	}

	// THE ROW THE SURVIVOR READS IS DONE, completed by this host, because this
	// host could still reach the ledger it retired from.
	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementDone || r.CompletedBy != requestRetiring {
			t.Fatalf("the row: %+v (present %v)", r, present)
		}
	})

	// AND THE GUARD'S MARKER IS GONE, so the converge that holds it can
	// release it.
	shape, err := classifyClaim()
	mustOK(t, err)

	if shape.Guard.Transition != nil {
		t.Fatalf("the guard still carries the retirement's marker: %+v", shape.Guard.Transition)
	}
}

// A RETAINED NODE KEEPS ITS CONFIGURATION, which is the staged rendering byte
// for byte with the installed file's own owner and mode, and the node is
// persistently enabled and restarted before the retirement is done.
func TestARetainedNodesTransitionInstallsTheStageAndRestartsTheNode(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	before, err := os.Stat(f.cfg)
	mustOK(t, err)

	// The node publishes its record when it comes back, as a node does after a
	// registration; until then the retirement is not done.
	record := useRegistrationRecord(t)
	f.svc.onStart = func(unit string) {
		if unit == nodeUnit {
			restartedNode(t, f, record, retainedEndpoint)
		}
	}

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

	retiredAnswer(t, out, code)

	if body := mustRead(t, f.cfg); body != f.rendering(t) {
		t.Fatalf("the installed configuration is not the stage byte for byte:\n%s", body)
	}

	after, err := os.Stat(f.cfg)
	mustOK(t, err)

	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("the installed configuration's mode moved: %v then %v", before.Mode(), after.Mode())
	}

	// THE ENABLEMENT PRECEDES THE RESTART, and both precede done.
	want := "enable " + nodeUnit + ",stop " + nodeUnit + ",start " + nodeUnit
	if !strings.Contains(strings.Join(f.svc.trace, ","), want) {
		t.Fatalf("the node was not enabled and restarted in order: %v", f.svc.trace)
	}

	j, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if j.Phase != retirement.PhaseDone || j.Variant != retirement.VariantRetainedNode {
		t.Fatalf("the journal: %+v", j)
	}
}

// A NODE THAT DOES NOT COME BACK UNDER ITS OWN RECORD leaves the retirement at
// node-restarted: the phase is written behind a restart that happened, and
// `done` waits for the node's own account of itself.
func TestARetainedNodeThatPublishesNoRecordIsNotDone(t *testing.T) {
	for name, endpoint := range map[string]string{
		"no record at all":                 "",
		"only the pre-restart record":      "",
		"a record naming another endpoint": "https://10.0.0.9:7717",
	} {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)

			record := useRegistrationRecord(t)

			f.svc.onStart = func(unit string) {
				if unit != nodeUnit {
					return
				}
				if name == "no record at all" {
					mustOK(t, os.Remove(record))
				} else if endpoint != "" {
					restartedNode(t, f, record, endpoint)
				}
			}

			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

			m := retireAnswer(t, out)
			if code != exitUnknown || m["reason"] != retireReasonPhase ||
				m["state"] != string(retirement.PhaseNodeRestarted) {
				t.Fatalf("a node that did not come back on the configuration it was given: %s", out)
			}

			if !strings.Contains(whyOf(m), "node_unit") {
				t.Fatalf("the refusal does not name the fact that refused: %s", out)
			}

			props, err := endpointInspector().UnitProperties(t.Context(), nodeUnit, "InvocationID")
			mustOK(t, err)
			if firstProp(props, "InvocationID") != retainedRestartInvocation {
				t.Fatal("the fake restart did not change the manager's invocation")
			}
			if name == "no record at all" {
				if _, err := os.Lstat(record); !os.IsNotExist(err) {
					t.Fatalf("the absent-record case kept a record: %v", err)
				}
			} else {
				ev := readRegistrationRecord(record)
				invocation := retainedInvocation
				if endpoint != "" {
					invocation = retainedRestartInvocation
				}
				if ev.record == nil || ev.record.InvocationID != invocation {
					t.Fatalf("the record case did not establish invocation %s: %+v", invocation, ev)
				}
			}
			j := requireRetireJournal(t)
			if j.Phase != retirement.PhaseNodeRestarted || j.DoneAt != "" || j.RowDone || j.Settled {
				t.Fatalf("an unproved registration completed retirement: %+v", j)
			}
		})
	}
}

// EVERY INTERRUPTION IS RESUMED FROM THE STATE IT LEFT, and the table's own
// remainders (a move made before its phase, a rewrite made before its phase)
// are recognised rather than repeated.
func TestATransitionResumesFromWhereItWasInterrupted(t *testing.T) {
	cases := map[string]struct {
		phase retirement.Phase
		// variant is server-only unless a case names the other.
		variant retirement.Variant
		// prepare runs before the journal is planted, for a case that needs a
		// node on the host.
		prepare func(t *testing.T, f *requestFixture)
		stage   func(t *testing.T, f *requestFixture, j retirement.Journal)
		actions string
	}{
		"the timers stopped, nothing else": {
			phase:   retirement.PhaseIntent,
			actions: "intent:stop stopped:archive archived:rewrite config-rewritten:done",
		},
		"the stop finished, the phase unwritten": {
			phase: retirement.PhaseIntent,
			stage: func(t *testing.T, _ *requestFixture, _ retirement.Journal) {
				t.Helper()

				mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))
			},
			actions: "intent:stop stopped:archive archived:rewrite config-rewritten:done",
		},
		"the move completed, the phase unwritten": {
			phase: retirement.PhaseStopped,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
			},
			actions: "stopped:advance-archived archived:rewrite config-rewritten:done",
		},
		"the rewrite completed, the phase unwritten": {
			phase: retirement.PhaseArchived,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
				mustOK(t, os.Remove(f.cfg))
			},
			actions: "archived:advance-config-rewritten config-rewritten:done",
		},
		"the configuration is gone and the phase is written": {
			phase: retirement.PhaseConfigRewritten,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
				mustOK(t, os.Remove(f.cfg))
			},
			actions: "config-rewritten:done",
		},
		"a retained node's rewrite completed, the phase unwritten": {
			phase:   retirement.PhaseArchived,
			variant: retirement.VariantRetainedNode,
			prepare: func(t *testing.T, f *requestFixture) {
				t.Helper()
				f.retainANode(t)

				record := useRegistrationRecord(t)
				f.svc.onStart = func(unit string) {
					if unit == nodeUnit {
						restartedNode(t, f, record, retainedEndpoint)
					}
				}
			},
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))

				// The staged rendering is installed, which is what makes the
				// node's own configuration newer than the process reading it.
				writeFile(t, f.cfg, f.rendering(t), 0o600)
			},
			actions: "archived:advance-config-rewritten config-rewritten:restart node-restarted:done",
		},
		"done, which repeats nothing": {
			phase: retirement.PhaseDone,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
				mustOK(t, os.Remove(f.cfg))
			},
			actions: "",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)

			variant := c.variant
			if variant == "" {
				variant = retirement.VariantServerOnly
			}

			if c.prepare != nil {
				c.prepare(t, f)
			}

			j := f.plantJournal(t, c.phase, variant)
			if c.stage != nil {
				c.stage(t, f, j)
			}

			j, steps, r := f.drive(t, j)
			if r != nil {
				t.Fatalf("the resume: %+v", r)
			}

			if actionsOf(steps) != c.actions {
				t.Fatalf("the resume performed %q, want %q", actionsOf(steps), c.actions)
			}

			if j.Phase != retirement.PhaseDone {
				t.Fatalf("the resume ended at %s", j.Phase)
			}
		})
	}
}

// A RUNNING BACKUP IS AWAITED AND NEVER KILLED, and the one failure a
// retirement reconciles is the refusal it caused itself.
func TestABackupIsAwaitedAndOnlyItsOwnRefusalIsReconciled(t *testing.T) {
	cases := map[string]struct {
		unit      string
		resolve   string
		refuses   bool
		reconcile bool
	}{
		"a backup that finishes is waited for": {
			unit: "LoadState=loaded\nActiveState=activating\nSubState=start\nJob=42\nResult=success\nMainPID=99\n",
			resolve: "LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\n" +
				"UnitFileState=static\nExecMainCode=1\nExecMainStatus=0\n",
		},
		"the refusal this retirement caused is cleared": {
			unit: "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nMainPID=0\n" +
				"ExecMainCode=1\nExecMainStatus=6\nExecMainStartTimestamp=Fri 2026-09-11 10:00:30 UTC\n",
			resolve:   "LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\nUnitFileState=static\n",
			reconcile: true,
		},
		"a backup that failed with another status is not this retirement's": {
			unit: "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nMainPID=0\n" +
				"ExecMainCode=1\nExecMainStatus=1\nExecMainStartTimestamp=Fri 2026-09-11 10:00:30 UTC\n",
			refuses: true,
		},
		"a backup killed by a signal is not this retirement's": {
			unit: "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=signal\nMainPID=0\n" +
				"ExecMainCode=2\nExecMainStatus=6\nExecMainStartTimestamp=Fri 2026-09-11 10:00:30 UTC\n",
			refuses: true,
		},
		"a backup that failed before the timers stopped is not this retirement's": {
			unit: "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nMainPID=0\n" +
				"ExecMainCode=1\nExecMainStatus=6\nExecMainStartTimestamp=Fri 2026-09-11 09:00:00 UTC\n",
			refuses: true,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			unit := filepath.Join(f.unitsDir, backupServiceUnit)
			effects := filepath.Join(f.unitsDir, backupServiceUnit+".effects")
			writeFile(t, effects, strings.ReplaceAll(mustRead(t, effects), "Job=\n", ""), 0o644)
			if !strings.Contains(c.unit, "Job=") {
				c.unit += "Job=\n"
			}
			if c.resolve != "" {
				c.resolve += "Job=\n"
			}
			writeFile(t, unit, c.unit+"UnitFileState=static\n", 0o644)

			// The wait is a test's: a poll and a bound this case can reach.
			savedWait, savedPoll := retireBackupWait, retireBackupPoll
			retireBackupWait, retireBackupPoll = 2*time.Second, time.Millisecond

			t.Cleanup(func() { retireBackupWait, retireBackupPoll = savedWait, savedPoll })

			var reset bool

			savedReset := retireResetFailedFn
			retireResetFailedFn = func(context.Context, string) error {
				reset = true

				writeFile(t, unit, c.resolve, 0o644)

				return nil
			}

			t.Cleanup(func() { retireResetFailedFn = savedReset })

			// Admission reads dependency evidence separately. Count only completed
			// backup-state observations: initial remaining-work observation, table
			// observation, then the wait's first poll. An early-returning wait
			// still produces a second await step and fails the action assertion.
			waited := make(chan struct{})
			stop := make(chan struct{})

			if c.resolve != "" && !c.reconcile {
				go func() {
					defer close(waited)

					deadline := time.After(30 * time.Second)

					for {
						if strings.Count(readIfAny(filepath.Join(f.unitsDir, ".backup-observed")), backupServiceUnit) >= 3 {
							publishUnit(t, unit, c.resolve)

							return
						}

						select {
						case <-stop:
							return
						case <-deadline:
							t.Error("the transition never waited on the backup")

							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
			} else {
				close(waited)
			}

			// THE GOROUTINE IS JOINED WHATEVER THE RUN DID, so a case that
			// refuses before the first question does not leave it behind.
			t.Cleanup(func() {
				close(stop)
				<-waited
			})

			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)

			_, steps, r := f.drive(t, j)

			if c.resolve != "" && !c.reconcile {
				want := "intent:await-backup intent:stop stopped:archive archived:rewrite config-rewritten:done"
				if actionsOf(steps) != want {
					t.Fatalf("a running backup was awaited %q, want exactly one await: %q", actionsOf(steps), want)
				}
			}

			switch {
			case c.refuses && (r == nil || r.Reason != retireReasonPhase || !strings.Contains(r.Why, "backup")):
				t.Fatalf("a backup failure this retirement did not cause was admitted: %+v", r)
			case !c.refuses && r != nil:
				t.Fatalf("the transition: %+v", r)
			case c.reconcile != reset:
				t.Fatalf("the reconciliation ran: %v, want %v", reset, c.reconcile)
			}
		})
	}
}

// THE LIFECYCLE LOCK COVERS THE STOP-TO-ARCHIVE WINDOW: `local up` cannot
// start what the transition is stopping, and is not queued behind the node's
// restart, which is a drain.
func TestTheLifecycleLockCoversTheStopToArchiveWindow(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	held := map[string]bool{}

	note := func(when string) {
		l, err := takeHostLock()
		if err != nil {
			held[when] = true

			return
		}

		mustOK(t, l.release())
	}

	f.svc.onStop = func(unit string) {
		if unit == serverUnit {
			note("stop")
		}
	}

	savedRename := retireBeforeRename
	retireBeforeRename = func() { note("archive") }

	t.Cleanup(func() { retireBeforeRename = savedRename })

	record := useRegistrationRecord(t)
	f.svc.onStart = func(unit string) {
		if unit == nodeUnit {
			note("restart")
			restartedNode(t, f, record, retainedEndpoint)
		}
	}

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	retiredAnswer(t, out, code)

	if !held["stop"] || !held["archive"] || held["restart"] {
		t.Fatalf("the lifecycle lock was held at: %+v", held)
	}
}

// A RESUME IS HELD TO ITS JOURNAL AND TO THIS GUARD'S MARKER: a journal
// another retirement wrote, and a guard whose marker is gone, are refused with
// nothing stopped.
func TestAResumeIsHeldToItsJournalAndMarker(t *testing.T) {
	cases := map[string]struct {
		break_ func(t *testing.T, f *requestFixture)
		reason string
	}{
		"a journal naming another transition": {
			break_: func(t *testing.T, f *requestFixture) {
				t.Helper()

				j, _, err := retirement.ReadJournal()
				mustOK(t, err)

				j.Provenance.TransitionID = strings.Repeat("b", 32)
				mustOK(t, j.Write(retireNow()))
			},
			reason: retireReasonJournal,
		},
		"a guard with no marker": {
			break_: func(t *testing.T, f *requestFixture) {
				t.Helper()

				markGuard(t, f.guard, nil, nil)
			},
			reason: retireReasonMarker,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)

			// A retirement already at intent, as a converge that was
			// interrupted between the intent and the stop leaves it.
			f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)
			markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
			advanceRowToIntent(t, f)

			c.break_(t, f)

			out, code := f.request(t, f.input(t, nil))

			m := retireAnswer(t, out)
			if code != exitUnknown || m["reason"] != c.reason {
				t.Fatalf("the resume: %s", out)
			}

			if len(f.svc.trace) != 0 {
				t.Fatalf("a refused resume touched the host: %v", f.svc.trace)
			}
		})
	}
}

var retireRegistrationFixturePath string

// useRegistrationRecord points the node's runtime record at a path this test
// owns, and admits the file it writes: the reader requires a root-owned 0600
// record, which no test process can create.
func useRegistrationRecord(t *testing.T) string {
	t.Helper()

	if retireRegistrationFixturePath != "" && registrationRecordPath == retireRegistrationFixturePath {
		return registrationRecordPath
	}

	dir := filepath.Join(t.TempDir(), "registration")
	mustOK(t, os.MkdirAll(dir, 0o700))

	path := filepath.Join(dir, "current")
	savedPath, savedOpen := registrationRecordPath, registrationOpen
	savedFixture := retireRegistrationFixturePath
	registrationRecordPath, retireRegistrationFixturePath = path, path
	registrationOpen = func(name string) (*os.File, os.FileInfo, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, nil, err
		}

		info, err := f.Stat()
		if err != nil {
			return nil, nil, err
		}

		return f, rootOwned{info}, nil
	}

	t.Cleanup(func() {
		registrationRecordPath, registrationOpen = savedPath, savedOpen
		retireRegistrationFixturePath = savedFixture
	})

	return path
}

// rootOwned answers the ownership and mode the reader requires over a file a
// test wrote, and nothing else.
type rootOwned struct{ os.FileInfo }

func (rootOwned) Mode() os.FileMode { return 0o600 }

func (r rootOwned) Sys() any {
	under, ok := r.FileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return r.FileInfo.Sys()
	}

	st := *under
	st.Uid = 0

	return &st
}

// writeRegistrationRecord publishes the record a node writes after it
// registers, for this deployment, the endpoint its configuration names and the
// invocation it is running under.
func writeRegistrationRecord(t *testing.T, path, deployment, endpoint, invocation string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"schema": 1, "node": "node-a", "deployment": deployment, "incarnation": retainedIncarnation,
		"invocation_id": invocation, "endpoint": endpoint,
		"registered_at": "2026-09-11T10:00:00Z",
	})
	mustOK(t, err)

	writeFile(t, path, string(body), 0o600)
}

// restartedNode is what the fake converger's start of the node unit leaves
// behind: SYSTEMD MINTS A NEW INVOCATION for every start, so the unit's
// property moves with it, and the node publishes its record under that new
// invocation when it registers. A fixture whose restart kept the invocation
// could not tell a receipt written for the restart from the one that was
// already there.
func restartedNode(t *testing.T, f *requestFixture, record, endpoint string) {
	t.Helper()

	unit := filepath.Join(f.unitsDir, nodeUnit)
	writeFile(t, unit, strings.Replace(mustRead(t, unit), "InvocationID="+retainedInvocation,
		"InvocationID="+retainedRestartInvocation, 1), 0o644)

	writeRegistrationRecord(t, record, f.identity, endpoint, retainedRestartInvocation)
}

// advanceRowToIntent moves the reserved row to intent, as the intent does.
func advanceRowToIntent(t *testing.T, f *requestFixture) {
	t.Helper()

	f.pgLedger(t, func(db *state.DB) {
		mustOK(t, db.AdvanceRetirementToIntent(t.Context(), f.identity, requestRetiring, retireTestID, requestRun,
			retireNow()))
	})
}

// readIfAny answers a file's contents, or nothing when it is not there yet.
func readIfAny(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return string(body)
}

// A RESUME PAST THE ARCHIVE NEEDS NO PUBLISHED STATUS, and finishing leaves
// one. The status is what closes the authority to every ordinary writer, and a
// host that lost it between the stop and the interruption is the LEAST closed a
// retired host can be; the resume takes no identity exclusion, so nothing about
// it depends on the status being there, and the host is finished rather than
// refused. What publishes `done` here is the TRANSITION, at its last phase —
// the tail's repair of a status that went missing AFTER that is its own test.
func TestAResumePastTheArchiveWithNoPublishedStatusFinishes(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	j := f.plantJournal(t, retirement.PhaseArchived, retirement.VariantServerOnly)
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
	advanceRowToIntent(t, f)

	mustOK(t, os.Rename(f.stateDir, j.Archive))

	// THE STATUS IS NOT THERE, which is the whole case: every other resume
	// fixture publishes the one the stop wrote.
	if _, presence, err := retirement.ReadStatus(); presence != retirement.StatusAbsent || err != nil {
		t.Fatalf("this host already publishes a status: %d %v", presence, err)
	}

	out, code := f.request(t, f.input(t, nil))

	retiredAnswer(t, out, code)

	st, presence, err := retirement.ReadStatus()
	mustOK(t, err)

	if presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
		t.Fatalf("the status the tail left: %+v (presence %d)", st, presence)
	}

	after, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if after.Phase != retirement.PhaseDone || !after.Settled {
		t.Fatalf("the resume left the journal at %s (settled %v)", after.Phase, after.Settled)
	}
}

// A NODE ENABLED ONLY FOR THIS BOOT IS NOT ENABLED: `enabled-runtime` passes
// every running predicate and is gone at the next boot, so a retirement that
// accepted it would leave a host whose node never comes back.
func TestARetirementRequiresTheNodesPersistentEnablement(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	f.svc.enableLeaves = map[string]string{nodeUnit: "enabled-runtime"}

	useRegistrationRecord(t)

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonRestart ||
		!strings.Contains(whyOf(m), "persistently enabled") {
		t.Fatalf("a runtime enablement was accepted: %s", out)
	}

	// AND NOTHING WAS RESTARTED behind it.
	if strings.Contains(strings.Join(f.svc.trace, ","), "start "+nodeUnit) {
		t.Fatalf("the node was restarted under an enablement that does not survive a boot: %v", f.svc.trace)
	}
}

// THE STATUS CLOSES BEFORE THE JOURNAL LEAVES `intent`, and the remainder of a
// crash between the two is the state the table admits: an authority already
// closed to every ordinary writer beside a journal that still says the
// transition has stopped nothing. The reverse order would admit a backup or a
// `local up` into a directory the next step renames, and no schedule can see
// between two sequential writes — so the journal's own write is failed right
// after the status is published, and what the host holds then is the
// assertion.
func TestTheStatusClosesBeforeTheJournalLeavesIntent(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// The SECOND status published is the stop's: the first is the intent's.
	statuses := 0

	retirement.Publishing = func(path string) error {
		switch path {
		case retirement.StatusPath():
			statuses++
		case retirement.JournalPath():
			if statuses >= 2 {
				return errors.New("the journal could not be written")
			}
		}

		return nil
	}

	t.Cleanup(func() { retirement.Publishing = nil })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonJournal || m["state"] != string(retirement.PhaseIntent) {
		t.Fatalf("the interrupted stop: %s", out)
	}

	// THE STATUS IS CLOSED: every ordinary authority writer is already refused.
	st, presence, err := retirement.ReadStatus()
	if err != nil || presence != retirement.StatusPresent || st.Phase != retirement.PhaseStopped {
		t.Fatalf("the status: %+v %d %v", st, presence, err)
	}

	// AND THE JOURNAL IS STILL AT INTENT, carrying the timers' stop, which is
	// what lets a resume recognise a backup that refused in this window.
	j, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if j.Phase != retirement.PhaseIntent || j.TimerStoppedAt == "" {
		t.Fatalf("the journal: %+v", j)
	}
}

// A RESUME TAKES THE TRANSITION'S OWN EXCLUSION: from `stopped` on, the status
// this host published refuses every ordinary authority writer, and a run
// resuming its own interrupted transition is refused with it unless it takes
// the exclusion that acquires without admitting. Without this a retirement
// interrupted after its stop could never be finished by a converge.
func TestAResumeRunsUnderTheStatusItPublished(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// The host as an interrupted converge leaves it: the timers and the server
	// stopped, the authority closed, the journal at `stopped`, the identity
	// still where the configuration names it.
	f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
	advanceRowToIntent(t, f)
	mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))

	out, code := f.request(t, f.input(t, nil))

	retiredAnswer(t, out, code)

	// NOTHING WAS STOPPED AGAIN: the resume began at the archive.
	if len(f.svc.trace) != 0 {
		t.Fatalf("a resume past the stop stopped units again: %v", f.svc.trace)
	}

	if _, err := os.Stat(archivedIdentity(t)); err != nil {
		t.Fatalf("the identity was not archived: %v", err)
	}
}

// A PHASE IS NOT PUBLISHED OVER AN UNFLUSHED CHANGE, the recovery paths
// included: an interrupted run's rename is visible to the next observation
// long before its parent's entry is durable, so the run that recognises the
// move completes the flush that run owed before it records the phase.
func TestAnAdvanceFlushesWhatTheInterruptedRunOwed(t *testing.T) {
	cases := map[string]struct {
		phase   retirement.Phase
		variant retirement.Variant
		// nth is which of the advance's flushes fails, and want the
		// directories it owes up to and including that one, in order.
		nth      int
		want     func(f *requestFixture, j retirement.Journal) []string
		action   string
		recorded string
	}{
		"the moved directory's parent": {
			phase: retirement.PhaseStopped, variant: retirement.VariantServerOnly, nth: 1,
			want: func(f *requestFixture, _ retirement.Journal) []string {
				return []string{filepath.Dir(f.stateDir)}
			},
			action: "stopped:advance-archived", recorded: "before recording archived",
		},
		"the archive's parent": {
			phase: retirement.PhaseStopped, variant: retirement.VariantServerOnly, nth: 2,
			want: func(f *requestFixture, j retirement.Journal) []string {
				return []string{filepath.Dir(f.stateDir), filepath.Dir(j.Archive)}
			},
			action: "stopped:advance-archived", recorded: "before recording archived",
		},
		"the configuration's directory": {
			phase: retirement.PhaseArchived, variant: retirement.VariantRetainedNode, nth: 1,
			want: func(f *requestFixture, _ retirement.Journal) []string {
				return []string{filepath.Dir(f.cfg)}
			},
			action: "archived:advance-config-rewritten", recorded: "before recording config-rewritten",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)

			if c.variant == retirement.VariantRetainedNode {
				f.retainANode(t)
			}

			f.reserve(t)

			j := f.plantJournal(t, c.phase, c.variant)

			// The act completed and its phase was never written: the remainder
			// the table admits as an advance.
			mustOK(t, os.Rename(f.stateDir, j.Archive))

			if c.phase == retirement.PhaseArchived {
				writeFile(t, f.cfg, f.rendering(t), 0o600)
			}

			var flushed []string

			savedSync := retireSyncDir
			retireSyncDir = func(dir string) error {
				flushed = append(flushed, dir)

				if len(flushed) == c.nth {
					return errors.New("the directory could not be flushed")
				}

				return savedSync(dir)
			}

			t.Cleanup(func() { retireSyncDir = savedSync })

			_, steps, r := f.drive(t, j)

			switch {
			case r == nil || r.Reason != retireReasonJournal || !strings.Contains(r.Why, c.recorded):
				t.Fatalf("the advance over an unflushed act: %+v", r)
			case actionsOf(steps) != c.action:
				t.Fatalf("the resume performed %q, want %q", actionsOf(steps), c.action)
			case !slices.Equal(flushed, c.want(f, j)):
				t.Fatalf("the advance flushed %v, want %v", flushed, c.want(f, j))
			}

			// THE PHASE WAS NOT WRITTEN, because a flush it certifies did not
			// happen.
			after, _, err := retirement.ReadJournal()
			mustOK(t, err)

			if after.Phase != c.phase {
				t.Fatalf("the phase advanced over a failed flush: %s", after.Phase)
			}
		})
	}
}

// A JOURNAL WRITE THAT FAILS AFTER ITS RENAME leaves the next phase on disk,
// and the answer says what the host HOLDS: a reader would find that phase, and
// a refusal naming the previous one would send the next converge to a state
// nothing is in.
func TestAJournalWriteThatFailsAfterItsRenameAnswersWhatIsOnDisk(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	statuses := 0

	retirement.Publishing = func(path string) error {
		if path == retirement.StatusPath() {
			statuses++
		}

		return nil
	}

	retirement.SyncingDir = func(dir string) error {
		// The flush that follows the journal's rename, once the stop's status
		// has closed the authority: everything before it is left alone.
		if statuses >= 2 && dir == retirement.RetiredDir() {
			return errors.New("the directory could not be flushed")
		}

		return nil
	}

	t.Cleanup(func() { retirement.Publishing, retirement.SyncingDir = nil, nil })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonJournal ||
		m["state"] != string(retirement.PhaseStopped) || !strings.Contains(whyOf(m), "on disk") {
		t.Fatalf("the interrupted publish: %s", out)
	}

	j, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if j.Phase != retirement.PhaseStopped {
		t.Fatalf("the journal on disk: %s", j.Phase)
	}
}

// NEITHER AN UNKNOWN ENABLEMENT NOR A RECORD FROM ANOTHER INVOCATION IS
// PERMISSION TO RESTART a node that is positively active: the first is a word
// this billet does not know, the second a registration not published yet, and
// a restart is a drain.
//
// EACH CASE STAGES EXACTLY ONE DEFECT, which is why the record's invocation is
// the case's to choose: a case that left both the enablement and the record
// wrong would pass for whichever the fact happened to judge first.
func TestAnUncertainNodeIsNotRestarted(t *testing.T) {
	// The unit as the node runs after its restart, which is what both cases
	// vary from.
	running := "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nKillMode=mixed\n" +
		"MainPID=4242\nUnitFileState=%s\nInvocationID=%s\nExecMainStartTimestamp=" + retainedNodeStarted + "\n"

	for name, c := range map[string]struct {
		unit string
		// record is the invocation the node's published record names.
		record string
	}{
		"an enablement this billet does not know": {
			unit:   fmt.Sprintf(running, "refreshing", retainedRestartInvocation),
			record: retainedRestartInvocation,
		},
		"a record from another invocation": {
			unit:   fmt.Sprintf(running, "enabled", retainedRestartInvocation),
			record: retainedInvocation,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)

			record := useRegistrationRecord(t)
			writeRegistrationRecord(t, record, f.identity, retainedEndpoint, c.record)

			j := f.plantJournal(t, retirement.PhaseNodeRestarted, retirement.VariantRetainedNode)

			mustOK(t, os.Rename(f.stateDir, j.Archive))
			writeFile(t, f.cfg, f.rendering(t), 0o600)
			writeFile(t, filepath.Join(f.unitsDir, nodeUnit), c.unit, 0o644)

			_, _, r := f.drive(t, j)

			if r == nil || r.Reason != retireReasonPhase || !strings.Contains(r.Why, "node_unit") {
				t.Fatalf("an uncertain node was acted on: %+v", r)
			}

			if len(f.svc.trace) != 0 {
				t.Fatalf("a positively active node was touched: %v", f.svc.trace)
			}
		})
	}
}

// A CONFIGURATION WRITTEN INSIDE THE SECOND A NODE STARTED has no observable
// order against it: systemd renders seconds and the filesystem keeps
// nanoseconds, and a truncation is not an ordering.
func TestAConfigurationWrittenInsideTheStartsSecondIsCouldNotTell(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	j := f.plantJournal(t, retirement.PhaseArchived, retirement.VariantRetainedNode)
	mustOK(t, os.Rename(f.stateDir, j.Archive))

	// The node's rendered start is 09:00:00 and the file's modification time is
	// that same instant: the process started somewhere in that whole second, so
	// the file is not observably older than it. A comparison that answered
	// `not after` here would call the configuration unchanged and let the
	// transition go on.
	started, err := time.Parse(time.RFC3339, "2026-09-11T09:00:00Z")
	mustOK(t, err)
	mustOK(t, os.Chtimes(f.cfg, started, started))

	_, _, r := f.drive(t, j)

	if r == nil || r.Reason != retireReasonPhase || !strings.Contains(r.Why, "node_changed: unknown") {
		t.Fatalf("an ordering was manufactured out of a truncated timestamp: %+v", r)
	}
}

// AND EXACTLY ONE SECOND LATER IS AFTER EVERY START THE RENDERING ADMITS: the
// true start lies inside the second systemd printed, so a modification at its
// end is observably later. The verdict is read where it DECIDES — at `archived`
// over the installed configuration, which the table admits only when the node's
// file has NOT changed — and by value, because could-not-tell and `true` refuse
// there for different reasons and only the value tells them apart.
func TestAConfigurationOneSecondAfterTheStartIsChanged(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	j := f.plantJournal(t, retirement.PhaseArchived, retirement.VariantRetainedNode)
	mustOK(t, os.Rename(f.stateDir, j.Archive))

	started, err := time.Parse(time.RFC3339, "2026-09-11T09:00:00Z")
	mustOK(t, err)
	mustOK(t, os.Chtimes(f.cfg, started.Add(time.Second), started.Add(time.Second)))

	_, _, r := f.drive(t, j)

	if r == nil || !strings.Contains(r.Why, "node_changed: true") {
		t.Fatalf("a modification a whole second after the start was not read as a change: %+v", r)
	}
}

// A ZONE THIS HOST CANNOT RESOLVE IS NOT READ AS UTC: systemd renders the
// host's own abbreviation, and reading a positive offset as UTC moves the
// instant later, which is the ADMITTING direction for a backup that failed
// before the timers stopped.
func TestARenderedTimestampIsUTCOrNothing(t *testing.T) {
	for _, c := range []struct {
		rendered string
		want     bool
	}{
		{"Fri 2026-09-11 10:00:30 UTC", true},
		{"Fri 2026-09-11 10:00:30 GMT", true},
		{"Fri 2026-09-11 11:30:00 CEST", false},
		{"Fri 2026-09-11 10:00:30", false},
		{"", false},
		{"Fri 2026-09-11 25:00:30 UTC", false},
	} {
		if _, ok := retireSystemdTimestamp(c.rendered); ok != c.want {
			t.Errorf("%q was read as a time: %v, want %v", c.rendered, ok, c.want)
		}
	}
}

// WHICH RESUME A `stopped` JOURNAL TAKES IS DECIDED BY THE FILESYSTEM AND NOT
// BY ITS PHASE. A move that completed before its phase could be written leaves
// a journal saying `stopped` on a host whose identity is already at the
// archive, and the ordinary path — whose exclusion and identity are inside the
// directory that has moved — has nothing to read there. The same journal before
// the move takes that ordinary path, and the completion of both is the resume
// table's. This is the decision itself, because a run that took the wrong path
// would fail on a missing file rather than say which it chose.
func TestAStoppedJournalsResumeIsChosenByWhereTheIdentityIs(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	j := f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)

	if past, r := retireResumeIsPastTheArchive(j, retirement.JournalFactIncomplete); past || r != nil {
		t.Fatalf("a journal whose identity has not moved was routed past the archive: %v %+v", past, r)
	}

	mustOK(t, os.Rename(f.stateDir, j.Archive))

	if past, r := retireResumeIsPastTheArchive(j, retirement.JournalFactIncomplete); !past || r != nil {
		t.Fatalf("a journal whose move completed was not routed past the archive: %v %+v", past, r)
	}

	// AND A JOURNAL THAT IS NOT INCOMPLETE TAKES NEITHER RESUME, whatever the
	// filesystem says: `done` is answered from the journal itself before this
	// decision is reached, and an absent one is a request rather than a resume.
	for _, fact := range []retirement.JournalFact{retirement.JournalFactDone, retirement.JournalFactIntent,
		retirement.JournalFactAbsent} {
		if past, r := retireResumeIsPastTheArchive(j, fact); past || r != nil {
			t.Fatalf("a %v journal was routed past the archive: %v %+v", fact, past, r)
		}
	}
}

// THE TRANSITION'S EXCLUSION IS THIS TRANSITION'S ALONE. The exception exists
// because a run cannot admit itself through the status it published; it is not
// a licence to touch a closed authority on the strength of any journal being
// present. A journal no marker on this guard claims meets the status like
// every other writer, and one that does not describe this deployment is
// refused BEFORE any ledger is opened, since an open migrates a schema,
// repairs ownership and hands artefacts back.
func TestTheTransitionsExclusionIsGrantedToThisTransitionAlone(t *testing.T) {
	t.Run("a journal this guard's marker does not claim", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
		advanceRowToIntent(t, f)
		mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))

		// The guard carries ANOTHER transition's marker, so this journal is not
		// the transition this converge is driving.
		markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: strings.Repeat("b", 32)}, nil)

		out, code := f.request(t, f.input(t, nil))

		m := retireAnswer(t, out)
		if code != exitUnknown || m["reason"] != retireReasonIdentity ||
			!strings.Contains(whyOf(m), "authority is closed") {
			t.Fatalf("an unclaimed journal took the transition's exclusion: %s", out)
		}

		if len(f.svc.trace) != 0 {
			t.Fatalf("a refused run touched the host: %v", f.svc.trace)
		}
	})

	t.Run("a journal this holder and its chain never owned", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		j := f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
		markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
		advanceRowToIntent(t, f)
		mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))

		// THE MARKER MATCHES AND THE OWNER DOES NOT: a guard that took over
		// would list the journal's owner in its chain, and this one does not,
		// so the transition this run drives is not the one the journal records.
		j.Ownership.Owner = "ci-9"
		mustOK(t, j.Write(retireNow()))

		// The ledger is unreachable, so an administrative open is observable.
		t.Setenv("BILLET_STATE_DSN", "postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable")

		out, code := f.request(t, f.input(t, nil))

		m := retireAnswer(t, out)
		if code != exitUnknown || m["reason"] != retireReasonIdentity ||
			!strings.Contains(whyOf(m), "authority is closed") {
			t.Fatalf("a journal nobody in this guard's chain owns took the transition's exclusion: %s", out)
		}
	})

	t.Run("a journal describing another deployment", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		j := f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
		markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
		advanceRowToIntent(t, f)
		mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))

		j.Deployment = strings.Repeat("e", 32)
		j.Provenance.Deployment = j.Deployment
		mustOK(t, j.Write(retireNow()))

		// THE LEDGER IS UNREACHABLE, so an open is observable: a run that
		// judged the journal first answers about the journal, and one that
		// opened first answers about the ledger.
		t.Setenv("BILLET_STATE_DSN", "postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable")

		out, code := f.request(t, f.input(t, nil))

		m := retireAnswer(t, out)
		if code != exitUnknown || m["reason"] != retireReasonJournal ||
			!strings.Contains(whyOf(m), "names deployment") {
			t.Fatalf("the journal's deployment was not judged before the ledger was opened: %s", out)
		}
	})
}

// A WRITE THAT FAILED AND A READ-BACK THAT FAILED LEAVE THE PHASE UNKNOWN, and
// the answer says `unknown` rather than the phase this run last knew: the
// rename may have installed the next phase, and nothing here can tell.
func TestAJournalWriteWhoseReadBackAlsoFailsSaysThePhaseIsUnknown(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	statuses := 0

	retirement.Publishing = func(path string) error {
		if path == retirement.StatusPath() {
			statuses++
		}

		return nil
	}

	retirement.SyncingDir = func(dir string) error {
		if statuses < 2 || dir != retirement.RetiredDir() {
			return nil
		}

		// The flush after the journal's rename fails, and the journal's own
		// mode moves under it, so the read that would say which phase is on
		// disk refuses as could-not-tell.
		mustOK(t, os.Chmod(retirement.JournalPath(), 0o644))

		return errors.New("the directory could not be flushed")
	}

	t.Cleanup(func() { retirement.Publishing, retirement.SyncingDir = nil, nil })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonJournal || m["state"] != "unknown" ||
		!strings.Contains(whyOf(m), "reading the journal back") {
		t.Fatalf("a phase nothing could read was named anyway: %s", out)
	}
}

// THE WAIT IS EXERCISED DIRECTLY, because the driver's own observations can
// stand in for one: an await that looked once and returned success would be
// supplying the observation the fixture resolves behind. Here nothing else
// observes the unit, so the only way past the first answer is the wait's own
// polling, and the wait must not answer until it has seen the backup finish.
func TestTheBackupWaitDoesNotAnswerWhileTheBackupRuns(t *testing.T) {
	f := newRequestFixture(t)

	unit := filepath.Join(f.unitsDir, backupServiceUnit)
	publishUnit(t, unit, "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nMainPID=99\n")

	savedWait, savedPoll := retireBackupWait, retireBackupPoll
	retireBackupWait, retireBackupPoll = 30*time.Second, time.Millisecond

	t.Cleanup(func() { retireBackupWait, retireBackupPoll = savedWait, savedPoll })

	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)

	answered := make(chan *retireRefusal, 1)

	// THE WAIT IS ENDED AND JOINED BY THE CLEANUP whatever this case does, so
	// no poll of it outlives the seams and the files it reads. The join is its
	// own channel: the answer's is buffered and may already have been taken.
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})

	t.Cleanup(func() {
		cancel()
		<-finished
	})

	go func() {
		defer close(finished)

		answered <- awaitRetireBackup(ctx, j)
	}()

	// THE WAIT POLLS: three completed observations of a unit that is still
	// running are three answers only a wait makes, and it must not have
	// answered at any of them.
	asked := filepath.Join(f.unitsDir, ".asked")

	for deadline := time.After(30 * time.Second); ; {
		if strings.Count(readIfAny(asked), backupServiceUnit) >= 3 {
			break
		}

		select {
		case r := <-answered:
			t.Fatalf("the wait answered while the backup was running: %+v", r)
		case <-deadline:
			t.Fatal("the wait never polled the backup")
		case <-time.After(time.Millisecond):
		}
	}

	select {
	case r := <-answered:
		t.Fatalf("the wait answered while the backup was running: %+v", r)
	default:
	}

	// PUBLISHED BY RENAME: a truncate-then-write leaves a window in which a
	// poll reads an empty file, which is could-not-tell and would refuse a
	// wait that is behaving correctly.
	publishUnit(t, unit, "LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\n")

	select {
	case r := <-answered:
		if r != nil {
			t.Fatalf("the wait refused a backup that finished: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait never noticed the backup had finished")
	}

	// AND IT ANSWERED FROM AN OBSERVATION THAT SAW THE BACKUP FINISH, not from
	// one of the active answers it had already been given: the fake records
	// what it answered, and an inactive answer is what only a later poll got.
	if !strings.Contains(readIfAny(asked), backupServiceUnit+" ActiveState=inactive") {
		t.Fatalf("the wait answered without observing the backup finish:\n%s", readIfAny(asked))
	}
}

// publishUnit replaces a fake unit's properties ATOMICALLY, by rename: the
// fake reads the file while the transition polls, and a truncate-then-write
// would let a poll see an empty file, which is could-not-tell.
func publishUnit(t *testing.T, path, body string) {
	t.Helper()

	tmp := path + ".next"
	writeFile(t, tmp, body, 0o644)
	mustOK(t, os.Rename(tmp, path))
}

// AND A BACKUP THAT NEVER FINISHES ENDS THE WAIT AT ITS BOUND, leaving the
// retirement where it stood: a backup is awaited and never killed.
func TestTheBackupWaitEndsAtItsBound(t *testing.T) {
	f := newRequestFixture(t)

	publishUnit(t, filepath.Join(f.unitsDir, backupServiceUnit),
		"LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nMainPID=99\n")

	savedWait, savedPoll := retireBackupWait, retireBackupPoll
	retireBackupWait, retireBackupPoll = 20*time.Millisecond, time.Millisecond

	t.Cleanup(func() { retireBackupWait, retireBackupPoll = savedWait, savedPoll })

	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)

	r := awaitRetireBackup(t.Context(), j)
	if r == nil || r.Reason != retireReasonBackup || !strings.Contains(r.Why, "still running after") {
		t.Fatalf("a backup that never finished: %+v", r)
	}
}

// A JOURNAL WHOSE PHASE CANNOT BE ESTABLISHED SAYS SO EVERY TIME. The first
// run's write may have installed a phase nobody can now read, and a retry over
// the same unreadable journal that answered `nothing` would say this host is
// one where no retirement has happened.
func TestAnUnreadableJournalKeepsSayingThePhaseIsUnknown(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
	advanceRowToIntent(t, f)

	// A mode billet does not write: the reader refuses it as untrusted, which
	// is could-not-tell and never an absence.
	mustOK(t, os.Chmod(retirement.JournalPath(), 0o644))

	for attempt := range 2 {
		out, code := f.request(t, f.input(t, nil))

		m := retireAnswer(t, out)
		if code != exitUnknown || m["reason"] != retireReasonJournal || m["state"] != "unknown" {
			t.Fatalf("attempt %d over an unreadable journal: %s", attempt+1, out)
		}
	}
}

// A `done` JOURNAL IS ANSWERED BEFORE ANY CONFIGURATION IS READ: a server-only
// host at `done` has no configuration at all, and reading one there would
// answer about a missing file instead of the tail that is still owed. This host
// has none, and the tail finishes anyway.
func TestADoneJournalIsAnsweredBeforeAnyConfigurationIsRead(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	j := f.plantJournal(t, retirement.PhaseDone, retirement.VariantServerOnly)
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
	advanceRowToIntent(t, f)

	// The request document and the digest operand are what the role would hand
	// this run; both are built while the configuration is still there, because
	// the role builds them on a host that has one.
	in, digest := f.input(t, nil), f.installedSHA(t)

	// The host as a completed transition leaves it: the identity archived, the
	// configuration removed, the units quiescent, the authority closed at done.
	mustOK(t, os.Rename(f.stateDir, j.Archive))
	mustOK(t, os.Remove(f.cfg))
	mustOK(t, retirement.WriteStatus(retirement.PhaseDone, retirement.VariantServerOnly, retireNow()))
	retiredUnits(t, f)

	out, code := f.run(t, in, "--input", "-", "--run", requestRun, "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor, "--server-only", "--installed-sha256", digest)

	retiredAnswer(t, out, code)

	// WHAT THIS PROVES is that no configuration was NEEDED: moving the
	// ordinary observation ahead of the journal's dispatch fails this test on
	// the missing file. It does not prove that no read of it happened at all,
	// and does not claim to.
	done, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if !done.Settled {
		t.Fatalf("the tail did not finish over a host with no configuration: %+v", done)
	}
}

// AND A REFUSAL PAST THE JOURNAL SAYS WHAT THE JOURNAL ESTABLISHED, whatever
// it is that refuses: a host whose retirement stands at `stopped` is not one
// where nothing has happened, and an operator sent looking for that host would
// find a state it is not in. Two refusals with nothing to do with the journal
// prove it: an archive that cannot be examined, and a ledger nothing can dial.
func TestARefusalPastTheJournalNamesThePhaseTheHostReached(t *testing.T) {
	t.Run("an archive that cannot be examined", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		j := f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
		markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
		advanceRowToIntent(t, f)

		// A regular file where the archive would be: neither a directory that
		// holds the identity nor an absence, so where the identity is cannot
		// be told.
		writeFile(t, j.Archive, "not a directory\n", 0o600)

		out, code := f.request(t, f.input(t, nil))

		m := retireAnswer(t, out)
		if code != exitUnknown || m["reason"] != retireReasonIdentity ||
			m["state"] != string(retirement.PhaseStopped) {
			t.Fatalf("an unexaminable archive: %s", out)
		}
	})

	t.Run("a ledger nothing can dial", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
		markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
		advanceRowToIntent(t, f)
		mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))

		in := f.input(t, nil)

		t.Setenv("BILLET_STATE_DSN", "postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable")

		out, code := f.request(t, in)

		m := retireAnswer(t, out)
		if code != exitUnknown || m["state"] != string(retirement.PhaseStopped) {
			t.Fatalf("an unreachable ledger: %s", out)
		}
	})
}

// A REQUEST THAT RECORDED ITS INTENT AND THEN FAILED SAYS `intent`, not
// `nothing`. The fact the run started with — no journal on this host — is not
// what the host holds once the run has written one, and a refusal that said
// `nothing` would send an operator to a host in another state entirely.
func TestARequestThatRecordedItsIntentAndFailedSaysIntent(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// The status is the intent's last write: the journal and the row are
	// already at `intent` when it fails.
	retirement.Publishing = func(path string) error {
		if path == retirement.StatusPath() {
			return errors.New("the status could not be published")
		}

		return nil
	}

	t.Cleanup(func() { retirement.Publishing = nil })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonStatus || m["state"] != string(retirement.PhaseIntent) {
		t.Fatalf("a request that recorded its intent and then failed: %s", out)
	}

	j, presence, err := retirement.ReadJournal()
	if err != nil || presence != retirement.JournalPresent || j.Phase != retirement.PhaseIntent {
		t.Fatalf("the journal: %+v %d %v", j, presence, err)
	}
}

// AND A DRY RUN READS THE JOURNAL BEFORE THE CONFIGURATION, saying what it
// found: a preview describes a REQUEST, and a host with a transition under way
// may have no configuration left to read, so judging that first answers about
// the wrong thing.
func TestADryRunJudgesTheJournalFirstAndNamesItsPhase(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
	advanceRowToIntent(t, f)

	in, digest := f.input(t, nil), f.installedSHA(t)
	before := f.guard.record(t)

	// The configuration is gone, as a retiring host's eventually is: a preview
	// that read it first would answer about a missing file.
	mustOK(t, os.Remove(f.cfg))

	out, code := f.run(t, in, "--input", "-", "--run", requestRun, "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor, "--server-only", "--installed-sha256", digest, "--dry-run")

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonPhase || m["state"] != string(retirement.PhaseStopped) ||
		!strings.Contains(whyOf(m), "a dry run describes a request") {
		t.Fatalf("a preview over a transition under way: %s", out)
	}

	// AND IT TOOK NOTHING: the guard's record is the same record, field for
	// field, and the authority status it might have published is still absent.
	if after := f.guard.record(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("the dry run changed the guard's record:\n%+v\n%+v", before, after)
	}

	if _, presence, err := retirement.ReadStatus(); presence != retirement.StatusAbsent || err != nil {
		t.Fatalf("the dry run published a status: %d %v", presence, err)
	}
}

// AND A JOURNAL THAT CANNOT BE READ WHEN THE REFUSAL IS MADE IS `unknown`: the
// run wrote one, so `nothing` is false, and what phase it now holds is exactly
// what could not be established.
func TestARefusalOverAnUnreadableJournalSaysUnknown(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	retirement.Publishing = func(path string) error {
		if path != retirement.StatusPath() {
			return nil
		}

		// The journal is written by now, and its mode moves under the refusal
		// that is about to be made: a mode billet does not write is untrusted,
		// which is could-not-tell and never an absence.
		mustOK(t, os.Chmod(retirement.JournalPath(), 0o644))

		return errors.New("the status could not be published")
	}

	t.Cleanup(func() { retirement.Publishing = nil })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["state"] != "unknown" {
		t.Fatalf("a refusal over a journal nothing could read: %s", out)
	}
}

// A REFUSAL THAT LOOKED AT NOTHING SAYS SO. The lock, the guard and the input
// are judged before any journal is read, so such a refusal cannot state that
// no retirement is under way here — which is what `nothing` states, and which
// on a host at `stopped` is false.
func TestARefusalBeforeAnythingIsReadSaysUnknown(t *testing.T) {
	t.Run("the transaction lock is another run's", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)

		// Another process holds the host's transaction lock.
		held, err := takeTxLock()
		mustOK(t, err)

		t.Cleanup(func() { held.release() })

		out, code := f.request(t, f.input(t, nil))

		// THE OUTCOME AND THE STATE ARE DIFFERENT AXES: this is a refusal an
		// operator can act on (exit 2), and what the host holds is what it
		// could not establish.
		m := retireAnswer(t, out)
		if code != exitRefused || m["reason"] != retireReasonLock || m["state"] != "unknown" {
			t.Fatalf("a refusal that took nothing: %s", out)
		}
	})

	t.Run("the input does not decode", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)

		out, code := f.request(t, "{not json")

		m := retireAnswer(t, out)
		if code != exitRefused || m["reason"] != retireReasonInput || m["state"] != "unknown" {
			t.Fatalf("a refusal that read no journal: %s", out)
		}
	})
}

// AND A TRANSITION'S OWN REFUSAL TAKES THE SAME ANNOTATION: the driver's
// remembered phase is not what the host holds once its journal has gone or
// become unreadable under it. A record that VANISHED mid-transition is not
// `nothing` either — that word says no retirement is under way here, over a
// host whose server this run has already stopped.
func TestATransitionRefusalOverAJournalItCannotReadSaysUnknown(t *testing.T) {
	for name, disturb := range map[string]func(t *testing.T){
		"a journal a stranger's mode makes untrusted": func(t *testing.T) {
			t.Helper()
			mustOK(t, os.Chmod(retirement.JournalPath(), 0o644))
		},
		"a journal that has gone": func(t *testing.T) {
			t.Helper()
			mustOK(t, os.Remove(retirement.JournalPath()))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)

			statuses := 0

			retirement.Publishing = func(path string) error {
				if path != retirement.StatusPath() {
					return nil
				}

				statuses++

				// The SECOND status is the stop's: by then the intent's journal
				// is on disk, and it is disturbed under the refusal about to be
				// made.
				if statuses < 2 {
					return nil
				}

				disturb(t)

				return errors.New("the status could not be published")
			}

			t.Cleanup(func() { retirement.Publishing = nil })

			out, code := f.request(t, f.input(t, nil))

			m := retireAnswer(t, out)
			if code != exitUnknown || m["reason"] != retireReasonStatus || m["state"] != "unknown" {
				t.Fatalf("a transition refusal over a journal it cannot read: %s", out)
			}
		})
	}
}

// EVERY ANSWER THE REQUEST GIVES PASSES THROUGH ONE EXIT that says what the
// host holds, so no path can be added that forgets: a refusal made before the
// flags were agreed on, a status left beside no journal, and a transition
// whose terminal answer is no more exempt than a failed one. The preview's
// corners are beside the preview's own rules, below.
func TestTheRequestsAnswerAlwaysSaysWhatTheHostHolds(t *testing.T) {
	t.Run("a flag refusal establishes nothing", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)

		// `--input -` with an operand the flag table refuses, on a host whose
		// retirement stands at `stopped`.
		out, code := f.run(t, "", "--input", "-", "--run", requestRun, "--retiring-host", requestRetiring,
			"--survivor-host", requestSurvivor, "--server-only", "--installed-sha256", "not-a-digest")

		m := retireAnswer(t, out)
		if code != exitRefused || m["reason"] != retireReasonCombination || m["state"] != "unknown" {
			t.Fatalf("a flag refusal: %s", out)
		}
	})

	t.Run("a status this host published beside no journal", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
		markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
		advanceRowToIntent(t, f)
		mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))

		in := f.input(t, nil)

		// THE RECORD IS GONE AND THE AUTHORITY IT CLOSED IS STILL CLOSED: the
		// run meets the status like any other writer, and the answer must not
		// say that no retirement is under way on a host whose server a
		// transition has already stopped.
		mustOK(t, os.Remove(retirement.JournalPath()))

		out, code := f.request(t, in)

		m := retireAnswer(t, out)
		if code != exitUnknown || m["reason"] != retireReasonIdentity || m["state"] != "unknown" {
			t.Fatalf("an orphan status: %s", out)
		}
	})

	t.Run("the transition's terminal answer", func(t *testing.T) {
		f := newRequestFixture(t)
		f.reserve(t)

		// The journal goes at the flush that FOLLOWS the tail's last write,
		// which is the one moment nothing writes it again: the run answers a
		// retirement it carried to done and settled, over a host that holds no
		// record of it.
		retirement.SyncingDir = func(string) error {
			j, presence, err := retirement.ReadJournal()
			mustOK(t, err)

			if presence == retirement.JournalPresent && j.Settled {
				mustOK(t, os.Remove(retirement.JournalPath()))
			}

			return nil
		}

		t.Cleanup(func() { retirement.SyncingDir = nil })

		out, code := f.request(t, f.input(t, nil))

		// THE SUCCESS IS AS BOUND BY THE RULE AS THE FAILURES: the run
		// carried the retirement to done and settled it, and what it says the
		// host holds is read when it answers, so a journal that has gone is
		// `unknown` and not the phase this run remembers.
		m := retireAnswer(t, out)
		if code != 0 || m["outcome"] != retireOutcomeRetired || m["state"] != "unknown" {
			t.Fatalf("the transition's terminal answer over a journal that has gone: %s", out)
		}
	})
}

// A PREVIEW REQUIRES BOTH ABSENCES, the journal's and the published authority
// status's, because it exists to say what a REQUEST would do and a mutating
// run meets the status before it reaches anything a preview describes. A
// status beside no journal, and one that cannot be read, are each could-not-
// tell — never a reported eligibility.
func TestAPreviewRequiresTheStatusAbsentToo(t *testing.T) {
	for name, stage := range map[string]func(t *testing.T){
		"a status this host published": func(t *testing.T) {
			t.Helper()
			mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, retirement.VariantServerOnly, retireNow()))
		},
		"a status nothing can parse": func(t *testing.T) {
			t.Helper()
			writeFile(t, retirement.StatusPath(), "{not json\n", 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)

			stage(t)

			out, code := f.request(t, f.input(t, nil), "--dry-run")

			m := retireAnswer(t, out)
			if code != exitUnknown || m["reason"] != retireReasonStatus || m["state"] != "unknown" {
				t.Fatalf("a preview over an authority it cannot account for: %s", out)
			}
		})
	}
}

// A PREVIEW THAT SUCCEEDS SAYS WHAT THE HOST HOLDS TOO. It takes no lock, so a
// mutating run can publish `intent` while it reads, and a report carrying the
// `nothing` it was built with would tell the role this host is free when a
// transition owns it. A server-only request is the case that reaches success
// there, because it stages nothing for a later check to refuse on.
func TestAPreviewThatSucceedsSaysWhatTheHostHolds(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	// The journal appears while the preview is judging the node's endpoint,
	// which is the last thing it reads before it reports.
	planted := false

	saved := hostInterfaceAddresses
	hostInterfaceAddresses = func() ([]hostAddress, error) {
		if !planted {
			planted = true

			// A SERVER-ONLY JOURNAL, which stages nothing: no later check of
			// this preview's own refuses on it, so the run reaches success.
			f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)
		}

		return saved()
	}

	t.Cleanup(func() { hostInterfaceAddresses = saved })

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)), "--dry-run")

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeReported {
		t.Fatalf("the preview: %s", out)
	}

	if m["state"] != string(retirement.PhaseIntent) {
		t.Fatalf("a preview reported a free host while a transition owned it: %s", out)
	}
}

// AND A PREVIEW NEVER TELLS AN OPERATOR TO MOVE A STAGE IT CANNOT ESTABLISH IS
// ORPHANED: it holds neither the transaction lock nor the guard, so a journal
// and the stage it owns can appear between its own two reads, and the advice
// that fits an orphan would take away the bytes a live transition installs.
func TestAPreviewDoesNotOfferToMoveALiveStage(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	// A retained-node journal owns a stage, and both appear while the preview
	// is judging the node's endpoint.
	saved := hostInterfaceAddresses
	hostInterfaceAddresses = func() ([]hostAddress, error) {
		f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)

		return saved()
	}

	t.Cleanup(func() { hostInterfaceAddresses = saved })

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)), "--dry-run")

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonStage ||
		m["state"] != string(retirement.PhaseIntent) {
		t.Fatalf("a preview over a stage it cannot account for: %s", out)
	}

	next, ok := m["next"].(string)
	if !ok {
		t.Fatalf("the answer carries no next: %s", out)
	}

	if strings.Contains(whyOf(m), "move it") || strings.Contains(next, "audit-move") {
		t.Fatalf("a preview offered to move a stage a transition may own: %s", out)
	}

	// AND IT NAMES THE RUN THAT CAN TELL: the request itself, which judges the
	// stage with the transaction lock and the guard held.
	if !strings.Contains(next, "run the request itself") {
		t.Fatalf("a preview left an operator with nowhere to go: %s", out)
	}
}

// AND THE MUTATING RUN'S ADVICE STANDS, because there the judgement means what
// it says: it holds the transaction lock and the guard, so a stage beside no
// journal is an orphan, and moving it to an audit location is what clears it.
func TestTheRequestOffersTheAuditMoveForAnOrphanStage(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// A stage with no journal anywhere: the remainder of an attempt that died
	// between the stage and the journal it would have been recorded in.
	mustOK(t, retirement.WriteStage([]byte("server: {}\n")))

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != exitRefused || m["reason"] != retireReasonStage {
		t.Fatalf("an orphan stage under the lock: %s", out)
	}

	next, ok := m["next"].(string)
	if !ok || !strings.Contains(next, "audit-move") || !strings.Contains(whyOf(m), "beside no journal") {
		t.Fatalf("the refusal does not name the audit move: %s", out)
	}
}
