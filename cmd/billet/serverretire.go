package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// `billet server retire` is a controller's retirement as a command, the role
// its thin caller. This file holds the modes that touch the records alone: the
// reservation the request writes before it collects a single report, its
// abandonment, the survivor's completion of the ledger row and the retiring
// host's acknowledgement of it, and the read-only dry run that classifies the
// host. The request itself (`--input -`) and the transition's phases are the
// next change's.

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
}

func retireRefuse(reason, why, next string) *retireRefusal {
	return &retireRefusal{Schema: retireSchema, Outcome: retireOutcomeRefused, Reason: reason, Why: why, Next: next,
		State: stateNothingRetire}
}

func retireUnknown(reason, why, next string) *retireRefusal {
	return &retireRefusal{Schema: retireSchema, Outcome: retireOutcomeUnknown, Reason: reason, Why: why, Next: next,
		State: stateNothingRetire}
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
}

// retireAbandonedAnswer is `--abandon-reservation`'s answer.
type retireAbandonedAnswer struct {
	Schema       int    `json:"schema"`
	Outcome      string `json:"outcome"`
	TransitionID string `json:"transition_id"`
	// Marker says what happened to the guard's marker: cleared, or absent.
	Marker string `json:"marker"`
}

// retireAcknowledgedAnswer is `--acknowledge-row`'s answer.
type retireAcknowledgedAnswer struct {
	Schema       int    `json:"schema"`
	Outcome      string `json:"outcome"`
	TransitionID string `json:"transition_id"`
	CompletedBy  string `json:"completed_by"`
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
}

// The seams: the clock, stdin, and where the retirement's files live is the
// retirement package's own seam (retirement.Root).
var (
	retireNow                    = time.Now
	retireStdin        io.Reader = os.Stdin
	retireTransitionID           = newGuardID
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
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("server retire answers as JSON; pass --json")
	}

	if r := checkRetireCombination(m); r != nil {
		return answerRetireRefusal(r)
	}

	if hostOS == "darwin" {
		return answerRetireRefusal(retireRefuse(retireReasonPlatform,
			"a controller's retirement needs systemd and the global authority exclusion, and this platform has neither", ""))
	}

	var (
		answer any
		r      *retireRefusal
	)

	switch {
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
// exactly one mode, the operands the mode takes and no others.
func checkRetireCombination(m retireMode) *retireRefusal {
	modes := 0

	for _, on := range []bool{m.reserve, m.abandon, m.completeRow, m.acknowledge, m.dryRun} {
		if on {
			modes++
		}
	}

	refuse := func(why string) *retireRefusal {
		return retireRefuse(retireReasonCombination, why, "")
	}

	switch {
	case modes != 1:
		return refuse("exactly one of --reserve, --abandon-reservation, --complete-row, --acknowledge-row and --dry-run")
	case m.configPath == "":
		return refuse("--config names the installed configuration and it is empty")
	case m.retiringHost == "" && !m.completeRow:
		return refuse("--retiring-host names this host's inventory name and it is empty")
	case m.run == "" && !m.dryRun:
		return refuse("--run names the holder of this converge's guard and it is empty")
	case m.run != "":
		if err := checkHolder(m.run); err != nil {
			return refuse("--run: " + err.Error())
		}
	}

	if m.retiringHost != "" {
		if err := checkHolder(m.retiringHost); err != nil {
			return refuse("--retiring-host: " + err.Error())
		}
	}

	switch {
	case m.reserve && m.survivorHost == "":
		return refuse("--reserve needs --survivor-host")
	case m.reserve && m.survivorHost == m.retiringHost:
		return refuse("--survivor-host names the retiring host; a controller cannot survive its own retirement")
	case !m.reserve && (m.survivorHost != "" || m.installedSHA != ""):
		return refuse("--survivor-host and --installed-sha256 belong to --reserve")
	case m.reserve && m.installedSHA != "" && !sha256Hex.MatchString(m.installedSHA):
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

	if m.asHost != "" {
		if err := checkHolder(m.asHost); err != nil {
			return refuse("--as-host: " + err.Error())
		}
	}

	return nil
}

// readRetireDocument reads one JSON document from stdin, whole, bounded, before
// anything is examined, so a refusal never meets a writer on a closed pipe.
func readRetireDocument(limit int64) ([]byte, *retireRefusal) {
	body, err := io.ReadAll(io.LimitReader(retireStdin, limit+1))
	if err != nil {
		return nil, retireUnknown(retireReasonInput, "read the document on stdin: "+err.Error(), "")
	}

	if int64(len(body)) > limit {
		return nil, retireRefuse(retireReasonInput, fmt.Sprintf("the document on stdin is longer than %d bytes", limit), "")
	}

	return body, nil
}

// retireGuard takes the transaction lock and requires this converge's guard:
// a guard directory whose record reads, names --run and carries no transaction
// pointer. The lock and the guard directory are the caller's to release.
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

// retireLoadServerConfig loads the configuration and requires its server
// section, which every record-touching mode of this file needs (a host whose
// `server:` is gone is one whose journal names the ledger, the next change's).
func retireLoadServerConfig(path string) (*config.Config, *retireRefusal) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, retireRefuse(retireReasonConfig, err.Error(), "")
	}

	if cfg.Server == nil {
		return nil, retireRefuse(retireReasonConfig, path+" has no server section", "")
	}

	return cfg, nil
}

