package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		Provenance: retirement.Provenance{ReservingHolder: requestRun, TransitionID: retireTestID,
			Reservation: retireNow().UTC().Format(time.RFC3339Nano), Deployment: f.identity, Retiring: requestRetiring,
			Survivor: requestSurvivor},
		Ownership: retirement.Ownership{Owner: requestRun},
	}

	if variant == retirement.VariantRetainedNode {
		body := []byte(f.rendering(t))
		mustOK(t, retirement.WriteStage(body))

		j.StagedSHA256, j.Config = retirement.Digest(body), "present"
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

	m := retireAnswer(t, out)
	if code != exitUnknown || m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("the transition: %s", out)
	}

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

	// THE ROW IS STILL AT INTENT: the tail is what completes it, and nothing
	// here pretends the survivor has been told.
	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementIntent {
			t.Fatalf("the row: %+v (present %v)", r, present)
		}
	})
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
			writeRegistrationRecord(t, record, f.identity, retainedEndpoint)
		}
	}

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("the transition: %s", out)
	}

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
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	useRegistrationRecord(t)

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonPhase || m["state"] != string(retirement.PhaseNodeRestarted) {
		t.Fatalf("a node with no record: %s", out)
	}

	if !strings.Contains(whyOf(m), "node_unit") {
		t.Fatalf("the refusal does not name the fact that refused: %s", out)
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
						writeRegistrationRecord(t, record, f.identity, retainedEndpoint)
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
			unit: "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nMainPID=99\n",
			resolve: "LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\n" +
				"ExecMainCode=1\nExecMainStatus=0\n",
		},
		"the refusal this retirement caused is cleared": {
			unit: "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nMainPID=0\n" +
				"ExecMainCode=1\nExecMainStatus=6\nExecMainStartTimestamp=Fri 2026-09-11 10:00:30 UTC\n",
			resolve:   "LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\n",
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
			writeFile(t, unit, c.unit, 0o644)

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

			// A BACKUP THAT FINISHES ON ITS OWN does so WHILE THE WAIT RUNS:
			// the resolution is written once the transition has actually asked
			// about the unit, so the case stages a wait rather than a delay.
			waited := make(chan struct{})

			if c.resolve != "" && !c.reconcile {
				go func() {
					defer close(waited)

					for {
						if strings.Contains(readIfAny(filepath.Join(f.unitsDir, ".asked")), backupServiceUnit) {
							writeFile(t, unit, c.resolve, 0o644)

							return
						}

						time.Sleep(time.Millisecond)
					}
				}()
			} else {
				close(waited)
			}

			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)

			_, _, r := f.drive(t, j)

			<-waited

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
			writeRegistrationRecord(t, record, f.identity, retainedEndpoint)
		}
	}

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	if m := retireAnswer(t, out); code != exitUnknown || m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("the transition: %s", out)
	}

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

// useRegistrationRecord points the node's runtime record at a path this test
// owns, and admits the file it writes: the reader requires a root-owned 0600
// record, which no test process can create.
func useRegistrationRecord(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "registration")
	mustOK(t, os.MkdirAll(dir, 0o700))

	path := filepath.Join(dir, "current")

	savedPath, savedOpen := registrationRecordPath, registrationOpen
	registrationRecordPath = path
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

	t.Cleanup(func() { registrationRecordPath, registrationOpen = savedPath, savedOpen })

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
// registers, for this deployment and the endpoint its configuration names.
func writeRegistrationRecord(t *testing.T, path, deployment, endpoint string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"schema": 1, "node": "node-a", "deployment": deployment, "incarnation": retainedIncarnation,
		"invocation_id": "0123456789abcdef0123456789abcdef", "endpoint": endpoint,
		"registered_at": "2026-09-11T10:00:00Z",
	})
	mustOK(t, err)

	writeFile(t, path, string(body), 0o600)
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

// THE COMMAND'S RESUME ENDS WHERE THE HOST STOPS HOLDING WHAT THE REQUEST
// READ: past the archive there is no configured identity directory and, on a
// server-only host, no configuration at all, and the reader that works from
// the journal's locator alone is the tail's. The driver itself goes on working
// there, which is what the resume table above proves.
func TestACommandResumePastTheArchiveIsRefusedByName(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	j := f.plantJournal(t, retirement.PhaseArchived, retirement.VariantServerOnly)
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
	advanceRowToIntent(t, f)

	mustOK(t, os.Rename(f.stateDir, j.Archive))

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != exitUnknown || m["reason"] != retireReasonPhase || !strings.Contains(whyOf(m), "past the archive") {
		t.Fatalf("the resume: %s", out)
	}

	if len(f.svc.trace) != 0 {
		t.Fatalf("a refused resume touched the host: %v", f.svc.trace)
	}

	// AND THE JOURNAL IS LEFT WHERE IT STANDS.
	after, _, err := retirement.ReadJournal()
	mustOK(t, err)

	if after.Phase != retirement.PhaseArchived {
		t.Fatalf("the refused resume moved the journal to %s", after.Phase)
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
