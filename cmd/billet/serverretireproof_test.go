package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// retireServiceManager keeps systemd's readbacks coherent with the operations
// the fixture performs. Hooks may replace those observations; it judges none.
type retireServiceManager struct {
	*fakeConverger
	t          *testing.T
	unitsDir   string
	operations []string
	onEnable   func(string)
	onDisable  func(string)
	onSubmit   func(string)
}

func (s *retireServiceManager) Enable(ctx context.Context, unit string) error {
	if s.onSubmit != nil {
		s.onSubmit("enable " + unit)
	}
	s.operations = append(s.operations, "enable "+unit)
	err := s.fakeConverger.Enable(ctx, unit)
	if err == nil {
		s.set(unit, "UnitFileState", s.enabled[unit])
		if also := s.alsoEnables[unit]; also != "" {
			s.set(also, "UnitFileState", s.enabled[also])
		}
	}
	if s.onEnable != nil {
		s.onEnable(unit)
	}

	return err
}

func (s *retireServiceManager) EnabledNow(ctx context.Context, unit string) (lifeops.Enablement, error) {
	s.operations = append(s.operations, "read enablement "+unit)

	return s.fakeConverger.EnabledNow(ctx, unit)
}

func (s *retireServiceManager) StopAndProve(ctx context.Context, unit string) (lifeops.StopResult, error) {
	if s.onSubmit != nil {
		s.onSubmit("stop " + unit)
	}
	s.operations = append(s.operations, "stop "+unit)
	s.set(unit, "ActiveState", "inactive")
	s.set(unit, "SubState", "dead")
	s.set(unit, "Result", "success")
	s.set(unit, "MainPID", "0")

	return s.fakeConverger.StopAndProve(ctx, unit)
}

func (s *retireServiceManager) StartAndProve(ctx context.Context, unit string) (string, error) {
	if s.onSubmit != nil {
		s.onSubmit("start " + unit)
	}
	s.operations = append(s.operations, "start "+unit)
	s.set(unit, "ActiveState", "active")
	s.set(unit, "SubState", "running")
	s.set(unit, "MainPID", "4242")
	if unit == nodeUnit {
		// The manager starts a new invocation even if the node never registers.
		s.set(unit, "InvocationID", retainedRestartInvocation)
	}

	return s.fakeConverger.StartAndProve(ctx, unit)
}

func (s *retireServiceManager) Disable(ctx context.Context, unit string) error {
	if s.onSubmit != nil {
		s.onSubmit("disable " + unit)
	}
	s.operations = append(s.operations, "disable "+unit)
	err := s.fakeConverger.Disable(ctx, unit)
	if err == nil {
		s.set(unit, "UnitFileState", "disabled")
	}
	if s.onDisable != nil {
		s.onDisable(unit)
	}

	return err
}

// The retirement helper tests use the production timer observation and actual
// command submission. Only the manager's returned state and completion hooks
// are faked, so a query hidden inside a helper remains observable.
func (s *retireServiceManager) admittedConverger() *lifeops.Converger {
	return lifeops.NewConverger(lifeops.NewInspector(lifeops.WithSystemctl(systemctlBinary), managerRunnerOption(),
		lifeops.WithObserver(func(_ context.Context, args []string) {
			if args[0] == "show" {
				noteRetireMutation("wait", "manager observation")
				return
			}
			command := args[0] + " " + args[len(args)-1]
			if s.onSubmit != nil {
				s.onSubmit(command)
			}
			s.operations = append(s.operations, command)
		})))
}

func (s *retireServiceManager) StopAndProveAdmitted(ctx context.Context, unit string, admit func() error) (lifeops.StopResult, error) {
	result, err := s.admittedConverger().StopAndProveAdmitted(ctx, unit, admit)
	if err != nil {
		return result, err
	}
	return s.fakeConverger.StopAndProve(ctx, unit)
}

func (s *retireServiceManager) DisableAdmitted(ctx context.Context, unit string, admit func() error) error {
	if err := s.admittedConverger().DisableAdmitted(ctx, unit, admit); err != nil {
		return err
	}
	if err := s.fakeConverger.Disable(ctx, unit); err != nil {
		return err
	}
	if s.onDisable != nil {
		s.onDisable(unit)
	}
	return nil
}

func (s *retireServiceManager) set(unit, key, value string) {
	s.t.Helper()
	setRetireUnitProperty(s.t, filepath.Join(s.unitsDir, unit), key, value)
}

