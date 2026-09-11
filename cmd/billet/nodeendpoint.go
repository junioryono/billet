package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/regularfile"
)

// What the endpoint commands share: the configuration as ONE OBSERVATION
// (read once through a descriptor, its bytes parsed, its identity kept and
// re-examined at the close, so an answer is never given over a file that is
// not the one judged); the node unit observed through lifeops; the running
// node's registration record read INSIDE A BRACKET of unit observations, so
// evidence is attributed to the process the observations saw; and the
// typed answers, one envelope for every outcome, with the exit codes the
// preparation uses (0 an answer, 2 a refusal, 3 could-not-tell).

const (
	// endpointSchema is the answers' schema number.
	endpointSchema = 1

	// The outcomes.
	outcomeMigrated  = "migrated"
	outcomeUnchanged = "unchanged"
	outcomeReported  = "reported"
	outcomeWritten   = "written"
	outcomeCurrent   = "current"
	outcomeConfirmed = "confirmed"
	outcomeTimeout   = "timeout"
	outcomeRefused   = "refused"
	outcomeUnknown   = "unknown"

	// The refusals' reasons, machine-readable beside the prose.
	endpointReasonPlatform    = "platform"
	endpointReasonCombination = "combination"
	endpointReasonConfig      = "config"
	endpointReasonDesired     = "desired"
	endpointReasonStopping    = "stopping"
	endpointReasonPolicy      = "policy"
	endpointReasonUnit        = "unit"
	endpointReasonRecord      = "record"
	endpointReasonPreR        = "pre-r"
	endpointReasonProcess     = "process"
	endpointReasonUnproved    = "unproved"
	endpointReasonNotRunning  = "not-running"
	endpointReasonMigration   = "migration"
	endpointReasonEvidence    = "evidence"
	endpointReasonConfirm     = "confirmation"
	endpointReasonAgreement   = "agreement"
	endpointReasonTrust       = "trust"
	endpointReasonUnbound     = "unbound"
	endpointReasonUnexamined  = "unexamined"

	// The states a migration is left in, named on every refusal.
	stateNothing         = "nothing"
	stateStopped         = "stopped"
	stateStarted         = "started"
	stateStartedNoRecord = "started-no-record"

	// The record's kinds on an answer.
	recordCurrent    = "current"
	recordNone       = "none"
	recordAbsentPreR = "absent-pre-r"
	recordUnread     = "unread"

	// maxRenderingBytes bounds the rendering read from stdin or a file.
	maxRenderingBytes = 1 << 20
	// maxEvidenceBytes bounds an evidence or a confirmation file.
	maxEvidenceBytes = 64 << 10
	// bracketAttempts is how many times a record read is retried when the
	// unit moved under it.
	bracketAttempts = 3
)

// endpointPoll is the interval the record waits and the ledger polls use;
// a variable so a test can shorten it.
var endpointPoll = time.Second

