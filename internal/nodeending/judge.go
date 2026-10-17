package nodeending

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

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
	// Acknowledge is set on a verdict the record decided against: the
	// `billet node ending acknowledge` arguments that would admit a start over
	// it, which is all an acknowledgement does.
	Acknowledge string `json:"acknowledge,omitempty"`
	// Floor says systemd's account of a stop the record holds no entry for
	// decided the verdict, which the caller records (Observe) so the start
	// after it reads the same answer.
	Floor bool `json:"-"`
}

func judged(v Verdict, proof string, e *Entry, why string, args ...any) Judgement {
	return Judgement{Verdict: v, Answer: v.String(), Proof: proof, Entry: e, Why: fmt.Sprintf(why, args...)}
}

// Unit is what systemd answered about the node unit; an empty field is a
// property it did not answer, except Job, which is empty when no job is
// pending, so JobAnswered says whether systemd answered it.
type Unit struct {
	ActiveState string
	SubState    string
	Result      string
	MainPID     string
	Job         string
	JobAnswered bool
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
	case !u.JobAnswered:
		return Unknown, "systemd did not answer Job"
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

// standard is what a judgement asks of the host a stop left.
type standard int

const (
	// forStart asks whether a node may run over it: whatever was left is the
	// next process's to adopt, so a handoff and an acknowledgement pass.
	forStart standard = iota
	// forDrain asks whether nothing is left running.
	forDrain
)

// standing says what the node processes up to and including entry i left this
// host, read newest first. AN ENDING THAT TOUCHED NOTHING INHERITS: an idle
// process never registered and so never adopted what the one before it left,
// and systemd's clean stop of a process that kept no record says nothing of
// what it left; both pass the question to the process before them, so neither
// can cover an ending that did not pass. Running out of entries is the first
// recorded process, proved, unless the record was trimmed, when it is
// could-not-tell.
func standing(rec Record, i int, std standard, bootID string) Judgement {
	var via []string

	proof := ProofRecord

	for ; i >= 0; i-- {
		e := rec.Entries[i]
		if e.Role == RoleProbe {
			continue
		}

		switch {
		case std == forStart && e.Acknowledged:
			return through(via, judged(Proved, weakest(proof, ProofAcknowledged), &e,
				"the stop of %s was acknowledged", describe(e)))
		case e.Source == SourceNode && e.Ending == Drained:
			return through(via, judged(Proved, proof, &e, "%s drained", describe(e)))
		case e.RunsOnHost && e.BootID != "" && bootID != "" && e.BootID != bootID:
			return through(via, judged(Proved, weakest(proof, ProofReboot), &e,
				"%s ran in an earlier boot, and the reboot ended the compute it ran on this host", describe(e)))
		case e.Source == SourceNode && e.Ending == HandedOff && std == forStart:
			return through(via, judged(Proved, proof, &e, "%s handed its compute to the next process to adopt",
				describe(e)))
		case e.Source == SourceNode && e.Ending == HandedOff:
			return through(via, judged(Unproved, "", &e, "%s handed its compute to a next process, which never "+
				"adopted it", describe(e)))
		case e.Source == SourceNode && e.Ending == Idle:
			via = append(via, describe(e)+" ended idle")
		case e.Source == SourceSystemd && e.Ending == Stopped:
			via = append(via, describe(e)+" kept no record, and systemd reported it stopped cleanly")
			proof = ProofSystemd
		case e.Ending == Running:
			return through(via, judged(Unproved, "", &e, "%s never wrote its ending, so it was killed or crashed",
				describe(e)))
		default:
			return through(via, judged(Unproved, "", &e, "%s ended %s", describe(e), e.Ending))
		}
	}

	if rec.Trimmed {
		return through(via, judged(Unknown, "", nil, "the record was trimmed past the process before it"))
	}

	return through(via, judged(Proved, proof, nil, "no earlier process is recorded"))
}

// weakest is a proved chain's label: systemd's floor, once the chain passed a
// stop only systemd spoke for, whatever proved the entry beneath it.
func weakest(chain, proof string) string {
	if chain == ProofSystemd {
		return ProofSystemd
	}

	return proof
}

// through prefixes a judgement with the endings it inherited through.
func through(via []string, j Judgement) Judgement {
	if len(via) > 0 {
		j.Why = strings.Join(via, "; before it, ") + "; before it, " + j.Why
	}

	return j
}

// Judge says whether a stop of the node invocation named is proved: no process
// remains, the invocation is the newest process the record holds, and its own
// entry says it ended as the stop required. drainRequired, or a drain the
// process itself was asked for, means nothing may be left running; otherwise a
// handoff passes too. An idle ending is judged by the process before it. With
// no entry for the invocation (a node binary from before the record) systemd's
// inactive/dead/success proves it, as the floor labelled ProofSystemd, and only
// over a history that passes the stop's own standard.
func Judge(rec Record, readErr error, invocation string, after Unit, drainRequired bool, bootID string) Judgement {
	if readErr != nil {
		return unreadable(readErr)
	}

	if v, why := after.NoProcess(); v != Proved {
		return judged(v, "", nil, "%s", why)
	}

	if invocation == "" {
		return judged(Unknown, "", nil, "the stopped invocation is not known, so no entry can be bound to it")
	}

	// AN UNANSWERED PROPERTY IS NOT A FAILURE. Where systemd's triple is what
	// decides, a stop it did not fully describe is could-not-tell, and is not
	// recorded, since a recorded failure stands for good.
	if entryFor(rec, invocation) < 0 && observedFor(rec, invocation) < 0 &&
		(after.SubState == "" || after.Result == "") {
		return judged(Unknown, "", nil, "invocation %s kept no ending record and systemd reports %s",
			invocation, after.triple())
	}

	return remedied(rec, judgeInvocation(rec, invocation, after, drainRequired, bootID))
}

func judgeInvocation(rec Record, invocation string, after Unit, drainRequired bool, bootID string) Judgement {
	i := entryFor(rec, invocation)
	if i < 0 {
		// WHAT SYSTEMD SAID FIRST STANDS: a stop recorded failed before
		// reset-failed made the triple clean is still the stop that failed.
		if o := observedFor(rec, invocation); o >= 0 && rec.Entries[o].Ending != Stopped {
			e := rec.Entries[o]

			return judged(Unproved, "", &e, "invocation %s kept no ending record, and systemd reported its stop "+
				"%s when it was first judged", invocation, e.Ending)
		}

		if !after.cleanTriple() {
			j := judged(Unproved, "", nil, "invocation %s kept no ending record and systemd reports %s",
				invocation, after.triple())
			j.Floor = true

			return j
		}

		if n := newestNode(rec.Entries); n >= 0 {
			if s := standing(rec, n, standardFor(drainRequired), bootID); s.Verdict != Proved {
				s.Why = fmt.Sprintf("invocation %s kept no ending record, so only systemd's %s speaks for it, "+
					"and that covers a process only over a history that passes this stop as well: %s",
					invocation, after.triple(), s.Why)
				if drainRequired {
					s.Why += "; a release that keeps no record cannot prove a drain itself, so one is proved " +
						"only once the node runs a release that does"
				}

				return s
			}
		}

		j := judged(Proved, ProofSystemd, nil, "invocation %s kept no ending record (a node from before "+
			"the record), and systemd reports %s: the floor, which a handoff or a cut-short drain would "+
			"also pass", invocation, after.triple())
		j.Floor = true

		return j
	}

	e := rec.Entries[i]

	if n := newestNode(rec.Entries); n != i {
		newer := rec.Entries[n]

		return judged(Unproved, "", &newer, "invocation %s is not the newest process: %s came after it, so "+
			"the stop may have ended that one", invocation, describe(newer))
	}

	std := standardFor(drainRequired || e.DrainRequested)

	switch {
	case e.Ending == Drained:
		return judged(Proved, ProofRecord, &e, "invocation %s drained", invocation)
	case e.Ending == HandedOff && std == forStart:
		return judged(Proved, ProofRecord, &e, "invocation %s ended %s", invocation, e.Ending)
	case e.Ending == HandedOff:
		return judged(Unproved, "", &e, "invocation %s ended %s, and this stop required a drain", invocation,
			e.Ending)
	case e.Ending == Idle:
		s := standing(rec, i-1, std, bootID)
		s.Why = fmt.Sprintf("invocation %s ended idle, so the host is as the process before it left it: %s",
			invocation, s.Why)

		return s
	case e.Ending == Running:
		return judged(Unproved, "", &e, "invocation %s never wrote its ending, so it was killed or crashed "+
			"(systemd: %s)", invocation, after.triple())
	default:
		return judged(Unproved, "", &e, "invocation %s ended %s", invocation, e.Ending)
	}
}

// standardFor is the standard a stop that needs a drain, or does not, is
// judged at.
func standardFor(drainRequired bool) standard {
	if drainRequired {
		return forDrain
	}

	return forStart
}

// observedFor is the index of the systemd observation of invocation, or -1.
func observedFor(rec Record, invocation string) int {
	for i := len(rec.Entries) - 1; i >= 0; i-- {
		e := rec.Entries[i]
		if e.InvocationID == invocation && e.Role == RoleNode && e.Source == SourceSystemd {
			return i
		}
	}

	return -1
}

// entryFor is the index of the newest node-written entry for invocation, or -1.
func entryFor(rec Record, invocation string) int {
	for i := len(rec.Entries) - 1; i >= 0; i-- {
		e := rec.Entries[i]
		if e.InvocationID == invocation && e.Role == RoleNode && e.Source == SourceNode {
			return i
		}
	}

	return -1
}

// AdmitStart says whether a node may be started on this host: no process
// remains, and the node processes the record holds left nothing a start may not
// run over (an acknowledgement, a drain, a handoff, a reboot since a host-run
// node, read through idle endings and systemd's clean stops). With no node
// entry at all, systemd's account of the unit as it is now decides. now is the
// unit as systemd reports it before the start.
func AdmitStart(rec Record, readErr error, now Unit, bootID string) Judgement {
	return Standing(rec, readErr, now, false, bootID)
}

// Standing judges what the newest node process left this host, for a stop that
// found no process to stop and for a start: drainRequired asks that nothing be
// left running, otherwise only what a start may run over. With no node entry at
// all, systemd's account of the unit as it is now decides.
func Standing(rec Record, readErr error, now Unit, drainRequired bool, bootID string) Judgement {
	if readErr != nil {
		return unreadable(readErr)
	}

	if v, why := now.NoProcess(); v != Proved {
		return judged(v, "", nil, "%s", why)
	}

	i := newestNode(rec.Entries)
	if i < 0 {
		if now.cleanTriple() {
			return judged(Proved, ProofSystemd, nil, "no ending is recorded and systemd reports %s", now.triple())
		}

		return remedied(rec, judged(Unproved, "", nil, "no ending is recorded and systemd reports %s",
			now.triple()))
	}

	return remedied(rec, standing(rec, i, standardFor(drainRequired), bootID))
}

// unreadable is could-not-tell for a record that cannot be read. Only a
// record that was opened and found wanting is reset; one this process may not
// open (a check run as someone other than root) is root's to read, not to
// set aside.
func unreadable(readErr error) Judgement {
	j := judged(Unknown, "", nil, "the ending record could not be read: %v", readErr)
	if errors.Is(readErr, fs.ErrPermission) {
		j.Why += "; it is root's, so judge it as root"

		return j
	}

	j.Acknowledge = "--reset-unreadable"

	return j
}

// remedied names, on a verdict the record decided against, the
// acknowledgement that would admit a start: the newest node entry by its
// invocation, or --unrecorded where there is none or it has no invocation.
func remedied(rec Record, j Judgement) Judgement {
	if j.Verdict == Proved {
		return j
	}

	// An acknowledgement already made is not named again: what it could admit,
	// it has, and what is refused needs a drain it cannot give.
	if i := newestNode(rec.Entries); i < 0 || !rec.Entries[i].Acknowledged {
		j.Acknowledge = Remedy(rec)
	}

	return j
}

// Remedy is the `billet node ending acknowledge` arguments that admit a start
// over rec: its newest node entry by invocation, or --unrecorded where there is
// none or it has no invocation.
func Remedy(rec Record) string {
	if i := newestNode(rec.Entries); i >= 0 && rec.Entries[i].InvocationID != "" {
		return "--invocation " + rec.Entries[i].InvocationID
	}

	return "--unrecorded"
}

func describe(e Entry) string {
	return fmt.Sprintf("invocation %q (pid %d, started %s)", e.InvocationID, e.PID,
		e.StartedAt.Format("2006-01-02T15:04:05Z"))
}