// An empty value removes the property, distinguishing an unanswered Result
// from a successful stop even when the converger positively proved disappearance.
func setRetireUnitProperty(t *testing.T, path, key, value string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(mustRead(t, path), "\n"), "\n")
	lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(line, key+"=") })
	if value != "" {
		lines = append(lines, key+"="+value)
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n", 0o644)
}

func retireProofMode(f *requestFixture) retireMode {
	return retireMode{configPath: f.cfg, run: requestRun, retiringHost: requestRetiring}
}

func requireRetireJournal(t *testing.T) retirement.Journal {
	t.Helper()
	j, presence, err := retirement.ReadJournal()
	mustOK(t, err)
	if presence != retirement.JournalPresent {
		t.Fatalf("journal presence: %v", presence)
	}

	return j
}

func requireRetireProofRefusal(t *testing.T, r *retireRefusal, why string) {
	t.Helper()
	if r == nil || r.Reason != retireReasonPostcondition || !strings.Contains(r.Why, why) {
		t.Fatalf("want the %q postcondition refusal, got %+v", why, r)
	}
}

// The converger's positive disappearance answer must not authorize a restart
// after an unsuccessful drain. Removing the caller's predicate advances the
// journal and starts the node after a failed or incomplete observation. The
// unreadable case independently requires the post-stop read to succeed.
func TestRetirementRequiresASuccessfulNodeStop(t *testing.T) {
	cases := []struct {
		name, active, sub, result string
		unreadable                bool
	}{
		{name: "successful drain", active: "inactive", sub: "dead", result: "success"},
		{name: "failed", active: "failed", sub: "failed", result: "exit-code"},
		{name: "timeout", active: "inactive", sub: "dead", result: "timeout"},
		{name: "missing result", active: "inactive", sub: "dead"},
		{name: "missing substate", active: "inactive", result: "success"},
		{name: "still deactivating", active: "deactivating", sub: "stop-sigterm", result: "success"},
		{name: "unreadable", unreadable: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			record := useRegistrationRecord(t)
			f.svc.onStart = func(unit string) {
				if unit == nodeUnit {
					restartedNode(t, f, record, retainedEndpoint)
				}
			}
			f.svc.onStop = func(unit string) {
				if unit != nodeUnit {
					return
				}
				if c.unreadable {
					systemctlBinary = filepath.Join(t.TempDir(), "no-systemctl")
					return
				}
				f.manager.set(unit, "ActiveState", c.active)
				f.manager.set(unit, "SubState", c.sub)
				f.manager.set(unit, "Result", c.result)
			}

			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			if c.name == "successful drain" {
				retiredAnswer(t, out, code)
				if !slices.Contains(f.svc.trace, "start "+nodeUnit) {
					t.Fatal("successful drain never restarted the node")
				}
				return
			}
			m := retireAnswer(t, out)
			why := "only inactive/dead/success permits restart"
			if c.unreadable {
				why = "after the node stop: ask systemd"
			}
			if code != exitUnknown || m["reason"] != retireReasonRestart || !strings.Contains(whyOf(m), why) ||
				m["state"] != string(retirement.PhaseConfigRewritten) {
				t.Fatalf("unsuccessful stop: %s", out)
			}
			if !slices.Contains(f.svc.trace, "stop "+nodeUnit) || slices.Contains(f.svc.trace, "start "+nodeUnit) {
				t.Fatalf("unsuccessful stop restarted the node: %v", f.svc.trace)
			}
			j := requireRetireJournal(t)
			if j.Phase != retirement.PhaseConfigRewritten || j.DoneAt != "" || j.RowDone || j.Settled {
				t.Fatalf("unsuccessful stop advanced retirement: %+v", j)
			}
			st, presence, err := retirement.ReadStatus()
			mustOK(t, err)
			if presence != retirement.StatusPresent || st.Phase != retirement.PhaseStopped {
				t.Fatalf("unsuccessful stop published done: %+v %v", st, presence)
			}
		})
	}
}

