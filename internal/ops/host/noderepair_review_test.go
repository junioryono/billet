package host

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/retirement"
)

// A REPAIR RUNS UNDER THIS CONVERGE'S GUARD, and only there: another
// converge's guard refuses before anything is stopped or locked, and an action
// with no holder named is refused at its flags. A dry run needs no guard.
func TestTheRepairRunsUnderThisConvergesGuard(t *testing.T) {
	t.Run("another converge's guard", func(t *testing.T) {
		f := newRepairFixture(t)

		o := f.repair(t, "--run", "h2")
		if o.str("outcome") != outcomeRefused || o.str("reason") != endpointReasonGuard || o.str("state") != stateNothing ||
			!strings.Contains(o.str("why"), "the guard names "+repairHolder+", not h2") {
			t.Fatalf("outcome %q reason %q state %q (%s), want a guard refusal with nothing done", o.str("outcome"),
				o.str("reason"), o.str("state"), o.str("why"))
		}

		if got := f.acted(t); len(got) != 0 || f.locks != 0 {
			t.Errorf("a repair under another converge's guard asked systemctl to %q and took %d lifecycle locks", got,
				f.locks)
		}
	})

	t.Run("no holder named", func(t *testing.T) {
		f := newRepairFixture(t)

		o := runEndpoint(t, NodeRepairUnit, "", "--config", f.configPath, "--wait", f.wait)
		if o.str("outcome") != outcomeRefused || o.str("reason") != endpointReasonCombination {
			t.Fatalf("outcome %q reason %q (%s), want the combination refused", o.str("outcome"), o.str("reason"),
				o.str("why"))
		}

		dry := runEndpoint(t, NodeRepairUnit, "", "--config", f.configPath, "--wait", f.wait, "--dry-run")
		if dry.code != 0 || !dry.boolean("required") {
			t.Fatalf("a dry run without a holder answered code %d (%s), want the report", dry.code, dry.str("why"))
		}
	})
}

// A DRY RUN THAT CANNOT READ THE GUARD CANNOT SAY A RETIREMENT IS ABSENT: an
// unreadable record, and a root that cannot be opened, are could-not-tell
// rather than a report that a converge would repair the host.
func TestTheRepairsDryRunCannotTellWithoutTheGuard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T)
		why   string
	}{
		{"a record that cannot be read", func(t *testing.T) {
			t.Helper()
			record := filepath.Join(upgradeRoot, activePointer, guardRecordName)
			body, err := os.ReadFile(record)
			mustOK(t, err)
			mustOK(t, os.WriteFile(record, []byte(strings.Replace(string(body), `"holder":`,
				`"transition":{"kind":"unheard-of","id":"x"},"holder":`, 1)), 0o600))
		}, "guard record could not be read"},
		{"a root that cannot be opened", func(t *testing.T) {
			t.Helper()
			notADirectory := filepath.Join(t.TempDir(), "upgrades")
			writeFile(t, notADirectory, "", 0o600)

			prev := upgradeRoot
			upgradeRoot = notADirectory

			t.Cleanup(func() { upgradeRoot = prev })
		}, "converge guard could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.stage(t)

			o := f.repair(t, "--dry-run")
			if o.str("outcome") != outcomeUnknown || o.str("reason") != endpointReasonGuard ||
				!strings.Contains(o.str("why"), tc.why) {
				t.Fatalf("outcome %q reason %q (%s), want could-not-tell saying %q", o.str("outcome"), o.str("reason"),
					o.str("why"), tc.why)
			}

			if got := f.acted(t); len(got) != 0 || f.locks != 0 {
				t.Errorf("a dry run asked systemctl to %q and took %d lifecycle locks", got, f.locks)
			}
		})
	}
}