// endpointRefusal is the answer of a refusal (exit 2) or of something that
// could not be examined (exit 3), with the state the host was left in.
type endpointRefusal struct {
	Schema  int    `json:"schema"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Why     string `json:"why"`
	Next    string `json:"next,omitempty"`
	State   string `json:"state,omitempty"`
}

func endpointRefuse(reason, why, next, state string) *endpointRefusal {
	return &endpointRefusal{Schema: endpointSchema, Outcome: outcomeRefused, Reason: reason, Why: why, Next: next,
		State: state}
}

func endpointUnknown(reason, why, next, state string) *endpointRefusal {
	return &endpointRefusal{Schema: endpointSchema, Outcome: outcomeUnknown, Reason: reason, Why: why, Next: next,
		State: state}
}

// answerEndpointRefusal prints the refusal and returns the exit it carries.
func answerEndpointRefusal(r *endpointRefusal) error {
	code := exitRefused
	if r.Outcome == outcomeUnknown {
		code = exitUnknown
	}

	msg := r.Why
	if r.Next != "" {
		msg += ". Next: " + r.Next
	}

	return answerJSON(r, code, msg)
}

// installedConfigObservation is the installed configuration read ONCE: its bytes
// parsed, its digest taken, and the identity and metadata of the file they
// came from kept for the closing check. Absent is a positive answer of its
// own (the open's ENOENT), never a failed read.
type installedConfigObservation struct {
	path    string
	present bool
	info    os.FileInfo
	sha256  string
	cfg     *config.Config
}

// observeConfig reads the configuration at path through the identity-first
// open and parses it. The three outcomes are typed: present and parsed; absent
// (the positive ENOENT); or a refusal (unreadable is could-not-tell, malformed
// is refused).
func observeInstalledConfig(path string) (*installedConfigObservation, *endpointRefusal) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, endpointUnknown(endpointReasonConfig, fmt.Sprintf("resolve %s: %v", path, err), "", stateNothing)
	}

	obs := &installedConfigObservation{path: abs}

	f, info, err := regularfile.Open(abs, regularfile.Options{})
	switch {
	case errors.Is(err, os.ErrNotExist) && !errors.Is(err, regularfile.ErrReopen):
		return obs, nil
	case err != nil:
		return nil, endpointUnknown(endpointReasonConfig, fmt.Sprintf("open the installed configuration %s: %v", abs, err),
			"", stateNothing)
	}

	defer func() { _ = f.Close() }()

	body, err := regularfile.ReadAllLimited(f, abs, maxConfigBytes)
	if err != nil {
		return nil, endpointUnknown(endpointReasonConfig, fmt.Sprintf("read the installed configuration %s: %v", abs, err),
			"", stateNothing)
	}

	cfg, err := config.Parse(abs, body)
	if err != nil {
		return nil, endpointRefuse(endpointReasonConfig, fmt.Sprintf("the installed configuration %s does not parse: %v",
			abs, err), "", stateNothing)
	}

	sum := sha256.Sum256(body)
	obs.present, obs.info, obs.sha256, obs.cfg = true, info, hex.EncodeToString(sum[:]), cfg

	return obs, nil
}

// closingStat is the closing examination's stat, FOLLOWING the name as the
// open did: a configuration that is a symlink is compared through its
// target (the same file, or one retargeted since), never as the link's own
// inode against the target's. A test fails it.
var closingStat = os.Stat

// closeConfigObservation re-examines the pathname: the file must still be the
// one observed (identity, size, modification time), and an absence must still
// be an absence. The problem names what moved; "" means unchanged.
func closeInstalledConfig(obs *installedConfigObservation) string {
	info, err := closingStat(obs.path)

	switch {
	case !obs.present && errors.Is(err, os.ErrNotExist):
		return ""
	case !obs.present && err == nil:
		return "a configuration appeared at " + obs.path + " after it was observed absent"
	case err != nil:
		return fmt.Sprintf("re-examine the installed configuration %s: %v", obs.path, err)
	case !os.SameFile(info, obs.info):
		return "the installed configuration " + obs.path + " was replaced after it was observed"
	case info.Size() != obs.info.Size() || !info.ModTime().Equal(obs.info.ModTime()):
		return "the installed configuration " + obs.path + " was modified after it was observed"
	}

	return ""
}

// The input readers, seams so a test can fail a read the way a filesystem
// does (a chmod proves nothing under root).
var (
	renderingReadFile = regularfile.ReadFile
	answerReadFile    = regularfile.ReadFile
)

// inputReadUnknown says whether a failed read of an input file establishes
// nothing about the file: a positive absence, a special file at the name
// or a size over the bound are judgements (refused); anything else (EIO,
// EACCES, a failed reopen) is could-not-tell.
func inputReadUnknown(err error) bool {
	if errors.Is(err, regularfile.ErrReopen) {
		return true
	}

	return !errors.Is(err, os.ErrNotExist) && !errors.Is(err, regularfile.ErrNotRegular) &&
		!errors.Is(err, regularfile.ErrTooLarge)
}

// readRendering reads the rendering the role passes: "-" is stdin, anything
// else a file; bounded, and parsed under the configuration's own rules.
func readRendering(source string) ([]byte, *config.Config, *endpointRefusal) {
	var (
		body []byte
		err  error
	)

	if source == "-" {
		body, err = io.ReadAll(io.LimitReader(renderingStdin, maxRenderingBytes+1))
		if err == nil && len(body) > maxRenderingBytes {
			err = fmt.Errorf("%w: more than %d bytes", regularfile.ErrTooLarge, maxRenderingBytes)
		}
	} else {
		body, err = renderingReadFile(source, maxRenderingBytes, regularfile.Options{})
	}

	if err != nil {
		if inputReadUnknown(err) {
			return nil, nil, endpointUnknown(endpointReasonDesired, fmt.Sprintf("read the rendering (%s): %v", source, err),
				"", stateNothing)
		}

		return nil, nil, endpointRefuse(endpointReasonDesired, fmt.Sprintf("read the rendering (%s): %v", source, err),
			"", stateNothing)
	}

	cfg, err := config.Parse("the rendering", body)
	if err != nil {
		return nil, nil, endpointRefuse(endpointReasonDesired, "the rendering does not parse: "+err.Error(), "",
			stateNothing)
	}

	return body, cfg, nil
}

// renderingStdin is where "-" reads from; a variable so a test can supply it.
var renderingStdin io.Reader = os.Stdin

// nodeEndpointOf is a configuration's node endpoint as the one
// representation, and whether the configuration has a node section at all.
func nodeEndpointOf(cfg *config.Config) (endpoint.Endpoint, bool, error) {
	if cfg == nil || cfg.Node == nil {
		return endpoint.Endpoint{}, false, nil
	}

	e, err := endpoint.Parse(cfg.Node.ServerAddr, cfg.Node.TLS != nil)
	if err != nil {
		return endpoint.Endpoint{}, true, err
	}

	return e, true, nil
}

// unitObservation is one `systemctl show` of the node unit, the properties
// the endpoint commands decide on.
type unitObservation struct {
	LoadState, ActiveState, SubState, Result, KillMode string
	MainPID, InvocationID, StateChangeTimestamp        string
	// raw is the answer as read, for the missing-member rule: a property
	// systemd did not answer is absent from it.
	raw map[string][]string
}

var unitObservationNames = []string{"LoadState", "ActiveState", "SubState", "Result", "KillMode", "MainPID",
	"InvocationID", "StateChangeTimestamp"}

// observeUnit reads the node unit through the lifeops inspector (bounded by
// its timeout). A failed read is could-not-tell; a zero exit without
// ActiveState is could-not-tell too, never "not running".
func observeUnit(ctx context.Context, insp *lifeops.Inspector, unit string) (unitObservation, string) {
	props, err := insp.UnitProperties(ctx, unit, unitObservationNames...)
	if err != nil {
		return unitObservation{}, fmt.Sprintf("ask systemd about %s: %v", unit, err)
	}

	obs := unitObservation{
		LoadState: firstProp(props, "LoadState"), ActiveState: firstProp(props, "ActiveState"),
		SubState: firstProp(props, "SubState"), Result: firstProp(props, "Result"), KillMode: firstProp(props, "KillMode"),
		MainPID: firstProp(props, "MainPID"), InvocationID: firstProp(props, "InvocationID"),
		StateChangeTimestamp: firstProp(props, "StateChangeTimestamp"), raw: props,
	}

	if _, ok := props["ActiveState"]; !ok || obs.ActiveState == "" {
		return obs, fmt.Sprintf("systemd answered no ActiveState for %s, so its state cannot be told", unit)
	}

	return obs, ""
}

// runningPID reads the observation's process: the pid of a running main
// process, or running=false for a unit positively not running (inactive or
// failed), or a problem when the answer says neither (a state this does not
// know; a MainPID that is missing, empty or not a number; 0 beside a state
// that is not inactive or failed), which is uncertainty and never "not
// running".
func runningPID(obs unitObservation) (int, bool, string) {
	switch obs.ActiveState {
	case "inactive", "failed":
		return 0, false, ""
	case "active", "reloading", "activating", "deactivating":
	default:
		// A STATE THIS DOES NOT KNOW is neither running nor not: systemd's
		// vocabulary grows (maintenance, refreshing), and a guess either way
		// would authorise a stop or a receipt over it.
		return 0, false, "systemd answered ActiveState=" + strconv.Quote(obs.ActiveState) + ", which this does not judge"
	}

	if _, ok := obs.raw["MainPID"]; !ok || obs.MainPID == "" {
		return 0, false, "systemd answered no MainPID for a unit that is " + obs.ActiveState
	}

	n, err := strconv.Atoi(obs.MainPID)
	if err != nil || n < 0 {
		return 0, false, "systemd answered MainPID=" + strconv.Quote(obs.MainPID) + ", which is not a process"
	}

	if n == 0 {
		return 0, false, "systemd answered MainPID=0 for a unit that is " + obs.ActiveState + ", so whether a process runs cannot be told"
	}

	return n, true, ""
}

// processMoved says whether two observations of the unit disagree on the
// process the evidence is attributed to.
func processMoved(a, b unitObservation) bool {
	return a.MainPID != b.MainPID || a.InvocationID != b.InvocationID
}

// recordClass is what a record read under the bracket turned out to be.
type recordClass int

const (
	recordUsable     recordClass = iota + 1 // a record naming this invocation, this node, this deployment, canonical
	recordAbsent                            // no record, or one naming an earlier invocation: a node that has not registered yet
	recordForeign                           // a record that is not this node's, or malformed: refused at once
	recordUnreadable                        // the read failed, or the identity it is judged against could not be derived: could-not-tell
	recordInvalid                           // a record billet did not write (owner, mode, size, bytes, its directory): refused at once, never waited for
)

// classifyRecord judges the evidence as the inspector judges it, typed rather
// than by its words: usable, absent-or-stale (worth another poll), or foreign
// (refused at once), with the reason.
func classifyRecord(ev registrationEvidence, invocation string, identity registrationIdentity) (registrationReport, recordClass, string) {
	if ev.unreadable {
		return registrationReport{}, recordUnreadable, ev.why
	}

	if ev.invalid {
		return registrationReport{}, recordInvalid, ev.why
	}

	if ev.record == nil {
		return registrationReport{}, recordAbsent, ev.why
	}

	// A RECORD BILLET COULD NOT HAVE WRITTEN is invalid whatever invocation
	// it names: the endpoint is judged before the stale-invocation shortcut,
	// so a malformed record never waits and never reaches the pre-R probe.
	if _, err := endpoint.ParseCanonical(ev.record.Endpoint); err != nil {
		return registrationReport{}, recordInvalid, "the record's endpoint is not a canonical spelling: " + err.Error()
	}

	if ev.record.InvocationID != invocation {
		return registrationReport{}, recordAbsent, fmt.Sprintf("the record was written by invocation %s and the node is invocation %s",
			ev.record.InvocationID, invocation)
	}

	// THE IDENTITY THE RECORD IS JUDGED AGAINST: one that could not be
	// derived (a certificate or an identity file that could not be read, an
	// identity not minted) makes the judgement could-not-tell, never a
	// proved mismatch.
	if identity.why != "" {
		return registrationReport{}, recordUnreadable, "the record's identity cannot be judged: " + identity.why
	}

	judged := judgeRegistration(ev, known(invocation), identity)
	if !judged.known {
		return registrationReport{}, recordForeign, judged.why
	}

	rec, ok := judged.value.(registrationReport)
	if !ok {
		return registrationReport{}, recordForeign, "the registration report has an unexpected shape"
	}

	return rec, recordUsable, ""
}

// bracketedRecord is the result of a record read inside a bracket.
type bracketedRecord struct {
	obs     unitObservation // the observation the record is attributed to
	record  registrationReport
	class   recordClass
	why     string
	running bool
}

// recordWaitEndedUnderMove is the problem a bracket answers when the unit
// moved under its read and the wait ended before it could read again.
const recordWaitEndedUnderMove = "the wait ended while the unit moved under the read of its record"

// readRecordUnderBracket reads the running node's record inside a bracket of
// unit observations: the unit observed, the record read, the unit observed
// again, and the read kept only when the two observations agree on the
// process; a unit that moved is retried, three attempts, then could-not-tell.
// A retry is a bracket too, and none starts after a non-zero deadline. It
// answers running=false for a unit positively not running (no record
// consulted), a problem for an observation that could not decide, and the
// record's class otherwise.
func readRecordUnderBracket(ctx context.Context, insp *lifeops.Inspector, unit string, identity registrationIdentity, deadline time.Time) (bracketedRecord, string) {
	for attempt := range bracketAttempts {
		if attempt > 0 && !deadline.IsZero() && time.Now().After(deadline) {
			return bracketedRecord{}, recordWaitEndedUnderMove
		}

		before, problem := observeUnit(ctx, insp, unit)
		if problem != "" {
			return bracketedRecord{}, problem
		}

		_, running, problem := runningPID(before)
		if problem != "" {
			return bracketedRecord{}, problem
		}

		if !running {
			return bracketedRecord{obs: before, running: false}, ""
		}

		if before.InvocationID == "" {
			return bracketedRecord{}, "systemd answered no InvocationID for the running " + unit + ", so no record can be bound to it"
		}

		ev := readRegistrationRecord(registrationRecordPath)

		if inspectAfterRecordRead != nil {
			inspectAfterRecordRead()
		}

		after, problem := observeUnit(ctx, insp, unit)
		if problem != "" {
			return bracketedRecord{}, problem
		}

		// THE CLOSING OBSERVATION IS READ AS THE OPENING ONE WAS: a state this
		// does not judge is could-not-tell, a unit positively stopped under the
		// read is not running, and only then is the process compared.
		if _, stillRunning, problem := runningPID(after); problem != "" {
			return bracketedRecord{}, "at the close of the read: " + problem
		} else if !stillRunning {
			return bracketedRecord{obs: after, running: false}, ""
		}

		if processMoved(before, after) {
			continue
		}

		rec, class, why := classifyRecord(ev, before.InvocationID, identity)

		return bracketedRecord{obs: before, record: rec, class: class, why: why, running: true}, ""
	}

	return bracketedRecord{}, fmt.Sprintf("the %s moved under every attempt to read its record", unit)
}

// waitForRecord polls readRecordUnderBracket until the record is usable, the
// unit stops running, a foreign record refuses, or the wait elapses; the last
// bracket is returned with elapsed=true on exhaustion.
func waitForRecord(ctx context.Context, insp *lifeops.Inspector, unit string, identity registrationIdentity, wait time.Duration) (bracketedRecord, bool, string) {
	// NO BRACKET STARTS AFTER THE WAIT, and the sleep between brackets never
	// outlasts it: a record that appears after the deadline is the next
	// converge's. A bracket already running finishes under its own bounds
	// (the inspector's property timeout), so its observation is whole.
	deadline := time.Now().Add(wait)

	var last bracketedRecord

	for attempt := 0; ; attempt++ {
		// THE DEADLINE IS JUDGED BEFORE EVERY BRACKET BUT THE FIRST: a sleep
		// that ended at the deadline starts nothing, and what was last
		// observed is the answer.
		if attempt > 0 && (time.Now().After(deadline) || ctx.Err() != nil) {
			return last, true, ""
		}

		br, problem := readRecordUnderBracket(ctx, insp, unit, identity, deadline)
		if problem == recordWaitEndedUnderMove && last.obs.ActiveState != "" {
			return last, true, ""
		}

		if problem != "" {
			return bracketedRecord{}, false, problem
		}

		last = br

		if !br.running || br.class != recordAbsent {
			return br, false, ""
		}

		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			return br, true, ""
		}

		select {
		case <-ctx.Done():
			return br, true, ""
		case <-time.After(min(endpointPoll, remaining)):
		}
	}
}

// identityFor derives the effective identity the record must name from the
// configuration given (the installed one when it has a node section, else
// the rendering's), as the inspector derives it.
func identityFor(installed, rendering *config.Config) registrationIdentity {
	if installed != nil && installed.Node != nil {
		return expectedRegistrationIdentity(installed)
	}

	if rendering != nil && rendering.Node != nil {
		return expectedRegistrationIdentity(rendering)
	}

	return registrationIdentity{why: "neither configuration has a node section"}
}

// endpointInspector is the lifeops inspector the endpoint commands run
// systemctl through, on the binary the inspector uses (a test's fake).
func endpointInspector() *lifeops.Inspector {
	return lifeops.NewInspector(lifeops.WithSystemctl(systemctlBinary), lifeops.WithWaitDelay(guardWaitDelay))
}

// preRProcess says whether a running process is positively a release before
// the guard: its image is the managed binary's bytes, and the managed binary
// answers "unknown command" to the preparation's dry run.
func preRProcess(ctx context.Context, pid int) (bool, string) {
	imageSum, _, err := hashImage(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
	if err != nil {
		return false, fmt.Sprintf("hash the process image of %d: %v", pid, err)
	}

	managedSum, _, err := hashRegular(installedBinary, maxExecutableBytes)
	if err != nil {
		return false, fmt.Sprintf("hash the managed binary %s: %v", installedBinary, err)
	}

	if imageSum != managedSum {
		return false, "the running process is not the managed binary's image"
	}

	stdout, stderr, rc, err := runBounded(ctx, installedBinary, "converge-guard", "prepare", "--dry-run", "--json")

	// ONLY A COMPLETED RUN IS JUDGED: a run the bound ended, one whose output
	// overflowed, or one that could not be run has no answer, and a prefix
	// holding the words is not the diagnostic.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return false, "the managed binary did not answer the preparation's dry run within its bound"
	case errors.Is(err, errOutputOverflow):
		return false, "the managed binary's answer to the preparation's dry run overflowed its bound"
	case rc < 0:
		return false, "the managed binary could not be run: " + err.Error()
	}

	if rc != 0 && unknownCommandPattern.Match(append(append([]byte{}, stdout...), stderr...)) {
		return true, ""
	}

	return false, "the managed binary answers the preparation, so the running process is a release that carries the guard"
}

// answerObject prints one successful answer as JSON with exit 0.
func answerObject(v any) error {
	return answerJSON(v, 0, "")
}

// canonicalOrNull is an endpoint's canonical text, or nil for none.
func canonicalOrNull(e endpoint.Endpoint, present bool) *string {
	if !present {
		return nil
	}

	s := e.String()

	return &s
}

// stringOrNull is a string, or nil for an empty one.
func stringOrNull(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