// A final done refusal cannot mask removing the early enablement observation:
// the operation sequence itself must end at the controller readback.
func TestRetirementRefusesCollateralEnablementBeforeDisturbingTheNode(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	f.svc.alsoEnables = map[string]string{nodeUnit: serverUnit}
	record := useRegistrationRecord(t)
	f.svc.onStart = func(unit string) {
		if unit == nodeUnit {
			restartedNode(t, f, record, retainedEndpoint)
		}
	}
	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	m := retireAnswer(t, out)
	start := slices.Index(f.manager.operations, "enable "+nodeUnit)
	want := []string{"enable " + nodeUnit, "read enablement " + nodeUnit, "read enablement " + serverUnit}
	if start < 0 || !slices.Equal(f.manager.operations[start:], want) {
		t.Errorf("collateral enablement did not end at its readbacks: %v", f.manager.operations)
	}
	for _, forbidden := range []string{"stop " + nodeUnit, "start " + nodeUnit} {
		if slices.Contains(f.manager.operations, forbidden) {
			t.Errorf("collateral enablement reached %s", forbidden)
		}
	}
	j := requireRetireJournal(t)
	if j.Phase != retirement.PhaseConfigRewritten || j.DoneAt != "" || j.RowDone || j.Settled {
		t.Errorf("collateral enablement advanced retirement: %+v", j)
	}
	if code != exitUnknown || m["reason"] != retireReasonRestart ||
		!strings.Contains(whyOf(m), "after enabling "+nodeUnit+", "+serverUnit+" is \"enabled\"") {
		t.Fatalf("missing early collateral-enablement refusal: %s", out)
	}
}

// The journal and host stand at the selected production boundary. No done
// proof is substituted: the caller still observes every real fixture file.
func retireProofHost(t *testing.T, variant retirement.Variant, phase retirement.Phase) (*requestFixture, retirement.Journal) {
	t.Helper()
	f := newRequestFixture(t)
	if variant == retirement.VariantRetainedNode {
		f.retainANode(t)
	}
	f.reserve(t)
	j := plantResumedRetirement(t, f, phase, variant)
	mustOK(t, os.Rename(f.stateDir, j.Archive))
	if variant == retirement.VariantRetainedNode {
		writeFile(t, f.cfg, f.rendering(t), 0o600)
		restartedNode(t, f, useRegistrationRecord(t), retainedEndpoint)
	} else {
		mustOK(t, os.Remove(f.cfg))
	}
	f.manager.set(backupServiceUnit, "LoadState", "loaded")
	f.manager.set(backupServiceUnit, "UnitFileState", "static")

	return f, j
}

// Removing the pre-publication proof must publish done, even if the later
// ActionPostconditions would refuse. Enter at the action boundary so a phase
// table's narrower configuration or node check cannot mask this proof either.
func TestRetirementProvesDoneBeforePublishingIt(t *testing.T) {
	cases := []struct {
		name, unit, key, value, why string
	}{
		{name: "healthy static backup"},
		{name: "controller", unit: serverUnit, key: "UnitFileState", value: "enabled", why: serverUnit},
		{name: "upgrade timer", unit: upgradeTimerUnit, key: "ActiveState", value: "active", why: upgradeTimerUnit},
		{name: "backup active", unit: backupServiceUnit, key: "ActiveState", value: "active", why: backupServiceUnit},
		{name: "backup process", unit: backupServiceUnit, key: "MainPID", value: "91", why: "main process 91"},
		{name: "backup timer active", unit: backupTimerUnit, key: "ActiveState", value: "active", why: backupTimerUnit},
		{name: "backup timer enabled", unit: backupTimerUnit, key: "UnitFileState", value: "enabled", why: backupTimerUnit},
		{name: "runtime mask", unit: backupServiceUnit, key: "UnitFileState", value: "masked-runtime", why: "masked-runtime"},
		{name: "unknown backup", unit: backupServiceUnit, key: "MainPID", why: "did not answer"},
		{name: "unknown timer", unit: backupTimerUnit, key: "UnitFileState", why: "does not know"},
		{name: "node", unit: nodeUnit, key: "ActiveState", value: "failed", why: nodeUnit},
		{name: "configuration", why: "no configuration is installed"},
		{name: "archive", why: "is not there"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseNodeRestarted)
			if _, r := observeRetirePostconditions(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("healthy baseline: %+v", r)
			}
			beforeJournal := mustRead(t, retirement.JournalPath())
			beforeStatus := mustRead(t, retirement.StatusPath())
			switch c.name {
			case "configuration":
				mustOK(t, os.Remove(f.cfg))
			case "archive":
				mustOK(t, os.Rename(j.Archive, j.Archive+"-moved"))
			default:
				if c.unit != "" {
					f.manager.set(c.unit, c.key, c.value)
				}
			}
			next, r := performRetireAction(t.Context(), retireProofMode(f), nil, j, retirement.ActionDone)
			if c.why == "" {
				if r != nil || next.Phase != retirement.PhaseDone || requireRetireJournal(t).Phase != retirement.PhaseDone {
					t.Fatalf("healthy done publication: %+v %+v", next, r)
				}
				st, presence, err := retirement.ReadStatus()
				mustOK(t, err)
				if presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
					t.Fatalf("healthy done publication left its status behind: %+v %v", st, presence)
				}
				return
			}
			requireRetireProofRefusal(t, r, c.why)
			if next.Phase != j.Phase || mustRead(t, retirement.JournalPath()) != beforeJournal ||
				mustRead(t, retirement.StatusPath()) != beforeStatus {
				t.Fatal("failed proof published a new done journal or status")
			}
		})
	}
}