// A RETIREMENT ADMITS NO REPAIR, whoever holds the guard: a guard carrying the
// retirement's marker is this converge's (a retirement's holder is a guard's
// holder), and a journal, finished or not, is a host whose node operations the
// retirement admits. Neither stops anything; a journal that cannot be read is
// could-not-tell.
func TestTheRepairRefusesAHostARetirementTouched(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, why, next string
		plant                    func(t *testing.T)
	}{
		{"the guard carries the marker", outcomeRefused, "guard carries retirement " + markerID,
			"finish the retirement", func(t *testing.T) {
				t.Helper()
				markGuard(t, &guardFixture{root: upgradeRoot}, &guardTransition{Kind: transitionRetirement, ID: markerID},
					nil)
			}},
		{"a journal mid-retirement", outcomeRefused, "retirement journal (phase stopped, settled false)",
			"finish the retirement", func(t *testing.T) {
				t.Helper()
				plantJournal(t, retirement.PhaseStopped, repairHolder, false)
			}},
		{"a settled retained host", outcomeRefused, "retirement journal (phase done, settled true)",
			"follow the manual repair", func(t *testing.T) {
				t.Helper()
				plantJournal(t, retirement.PhaseDone, repairHolder, true)
			}},
		{"a journal that cannot be read", outcomeUnknown, "the retirement journal could not be judged",
			"", func(t *testing.T) {
				t.Helper()
				mustOK(t, os.MkdirAll(retirement.JournalPath(), 0o700))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.plant(t)

			// THE DRY RUN PREDICTS THE ACTION'S REFUSAL, so a converge's check mode
			// never says it would repair a host the action refuses.
			for _, args := range [][]string{{"--dry-run"}, nil} {
				o := f.repair(t, args...)
				if o.str("outcome") != tc.outcome || o.str("reason") != endpointReasonRetirement ||
					o.str("state") != stateNothing || !strings.Contains(o.str("why"), tc.why) ||
					!strings.HasPrefix(o.str("next"), tc.next) {
					t.Fatalf("%q: outcome %q reason %q state %q (%s, next %q), want %s for the retirement saying %q "+
						"and next %q", args, o.str("outcome"), o.str("reason"), o.str("state"), o.str("why"),
						o.str("next"), tc.outcome, tc.why, tc.next)
				}
			}

			if got := f.acted(t); len(got) != 0 || f.locks != 0 {
				t.Errorf("a repair under a retirement asked systemctl to %q and took %d lifecycle locks", got, f.locks)
			}
		})
	}
}

// THE LIFECYCLE LOCK IS TAKEN BY THE ACTION, and only by the action: a dry run
// and a repair that is not required take none.
func TestTheRepairTakesTheLifecycleLockOnlyToAct(t *testing.T) {
	f := newRepairFixture(t)

	if o := f.repair(t, "--dry-run"); o.code != 0 || f.locks != 0 {
		t.Fatalf("the dry run answered code %d and took %d lifecycle locks, want none", o.code, f.locks)
	}

	if o := f.repair(t); o.str("outcome") != outcomeRepaired || f.locks != 1 {
		t.Fatalf("the repair answered %q (%s) and took %d lifecycle locks, want one", o.str("outcome"), o.str("why"),
			f.locks)
	}
}

// ONLY A NODE POSITIVELY OF A RELEASE THAT PUBLISHES IS REPAIRED: a release
// before the guard publishes nothing under any unit, and a process that cannot
// be identified is no premise for a stop.
func TestTheRepairNeedsAProcessThatPublishes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		stage           func(t *testing.T, f *repairFixture)
		outcome, reason string
	}{
		{"a release before the guard", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.preRBinary(t)
		}, outcomeRefused, endpointReasonPreR},
		{"a managed binary that fails before the guard answers", func(t *testing.T, f *repairFixture) {
			t.Helper()
			script := "#!/bin/sh\necho 'panic: runtime error' >&2\nexit 2\n"
			writeFile(t, installedBinary, script, 0o755)
			writeFile(t, filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image"), script, 0o755)
		}, outcomeUnknown, endpointReasonProcess},
		{"an image that is not the managed binary", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image"), "#!/bin/sh\nexit 0\n", 0o755)
		}, outcomeUnknown, endpointReasonProcess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.stage(t, f)

			for _, extra := range [][]string{{"--dry-run"}, nil} {
				o := f.repair(t, extra...)
				if o.str("outcome") != tc.outcome || o.str("reason") != tc.reason || o.str("state") != stateNothing {
					t.Fatalf("%v: outcome %q reason %q state %q (%s), want %s/%s with nothing done", extra,
						o.str("outcome"), o.str("reason"), o.str("state"), o.str("why"), tc.outcome, tc.reason)
				}
			}

			if got := f.acted(t); len(got) != 0 {
				t.Errorf("a repair over such a process asked systemctl to %q", got)
			}
		})
	}
}