// retireIdentity is the deployment identity, peeked and never minted.
func retireIdentity(cfg *config.Config) (string, *retireRefusal) {
	identity, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)

	switch {
	case err != nil:
		return "", retireUnknown(retireReasonIdentity, "read the deployment identity: "+err.Error(), "")
	case !ok:
		return "", retireRefuse(retireReasonIdentity, "no deployment identity is minted in "+cfg.Server.IdentityDir, "")
	}

	return identity, nil
}

// retireOpenLedger opens this host's ledger for a write, under the identity
// exclusion the factory takes (the transaction lock is already held, so the
// order is transaction, global, inner).
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

// retireReserve is `--reserve`: the local eligibility that needs no report,
// then the row, inside one transaction the ledger's single writer decides.
func retireReserve(ctx context.Context, m retireMode) (any, *retireRefusal) {
	cfg, r := retireLoadServerConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	if cfg.Server.LedgerBackend() != config.StatePostgres {
		return nil, retireRefuse(retireReasonBackend, "a retirement is defined for a PostgreSQL active-passive pair, and this "+
			"deployment's ledger is "+string(cfg.Server.LedgerBackend()), "")
	}

	if cfg.Server.Controllers != config.ControllersActivePassive {
		return nil, retireRefuse(retireReasonControllers, "a retirement is defined for an active-passive pair, and this "+
			"deployment's controllers are "+string(cfg.Server.Controllers), "")
	}

	if m.installedSHA != "" {
		sum, _, err := hashRegular(m.configPath, maxRenderingBytes)
		if err != nil {
			return nil, retireUnknown(retireReasonConfig, "digest the installed configuration: "+err.Error(), "")
		}

		if sum != m.installedSHA {
			return nil, retireRefuse(retireReasonConfig, "the installed configuration moved since the role read it "+
				"(its digest is not --installed-sha256)", "converge again")
		}
	}

	root, dir, _, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

	if r := requireNoJournal("a reservation"); r != nil {
		return nil, r
	}

	identity, r := retireIdentity(cfg)
	if r != nil {
		return nil, r
	}

	db, r := retireOpenLedger(ctx, cfg, m.environmentFile)
	if r != nil {
		return nil, r
	}

	defer func() { _ = db.Close() }()

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
	}, nil
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
// validates the row alone) and a marker beside no row, which this order never
// produces and which refuses with the marker kept.
func retireAbandon(ctx context.Context, m retireMode) (any, *retireRefusal) {
	cfg, r := retireLoadServerConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	root, dir, shape, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

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

	identity, r := retireIdentity(cfg)
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

	switch {
	case !present && marker != nil:
		return nil, retireUnknown(retireReasonMarker, fmt.Sprintf("the guard carries the retirement marker %s beside no "+
			"ledger row, which no step of the protocol produces; the marker is kept", marker.ID),
			"the runbook in docs/operating/upgrades.md")
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
	case marker != nil && marker.ID != row.TransitionID:
		return nil, retireUnknown(retireReasonMarker, fmt.Sprintf("the guard's marker names transition %s and the row "+
			"transition %s; the marker is kept", marker.ID, row.TransitionID), "")
	}

	markerWord := "absent"

	if marker != nil {
		record := shape.Guard
		record.Transition = nil

		if err := rewriteGuardRecord(root, dir, record); err != nil {
			return nil, retireUnknown(retireReasonMarker, "clear the guard's marker: "+err.Error(), "")
		}

		markerWord = "cleared"
	}

	if err := db.ReleaseRetirement(ctx, identity, m.retiringHost, m.run); err != nil {
		return nil, retireUnknown(retireReasonLedger, "release the reservation: "+err.Error(), "")
	}

	return &retireAbandonedAnswer{Schema: retireSchema, Outcome: retireOutcomeAbandoned, TransitionID: row.TransitionID,
		Marker: markerWord}, nil
}

