package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

// restartRetireProofNode leaves the previous record, removes it, or publishes
// the new invocation's record. The manager alone chooses the invocation.
func restartRetireProofNode(t *testing.T, f *requestFixture, registration string) {
	t.Helper()
	_, err := f.manager.StopAndProve(t.Context(), nodeUnit)
	mustOK(t, err)
	_, err = f.manager.StartAndProve(t.Context(), nodeUnit)
	mustOK(t, err)
	switch registration {
	case "missing":
		mustOK(t, os.Remove(registrationRecordPath))
	case "current":
		restartedNode(t, f, registrationRecordPath, retainedEndpoint)
	case "stale":
	default:
		t.Fatalf("unknown registration fixture %q", registration)
	}
}

func requireRetireRegistrationReady(t *testing.T, f *requestFixture) {
	t.Helper()
	if fact := retireNodeUnitFact(t.Context(), endpointInspector(), f.cfg); fact != retirement.NodeReady {
		t.Fatalf("the earlier node-unit fact was not accepted: %s", fact)
	}
}

// Reusing one InvocationID for every start makes the second stale record pass;
// dropping the invocation comparison makes either stale record pass.
func TestRetirementFakeRequiresNewRegistrationAfterTwoRestarts(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	requireRetireRegistrationReady(t, f)
	for restart := 1; restart <= 2; restart++ {
		previous := mustRead(t, registrationRecordPath)
		restartRetireProofNode(t, f, "stale")
		props, err := endpointInspector().UnitProperties(t.Context(), nodeUnit, "InvocationID")
		mustOK(t, err)
		invocation := firstProp(props, "InvocationID")
		if want := fmt.Sprintf("%032x", restart); invocation != want {
			t.Fatalf("restart %d invocation: got %s, want %s", restart, invocation, want)
		}
		if mustRead(t, registrationRecordPath) != previous {
			t.Fatal("starting the manager published registration")
		}
		if fact := retireNodeUnitFact(t.Context(), endpointInspector(), f.cfg); fact != retirement.NodeUnknown {
			t.Fatalf("restart %d accepted an earlier invocation's registration: %s", restart, fact)
		}
		restartedNode(t, f, registrationRecordPath, retainedEndpoint)
		ev := readRegistrationRecord(registrationRecordPath)
		if ev.record == nil || ev.record.InvocationID != invocation {
			t.Fatalf("restart %d did not publish the manager's invocation: %+v", restart, ev)
		}
		requireRetireRegistrationReady(t, f)
	}
}

// Removing the done observer's registration gate persists DoneAt and phase
// before any later status or receipt proof can refuse the restarted node.
func TestRetirementReprovesRegistrationBeforeMarkDone(t *testing.T) {
	for _, registration := range []string{"missing", "stale", "current"} {
		t.Run(registration, func(t *testing.T) {
			f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseNodeRestarted)
			requireRetireRegistrationReady(t, f)
			beforeJournal := mustRead(t, retirement.JournalPath())
			beforeStatus := mustRead(t, retirement.StatusPath())
			restartRetireProofNode(t, f, registration)

			next, r := retireMarkDone(t.Context(), retireProofMode(f), j)
			if registration == "current" {
				if r != nil || next.Phase != retirement.PhaseDone || next.DoneAt == "" {
					t.Fatalf("registered restart did not reach done: %+v %+v", next, r)
				}
				if after := requireRetireJournal(t); after.Phase != retirement.PhaseDone || after.DoneAt == "" {
					t.Fatalf("registered restart did not persist done: %+v", after)
				}
				st, presence, err := retirement.ReadStatus()
				mustOK(t, err)
				if presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
					t.Fatalf("registered restart did not publish status: %+v %v", st, presence)
				}
				return
			}
			requireRetireProofRefusal(t, r, "retained-node-registration-unproved")
			if r.Outcome != retireOutcomeUnknown || next.DoneAt != "" || next.Phase != j.Phase ||
				mustRead(t, retirement.JournalPath()) != beforeJournal || mustRead(t, retirement.StatusPath()) != beforeStatus {
				t.Fatalf("unregistered restart changed the done boundary: %+v %+v", next, r)
			}
		})
	}
}

