package retirement

import "fmt"

// THE PHASE TABLE: one predicate per phase, shared by the retire command's
// resume, by its dry run, and (through the vectors its test writes) by the
// role's and the loader's readers, so every reader decides alike. Each row
// names the filesystem facts it admits and the action a resume takes; a fact
// outside the row is a refusal naming the fact, never a guess.

// IdentityFact is where the identity directory is found: at the configured
// path, at the archive, at neither, or at both.
type IdentityFact string

const (
	IdentityConfigured IdentityFact = "configured"
	IdentityArchive    IdentityFact = "archive"
	IdentityNeither    IdentityFact = "neither"
	IdentityBoth       IdentityFact = "both"
)

// ConfigFact is what the installed configuration's digest says: the digest
// recorded at intent (installed), the staged rendering's (staged), the file
// positively absent, or another digest.
type ConfigFact string

const (
	ConfigInstalled ConfigFact = "installed"
	ConfigStaged    ConfigFact = "staged"
	ConfigAbsent    ConfigFact = "absent"
	ConfigOther     ConfigFact = "other"
)

// StageFact is the staged rendering: present with the recorded digest,
// positively absent, or present with another digest.
type StageFact string

const (
	StageRecorded StageFact = "recorded"
	StageAbsent   StageFact = "absent"
	StageOther    StageFact = "other"
)

// Verdict is a three-valued observation.
type Verdict string

const (
	VerdictTrue    Verdict = "true"
	VerdictFalse   Verdict = "false"
	VerdictUnknown Verdict = "unknown"
)

// BackupFact is the backup service's state.
type BackupFact string

const (
	BackupInactive BackupFact = "inactive"
	BackupActive   BackupFact = "active"
	BackupUnknown  BackupFact = "unknown"
)

// NodeUnitFact is the node unit at node-restarted: active, enabled and its
// record naming the installed endpoint (ready); inactive; active but not
// persistently enabled (unenabled); or unobservable.
type NodeUnitFact string

const (
	NodeReady     NodeUnitFact = "ready"
	NodeInactive  NodeUnitFact = "inactive"
	NodeUnenabled NodeUnitFact = "unenabled"
	NodeUnknown   NodeUnitFact = "unknown"
)

// Facts is one observation of the host, judged against a journal's phase.
type Facts struct {
	Identity    IdentityFact `json:"identity"`
	Config      ConfigFact   `json:"config"`
	Stage       StageFact    `json:"stage"`
	NodeChanged Verdict      `json:"node_changed"`
	Backup      BackupFact   `json:"backup"`
	NodeUnit    NodeUnitFact `json:"node_unit"`
}

// Action is what a resume does next.
type Action string

const (
	// ActionAwaitBackup waits for the backup service to become inactive, then
	// judges again.
	ActionAwaitBackup Action = "await-backup"
	// ActionStop is step 5: stop and disable the units, publish stopped.
	ActionStop Action = "stop"
	// ActionArchive is step 6: the rename, publish archived.
	ActionArchive Action = "archive"
	// ActionAdvanceArchived records a move that completed before its phase
	// could be written, then continues with the rewrite.
	ActionAdvanceArchived Action = "advance-archived"
	// ActionRewrite is step 7: install the stage (or unlink), publish
	// config-rewritten.
	ActionRewrite Action = "rewrite"
	// ActionAdvanceRewritten records a rewrite that completed before its phase
	// could be written, then continues with the restart (or done).
	ActionAdvanceRewritten Action = "advance-config-rewritten"
	// ActionRestart is step 8: enable and restart the node, publish
	// node-restarted.
	ActionRestart Action = "restart"
	// ActionDone publishes done (server-only after the rewrite, retained-node
	// after the restart proved).
	ActionDone Action = "done"
	// ActionPostconditions judges a done journal's postconditions and changes
	// nothing.
	ActionPostconditions Action = "postconditions"
	// ActionRefuse is could-not-tell: the facts are outside the row.
	ActionRefuse Action = "refuse"
)

// Decision is the table's answer: the action, and for a refusal the fact
// that refused.
type Decision struct {
	Action Action `json:"action"`
	Reason string `json:"reason,omitempty"`
}

