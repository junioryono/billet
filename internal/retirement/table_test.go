package retirement

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// vectorsPath is the collection's copy of the decision vectors, written by
// this test (BILLET_UPDATE_FIXTURES=1) and compared otherwise, so the role's
// and the loader's readers are held to the Go table's every cell.
func vectorsPath(t *testing.T) string {
	t.Helper()

	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}

	return filepath.Join(filepath.Dir(here), "..", "..", "ansible_collections", "junioryono", "billet",
		"tests", "fixtures", "retirement", "vectors.json")
}

type phaseVector struct {
	Variant  Variant  `json:"variant"`
	Phase    Phase    `json:"phase"`
	Facts    Facts    `json:"facts"`
	Decision Decision `json:"decision"`
}

type dispatchVector struct {
	Row      RowFact     `json:"row"`
	Journal  JournalFact `json:"journal"`
	Dispatch Dispatch    `json:"dispatch"`
}

type vectors struct {
	Schema   int              `json:"schema"`
	Phases   []phaseVector    `json:"phases"`
	Dispatch []dispatchVector `json:"dispatch"`
}

// allVectors enumerates every combination of the facts a phase CONSULTS (the
// table reads the backup at intent and stopped, the node verdict at archived
// and config-rewritten, the node unit at node-restarted, and the identity,
// the configuration and the stage everywhere), with the facts a phase does not
// consult held at one value, so the fixture stays a few hundred rows rather
// than the full product; the completeness of what each phase reads is what
// TestThePhaseTableHoldsTheSpecifiedRows pins by value.
func allVectors() vectors {
	out := vectors{Schema: 1}

	backups := func(p Phase) []BackupFact {
		if p == PhaseIntent || p == PhaseStopped {
			return []BackupFact{BackupInactive, BackupActive, BackupUnknown}
		}

		return []BackupFact{BackupInactive}
	}

	verdicts := func(p Phase) []Verdict {
		if p == PhaseArchived || p == PhaseConfigRewritten {
			return []Verdict{VerdictTrue, VerdictFalse, VerdictUnknown}
		}

		return []Verdict{VerdictFalse}
	}

	units := func(p Phase) []NodeUnitFact {
		if p == PhaseNodeRestarted {
			return []NodeUnitFact{NodeReady, NodeInactive, NodeUnenabled, NodeUnknown}
		}

		return []NodeUnitFact{NodeReady}
	}

	for _, variant := range []Variant{VariantServerOnly, VariantRetainedNode} {
		for _, phase := range []Phase{PhaseIntent, PhaseStopped, PhaseArchived, PhaseConfigRewritten, PhaseNodeRestarted, PhaseDone} {
			for _, identity := range []IdentityFact{IdentityConfigured, IdentityArchive, IdentityNeither, IdentityBoth} {
				for _, cfg := range []ConfigFact{ConfigInstalled, ConfigStaged, ConfigAbsent, ConfigOther} {
					for _, stage := range []StageFact{StageRecorded, StageAbsent, StageOther} {
						for _, changed := range verdicts(phase) {
							for _, backup := range backups(phase) {
								for _, unit := range units(phase) {
									f := Facts{Identity: identity, Config: cfg, Stage: stage, NodeChanged: changed, Backup: backup, NodeUnit: unit}
									out.Phases = append(out.Phases, phaseVector{Variant: variant, Phase: phase, Facts: f, Decision: Decide(variant, phase, f)})
								}
							}
						}
					}
				}
			}
		}
	}

	for _, row := range []RowFact{RowAbsent, RowUnreadable, RowReservedMine, RowIntentMine, RowDoneMine, RowOtherReserved, RowOtherIntent, RowDoneOther} {
		for _, journal := range []JournalFact{JournalFactAbsent, JournalFactIntent, JournalFactIncomplete, JournalFactDone} {
			out.Dispatch = append(out.Dispatch, dispatchVector{Row: row, Journal: journal, Dispatch: DispatchFor(row, journal)})
		}
	}

	return out
}