// Removing the republication observer writes status from the earlier proof.
// Calling the boundary directly prevents entry or receipt checks masking it.
func TestRetirementReprovesRegistrationBeforeStatusRepublication(t *testing.T) {
	for _, registration := range []string{"missing", "stale", "current"} {
		t.Run(registration, func(t *testing.T) {
			f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
			requireRetireRegistrationReady(t, f)
			if _, r := observeRetirePostconditions(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("earlier done proof: %+v", r)
			}
			mustOK(t, os.Remove(retirement.StatusPath()))
			before := mustRead(t, retirement.JournalPath())
			restartRetireProofNode(t, f, registration)

			status, r := retireStatusPostcondition(t.Context(), retireProofMode(f), j)
			if mustRead(t, retirement.JournalPath()) != before {
				t.Fatal("status republication changed the journal")
			}
			if registration == "current" {
				if r != nil || status != retireStatusRepublished {
					t.Fatalf("registered restart did not republish status: %q %+v", status, r)
				}
				st, presence, err := retirement.ReadStatus()
				mustOK(t, err)
				if presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
					t.Fatalf("republished status: %+v %v", st, presence)
				}
				return
			}
			requireRetireProofRefusal(t, r, "retained-node-registration-unproved")
			if r.Outcome != retireOutcomeUnknown || status != "" {
				t.Fatalf("unregistered restart's republication answer: %q %+v", status, r)
			}
			if _, err := os.Lstat(retirement.StatusPath()); !os.IsNotExist(err) {
				t.Fatalf("unregistered restart published status: %v", err)
			}
		})
	}
}

// Removing either tail observer permits its next write: clearing the marker
// after acknowledgement, or settling after the marker's persistence.
func TestRetirementReprovesRegistrationAtBothSettlementBoundaries(t *testing.T) {
	for _, afterMarker := range []bool{false, true} {
		for _, registration := range []string{"missing", "stale", "current"} {
			t.Run(fmt.Sprintf("after-marker-%t/%s", afterMarker, registration), func(t *testing.T) {
				f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
				requireRetireRegistrationReady(t, f)
				beforeStatus := mustRead(t, retirement.StatusPath())
				restarted, attemptedSettlement := false, false
				savedPublish, savedGuard := retirement.Publishing, guardHook
				retirement.Publishing = func(path string) error {
					if path != retirement.JournalPath() {
						return nil
					}
					if requireRetireJournal(t).RowDone {
						attemptedSettlement = true
					} else if !afterMarker && !restarted {
						restarted = true
						restartRetireProofNode(t, f, registration)
					}
					return nil
				}
				guardHook = func(op guardOp) error {
					if afterMarker && !restarted && op.Kind == "fsync" && op.Path == f.guard.active() &&
						requireRetireJournal(t).RowDone && f.guard.record(t).Transition == nil {
						restarted = true
						restartRetireProofNode(t, f, registration)
					}
					return nil
				}
				t.Cleanup(func() { retirement.Publishing, guardHook = savedPublish, savedGuard })

				out, code := retiredRequest(t, f, requestRun)
				m := retireAnswer(t, out)
				if !restarted {
					t.Fatalf("settlement never reached the restart boundary: %s", out)
				}
				after := requireRetireJournal(t)
				if after.Phase != j.Phase || after.DoneAt != j.DoneAt || !after.RowDone || after.CompletedBy != requestRetiring ||
					mustRead(t, retirement.StatusPath()) != beforeStatus {
					t.Fatalf("settlement changed historical completion or status: %+v", after)
				}
				if registration == "current" {
					if code != 0 || m["settled"] != true || !after.Settled || !attemptedSettlement ||
						f.guard.record(t).Transition != nil {
						t.Fatalf("registered restart did not settle: %s", out)
					}
					return
				}
				if code != exitUnknown || m["reason"] != retireReasonPostcondition ||
					!strings.Contains(whyOf(m), "retained-node-registration-unproved") || m["state"] != string(j.Phase) {
					t.Fatalf("unregistered restart's settlement refusal: %s", out)
				}
				if attemptedSettlement || after.Settled {
					t.Fatal("unregistered restart attempted settlement")
				}
				if markerPresent := f.guard.record(t).Transition != nil; markerPresent == afterMarker {
					t.Fatalf("marker present %v after marker-persistence restart %v", markerPresent, afterMarker)
				}
			})
		}
	}
}

