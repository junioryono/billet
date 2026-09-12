package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
)

// `billet server retire` is a controller's retirement as a command, the role
// its thin caller. This file holds the modes that touch the records alone: the
// reservation the request writes before it collects a single report, its
// abandonment, the survivor's completion of the ledger row and the retiring
// host's acknowledgement of it, and the read-only dry run that classifies the
// host. The request itself (`--input -`) and the transition's phases are the
// next change's.
//
// THE ORDER OF EVERY MUTATING MODE: the transaction lock and this converge's
// guard first; the configuration observed ONCE under them, from one read (it
// names the identity directory the next step is keyed by); then the identity
// exclusion for that directory, held through the identity read, the ledger's
// open and the row's write. Nothing a concurrent converge or transaction
// moved between an early check and the write is what the write rests on,
// because every check is made under the transaction lock.

// retireSchema numbers every answer this command prints.
const retireSchema = 1

// The outcomes.
const (
	retireOutcomeReserved     = "reserved"
	retireOutcomeAdopted      = "adopted"
	retireOutcomeAbandoned    = "abandoned"
	retireOutcomeAcknowledged = "acknowledged"
	retireOutcomeAlready      = "already"
	retireOutcomeReported     = "reported"
	retireOutcomeRefused      = "refused"
	retireOutcomeUnknown      = "unknown"
)

// The reason codes a refusal carries; a role dispatches on these words.
const (
	retireReasonCombination = "combination"
	retireReasonPlatform    = "platform"
	retireReasonConfig      = "config"
	retireReasonBackend     = "backend"
	retireReasonControllers = "controllers"
	retireReasonIdentity    = "identity"
	retireReasonLock        = "lock"
	retireReasonGuard       = "guard"
	retireReasonJournal     = "journal"
	retireReasonReserved    = "reserved"
	retireReasonRetired     = "retired"
	retireReasonLedger      = "ledger"
	retireReasonInput       = "input"
	retireReasonSelf        = "self"
	retireReasonDeployment  = "deployment"
	retireReasonMismatch    = "mismatch"
	retireReasonConflict    = "conflict"
	retireReasonStage       = "stage-stale"
	retireReasonStatus      = "status"
	retireReasonMarker      = "marker"
	retireReasonReservation = "reservation"
)

// stateNothingRetire is the one `state` this file's modes can leave: nothing
// on the host was stopped, moved or rewritten.
const stateNothingRetire = "nothing"