func TestRetirementExecutesActionPostconditionsBeforeReturningSuccess(t *testing.T) {
	f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
	f.manager.set(nodeUnit, "ActiveState", "failed")
	before := mustRead(t, retirement.JournalPath())
	next, steps, r := f.drive(t, j)
	requireRetireProofRefusal(t, r, nodeUnit+" is failed")
	if next.Phase != retirement.PhaseDone || len(steps) != 0 || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("postconditions changed the transition: %+v %v", next, steps)
	}
}

// The status already says done, so removing entry proof reaches receipt or
// ledger work before any later publication/settlement check can object.
func TestRetirementDoneEntryProvesBeforeTailOrSettledSuccess(t *testing.T) {
	for _, variant := range []retirement.Variant{retirement.VariantServerOnly, retirement.VariantRetainedNode} {
		for _, settled := range []bool{false, true} {
			name := string(variant) + "/unsettled"
			if settled {
				name = string(variant) + "/settled"
			}
			t.Run(name, func(t *testing.T) {
				f, j := retireProofHost(t, variant, retirement.PhaseDone)
				if settled {
					j.RowDone, j.CompletedBy, j.Settled = true, requestRetiring, true
					mustOK(t, j.Write(retireNow()))
					markGuard(t, f.guard, nil, nil)
				}
				unit := serverUnit
				if variant == retirement.VariantRetainedNode {
					unit = nodeUnit
				}
				f.manager.set(unit, "ActiveState", "failed")
				before := mustRead(t, retirement.JournalPath())
				out, code := retiredRequest(t, f, requestRun)
				m := retireAnswer(t, out)
				if code != exitRefused || m["reason"] != retireReasonPostcondition ||
					!strings.Contains(whyOf(m), unit+" is failed") {
					t.Fatalf("done entry did not refuse at its own boundary: %s", out)
				}
				if mustRead(t, retirement.JournalPath()) != before {
					t.Fatal("done entry reached journal work")
				}
				if variant == retirement.VariantRetainedNode {
					if _, err := os.Lstat(receiptPath); !os.IsNotExist(err) {
						t.Fatalf("done entry reached receipt work: %v", err)
					}
				}
				// Read through the archive, without recreating the identity path.
				f.stateDir = j.Archive
				f.pgLedger(t, func(db *state.DB) {
					row, present, err := db.ReadRetirement(t.Context(), f.identity)
					mustOK(t, err)
					if !present || row.State != state.RetirementIntent {
						t.Fatalf("done entry reached ledger completion: %+v", row)
					}
				})
			})
		}
	}
}

// A property observer can move a previously observed unit while the last unit
// is read. It changes the host, never the production proof's return value.
func observeRetireProofReads(t *testing.T, fn func(pass int, unit string)) {
	t.Helper()
	saved := retirePostconditionInspector
	pass := 0
	retirePostconditionInspector = func() *lifeops.Inspector {
		pass++
		current := pass
		return lifeops.NewInspector(lifeops.WithSystemctl(systemctlBinary), managerRunnerOption(), lifeops.WithWaitDelay(guardWaitDelay),
			lifeops.WithObserver(func(_ context.Context, args []string) {
				fn(current, args[len(args)-1])
			}))
	}
	t.Cleanup(func() { retirePostconditionInspector = saved })
}

// The first proof succeeds, then the host drifts before status repair. This
// independently exposes the publication proof on the settled and tail routes.
func TestRetirementReprovesBeforeStatusRepublication(t *testing.T) {
	for _, settled := range []bool{false, true} {
		name := "tail"
		if settled {
			name = "settled"
		}
		t.Run(name, func(t *testing.T) {
			f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
			if settled {
				j.RowDone, j.CompletedBy, j.Settled = true, requestRetiring, true
				mustOK(t, j.Write(retireNow()))
				markGuard(t, f.guard, nil, nil)
			}
			mustOK(t, os.Remove(retirement.StatusPath()))
			drifted := false
			observeRetireProofReads(t, func(pass int, unit string) {
				if pass == 1 && unit == backupServiceUnit {
					drifted = true
					f.manager.set(nodeUnit, "ActiveState", "failed")
				}
			})
			out, code := retiredRequest(t, f, requestRun)
			m := retireAnswer(t, out)
			if !drifted || code != exitRefused || m["reason"] != retireReasonPostcondition ||
				!strings.Contains(whyOf(m), nodeUnit+" is failed") {
				t.Fatalf("status repair after drift: %s (drift %v)", out, drifted)
			}
			if _, err := os.Lstat(retirement.StatusPath()); !os.IsNotExist(err) {
				t.Fatalf("status published under failed proof: %v", err)
			}
			if requireRetireJournal(t).RowDone != j.RowDone {
				t.Fatal("status refusal reached local row completion")
			}
		})
	}
}