// Each changed member independently kills omission of its closing comparison.
// The restart removes the pathname after the reader obtained the old bytes.
func TestRetirementRegistrationReadRequiresAStableUnit(t *testing.T) {
	for _, change := range []string{"restart", "active state", "main pid", "healthy"} {
		t.Run(change, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			requireRetireRegistrationReady(t, f)
			sampled := false
			saved := retireAfterRegistrationRead
			retireAfterRegistrationRead = func() {
				sampled = true
				switch change {
				case "restart":
					restartRetireProofNode(t, f, "missing")
				case "active state":
					f.manager.set(nodeUnit, "ActiveState", "deactivating")
				case "main pid":
					f.manager.set(nodeUnit, "MainPID", "4243")
				}
			}
			t.Cleanup(func() { retireAfterRegistrationRead = saved })
			fact := retireNodeUnitFact(t.Context(), endpointInspector(), f.cfg)
			want := retirement.NodeUnknown
			if change == "healthy" {
				want = retirement.NodeReady
			}
			if !sampled || fact != want {
				t.Fatalf("record read with %s: sampled=%v, got %s, want %s", change, sampled, fact, want)
			}
		})
	}
}

// Removing the closing sample accepts bytes from the old invocation and
// permits the protected write before a later boundary can reject the restart.
func TestRetirementBracketsRegistrationAtEveryDoneWrite(t *testing.T) {
	retireRegistrationWriteBoundaries(t, "record read")
}

// Moving a boundary's registration proof ahead of admission (or deleting it)
// permits that write after completed admission restarts the node.
func TestRetirementReprovesRegistrationAfterAdmissionAtEveryDoneWrite(t *testing.T) {
	retireRegistrationWriteBoundaries(t, "admission")
}

func TestRetirementHealthyRegistrationPermitsEveryDoneWrite(t *testing.T) {
	retireRegistrationWriteBoundaries(t, "healthy")
}