// WHAT CHANGED BETWEEN THE JUDGEMENT AND THE LOCKS IS NOT STOPPED OVER: the
// judgement is made again under the locks, and a process that moved, or a
// record that appeared, refuses with nothing done.
func TestTheRepairJudgesAgainUnderTheLocks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, f *repairFixture)
		why   string
		// outcome is the one outcome the refusal must have, or "" for any but repaired.
		outcome string
	}{
		{"the process moved", func(t *testing.T, f *repairFixture) {
			t.Helper()
			// The first judgement reads the unit three times: its shape and the
			// two observations of the record's bracket.
			body, err := os.ReadFile(f.unitFile(nodeUnit))
			mustOK(t, err)
			f.afterShows(t, 3, strings.ReplaceAll(string(body), "InvocationID="+nodeInvocation,
				"InvocationID="+newInvocation))
		}, "between the judgement and the locks", outcomeUnknown},
		{"the node unit was reloaded with the directory", func(t *testing.T, f *repairFixture) {
			t.Helper()
			body, err := os.ReadFile(f.unitFile(nodeUnit))
			mustOK(t, err)
			f.afterShows(t, 3, strings.Replace(string(body), "RuntimeDirectory=billet/locks\n",
				"RuntimeDirectory=billet/locks billet/registration\n", 1))
		}, "between the judgement and the locks", outcomeUnknown},
		{"the unit gained a drop-in after the second judgement", func(t *testing.T, f *repairFixture) {
			t.Helper()
			// Each judgement reads the unit three times and the stop's admission
			// once: the seventh read is the admission, and only it sees the
			// drop-in, so the shape is judged on the read the process is.
			body, err := os.ReadFile(f.unitFile(nodeUnit))
			mustOK(t, err)
			f.showsBetween(t, 6, 7, strings.Replace(string(body), "DropInPaths=\n",
				"DropInPaths=/etc/systemd/system/billet-node.service.d/x.conf\n", 1))
		}, "drop-ins", outcomeRefused},
		{"the unit was reloaded with the directory after the second judgement", func(t *testing.T, f *repairFixture) {
			t.Helper()
			body, err := os.ReadFile(f.unitFile(nodeUnit))
			mustOK(t, err)
			f.showsBetween(t, 6, 7, strings.Replace(string(body), "RuntimeDirectory=billet/locks\n",
				"RuntimeDirectory=billet/locks billet/registration\n", 1))
		}, "changed under the judgement", outcomeUnknown},
		{"the stop's admission answered no DropInPaths", func(t *testing.T, f *repairFixture) {
			t.Helper()
			body, err := os.ReadFile(f.unitFile(nodeUnit))
			mustOK(t, err)
			f.showsBetween(t, 6, 7, strings.Replace(string(body), "DropInPaths=\n", "", 1))
		}, "answered no DropInPaths", outcomeUnknown},
		{"systemd restarted the node after the second judgement", func(t *testing.T, f *repairFixture) {
			t.Helper()
			body, err := os.ReadFile(f.unitFile(nodeUnit))
			mustOK(t, err)
			f.afterShows(t, 6, strings.ReplaceAll(strings.ReplaceAll(string(body), "InvocationID="+nodeInvocation,
				"InvocationID="+newInvocation), "MainPID="+strconv.Itoa(inspectPID), "MainPID="+
				strconv.Itoa(inspectPID+7)))
		}, "moved between the judgement and the stop", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.stage(t, f)

			o := f.repair(t)
			if o.str("outcome") == outcomeRepaired || (tc.outcome != "" && o.str("outcome") != tc.outcome) ||
				o.str("state") != stateNothing || !strings.Contains(o.str("why"), tc.why) {
				t.Fatalf("outcome %q state %q (%s), want a refusal saying %q with nothing done", o.str("outcome"),
					o.str("state"), o.str("why"), tc.why)
			}

			// UNDER THE LOCKS: the refusal is the later look's, not the first
			// judgement's.
			if f.locks != 1 {
				t.Errorf("the repair took %d lifecycle locks, want the one it refused under", f.locks)
			}

			if got := f.acted(t); len(got) != 0 || f.unitOnDisk(t) != f.legacy {
				t.Errorf("a repair whose premise moved asked systemctl to %q or changed the unit", got)
			}
		})
	}
}

// A KILLMODE UNDER WHICH A STOP PROVES NOTHING IS REFUSED BEFORE THE STOP, as
// the migration refuses it: the stop's admission is the shared one.
func TestTheRepairAdmitsItsStopAsTheMigrationDoes(t *testing.T) {
	f := newRepairFixture(t)
	f.set(t, nodeUnit, "KillMode", "process")

	o := f.repair(t)
	if o.str("outcome") != outcomeRefused || o.str("reason") != endpointReasonPolicy || o.str("state") != stateNothing ||
		!strings.Contains(o.str("why"), "only mixed or control-group is repaired") {
		t.Fatalf("outcome %q reason %q state %q (%s), want the policy refused with nothing done", o.str("outcome"),
			o.str("reason"), o.str("state"), o.str("why"))
	}

	if got := f.acted(t); len(got) != 0 {
		t.Errorf("a refused stop asked systemctl to %q", got)
	}
}

