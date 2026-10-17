package nodeending

import "fmt"

// Verdict is a judgement's answer: a check answers yes, no or could-not-tell,
// and could-not-tell is never no.
type Verdict int

const (
	// Unknown is the zero value: a judgement nobody reached refuses.
	Unknown Verdict = iota
	// Proved is the yes.
	Proved
	// Unproved is the no.
	Unproved
)

func (v Verdict) String() string {
	switch v {
	case Proved:
		return "proved"
	case Unproved:
		return "unproved"
	default:
		return "unknown"
	}
}

// Proof names what a proved judgement rests on.
const (
	ProofRecord       = "record"
	ProofSystemd      = "systemd"
	ProofAcknowledged = "acknowledged"
	ProofReboot       = "reboot"
)

// Judgement is a verdict with what it rests on and why.
type Judgement struct {
	Verdict Verdict `json:"-"`
	Answer  string  `json:"verdict"`
	// Proof is set only on a proved verdict.
	Proof string `json:"proof,omitempty"`
	// Entry is the entry the verdict read, when one did.
	Entry *Entry `json:"entry,omitempty"`
	Why   string `json:"why"`
}

func judged(v Verdict, proof string, e *Entry, why string, args ...any) Judgement {
	return Judgement{Verdict: v, Answer: v.String(), Proof: proof, Entry: e, Why: fmt.Sprintf(why, args...)}
}

// Unit is what systemd answered about the node unit; an empty field is a
// property it did not answer.
type Unit struct {
	ActiveState string
	SubState    string
	Result      string
	MainPID     string
	Job         string
}

// NoProcess says whether systemd positively reports no process and no pending
// job: inactive or failed, MainPID 0, Job empty. "failed" is in the allowlist
// because the question here is the process; how it ended is the record's.
func (u Unit) NoProcess() (Verdict, string) {
	switch u.ActiveState {
	case "inactive", "failed":
	case "":
		return Unknown, "systemd did not answer ActiveState"
	default:
		return Unproved, fmt.Sprintf("the unit is %s, so its process is not proved gone", u.ActiveState)
	}

	switch {
	case u.MainPID == "":
		return Unknown, "systemd did not answer MainPID"
	case u.MainPID != "0":
		return Unproved, fmt.Sprintf("the unit still names MainPID %s", u.MainPID)
	case u.Job != "":
		return Unproved, fmt.Sprintf("the unit has a pending %s job", u.Job)
	}

	return Proved, ""
}

// cleanTriple is systemd's inactive/dead/success.
func (u Unit) cleanTriple() bool {
	return u.ActiveState == "inactive" && u.SubState == "dead" && u.Result == "success"
}

func (u Unit) triple() string {
	return fmt.Sprintf("%s/%s/%s", orUnanswered(u.ActiveState), orUnanswered(u.SubState), orUnanswered(u.Result))
}

func orUnanswered(s string) string {
	if s == "" {
		return "(unanswered)"
	}

	return s
}

// Judge says whether a stop of the node invocation named is proved: no process
// remains, and the invocation's own entry says it ended as the stop required.
// drainRequired means only a drain proves it; otherwise an idle stop, and a
// handoff that was not asked to drain, do too. With no entry for the invocation
// (a node binary from before the record) systemd's inactive/dead/success proves
// it, as the weaker floor, labelled ProofSystemd.
func Judge(rec Record, readErr error, invocation string, after Unit, drainRequired bool) Judgement {
	if readErr != nil {
		return judged(Unknown, "", nil, "the ending record could not be read: %v", readErr)
	}

	if v, why := after.NoProcess(); v != Proved {
		return judged(v, "", nil, "%s", why)
	}

	if invocation == "" {
		return judged(Unknown, "", nil, "the stopped invocation is not known, so no entry can be bound to it")
	}

	e := entryFor(rec, invocation)
	if e == nil {
		if after.cleanTriple() {
			return judged(Proved, ProofSystemd, nil, "invocation %s kept no ending record (a node from before "+
				"the record), and systemd reports %s: the floor, which a handoff or a cut-short drain would "+
				"also pass", invocation, after.triple())
		}

		return judged(Unproved, "", nil, "invocation %s kept no ending record and systemd reports %s", invocation,
			after.triple())
	}

	switch e.Ending {
	case Drained:
		return judged(Proved, ProofRecord, e, "invocation %s drained", invocation)
	case Idle, HandedOff:
		if drainRequired || e.DrainRequested {
			return judged(Unproved, "", e, "invocation %s ended %s, and this stop required a drain", invocation,
				e.Ending)
		}

		return judged(Proved, ProofRecord, e, "invocation %s ended %s", invocation, e.Ending)
	case Running:
		return judged(Unproved, "", e, "invocation %s never wrote its ending, so it was killed or crashed "+
			"(systemd: %s)", invocation, after.triple())
	default:
		return judged(Unproved, "", e, "invocation %s ended %s", invocation, e.Ending)
	}
}

// entryFor is the newest node-written entry for invocation.
func entryFor(rec Record, invocation string) *Entry {
	for i := len(rec.Entries) - 1; i >= 0; i-- {
		e := rec.Entries[i]
		if e.InvocationID == invocation && e.Role == RoleNode && e.Source == SourceNode {
			return &e
		}
	}

	return nil
}

// AdmitStart says whether a node may be started on this host, from the newest
// node entry: a drained, idle or handed-off ending, a clean stop systemd
// reported for a process that kept no record, an acknowledged stop, or an
// ending from an earlier boot on a host-run provider (the reboot ended its
// compute). With no node entry at all, systemd's account of the unit as it is
// now decides. now is the unit as systemd reports it before the start.
func AdmitStart(rec Record, readErr error, now Unit, bootID string) Judgement {
	if readErr != nil {
		return judged(Unknown, "", nil, "the ending record could not be read: %v", readErr)
	}

	if v, why := now.NoProcess(); v != Proved {
		return judged(v, "", nil, "%s", why)
	}

	i := newestNode(rec.Entries)
	if i < 0 {
		if now.cleanTriple() {
			return judged(Proved, ProofSystemd, nil, "no ending is recorded and systemd reports %s", now.triple())
		}

		return judged(Unproved, "", nil, "no ending is recorded and systemd reports %s; acknowledge "+
			"--unrecorded once the host is known to run nothing a node would not adopt", now.triple())
	}

	e := rec.Entries[i]

	switch {
	case e.Acknowledged:
		return judged(Proved, ProofAcknowledged, &e, "the newest stop (%s) was acknowledged", describe(e))
	case e.Source == SourceNode && (e.Ending == Drained || e.Ending == Idle || e.Ending == HandedOff):
		return judged(Proved, ProofRecord, &e, "the newest node process ended %s", e.Ending)
	case e.Source == SourceSystemd && e.Ending == Stopped:
		return judged(Proved, ProofSystemd, &e, "the newest stop was a node that kept no record, and systemd "+
			"reported it inactive/dead/success")
	case e.RunsOnHost && e.BootID != "" && bootID != "" && e.BootID != bootID:
		return judged(Proved, ProofReboot, &e, "the newest node process ended %s in an earlier boot, and the "+
			"reboot ended the compute it ran on this host", e.Ending)
	}

	return judged(Unproved, "", &e, "the newest node process ended %s (%s); acknowledge --invocation %q once "+
		"what it left is known", e.Ending, describe(e), e.InvocationID)
}

func describe(e Entry) string {
	return fmt.Sprintf("invocation %q, pid %d, started %s", e.InvocationID, e.PID,
		e.StartedAt.Format("2006-01-02T15:04:05Z"))
}