// Call each write boundary directly, after proving the healthy entry state,
// so another boundary cannot mask a missing proof at the one under test.
func retireRegistrationWriteBoundaries(t *testing.T, injection string) {
	t.Helper()
	for _, boundary := range []string{"mark done", "advance done", "status", "marker", "settlement"} {
		t.Run(boundary, func(t *testing.T) {
			phase := retirement.PhaseDone
			if boundary == "mark done" || boundary == "advance done" {
				phase = retirement.PhaseNodeRestarted
			}
			f, j := retireProofHost(t, retirement.VariantRetainedNode, phase)
			m := retireProofMode(f)
			if _, r := observeRetirePostconditions(t.Context(), m, j); r != nil {
				t.Fatalf("healthy entry proof: %+v", r)
			}
			if boundary == "status" {
				writeFile(t, retirement.StatusPath(), "damaged\n", 0o600)
			}
			var root *txLock
			var dir *os.File
			if boundary == "marker" || boundary == "settlement" {
				j.RowDone, j.CompletedBy = true, requestRetiring
				mustOK(t, j.Write(retireNow()))
				var r *retireRefusal
				root, dir, _, r = retireGuard(m.run)
				if r != nil {
					t.Fatal(r)
				}
				defer root.release()
				defer func() { _ = dir.Close() }()
				if boundary == "settlement" {
					if _, r := retireClearMarker(t.Context(), m, root, dir, j); r != nil {
						t.Fatalf("healthy marker setup: %+v", r)
					}
				}
			}
			beforeJournal := mustRead(t, retirement.JournalPath())
			beforeStatus := mustRead(t, retirement.StatusPath())
			guardPath := filepath.Join(f.guard.active(), guardRecordName)
			beforeGuard := mustRead(t, guardPath)
			beforeDoneAt := j.DoneAt
			admitted, injected, attemptedWrite := false, false, false
			savedEvent, savedRead := retireMutationEvent, retireAfterRegistrationRead
			retireMutationEvent = func(event, _ string) {
				if event == "admission" {
					admitted = true
					// Inertness also reads through the operation inspector before
					// admission; inject only after admission actually completes.
					if injection == "admission" && !injected {
						injected = true
						restartRetireProofNode(t, f, "missing")
					}
				}
				if event == "journal" || event == "status" || event == "marker" || event == "settlement" {
					attemptedWrite = true
				}
			}
			retireAfterRegistrationRead = func() {
				if injection == "record read" && admitted && !injected {
					injected = true
					restartRetireProofNode(t, f, "missing")
				}
			}
			t.Cleanup(func() {
				retireMutationEvent, retireAfterRegistrationRead = savedEvent, savedRead
			})

			next := j
			var r *retireRefusal
			switch boundary {
			case "mark done":
				next, r = retireMarkDone(t.Context(), m, j)
			case "advance done":
				next, r = retireAdvancePhase(t.Context(), j, retirement.PhaseDone)
			case "status":
				_, r = retireStatusPostcondition(t.Context(), m, j)
			case "marker":
				_, r = retireClearMarker(t.Context(), m, root, dir, j)
			case "settlement":
				r = retireMarkSettled(t.Context(), m, &next)
			}
			after := requireRetireJournal(t)
			if !admitted {
				t.Fatal("boundary did not finish admission")
			}
			if injection == "healthy" {
				if r != nil || !attemptedWrite {
					t.Fatalf("healthy boundary did not write: %+v", r)
				}
				switch boundary {
				case "mark done", "advance done":
					if next.Phase != retirement.PhaseDone || next.DoneAt == "" || after.Phase != next.Phase || after.DoneAt != next.DoneAt {
						t.Fatalf("healthy done write: returned %+v, persisted %+v", next, after)
					}
				case "status":
					st, presence, err := retirement.ReadStatus()
					mustOK(t, err)
					if presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
						t.Fatalf("healthy status write: %+v %v", st, presence)
					}
				case "marker":
					if f.guard.record(t).Transition != nil {
						t.Fatal("healthy marker was not cleared")
					}
				case "settlement":
					if !next.Settled || !after.Settled {
						t.Fatalf("healthy settlement was not recorded: %+v", after)
					}
				}
				return
			}
			requireRetireProofRefusal(t, r, "retained-node-registration-unproved")
			if !injected || r.Outcome != retireOutcomeUnknown || attemptedWrite {
				t.Fatalf("restart boundary: injected=%v, attempted write=%v, refusal=%+v", injected, attemptedWrite, r)
			}
			if next.Phase != phase || next.DoneAt != beforeDoneAt || next.Settled ||
				after.Phase != phase || after.DoneAt != beforeDoneAt || after.Settled ||
				mustRead(t, retirement.JournalPath()) != beforeJournal || mustRead(t, retirement.StatusPath()) != beforeStatus ||
				mustRead(t, guardPath) != beforeGuard {
				t.Fatalf("restart changed phase, completion, status, marker or settlement: returned %+v, persisted %+v", next, after)
			}
		})
	}
}