// AN UNANSWERED PROPERTY IS COULD-NOT-TELL: a DropInPaths systemd did not give
// is not "no drop-ins".
func TestTheRepairNeedsEveryShapePropertyAnswered(t *testing.T) {
	f := newRepairFixture(t)

	body, err := os.ReadFile(f.unitFile(nodeUnit))
	mustOK(t, err)
	writeFile(t, f.unitFile(nodeUnit), strings.Replace(string(body), "DropInPaths=\n", "", 1), 0o644)

	o := f.repair(t)
	if o.str("outcome") != outcomeUnknown || o.str("state") != stateNothing ||
		!strings.Contains(o.str("why"), "answered no DropInPaths") {
		t.Fatalf("outcome %q state %q (%s), want could-not-tell naming DropInPaths", o.str("outcome"), o.str("state"),
			o.str("why"))
	}

	if got := f.acted(t); len(got) != 0 {
		t.Errorf("a repair over an unanswered property asked systemctl to %q", got)
	}
}

// THE LATER STEPS SAY WHAT THEY LEFT. The repair's own stop deadline leaves the
// stop to systemd and claims nothing done; a configuration replaced under the
// stop leaves the node stopped with the repaired unit installed and does not
// start it; and an install whose rename landed but whose directory sync failed
// names the unit installed.
func TestTheRepairsLaterStepsSayWhatTheyLeft(t *testing.T) {
	t.Run("the repair's own stop deadline", func(t *testing.T) {
		f := newRepairFixture(t)
		writeFile(t, f.unitFile("stop.mode"), "hang\n", 0o644)

		o := f.repair(t, "--stop-timeout", "300ms")
		if o.str("outcome") != outcomeUnknown || o.str("reason") != endpointReasonUnproved ||
			o.str("state") != stateNothing || !strings.Contains(o.str("why"), "did not complete within the repair's deadline") {
			t.Fatalf("outcome %q reason %q state %q (%s), want the deadline named", o.str("outcome"), o.str("reason"),
				o.str("state"), o.str("why"))
		}

		if f.unitOnDisk(t) != f.legacy {
			t.Error("a stop that was not proved was followed by the install")
		}
	})

	t.Run("a configuration replaced under the stop", func(t *testing.T) {
		f := newRepairFixture(t)
		writeFile(t, f.unitFile("stop.mode"), "replace-config\n", 0o644)

		o := f.repair(t)
		if o.str("outcome") != outcomeUnknown || o.str("reason") != endpointReasonConfig ||
			o.str("state") != stateUnitInstalled {
			t.Fatalf("outcome %q reason %q state %q (%s), want the configuration named with the unit installed",
				o.str("outcome"), o.str("reason"), o.str("state"), o.str("why"))
		}

		for _, verb := range f.acted(t) {
			if verb == "start" {
				t.Fatal("the node was started on a configuration nobody judged")
			}
		}
	})

	t.Run("a directory sync that failed after the rename", func(t *testing.T) {
		f := newRepairFixture(t)

		prev := repairInstaller
		repairInstaller = durablefile.Installer{SyncDir: func(string) error { return errors.New("injected sync failure") }}

		t.Cleanup(func() { repairInstaller = prev })

		o := f.repair(t)
		if o.str("outcome") != outcomeUnknown || o.str("state") != stateUnitInstalled ||
			!strings.Contains(o.str("why"), "durability is unproved") {
			t.Fatalf("outcome %q state %q (%s), want the unit named installed", o.str("outcome"), o.str("state"),
				o.str("why"))
		}
	})
}

// THE OWNERSHIP RULE ITSELF: a unit file root does not own is refused. Run as
// another account, since a test as root owns every file it makes.
func TestARepairUnitRootDoesNotOwnIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root every file this test makes is root's")
	}

	path := filepath.Join(t.TempDir(), "billet-node.service")
	writeFile(t, path, "[Service]\n", 0o644)

	info, err := os.Stat(path)
	mustOK(t, err)

	if err := rootOwnedUnit(info); err == nil || !strings.Contains(err.Error(), "is not owned by root") {
		t.Fatalf("a unit this account owns was admitted as root's: %v", err)
	}
}