// THE VECTORS ARE THE TABLE'S OWN, written by the producer and compared on
// every run, so a row that changes changes the fixture the role reads.
func TestTheDecisionVectorsAreTheTablesOwn(t *testing.T) {
	body, err := json.MarshalIndent(allVectors(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	body = append(body, '\n')
	path := vectorsPath(t)

	if os.Getenv("BILLET_UPDATE_FIXTURES") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed vectors: %v (run with BILLET_UPDATE_FIXTURES=1 to write them)", err)
	}

	if !bytes.Equal(committed, body) {
		t.Fatalf("%s is not what the table decides today; run with BILLET_UPDATE_FIXTURES=1", path)
	}
}

// THE ROWS THE SPECIFICATION NAMES, each pinned by value.
func TestThePhaseTableHoldsTheSpecifiedRows(t *testing.T) {
	ok := Facts{Identity: IdentityConfigured, Config: ConfigInstalled, Stage: StageRecorded, NodeChanged: VerdictFalse, Backup: BackupInactive, NodeUnit: NodeReady}

	with := func(f Facts, mutate func(*Facts)) Facts { mutate(&f); return f }

	cases := []struct {
		name    string
		variant Variant
		phase   Phase
		facts   Facts
		want    Action
		reason  string
	}{
		{"intent stops", VariantRetainedNode, PhaseIntent, ok, ActionStop, ""},
		{"intent awaits an active backup", VariantRetainedNode, PhaseIntent, with(ok, func(f *Facts) { f.Backup = BackupActive }), ActionAwaitBackup, ""},
		{"intent refuses an unknown backup", VariantRetainedNode, PhaseIntent, with(ok, func(f *Facts) { f.Backup = BackupUnknown }), ActionRefuse, "backup"},
		{"intent refuses the identity at the archive", VariantRetainedNode, PhaseIntent, with(ok, func(f *Facts) { f.Identity = IdentityArchive }), ActionRefuse, "identity"},
		{"retained-node needs its stage", VariantRetainedNode, PhaseIntent, with(ok, func(f *Facts) { f.Stage = StageAbsent }), ActionRefuse, "stage"},
		{"server-only refuses a stage", VariantServerOnly, PhaseIntent, ok, ActionRefuse, "stage"},
		{"server-only stops", VariantServerOnly, PhaseIntent, with(ok, func(f *Facts) { f.Stage = StageAbsent }), ActionStop, ""},
		{"stopped archives", VariantRetainedNode, PhaseStopped, ok, ActionArchive, ""},
		{"stopped with the move done advances", VariantRetainedNode, PhaseStopped, with(ok, func(f *Facts) { f.Identity = IdentityArchive }), ActionAdvanceArchived, ""},
		{"stopped with identity at neither refuses", VariantRetainedNode, PhaseStopped, with(ok, func(f *Facts) { f.Identity = IdentityNeither }), ActionRefuse, "identity"},
		{"stopped with identity at both refuses", VariantRetainedNode, PhaseStopped, with(ok, func(f *Facts) { f.Identity = IdentityBoth }), ActionRefuse, "identity"},
		{"stopped awaits a backup first", VariantRetainedNode, PhaseStopped, with(ok, func(f *Facts) { f.Backup = BackupActive; f.Identity = IdentityNeither }), ActionAwaitBackup, ""},
		{"archived rewrites an installed config", VariantRetainedNode, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive }), ActionRewrite, ""},
		{"archived refuses a changed node before the rewrite", VariantRetainedNode, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.NodeChanged = VerdictTrue }), ActionRefuse, "node_changed"},
		{"archived advances a completed rewrite, changed", VariantRetainedNode, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged; f.NodeChanged = VerdictTrue }), ActionAdvanceRewritten, ""},
		{"archived advances a completed rewrite, unchanged", VariantRetainedNode, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged }), ActionAdvanceRewritten, ""},
		{"archived refuses a completed rewrite with an unknown verdict", VariantRetainedNode, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged; f.NodeChanged = VerdictUnknown }), ActionRefuse, "node_changed"},
		{"archived refuses a third digest", VariantRetainedNode, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigOther }), ActionRefuse, "config"},
		{"archived refuses a stage with another digest", VariantRetainedNode, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Stage = StageOther }), ActionRefuse, "stage"},
		{"server-only archived unlinks", VariantServerOnly, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Stage = StageAbsent }), ActionRewrite, ""},
		{"server-only archived with the config gone advances", VariantServerOnly, PhaseArchived, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Stage = StageAbsent; f.Config = ConfigAbsent }), ActionAdvanceRewritten, ""},
		{"config-rewritten always restarts, changed", VariantRetainedNode, PhaseConfigRewritten, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged; f.NodeChanged = VerdictTrue }), ActionRestart, ""},
		{"config-rewritten always restarts, unchanged", VariantRetainedNode, PhaseConfigRewritten, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged }), ActionRestart, ""},
		{"config-rewritten refuses an unobservable verdict", VariantRetainedNode, PhaseConfigRewritten, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged; f.NodeChanged = VerdictUnknown }), ActionRefuse, "node_changed"},
		{"config-rewritten refuses another config", VariantRetainedNode, PhaseConfigRewritten, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigOther }), ActionRefuse, "config"},
		{"server-only config-rewritten is done", VariantServerOnly, PhaseConfigRewritten, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Stage = StageAbsent; f.Config = ConfigAbsent }), ActionDone, ""},
		{"node-restarted with the node ready is done", VariantRetainedNode, PhaseNodeRestarted, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged }), ActionDone, ""},
		{"node-restarted with the node inactive restarts", VariantRetainedNode, PhaseNodeRestarted, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged; f.NodeUnit = NodeInactive }), ActionRestart, ""},
		{"node-restarted with enabled-runtime restarts", VariantRetainedNode, PhaseNodeRestarted, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged; f.NodeUnit = NodeUnenabled }), ActionRestart, ""},
		{"node-restarted with an unknown unit refuses", VariantRetainedNode, PhaseNodeRestarted, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Config = ConfigStaged; f.NodeUnit = NodeUnknown }), ActionRefuse, "node_unit"},
		{"node-restarted on server-only refuses", VariantServerOnly, PhaseNodeRestarted, with(ok, func(f *Facts) { f.Identity = IdentityArchive; f.Stage = StageAbsent; f.Config = ConfigAbsent }), ActionRefuse, "phase"},
		{"done judges postconditions", VariantRetainedNode, PhaseDone, ok, ActionPostconditions, ""},
		{"an unknown variant refuses", "hybrid", PhaseIntent, ok, ActionRefuse, "variant"},
	}

	for _, c := range cases {
		got := Decide(c.variant, c.phase, c.facts)
		if got.Action != c.want {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)

			continue
		}

		if c.reason != "" && !hasPrefix(got.Reason, c.reason+":") {
			t.Errorf("%s: refused on %q, want %s", c.name, got.Reason, c.reason)
		}
	}
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