// retireCompleteRow is the survivor's helper: it proves its host from
// --as-host (equal to the document's survivor, unequal to its retiring host),
// its identity from its own directory, and completes the row through its own
// ledger. The collector that carried the document is the trust boundary.
func retireCompleteRow(ctx context.Context, m retireMode) (any, *retireRefusal) {
	raw, r := readRetireDocument(retirement.MaxDocumentBytes)
	if r != nil {
		return nil, r
	}

	completion, err := retirement.DecodeCompletion(raw)
	if err != nil {
		return nil, retireRefuse(retireReasonInput, err.Error(), "")
	}

	// THE DECODER HAS ALREADY REFUSED a document naming one host as both, so
	// --as-host equal to the survivor is --as-host unequal to the retiring host.
	if m.asHost != completion.Survivor {
		return nil, retireRefuse(retireReasonSelf, fmt.Sprintf("this host is %s and the completion names %s as the "+
			"survivor; only the survivor completes the row", m.asHost, completion.Survivor), "")
	}

	cfg, r := retireLoadServerConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	root, dir, _, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

	identity, r := retireIdentity(cfg)
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
}

// retireAcknowledge is the retiring host's acknowledgement of the survivor's
// answer: validated field by field against the local `done` journal before
// `row_done` is written. The marker's clearing and `settled` are the tail's,
// finished by the next `server retire` run on this host.
func retireAcknowledge(_ context.Context, m retireMode) (any, *retireRefusal) {
	raw, r := readRetireDocument(retirement.MaxDocumentBytes)
	if r != nil {
		return nil, r
	}

	answer, err := retirement.DecodeAnswer(raw)
	if err != nil {
		return nil, retireRefuse(retireReasonInput, err.Error(), "")
	}

	root, dir, _, r := retireGuard(m.run)
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

	if j.Retiring != m.retiringHost {
		return nil, retireRefuse(retireReasonJournal, fmt.Sprintf("the journal is %s's, not %s's", j.Retiring, m.retiringHost), "")
	}

	if err := answer.MatchesJournal(j); err != nil {
		return nil, retireRefuse(retireReasonMismatch, err.Error(), "")
	}

	if j.RowDone {
		return &retireAcknowledgedAnswer{Schema: retireSchema, Outcome: retireOutcomeAlready,
			TransitionID: j.Provenance.TransitionID, CompletedBy: j.CompletedBy}, nil
	}

	j.RowDone = true
	j.CompletedBy = answer.CompletedBy

	if err := j.Write(retireNow()); err != nil {
		return nil, retireUnknown(retireReasonJournal, "acknowledge the row in the journal: "+err.Error(), "")
	}

	return &retireAcknowledgedAnswer{Schema: retireSchema, Outcome: retireOutcomeAcknowledged,
		TransitionID: j.Provenance.TransitionID, CompletedBy: j.CompletedBy}, nil
}

// retireDryRun classifies the host without a lock and writes nothing: the
// journal, the status, the stage, the guard and its marker as they stand, the
// row through the read-only open, and the dispatch the table decides.
func retireDryRun(ctx context.Context, m retireMode) (any, *retireRefusal) {
	report := &retireReport{Schema: retireSchema, Outcome: retireOutcomeReported}

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
		dsn, err := ledgerDSNFrom(cfg, m.environmentFile)
		if err != nil {
			return nil, retireUnknown(retireReasonLedger, err.Error(), "")
		}

		identity, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)

		switch {
		case err != nil:
			return nil, retireUnknown(retireReasonIdentity, err.Error(), "")
		case !ok:
			report.RowFact = retirement.RowAbsent
			report.Why = "no deployment identity is minted here, so no row can be this host's"
		default:
			report.Row, report.RowFact, report.Why = readRetireRow(ctx, cfg, dsn, identity, m.retiringHost)
		}
	}

	report.Dispatch = retirement.DispatchFor(report.RowFact, retirement.JournalFactOf(presence == retirement.JournalPresent, j.Phase))

	return report, nil
}

// readRetireRow reads the row through the read-only open and classifies it
// for THIS host; a read that fails is `unreadable` with its reason, never
// absence.
func readRetireRow(ctx context.Context, cfg *config.Config, dsn, identity, host string) (*retireReportRow, retirement.RowFact, string) {
	db, err := openStateInspect(ctx, cfg, dsn)
	if err != nil {
		return nil, retirement.RowUnreadable, "the ledger could not be opened for the report: " + err.Error()
	}

	defer func() { _ = db.Close() }()

	row, present, err := db.ReadRetirement(ctx, identity)

	switch {
	case err != nil:
		return nil, retirement.RowUnreadable, "the retirement row could not be read: " + err.Error()
	case !present:
		return nil, retirement.RowAbsent, ""
	}

	return &retireReportRow{Retiring: row.Retiring, Survivor: row.Survivor, Run: row.Run, State: row.State,
		TransitionID: row.TransitionID, ReservedAt: row.ReservedAt}, rowFactOf(row, host), ""
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