// The done journal's persistence can outlive its proof too. The status must
// remain at the previous phase if the host drifts during that journal write.
func TestRetirementReprovesStatusAfterDoneJournalPersistence(t *testing.T) {
	f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseNodeRestarted)
	beforeStatus := mustRead(t, retirement.StatusPath())
	saved := retirement.SyncingDir
	drifted := false
	retirement.SyncingDir = func(dir string) error {
		if !drifted && dir == retirement.RetiredDir() && requireRetireJournal(t).Phase == retirement.PhaseDone {
			drifted = true
			f.manager.set(nodeUnit, "ActiveState", "failed")
		}
		return nil
	}
	t.Cleanup(func() { retirement.SyncingDir = saved })

	next, r := performRetireAction(t.Context(), retireProofMode(f), nil, j, retirement.ActionDone)
	requireRetireProofRefusal(t, r, nodeUnit+" is failed")
	if !drifted || next.Phase != retirement.PhaseDone || mustRead(t, retirement.StatusPath()) != beforeStatus {
		t.Fatal("status was published after the done journal's persistence lost local proof")
	}
	if after := requireRetireJournal(t); after.RowDone || after.Settled {
		t.Fatalf("status refusal completed the tail: %+v", after)
	}
}

// Each settlement proof is removed independently: the pre-clear case forbids
// clearing the marker even when the post-clear proof would subsequently refuse;
// the post-clear case forbids the settled write after the marker flush drifts.
func TestRetirementReprovesBothSettlementBoundaries(t *testing.T) {
	for _, afterMarker := range []bool{false, true} {
		name := "during ledger acknowledgement"
		if afterMarker {
			name = "during marker persistence"
		}
		t.Run(name, func(t *testing.T) {
			f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
			drifted, attemptedSettlement := false, false
			savedPublish, savedGuard := retirement.Publishing, guardHook
			retirement.Publishing = func(path string) error {
				if path != retirement.JournalPath() {
					return nil
				}
				before := requireRetireJournal(t)
				if before.RowDone {
					attemptedSettlement = true
				} else if !afterMarker {
					drifted = true
					f.manager.set(nodeUnit, "ActiveState", "failed")
				}
				return nil
			}
			guardHook = func(op guardOp) error {
				if afterMarker && !drifted && op.Kind == "fsync" && op.Path == f.guard.active() &&
					requireRetireJournal(t).RowDone && f.guard.record(t).Transition == nil {
					drifted = true
					f.manager.set(nodeUnit, "ActiveState", "failed")
				}
				return nil
			}
			t.Cleanup(func() { retirement.Publishing, guardHook = savedPublish, savedGuard })

			out, code := retiredRequest(t, f, requestRun)
			m := retireAnswer(t, out)
			if !drifted || code != exitRefused || m["reason"] != retireReasonPostcondition ||
				!strings.Contains(whyOf(m), nodeUnit+" is failed") || m["state"] != string(retirement.PhaseDone) {
				t.Fatalf("settlement after drift: %s (drift %v)", out, drifted)
			}
			if attemptedSettlement {
				t.Fatal("failed proof attempted to write Settled")
			}
			after := requireRetireJournal(t)
			if !after.RowDone || after.CompletedBy != requestRetiring || after.Settled {
				t.Fatalf("historical completion was lost or settlement recorded: %+v", after)
			}
			markerPresent := f.guard.record(t).Transition != nil
			if markerPresent == afterMarker {
				t.Fatalf("marker present %v after marker-persistence drift %v", markerPresent, afterMarker)
			}
			f.stateDir = j.Archive
			f.pgLedger(t, func(db *state.DB) {
				row, present, err := db.ReadRetirement(t.Context(), f.identity)
				mustOK(t, err)
				if !present || row.State != state.RetirementDone || row.CompletedBy != requestRetiring {
					t.Fatalf("the historically completed row was not preserved: %+v", row)
				}
			})
			if err := guardRun(t, "release", "--holder", requestRun); err == nil {
				t.Fatal("unsettled retirement released its guard")
			}
		})
	}
}