// THE DISPATCH TABLE IS COMPLETE AND AS SPECIFIED, cell by cell.
func TestTheDispatchTableIsAsSpecified(t *testing.T) {
	want := map[[2]string]Dispatch{
		{"absent", "absent"}:            DispatchRequest,
		{"absent", "intent"}:            DispatchUnknownReservation,
		{"absent", "incomplete"}:        DispatchUnknownReservation,
		{"absent", "done"}:              DispatchUnknownReservation,
		{"reserved-mine", "absent"}:     DispatchAdopt,
		{"reserved-mine", "intent"}:     DispatchAdvanceRow,
		{"reserved-mine", "incomplete"}: DispatchUnknownReservation,
		{"reserved-mine", "done"}:       DispatchCompleteRow,
		{"intent-mine", "absent"}:       DispatchUnknownJournal,
		{"intent-mine", "intent"}:       DispatchResume,
		{"intent-mine", "incomplete"}:   DispatchResume,
		{"intent-mine", "done"}:         DispatchCompleteRow,
		{"done-mine", "absent"}:         DispatchUnknownJournal,
		{"done-mine", "intent"}:         DispatchUnknownReservation,
		{"done-mine", "incomplete"}:     DispatchUnknownReservation,
		{"done-mine", "done"}:           DispatchDone,
	}

	for cell, d := range want {
		if got := DispatchFor(RowFact(cell[0]), JournalFact(cell[1])); got != d {
			t.Errorf("%v: %s, want %s", cell, got, d)
		}
	}

	for _, journal := range []JournalFact{JournalFactAbsent, JournalFactIntent, JournalFactIncomplete, JournalFactDone} {
		if got := DispatchFor(RowUnreadable, journal); got != DispatchUnknownLedger {
			t.Errorf("unreadable x %s: %s, want unknown-ledger (a failed read is never absence)", journal, got)
		}

		for _, other := range []RowFact{RowOtherReserved, RowOtherIntent} {
			if got := DispatchFor(other, journal); got != DispatchRefusedReserved {
				t.Errorf("%s x %s: %s, want refused-reserved", other, journal, got)
			}
		}

		if got := DispatchFor(RowDoneOther, journal); got != DispatchRefusedRetired {
			t.Errorf("done-other x %s: %s, want refused-retired", journal, got)
		}
	}

	if JournalFactOf(false, PhaseDone) != JournalFactAbsent || JournalFactOf(true, PhaseStopped) != JournalFactIncomplete ||
		JournalFactOf(true, PhaseIntent) != JournalFactIntent || JournalFactOf(true, PhaseDone) != JournalFactDone {
		t.Error("JournalFactOf does not classify the phases as the dispatch wants")
	}
}