func refuseOn(fact string, value any) Decision {
	return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("%s: %v", fact, value)}
}

// Decide is the phase table.
//
// THE BACKUP IS JUDGED FIRST at intent and stopped, because a backup started
// under a stopped timer is an operator's and is awaited, never killed, and
// nothing else may proceed while it holds descriptors into the directory; an
// unknown backup refuses. A server-only journal has no stage (one present
// refuses) and no node verdict; a retained-node journal requires its stage
// recorded wherever the stage is consulted.
func Decide(variant Variant, phase Phase, f Facts) Decision {
	if variant != VariantServerOnly && variant != VariantRetainedNode {
		return refuseOn("variant", variant)
	}

	retained := variant == VariantRetainedNode

	if retained && f.Stage != StageRecorded && phase != PhaseDone {
		return refuseOn("stage", f.Stage)
	}

	if !retained && f.Stage != StageAbsent && phase != PhaseDone {
		return refuseOn("stage", f.Stage)
	}

	switch phase {
	case PhaseIntent, PhaseStopped:
		switch f.Backup {
		case BackupActive:
			return Decision{Action: ActionAwaitBackup}
		case BackupInactive:
		default:
			return refuseOn("backup", f.Backup)
		}

		if f.Config != ConfigInstalled {
			return refuseOn("config", f.Config)
		}

		if phase == PhaseIntent {
			if f.Identity != IdentityConfigured {
				return refuseOn("identity", f.Identity)
			}

			return Decision{Action: ActionStop}
		}

		switch f.Identity {
		case IdentityConfigured:
			return Decision{Action: ActionArchive}
		case IdentityArchive:
			return Decision{Action: ActionAdvanceArchived}
		default:
			return refuseOn("identity", f.Identity)
		}
	case PhaseArchived:
		if f.Identity != IdentityArchive {
			return refuseOn("identity", f.Identity)
		}

		switch {
		case f.Config == ConfigInstalled && retained:
			// Nothing rewrote the node's file yet, so it must not have changed.
			if f.NodeChanged != VerdictFalse {
				return refuseOn("node_changed", f.NodeChanged)
			}

			return Decision{Action: ActionRewrite}
		case f.Config == ConfigInstalled:
			return Decision{Action: ActionRewrite}
		case f.Config == ConfigStaged && retained:
			// The rewrite completed before its phase was written; the node's
			// file changed under it, so true is expected and false (a restart
			// happened) admitted, and only an unobservable verdict refuses.
			if f.NodeChanged == VerdictUnknown {
				return refuseOn("node_changed", f.NodeChanged)
			}

			return Decision{Action: ActionAdvanceRewritten}
		case f.Config == ConfigAbsent && !retained:
			return Decision{Action: ActionAdvanceRewritten}
		default:
			return refuseOn("config", f.Config)
		}
	case PhaseConfigRewritten:
		if f.Identity != IdentityArchive {
			return refuseOn("identity", f.Identity)
		}

		switch {
		case retained && f.Config == ConfigStaged:
			// ALWAYS the restart, whatever the verdict: false cannot tell a
			// completed restart from a node never restarted whose file kept an
			// old mtime.
			return Decision{Action: ActionRestart}
		case !retained && f.Config == ConfigAbsent:
			return Decision{Action: ActionDone}
		default:
			return refuseOn("config", f.Config)
		}
	case PhaseNodeRestarted:
		if !retained {
			return refuseOn("phase", "node-restarted on a server-only host")
		}

		if f.Identity != IdentityArchive {
			return refuseOn("identity", f.Identity)
		}

		if f.Config != ConfigStaged {
			return refuseOn("config", f.Config)
		}

		switch f.NodeUnit {
		case NodeReady:
			return Decision{Action: ActionDone}
		case NodeInactive, NodeUnenabled:
			return Decision{Action: ActionRestart}
		default:
			return refuseOn("node_unit", f.NodeUnit)
		}
	case PhaseDone:
		return Decision{Action: ActionPostconditions}
	default:
		return refuseOn("phase", phase)
	}
}

// RowFact is the ledger row as it relates to THIS host.
type RowFact string