// retireRefusal is the answer of a refusal (exit 2) or of something that
// could not be examined (exit 3).
type retireRefusal struct {
	Schema  int    `json:"schema"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Why     string `json:"why"`
	Next    string `json:"next,omitempty"`
	State   string `json:"state"`
	// Reservation says what a refused request did with the row: `released`
	// (this converge inserted it), `kept` (adopted, or the release failed).
	Reservation string `json:"reservation,omitempty"`
}

func retireRefuse(reason, why, next string) *retireRefusal {
	return &retireRefusal{Schema: retireSchema, Outcome: retireOutcomeRefused, Reason: reason, Why: why, Next: next,
		State: stateNothingRetire}
}

func retireUnknown(reason, why, next string) *retireRefusal {
	return &retireRefusal{Schema: retireSchema, Outcome: retireOutcomeUnknown, Reason: reason, Why: why, Next: next,
		State: stateNothingRetire}
}

// retireFromEndpoint carries a configuration observation's refusal, made by
// the endpoint commands' observer, into this command's vocabulary.
func retireFromEndpoint(r *endpointRefusal) *retireRefusal {
	if r.Outcome == outcomeUnknown {
		return retireUnknown(retireReasonConfig, r.Why, r.Next)
	}

	return retireRefuse(retireReasonConfig, r.Why, r.Next)
}

func answerRetireRefusal(r *retireRefusal) error {
	code := exitRefused
	if r.Outcome == retireOutcomeUnknown {
		code = exitUnknown
	}

	return answerJSON(r, code, r.Why)
}

// retireReservationAnswer is `--reserve`'s answer: the row as the ledger
// holds it, with the reservation time AS THE ROW SPELLS IT, because the
// request that follows admits only reports collected after it and the
// completion compares it for equality.
type retireReservationAnswer struct {
	Schema       int    `json:"schema"`
	Outcome      string `json:"outcome"`
	Deployment   string `json:"deployment"`
	Retiring     string `json:"retiring"`
	Survivor     string `json:"survivor"`
	Run          string `json:"run"`
	TransitionID string `json:"transition_id"`
	ReservedAt   string `json:"reserved_at"`
	State        string `json:"state"`
}

// retireAbandonedAnswer is `--abandon-reservation`'s answer.
type retireAbandonedAnswer struct {
	Schema       int    `json:"schema"`
	Outcome      string `json:"outcome"`
	TransitionID string `json:"transition_id"`
	// Marker says what happened to the guard's marker: cleared, or absent.
	Marker string `json:"marker"`
	State  string `json:"state"`
}

// retireAcknowledgedAnswer is `--acknowledge-row`'s answer.
type retireAcknowledgedAnswer struct {
	Schema       int    `json:"schema"`
	Outcome      string `json:"outcome"`
	TransitionID string `json:"transition_id"`
	CompletedBy  string `json:"completed_by"`
	State        string `json:"state"`
}

// retireReport is the dry run's answer: every record as it stands, read
// without a lock, and the dispatch the table decides from them.
type retireReport struct {
	Schema   int                  `json:"schema"`
	Outcome  string               `json:"outcome"`
	Journal  *retireReportJournal `json:"journal"`
	Status   *retirement.Status   `json:"status"`
	Stage    string               `json:"stage"`
	Guard    string               `json:"guard"`
	Marker   *guardTransition     `json:"marker"`
	Row      *retireReportRow     `json:"row"`
	RowFact  retirement.RowFact   `json:"row_fact"`
	Dispatch retirement.Dispatch  `json:"dispatch"`
	Why      string               `json:"why,omitempty"`
	State    string               `json:"state"`
}

type retireReportJournal struct {
	Phase   retirement.Phase   `json:"phase"`
	Variant retirement.Variant `json:"variant"`
	RowDone bool               `json:"row_done"`
	Settled bool               `json:"settled"`
	Owner   string             `json:"owner"`
	// TransitionID is the journal's provenance id.
	TransitionID string `json:"transition_id"`
}

type retireReportRow struct {
	Retiring     string `json:"retiring"`
	Survivor     string `json:"survivor"`
	Run          string `json:"run"`
	State        string `json:"state"`
	TransitionID string `json:"transition_id"`
	ReservedAt   string `json:"reserved_at"`
}

// retireMode is the parsed flag table.
type retireMode struct {
	configPath      string
	run             string
	retiringHost    string
	survivorHost    string
	installedSHA    string
	asHost          string
	completion      string
	answer          string
	environmentFile string

	reserve     bool
	abandon     bool
	completeRow bool
	acknowledge bool
	dryRun      bool

	// The request's operands.
	input            string
	serverOnly       bool
	survivorFlagged  bool
	sharedAddresses  []string
	failoverVerified bool
	reservationFresh bool
	reportMaxAge     time.Duration
}

// The seams: the clock, stdin, the transition id's minting, and the re-exec
// that reads a SQLite row as the ledger's owner.
var (
	retireNow                     = time.Now
	retireStdin         io.Reader = os.Stdin
	retireTransitionID            = newGuardID
	retireReexecCapture           = reexecCapture
)

func cmdServerRetire(ctx context.Context, args []string) error {
	flags := newFlagSet("billet server retire")
	m := retireMode{}

	flags.StringVar(&m.configPath, "config", defaultConfigPath(), "the installed configuration")
	flags.StringVar(&m.run, "run", "", "the holder of this converge's guard (required)")
	flags.StringVar(&m.retiringHost, "retiring-host", "", "this host's inventory name (required)")
	flags.StringVar(&m.survivorHost, "survivor-host", "", "the surviving controller's inventory name (with --reserve)")
	flags.StringVar(&m.installedSHA, "installed-sha256", "", "the installed configuration's digest as the role read it (with --reserve)")
	flags.StringVar(&m.asHost, "as-host", "", "the inventory name of the host running --complete-row, the survivor's")
	flags.StringVar(&m.completion, "completion", "", "the completion document: - for stdin (with --complete-row)")
	flags.StringVar(&m.answer, "answer", "", "the survivor's answer: - for stdin (with --acknowledge-row)")
	flags.StringVar(&m.environmentFile, "environment-file", "", "read the PostgreSQL connection string from this "+
		"systemd environment file instead of the process environment")
	flags.BoolVar(&m.reserve, "reserve", false, "reserve this deployment's retirement for this host, before any report is collected")
	flags.BoolVar(&m.abandon, "abandon-reservation", false, "release a reservation nothing has started")
	flags.BoolVar(&m.completeRow, "complete-row", false, "on the survivor: complete the retiring host's ledger row from its completion document")
	flags.BoolVar(&m.acknowledge, "acknowledge-row", false, "on the retiring host: acknowledge the survivor's completion in the journal")
	flags.BoolVar(&m.dryRun, "dry-run", false, "classify the host and report; take nothing, write nothing")
	flags.StringVar(&m.input, "input", "", "the request: - for the input document on stdin")
	flags.BoolVar(&m.serverOnly, "server-only", false, "with --input: this host keeps no node, and the request carries no rendering")
	flags.BoolVar(&m.survivorFlagged, "survivor-flagged", false, "with --input: the inventory flags the survivor for retirement too")
	flags.Func("shared-address", "with --input: a virtual, anycast or translated address the deployment shares (repeatable)",
		func(s string) error {
			m.sharedAddresses = append(m.sharedAddresses, s)

			return nil
		})
	flags.BoolVar(&m.failoverVerified, "endpoint-failover-verified", false, "with --input: the operator asserts every node "+
		"endpoint survives this controller's removal by a failover outside billet")
	flags.BoolVar(&m.reservationFresh, "reservation-fresh", false, "with --input: --reserve answered reserved, not adopted, so a "+
		"refusal releases the row")
	flags.DurationVar(&m.reportMaxAge, "report-max-age", 10*time.Minute, "with --input: how old the collection round may be")
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("server retire answers as JSON; pass --json")
	}

	if r := checkRetireCombination(m); r != nil {
		return answerRetireRefusal(drainedBefore(m, r))
	}

	if hostOS == "darwin" {
		return answerRetireRefusal(drainedBefore(m, retireRefuse(retireReasonPlatform,
			"a controller's retirement needs systemd and the global authority exclusion, and this platform has neither", "")))
	}

	var (
		answer any
		r      *retireRefusal
	)

	switch {
	case m.input != "":
		answer, r = retireRequest(ctx, m)
	case m.dryRun:
		answer, r = retireDryRun(ctx, m)
	case m.reserve:
		answer, r = retireReserve(ctx, m)
	case m.abandon:
		answer, r = retireAbandon(ctx, m)
	case m.completeRow:
		answer, r = retireCompleteRow(ctx, m)
	case m.acknowledge:
		answer, r = retireAcknowledge(ctx, m)
	}

	if r != nil {
		return answerRetireRefusal(r)
	}

	return answerJSON(answer, 0, "")
}

// checkRetireCombination is the flag table, refused before anything is opened:
// exactly one mode, the operands the mode takes and no others, every host name
// a holder by the guard's grammar.
func checkRetireCombination(m retireMode) *retireRefusal {
	modes := 0

	// A dry run over an input judges the request without writing, so the two
	// are one mode there.
	for _, on := range []bool{m.reserve, m.abandon, m.completeRow, m.acknowledge, m.dryRun && m.input == "", m.input != ""} {
		if on {
			modes++
		}
	}

	refuse := func(why string) *retireRefusal {
		return retireRefuse(retireReasonCombination, why, "")
	}

	switch {
	case modes != 1:
		return refuse("exactly one of --reserve, --input -, --abandon-reservation, --complete-row, --acknowledge-row and --dry-run")
	case m.input != "" && m.input != "-":
		return refuse("--input takes - alone: the request document arrives on stdin")
	case m.input != "" && m.survivorHost == "":
		return refuse("--input needs --survivor-host, the designated survivor's inventory name")
	case m.input == "" && (m.serverOnly || m.survivorFlagged || len(m.sharedAddresses) != 0 || m.failoverVerified || m.reservationFresh):
		return refuse("--server-only, --survivor-flagged, --shared-address, --endpoint-failover-verified and --reservation-fresh belong to --input")
	case m.input != "" && m.survivorHost == m.retiringHost:
		return refuse("--survivor-host names the retiring host; a controller cannot survive its own retirement")
	case m.reportMaxAge <= 0:
		return refuse("--report-max-age must be positive")
	case m.configPath == "":
		return refuse("--config names the installed configuration and it is empty")
	case m.retiringHost == "" && !m.completeRow:
		return refuse("--retiring-host names this host's inventory name and it is empty")
	case m.run == "" && !m.dryRun:
		return refuse("--run names the holder of this converge's guard and it is empty")
	}

	for _, operand := range []struct{ flag, value string }{
		{"--run", m.run}, {"--retiring-host", m.retiringHost}, {"--survivor-host", m.survivorHost}, {"--as-host", m.asHost},
	} {
		if operand.value == "" {
			continue
		}

		if err := checkHolder(operand.value); err != nil {
			return refuse(operand.flag + ": " + err.Error())
		}
	}

	switch {
	case m.reserve && m.survivorHost == "":
		return refuse("--reserve needs --survivor-host")
	case m.reserve && m.survivorHost == m.retiringHost:
		return refuse("--survivor-host names the retiring host; a controller cannot survive its own retirement")
	case !m.reserve && m.input == "" && (m.survivorHost != "" || m.installedSHA != ""):
		return refuse("--survivor-host and --installed-sha256 belong to --reserve and --input")
	case m.installedSHA != "" && !sha256Hex.MatchString(m.installedSHA):
		return refuse("--installed-sha256 is not a sha256")
	case m.completeRow && (m.asHost == "" || m.completion != "-"):
		return refuse("--complete-row needs --as-host and --completion -")
	case !m.completeRow && (m.asHost != "" || m.completion != ""):
		return refuse("--as-host and --completion belong to --complete-row")
	case m.acknowledge && m.answer != "-":
		return refuse("--acknowledge-row needs --answer -")
	case !m.acknowledge && m.answer != "":
		return refuse("--answer belongs to --acknowledge-row")
	}

	return nil
}

// drainedBefore consumes stdin before an answer made before any mode read it:
// the collector writes stdin before it reads stdout, so a refusal that left a
// large document unread would meet a writer on a closed pipe and be reported
// as a broken pipe with no answer at all. A mode that takes nothing on stdin
// drains nothing.
func drainedBefore(m retireMode, r *retireRefusal) *retireRefusal {
	if m.input != "-" && m.completion != "-" && m.answer != "-" {
		return r
	}

	if _, err := io.Copy(io.Discard, io.LimitReader(retireStdin, maxRetireInputBytes+1)); err != nil {
		r.Why += "; and draining stdin: " + err.Error()
	}

	return r
}

// readRetireDocument reads one JSON document from stdin, whole, bounded, before
// anything is examined. A document past the bound is DRAINED to EOF and then
// refused: the collector writes stdin before it reads stdout, and a writer
// that meets a closed pipe reports a failure with empty output, which would
// hide the typed refusal.
func readRetireDocument(limit int64) ([]byte, *retireRefusal) {
	body, err := io.ReadAll(io.LimitReader(retireStdin, limit+1))
	if err != nil {
		return nil, retireUnknown(retireReasonInput, "read the document on stdin: "+err.Error(), "")
	}

	if int64(len(body)) > limit {
		why := fmt.Sprintf("the document on stdin is longer than %d bytes", limit)

		if _, err := io.Copy(io.Discard, retireStdin); err != nil {
			why += "; and draining the rest of stdin: " + err.Error()
		}

		return nil, retireRefuse(retireReasonInput, why, "")
	}

	return body, nil
}

// retireGuard takes the transaction lock and requires this converge's guard:
// a guard directory whose record reads, names --run, carries no transaction
// pointer, is past its acquirer's cleanup window and has no interrupted
// rewrite beside it. The lock and the guard directory are the caller's to
// release.
func retireGuard(run string) (*txLock, *os.File, claimShape, *retireRefusal) {
	root, err := takeTxLock()
	if err != nil {
		return nil, nil, claimShape{}, retireRefuse(retireReasonLock, err.Error(), "")
	}

	dir, shape, err := openGuardForMutation(root)
	if err != nil {
		root.release()

		return nil, nil, claimShape{}, retireUnknown(retireReasonGuard, err.Error(), "")
	}

	var r *retireRefusal

	switch {
	case shape.Kind != claimGuard:
		r = retireRefuse(retireReasonGuard, fmt.Sprintf("this host is not held by a converge guard (%s); a retirement runs "+
			"under this converge's guard", shape), "")
	case shape.RecordErr != "":
		r = retireUnknown(retireReasonGuard, "the guard's record cannot be read: "+shape.RecordErr, "")
	case shape.Guard.Holder != run:
		r = retireRefuse(retireReasonGuard, fmt.Sprintf("the guard names %s, not %s", shape.Guard.Holder, run), "")
	case shape.Pointer:
		r = retireRefuse(retireReasonGuard, "the guard carries a binary transaction's pointer; a retirement never "+
			"runs inside a binary transaction", "")
	case shape.Guard.Preparing:
		// The acquirer's cleanup window is still open: a cleanup release could
		// take the guard away under the retirement's first mutation.
		r = retireRefuse(retireReasonGuard, "the guard is still preparing; settle it (`converge-guard settle`) before "+
			"a retirement mutates anything under it", "")
	case shape.StrayTemporary:
		r = retireUnknown(retireReasonGuard, "the guard carries an interrupted rewrite (guard.json.tmp); recover it "+
			"before a retirement runs under it", "")
	}

	if r != nil {
		if dir != nil {
			_ = dir.Close()
		}

		root.release()

		return nil, nil, claimShape{}, r
	}

	return root, dir, shape, nil
}

// observeRetireConfig reads the installed configuration ONCE, under the
// transaction lock and the guard the caller holds: the bytes, their digest
// and the parsed document from one read, the loader's whole judgement, and a
// server section required, since every mode of this file reaches the ledger
// through it.
func observeRetireConfig(path string) (*installedConfigObservation, *retireRefusal) {
	obs, r := observeInstalledConfig(path, true)
	if r != nil {
		return nil, retireFromEndpoint(r)
	}

	if !obs.present {
		return nil, retireRefuse(retireReasonConfig, "no configuration is installed at "+obs.path, "")
	}

	if obs.cfg.Server == nil {
		return nil, retireRefuse(retireReasonConfig, obs.path+" has no server section", "")
	}

	return obs, nil
}

// retireIdentity is the deployment identity, peeked and never minted, read
// under the identity exclusion the caller holds.
func retireIdentity(dir string) (string, *retireRefusal) {
	identity, ok, err := state.PeekDeploymentID(dir)

	switch {
	case err != nil:
		return "", retireUnknown(retireReasonIdentity, "read the deployment identity: "+err.Error(), "")
	case !ok:
		return "", retireRefuse(retireReasonIdentity, "no deployment identity is minted in "+dir, "")
	}

	return identity, nil
}

// withIdentityAccess runs fn holding the identity exclusion for dir, taken
// after the transaction lock and released after fn (the ledger factory
// borrows it from the registry); a release that fails is the answer, because
// the hand-back it performs is part of what the command promised.
func withIdentityAccess(ctx context.Context, dir string, fn func() (any, *retireRefusal)) (any, *retireRefusal) {
	acc, err := openIdentityAccess(ctx, dir, identityIntent{wait: identityAccessWait})
	if err != nil {
		return nil, retireUnknown(retireReasonIdentity, err.Error(), "")
	}

	answer, r := fn()

	if err := acc.Release(); err != nil {
		if r != nil {
			r.Why += "; and releasing the identity exclusion: " + err.Error()

			return nil, r
		}

		return nil, retireUnknown(retireReasonIdentity, "release the identity exclusion: "+err.Error(), "")
	}

	return answer, r
}

// retireOpenLedger opens this host's ledger for a write; the identity
// exclusion the caller holds is what the factory borrows.
func retireOpenLedger(ctx context.Context, cfg *config.Config, environmentFile string) (*state.DB, *retireRefusal) {
	dsn, err := ledgerDSNFrom(cfg, environmentFile)
	if err != nil {
		return nil, retireUnknown(retireReasonLedger, err.Error(), "")
	}

	db, err := openStateAdminWith(ctx, cfg, dsn)
	if err != nil {
		return nil, retireUnknown(retireReasonLedger, "open the ledger: "+err.Error(), "")
	}

	return db, nil
}

// requireMarkerAssociation holds the guard's marker to the row: no marker is
// nothing to hold; a marker beside no row is a state no step produces; a
// marker naming another transition than the row's is two records that
// disagree. Both are could-not-tell, the marker kept.
func requireMarkerAssociation(marker *guardTransition, row state.Retirement, present bool) *retireRefusal {
	switch {
	case marker == nil:
		return nil
	case !present:
		return retireUnknown(retireReasonMarker, fmt.Sprintf("the guard carries the retirement marker %s beside no "+
			"ledger row, which no step of the protocol produces; the marker is kept", marker.ID),
			"the runbook in docs/operating/upgrades.md")
	case marker.ID != row.TransitionID:
		return retireUnknown(retireReasonMarker, fmt.Sprintf("the guard's marker names transition %s and the row "+
			"transition %s; the marker is kept", marker.ID, row.TransitionID), "")
	}

	return nil
}

// retireReserve is `--reserve`: the guard, the configuration observed under
// it, the local eligibility that needs no report, then under the identity
// exclusion the marker held to any row and the row inside one transaction
// the ledger's single writer decides.
func retireReserve(ctx context.Context, m retireMode) (any, *retireRefusal) {
	root, dir, shape, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

	obs, r := observeRetireConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	cfg := obs.cfg

	if m.installedSHA != "" && obs.sha256 != m.installedSHA {
		return nil, retireRefuse(retireReasonConfig, "the installed configuration moved since the role read it "+
			"(its digest is not --installed-sha256)", "converge again")
	}

	if cfg.Server.LedgerBackend() != config.StatePostgres {
		return nil, retireRefuse(retireReasonBackend, "a retirement is defined for a PostgreSQL active-passive pair, and this "+
			"deployment's ledger is "+string(cfg.Server.LedgerBackend()), "")
	}

	if cfg.Server.Controllers != config.ControllersActivePassive {
		return nil, retireRefuse(retireReasonControllers, "a retirement is defined for an active-passive pair, and this "+
			"deployment's controllers are "+string(cfg.Server.Controllers), "")
	}

	if r := requireNoJournal("a reservation"); r != nil {
		return nil, r
	}

	return withIdentityAccess(ctx, cfg.Server.IdentityDir, func() (any, *retireRefusal) {
		identity, r := retireIdentity(cfg.Server.IdentityDir)
		if r != nil {
			return nil, r
		}

		db, r := retireOpenLedger(ctx, cfg, m.environmentFile)
		if r != nil {
			return nil, r
		}

		defer func() { _ = db.Close() }()

		// THE MARKER IS HELD TO THE ROW BEFORE ANYTHING IS INSERTED: a guard
		// already marked is a retirement in some state, and a fresh row
		// beside it, or an adoption of a row the marker does not name, would
		// change the ledger under a record that disagrees.
		existing, present, err := db.ReadRetirement(ctx, identity)
		if err != nil {
			return nil, retireUnknown(retireReasonLedger, err.Error(), "")
		}

		if r := requireMarkerAssociation(shape.Guard.Transition, existing, present); r != nil {
			return nil, r
		}

		id, err := retireTransitionID()
		if err != nil {
			return nil, retireUnknown(retireReasonLedger, "mint the transition id: "+err.Error(), "")
		}

		row, outcome, err := db.ReserveRetirement(ctx, state.RetirementReservation{
			Deployment: identity, Retiring: m.retiringHost, Survivor: m.survivorHost, Run: m.run,
			TransitionID: id, At: retireNow(),
		})

		switch {
		case errors.Is(err, state.ErrRetirementDone):
			return nil, retireRefuse(retireReasonRetired, retiredSentence(row), "")
		case errors.Is(err, state.ErrRetirementReserved):
			return nil, retireRefuse(retireReasonReserved, fmt.Sprintf("this deployment's retirement is reserved by %s "+
				"(run %s, state %s, since %s); one controller retires at a time", row.Retiring, row.Run, row.State,
				row.ReservedAt), "")
		case err != nil:
			return nil, retireUnknown(retireReasonLedger, err.Error(), "")
		}

		answerOutcome := retireOutcomeReserved
		if outcome == state.ReservationAdopted {
			answerOutcome = retireOutcomeAdopted
		}

		return &retireReservationAnswer{
			Schema: retireSchema, Outcome: answerOutcome, Deployment: row.Deployment, Retiring: row.Retiring,
			Survivor: row.Survivor, Run: row.Run, TransitionID: row.TransitionID, ReservedAt: row.ReservedAt,
			State: stateNothingRetire,
		}, nil
	})
}

// retiredSentence is the one sentence a `done` row refuses with.
func retiredSentence(row state.Retirement) string {
	return fmt.Sprintf("this deployment retired %s on %s; no survivor remains for a second retirement",
		row.Retiring, row.CompletedAt)
}

// requireNoJournal refuses when a retirement journal exists (or cannot be
// judged): the mode asking is one that runs before a retirement has begun.
func requireNoJournal(what string) *retireRefusal {
	j, presence, err := retirement.ReadJournal()

	switch presence {
	case retirement.JournalAbsent:
		return nil
	case retirement.JournalPresent:
		return retireRefuse(retireReasonJournal, fmt.Sprintf("%s is not the way on: a retirement journal exists at %s "+
			"(transition %s)", what, j.Phase, j.Provenance.TransitionID), "")
	default:
		return retireUnknown(retireReasonJournal, "the retirement journal could not be judged: "+err.Error(), "")
	}
}

// retireAbandon is `--abandon-reservation`: the association validated in full,
// then the marker cleared, then the row deleted, then `abandoned`. The two
// remainders are the marker cleared beside a still-reserved row (a retry
// validates the row alone, FLUSHES the guard again so the clearing is
// durable before the row goes, and deletes it) and a marker beside no row,
// which this order never produces and which refuses with the marker kept.
func retireAbandon(ctx context.Context, m retireMode) (any, *retireRefusal) {
	root, dir, shape, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

	obs, r := observeRetireConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	cfg := obs.cfg

	if r := requireNoJournal("an abandonment"); r != nil {
		return nil, r
	}

	if _, presence, err := retirement.ReadStatus(); presence != retirement.StatusAbsent {
		if presence == retirement.StatusPresent {
			return nil, retireRefuse(retireReasonStatus, "an authority status is published, so this retirement began; "+
				"an abandonment releases only a reservation nothing has started", "")
		}

		return nil, retireUnknown(retireReasonStatus, "the authority status could not be judged: "+err.Error(), "")
	}

	if _, err := os.Lstat(retirement.StagePath()); err == nil {
		return nil, retireRefuse(retireReasonStage, fmt.Sprintf("a staged configuration exists at %s beside no journal; "+
			"move it to an audit location by hand before abandoning the reservation", retirement.StagePath()),
			"the audit-move runbook in docs/operating/upgrades.md")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, retireUnknown(retireReasonStage, "examine the stage: "+err.Error(), "")
	}

	return withIdentityAccess(ctx, cfg.Server.IdentityDir, func() (any, *retireRefusal) {
		identity, r := retireIdentity(cfg.Server.IdentityDir)
		if r != nil {
			return nil, r
		}

		db, r := retireOpenLedger(ctx, cfg, m.environmentFile)
		if r != nil {
			return nil, r
		}

		defer func() { _ = db.Close() }()

		row, present, err := db.ReadRetirement(ctx, identity)
		if err != nil {
			return nil, retireUnknown(retireReasonLedger, err.Error(), "")
		}

		marker := shape.Guard.Transition

		if r := requireMarkerAssociation(marker, row, present); r != nil {
			return nil, r
		}

		switch {
		case !present:
			return nil, retireRefuse(retireReasonReservation, "this deployment holds no retirement reservation to abandon", "")
		case row.Retiring != m.retiringHost:
			return nil, retireRefuse(retireReasonReserved, fmt.Sprintf("the reservation is %s's (run %s), not this host's", row.Retiring, row.Run), "")
		case row.State != state.RetirementReserved:
			return nil, retireRefuse(retireReasonReservation, fmt.Sprintf("the retirement row is at %s; only a reservation "+
				"nothing has started is abandoned", row.State), "")
		case row.Run != m.run:
			return nil, retireRefuse(retireReasonReserved, fmt.Sprintf("the reservation is run %s's, and this converge is %s; "+
				"the same host adopts it by reserving again", row.Run, m.run), "")
		}

		markerWord := "absent"

		if marker != nil {
			record := shape.Guard
			record.Transition = nil

			if err := rewriteGuardRecord(root, dir, record); err != nil {
				return nil, retireUnknown(retireReasonMarker, "clear the guard's marker: "+err.Error(), "")
			}

			markerWord = "cleared"
		} else {
			// THE CLEARING'S DURABILITY IS OWED BY THE RETRY: an earlier run
			// may have renamed the marker-free record and died before its
			// flushes, and a row deleted over an unflushed clearing is the
			// marker-beside-no-row remainder a power loss would restore.
			if err := syncDirFD(dir); err != nil {
				return nil, retireUnknown(retireReasonMarker, err.Error(), "")
			}

			if err := syncDirFD(root.dir); err != nil {
				return nil, retireUnknown(retireReasonMarker, err.Error(), "")
			}
		}

		if err := db.ReleaseRetirement(ctx, identity, m.retiringHost, m.run); err != nil {
			return nil, retireUnknown(retireReasonLedger, "release the reservation: "+err.Error(), "")
		}

		return &retireAbandonedAnswer{Schema: retireSchema, Outcome: retireOutcomeAbandoned, TransitionID: row.TransitionID,
			Marker: markerWord, State: stateNothingRetire}, nil
	})
}

// retireCompleteRow is the survivor's helper: it proves its host from
// --as-host (equal to the document's survivor, which the decoder has already
// held unequal to its retiring host), its identity from its own directory,
// and completes the row through its own ledger. The collector that carried
// the document is the trust boundary.
func retireCompleteRow(ctx context.Context, m retireMode) (any, *retireRefusal) {
	raw, r := readRetireDocument(retirement.MaxDocumentBytes)
	if r != nil {
		return nil, r
	}

	completion, err := retirement.DecodeCompletion(raw)
	if err != nil {
		return nil, retireRefuse(retireReasonInput, err.Error(), "")
	}

	if m.asHost != completion.Survivor {
		return nil, retireRefuse(retireReasonSelf, fmt.Sprintf("this host is %s and the completion names %s as the "+
			"survivor; only the survivor completes the row", m.asHost, completion.Survivor), "")
	}

	root, dir, _, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

	obs, r := observeRetireConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	cfg := obs.cfg

	return withIdentityAccess(ctx, cfg.Server.IdentityDir, func() (any, *retireRefusal) {
		identity, r := retireIdentity(cfg.Server.IdentityDir)
		if r != nil {
			return nil, r
		}

		if identity != completion.Deployment {
			return nil, retireRefuse(retireReasonDeployment, fmt.Sprintf("the completion names deployment %s and this host's "+
				"identity is %s", completion.Deployment, identity), "")
		}

		db, r := retireOpenLedger(ctx, cfg, m.environmentFile)
		if r != nil {
			return nil, r
		}

		defer func() { _ = db.Close() }()

		row, outcome, err := db.CompleteRetirement(ctx, state.RetirementCompletion{
			Deployment: completion.Deployment, Retiring: completion.Retiring, Survivor: completion.Survivor,
			TransitionID: completion.TransitionID, ReservedAt: completion.Reservation, CompletedBy: m.asHost,
			At: retireNow(),
		})

		switch {
		case errors.Is(err, state.ErrRetirementDone):
			return nil, retireRefuse(retireReasonRetired, retiredSentence(row), "")
		case errors.Is(err, state.ErrRetirementReserved):
			return nil, retireRefuse(retireReasonConflict, fmt.Sprintf("the ledger's retirement row is %s's (run %s, state %s), "+
				"not %s's; nothing was written", row.Retiring, row.Run, row.State, completion.Retiring), "")
		case errors.Is(err, state.ErrRetirementMismatch):
			return nil, retireRefuse(retireReasonMismatch, err.Error(), "")
		case err != nil:
			return nil, retireUnknown(retireReasonLedger, err.Error(), "")
		}

		return &retirement.Answer{
			Schema: retirement.AnswerSchema, Deployment: row.Deployment, Retiring: row.Retiring, Survivor: row.Survivor,
			TransitionID: row.TransitionID, Reservation: row.ReservedAt, Row: string(outcome),
			CompletedBy: row.CompletedBy, CompletedAt: row.CompletedAt,
		}, nil
	})
}

// retireAcknowledge is the retiring host's acknowledgement of the survivor's
// answer: the local journal must be `done`, THIS GUARD'S (owned by the holder
// or one its chain reached, its provenance whole, the marker naming its
// transition, its identity where the journal archived it), and the answer
// validated field by field against it before `row_done` is written. The
// marker's clearing and `settled` are the tail's, finished by the next
// `server retire` run on this host.
func retireAcknowledge(_ context.Context, m retireMode) (any, *retireRefusal) {
	raw, r := readRetireDocument(retirement.MaxDocumentBytes)
	if r != nil {
		return nil, r
	}

	answer, err := retirement.DecodeAnswer(raw)
	if err != nil {
		return nil, retireRefuse(retireReasonInput, err.Error(), "")
	}

	root, dir, shape, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

	j, presence, err := retirement.ReadJournal()

	switch presence {
	case retirement.JournalPresent:
	case retirement.JournalAbsent:
		return nil, retireRefuse(retireReasonJournal, "no retirement journal exists on this host; an acknowledgement "+
			"follows a journal at done", "")
	default:
		return nil, retireUnknown(retireReasonJournal, "the retirement journal could not be judged: "+err.Error(), "")
	}

	if j.Phase != retirement.PhaseDone {
		return nil, retireRefuse(retireReasonJournal, fmt.Sprintf("the journal is at %s; the row is acknowledged at done", j.Phase), "")
	}

	// THE IDENTITY IS WHERE THE JOURNAL ARCHIVED IT: at done the directory
	// has moved, and a journal that names a deployment the archive does not
	// hold is not this host's retirement.
	identity, ok, err := state.PeekDeploymentID(j.Archive)

	switch {
	case err != nil:
		return nil, retireUnknown(retireReasonIdentity, fmt.Sprintf("read the archived identity at %s: %v", j.Archive, err), "")
	case !ok:
		return nil, retireUnknown(retireReasonIdentity, fmt.Sprintf("the archive %s the journal names holds no deployment "+
			"identity", j.Archive), "the runbook in docs/operating/upgrades.md")
	}

	if err := j.Validate(retirement.JournalExpectation{Retiring: m.retiringHost, Holder: m.run,
		TakenOverFrom: shape.Guard.TakenOverFrom, Identity: identity}); err != nil {
		return nil, retireRefuse(retireReasonJournal, err.Error(), "")
	}

	if marker := shape.Guard.Transition; marker != nil && marker.ID != j.Provenance.TransitionID {
		return nil, retireUnknown(retireReasonMarker, fmt.Sprintf("the guard's marker names transition %s and the journal "+
			"transition %s; the marker is kept", marker.ID, j.Provenance.TransitionID), "")
	}

	if err := answer.MatchesJournal(j); err != nil {
		return nil, retireRefuse(retireReasonMismatch, err.Error(), "")
	}

	// THE COMPLETER IS A PARTICIPANT: the survivor's helper names the
	// survivor, this host's own write names itself, and nobody else completes
	// a row of this retirement.
	if answer.CompletedBy != j.Survivor.Host && answer.CompletedBy != j.Retiring {
		return nil, retireRefuse(retireReasonMismatch, fmt.Sprintf("the answer says %s completed the row, and only %s or "+
			"%s takes part in this retirement", answer.CompletedBy, j.Survivor.Host, j.Retiring), "")
	}

	if j.RowDone {
		if answer.CompletedBy != j.CompletedBy {
			return nil, retireRefuse(retireReasonMismatch, fmt.Sprintf("the row is acknowledged as completed by %s, and "+
				"this answer says %s", j.CompletedBy, answer.CompletedBy), "")
		}

		return &retireAcknowledgedAnswer{Schema: retireSchema, Outcome: retireOutcomeAlready,
			TransitionID: j.Provenance.TransitionID, CompletedBy: j.CompletedBy, State: stateNothingRetire}, nil
	}

	j.RowDone = true
	j.CompletedBy = answer.CompletedBy

	if err := j.Write(retireNow()); err != nil {
		return nil, retireUnknown(retireReasonJournal, "acknowledge the row in the journal: "+err.Error(), "")
	}

	return &retireAcknowledgedAnswer{Schema: retireSchema, Outcome: retireOutcomeAcknowledged,
		TransitionID: j.Provenance.TransitionID, CompletedBy: j.CompletedBy, State: stateNothingRetire}, nil
}

// retireDryRun classifies the host without a lock and writes nothing: the
// journal, the status, the stage, the guard and its marker as they stand, the
// row through the read-only open, and the dispatch the table decides.
func retireDryRun(ctx context.Context, m retireMode) (any, *retireRefusal) {
	report := &retireReport{Schema: retireSchema, Outcome: retireOutcomeReported, State: stateNothingRetire}

	j, presence, err := retirement.ReadJournal()

	switch presence {
	case retirement.JournalPresent:
		report.Journal = &retireReportJournal{Phase: j.Phase, Variant: j.Variant, RowDone: j.RowDone, Settled: j.Settled,
			Owner: j.Ownership.Owner, TransitionID: j.Provenance.TransitionID}
	case retirement.JournalAbsent:
	default:
		return nil, retireUnknown(retireReasonJournal, "the retirement journal could not be judged: "+err.Error(), "")
	}

	st, statusPresence, err := retirement.ReadStatus()

	switch statusPresence {
	case retirement.StatusPresent:
		report.Status = &st
	case retirement.StatusAbsent:
	default:
		return nil, retireUnknown(retireReasonStatus, "the authority status could not be judged: "+err.Error(), "")
	}

	switch _, err := os.Lstat(retirement.StagePath()); {
	case err == nil:
		report.Stage = "present"
	case errors.Is(err, fs.ErrNotExist):
		report.Stage = "absent"
	default:
		return nil, retireUnknown(retireReasonStage, "examine the stage: "+err.Error(), "")
	}

	shape, err := classifyClaim()
	if err != nil {
		return nil, retireUnknown(retireReasonGuard, err.Error(), "")
	}

	report.Guard = string(shape.Kind)

	if shape.Kind == claimGuard && shape.RecordErr == "" {
		report.Marker = shape.Guard.Transition
	}

	cfg, err := config.Load(m.configPath)

	switch {
	case err != nil:
		return nil, retireRefuse(retireReasonConfig, err.Error(), "")
	case cfg.Server == nil:
		// A host whose server section is gone is read through its journal's
		// locator, which the next change adds; until then the row is unread.
		report.RowFact = retirement.RowUnreadable
		report.Why = "the configuration has no server section, so the row was not read"
	default:
		report.Row, report.RowFact, report.Why = readRetireRow(ctx, cfg, m)
	}

	report.Dispatch = retirement.DispatchFor(report.RowFact, retirement.JournalFactOf(presence == retirement.JournalPresent, j.Phase))

	return report, nil
}

// readRetireRow reads the row for the dry run and classifies it for THIS
// host: `unreadable` with its reason on anything but a positive read, never
// absence. An unminted identity cannot be associated with any row, so it is
// unreadable too. A SQLite ledger owned by another account is read AS THAT
// ACCOUNT through `rollout status --json`, because a read-only open of a
// stopped SQLite ledger creates the -wal and -shm sidecars owned by whoever
// opened it, and root-owned sidecars keep the service from reopening it.
func readRetireRow(ctx context.Context, cfg *config.Config, m retireMode) (*retireReportRow, retirement.RowFact, string) {
	identity, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)

	switch {
	case err != nil:
		return nil, retirement.RowUnreadable, "read the deployment identity: " + err.Error()
	case !ok:
		return nil, retirement.RowUnreadable, "no deployment identity is minted in " + cfg.Server.IdentityDir +
			", so no row can be associated with this host"
	}

	dsn, err := ledgerDSNFrom(cfg, m.environmentFile)
	if err != nil {
		return nil, retirement.RowUnreadable, err.Error()
	}

	if cfg.Server.LedgerBackend() != config.StatePostgres {
		// A SQLite read-only open creates the -wal and -shm sidecars owned by
		// whoever opened it; the row is read as the owner or not at all.
		uid, gid, err := statusOwnerOf(cfg.Server.IdentityDir)
		euid := statusEUID()

		switch {
		case err != nil:
			return nil, retirement.RowUnreadable, fmt.Sprintf("read who owns %s: %v", cfg.Server.IdentityDir, err)
		case int(uid) != euid && euid != 0:
			return nil, retirement.RowUnreadable, fmt.Sprintf("%s is owned by uid %d and this process runs as uid %d; a read "+
				"of a SQLite ledger leaves files owned by whoever read it, so the row is read as the owner or as root",
				cfg.Server.IdentityDir, uid, euid)
		case int(uid) != euid:
			return readRetireRowAsOwner(ctx, uid, gid, identity, m)
		}
	}

	db, err := openStateInspect(ctx, cfg, dsn)
	if err != nil {
		return nil, retirement.RowUnreadable, "the ledger could not be opened for the report: " + err.Error()
	}

	defer func() { _ = db.Close() }()

	// THE SAME READ THE OWNER'S REPORT MAKES: the status snapshot, whose row
	// is read only under a binding, so root and the owner answer alike for
	// an unbound ledger (could-not-tell: a row keyed by a deployment the
	// ledger is not bound to cannot be associated with this host).
	snapshot, err := rollout.New(db).StatusSnapshot(ctx)
	if err != nil {
		return nil, retirement.RowUnreadable, "the retirement row could not be read: " + err.Error()
	}

	return rowFromSnapshot(snapshot.Binding, snapshot.Retirement, identity, m.retiringHost)
}

// rowFromSnapshot classifies a status snapshot's row for this host: an
// unbound ledger or one bound elsewhere is unreadable, a bound one with no
// row is absence.
func rowFromSnapshot(binding string, row *state.Retirement, identity, host string) (*retireReportRow, retirement.RowFact, string) {
	switch {
	case binding == "":
		return nil, retirement.RowUnreadable, "the ledger is bound to no deployment, so no row can be associated with this host"
	case binding != identity:
		return nil, retirement.RowUnreadable, fmt.Sprintf("the ledger is bound to deployment %s and this host's identity is %s",
			binding, identity)
	case row == nil:
		return nil, retirement.RowAbsent, ""
	}

	return reportRow(*row), rowFactOf(*row, host), ""
}

// readRetireRowAsOwner reads the row through `billet rollout status --json`
// run as the ledger's owner, whose report carries the deployment's row.
func readRetireRowAsOwner(ctx context.Context, uid, gid uint32, identity string, m retireMode) (*retireReportRow, retirement.RowFact, string) {
	args := []string{"rollout", "status", "--json", "--config", m.configPath}
	if m.environmentFile != "" {
		args = append(args, "--environment-file", m.environmentFile)
	}

	out, code, err := retireReexecCapture(ctx, uid, gid, args)

	switch {
	case err != nil:
		return nil, retirement.RowUnreadable, "read the row as the ledger's owner: " + err.Error()
	case code != 0:
		return nil, retirement.RowUnreadable, fmt.Sprintf("`billet rollout status --json` as the ledger's owner exited %d", code)
	}

	var report rolloutStatusReport
	if err := retirement.DecodeDocument(bytes.TrimSpace(out), &report); err != nil {
		return nil, retirement.RowUnreadable, "the owner's status report does not decode: " + err.Error()
	}

	if report.Schema != rolloutStatusSchema {
		return nil, retirement.RowUnreadable, fmt.Sprintf("the owner's status report is schema %d, not %d", report.Schema,
			rolloutStatusSchema)
	}

	binding := ""
	if report.Deployment.Bound {
		binding = report.Deployment.ID
	}

	var row *state.Retirement

	if rr := report.Retirement; rr != nil {
		row = &state.Retirement{Deployment: identity, Retiring: rr.Retiring, Survivor: rr.Survivor, Run: rr.Run, State: rr.State,
			TransitionID: rr.TransitionID, ReservedAt: rr.ReservedAt}
	}

	return rowFromSnapshot(binding, row, identity, m.retiringHost)
}

func reportRow(row state.Retirement) *retireReportRow {
	return &retireReportRow{Retiring: row.Retiring, Survivor: row.Survivor, Run: row.Run, State: row.State,
		TransitionID: row.TransitionID, ReservedAt: row.ReservedAt}
}

// rowFactOf classifies a row for the dispatch table.
func rowFactOf(row state.Retirement, host string) retirement.RowFact {
	mine := row.Retiring == host

	switch row.State {
	case state.RetirementReserved:
		if mine {
			return retirement.RowReservedMine
		}

		return retirement.RowOtherReserved
	case state.RetirementIntent:
		if mine {
			return retirement.RowIntentMine
		}

		return retirement.RowOtherIntent
	default:
		if mine {
			return retirement.RowDoneMine
		}

		return retirement.RowDoneOther
	}
}

// maxOwnerReportBytes bounds the owner's status report: a fleet's
// registrations are kilobytes.
const maxOwnerReportBytes = 4 << 20

// reexecCapture runs this billet as uid:gid with args, its stdout captured
// through a BOUNDED writer (the excess discarded as it arrives, so a report
// past the bound costs no memory and is refused), its stderr passed through,
// and answers the output and the exit status.
func reexecCapture(ctx context.Context, uid, gid uint32, args []string) ([]byte, int, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, 0, fmt.Errorf("find this billet to read the row as the ledger's owner: %w", err)
	}

	out := &limitedWriter{w: &bytes.Buffer{}, n: maxOwnerReportBytes}

	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, out, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}},
	}

	err = cmd.Run()

	if out.overflow {
		return nil, 0, fmt.Errorf("the owner's report is longer than %d bytes", maxOwnerReportBytes)
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.w.Bytes(), exit.ExitCode(), nil
	}

	if err != nil {
		return nil, 0, fmt.Errorf("run the report as the ledger's owner: %w", err)
	}

	return out.w.Bytes(), 0, nil
}