const (
	RowAbsent        RowFact = "absent"
	RowUnreadable    RowFact = "unreadable"
	RowReservedMine  RowFact = "reserved-mine"
	RowIntentMine    RowFact = "intent-mine"
	RowDoneMine      RowFact = "done-mine"
	RowOtherReserved RowFact = "other-reserved"
	RowOtherIntent   RowFact = "other-intent"
	RowDoneOther     RowFact = "done-other"
)

// JournalFact is the journal as the dispatch sees it.
type JournalFact string

const (
	JournalFactAbsent     JournalFact = "absent"
	JournalFactIntent     JournalFact = "intent"
	JournalFactIncomplete JournalFact = "incomplete" // stopped .. node-restarted
	JournalFactDone       JournalFact = "done"
)

// Dispatch is what a mutating run does, from the row and the journal.
type Dispatch string

const (
	// DispatchRequest: no row and no journal, the request proceeds (the
	// reservation is the request's first write).
	DispatchRequest Dispatch = "request"
	// DispatchAdopt: this host's reserved row and no journal, adopted, then
	// the request.
	DispatchAdopt Dispatch = "adopt"
	// DispatchAdvanceRow: this host's reserved row beside a journal at intent
	// (the crash between the two intent writes): validate the journal, advance
	// the row, resume.
	DispatchAdvanceRow Dispatch = "advance-row"
	// DispatchResume: this host's intent row beside an incomplete journal.
	DispatchResume Dispatch = "resume"
	// DispatchCompleteRow: a done journal whose row is not yet done, completed
	// by this host or, failing that, by the survivor.
	DispatchCompleteRow Dispatch = "complete-row"
	// DispatchDone: a done journal beside a done row.
	DispatchDone Dispatch = "done"
	// DispatchRefusedReserved: another host holds the reservation.
	DispatchRefusedReserved Dispatch = "refused-reserved"
	// DispatchRefusedRetired: the deployment has retired a controller already.
	DispatchRefusedRetired Dispatch = "refused-retired"
	// DispatchUnknownLedger: the row could not be read.
	DispatchUnknownLedger Dispatch = "unknown-ledger"
	// DispatchUnknownReservation: the row and the journal contradict each
	// other (a ledger restored from before this retirement, or a foreign one).
	DispatchUnknownReservation Dispatch = "unknown-reservation"
	// DispatchUnknownJournal: a journal removed by hand (a row past reserved
	// with no journal).
	DispatchUnknownJournal Dispatch = "unknown-journal"
)

// DispatchFor is the row-by-journal table, complete.
func DispatchFor(row RowFact, journal JournalFact) Dispatch {
	switch row {
	case RowUnreadable:
		return DispatchUnknownLedger
	case RowOtherReserved, RowOtherIntent:
		return DispatchRefusedReserved
	case RowDoneOther:
		return DispatchRefusedRetired
	}

	switch journal {
	case JournalFactAbsent:
		switch row {
		case RowAbsent:
			return DispatchRequest
		case RowReservedMine:
			return DispatchAdopt
		default: // intent-mine, done-mine
			return DispatchUnknownJournal
		}
	case JournalFactIntent:
		switch row {
		case RowReservedMine:
			return DispatchAdvanceRow
		case RowIntentMine:
			return DispatchResume
		default: // absent, done-mine
			return DispatchUnknownReservation
		}
	case JournalFactIncomplete:
		switch row {
		case RowIntentMine:
			return DispatchResume
		default: // absent, reserved-mine, done-mine
			return DispatchUnknownReservation
		}
	case JournalFactDone:
		switch row {
		case RowReservedMine, RowIntentMine:
			return DispatchCompleteRow
		case RowDoneMine:
			return DispatchDone
		default: // absent
			return DispatchUnknownReservation
		}
	}

	return DispatchUnknownReservation
}

// JournalFactOf classifies a journal's phase for the dispatch.
func JournalFactOf(present bool, phase Phase) JournalFact {
	switch {
	case !present:
		return JournalFactAbsent
	case phase == PhaseIntent:
		return JournalFactIntent
	case phase == PhaseDone:
		return JournalFactDone
	default:
		return JournalFactIncomplete
	}
}
