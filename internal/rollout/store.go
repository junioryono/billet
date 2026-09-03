package rollout

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ErrOpen means a rollout is already running.
//
// A SENTINEL, because the caller's answer is not an error message. `billet
// rollout start` run twice must report the rollout that is running rather than
// creating a second one — which is the issue's requirement that a repeated
// instruction does not duplicate or retarget a decision.
var ErrOpen = errors.New("rollout: a rollout is already running")

// ErrOutstanding means a rollout cannot be completed yet because something is
// still unresolved.
//
// A SENTINEL, BECAUSE IT IS THE ORDINARY ANSWER AND A BROKEN LEDGER IS NOT.
// The coordinator attempts completion on every pass, so "a host has not converged
// yet" is what it hears on every tick but the last and must not be reported as a
// failed pass. Without a way to tell that from a storage error, the coordinator
// swallowed both — and a Finish failing for a real reason was retried forever,
// reported as a successful pass, and visible nowhere.
var ErrOutstanding = errors.New("rollout: this rollout has not converged")

// ErrNoRollout means nothing is running.
var ErrNoRollout = errors.New("rollout: no rollout is running")

// State is whether a rollout is still being worked on.
const (
	StateOpen      = "open"
	StateCompleted = "completed"
	StateAborted   = "aborted"
)

// Policy is how much of the fleet a rollout may disturb at once.
//
// PERSISTED AS JSON RATHER THAN AS COLUMNS, because it is the operator's
// instruction rather than billet's bookkeeping: a field added here should not be
// a migration, and nothing in the scheduler keys on it.
type Policy struct {
	// Cohort is how many nodes may be past `pending` at once. One is the default
	// and the safe answer: a fleet updated one host at a time never loses more
	// capacity than one host's worth.
	Cohort int `json:"cohort"`

	// FailureBudget is how many nodes may end blocked or rolled back before the
	// rollout stops starting new ones.
	//
	// EXPLICIT, because "keep going" and "stop on the first failure" are both
	// wrong as a default. A fleet of fifty should not stop for one bad host; a
	// fleet of two should not lose both.
	FailureBudget int `json:"failure_budget"`

	// AllowDowngrade records that an operator asked for a target older than the
	// release the fleet is running, by name.
	//
	// IN THE POLICY BECAUSE IT TRAVELS WITH THE DECISION. The controller host's
	// updater reads it out of the ledger to lower the release watermark before
	// the older candidate is probed; without it a downgrade is refused at that
	// probe and rolls back, which is the safe answer for a decision nobody made.
	// The automatic starter never sets it.
	AllowDowngrade bool `json:"allow_downgrade,omitempty"`
}

// DefaultPolicy is one host at a time, stopping after one failure.
func DefaultPolicy() Policy { return Policy{Cohort: 1, FailureBudget: 1} }

// Rollout is one durable fleet decision.
type Rollout struct {
	ID string
	// Generation fences instructions. A node that has acted on a newer one
	// refuses an older delivery rather than installing a release the rollout has
	// moved past.
	Generation int64
	// Channel is the channel this resolved from, or empty for an exact pin. Kept
	// so a report can say how the target was chosen; nothing re-resolves it.
	Channel string
	// TargetVersion is what an operator reads. TargetDigest is the identity.
	TargetVersion string
	TargetDigest  string
	// PriorVersion is what the controller was running when this began, so a
	// rollback has somewhere to go without re-deriving it.
	PriorVersion    string
	Policy          Policy
	ControllerPhase Phase
	State           string
	CreatedBy       string
	CreatedAt       string
	FinishedAt      string
	TerminalReason  string
}

// Node is where one host has got to.
type Node struct {
	Node           string
	Phase          Phase
	Attempts       int
	NextAttemptAt  string
	Blocker        string
	PriorRelease   string
	RollbackResult string
	ExemptReason   string
	UpdatedAt      string
	// ConvergedDigest is the release manifest that proved this host converged, or
	// empty for one that converged on its version alone.
	//
	// EMPTY IS A CONVERGED HOST NOTHING PROVED, not a host that failed. Read
	// beside the phase: `committed` with no digest is a host that reached the
	// target version and could not say which bytes it installed, which is every
	// host in the field before one billet-driven upgrade has run.
	ConvergedDigest string
	// DispatchEpoch is the host's registration epoch when it was told to upgrade.
	//
	// A CAUSAL FENCE, and the only one available. After the instruction, a host
	// that has not started yet and one that upgraded, failed and rolled itself
	// back are identical in every other field — both live, both reporting the
	// previous release. A registration bumps the epoch and nothing else does, so
	// a HIGHER one provably postdates the instruction. Zero means nothing was
	// recorded, and nothing is concluded from it.
	DispatchEpoch int64
}

// Store is the durable half of a rollout.
type Store struct {
	db  *state.DB
	now func() time.Time
}

// New builds a store over the control-plane ledger.
func New(db *state.DB, opts ...Option) *Store {
	s := &Store{db: db, now: time.Now}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Option configures a Store.
type Option func(*Store)

// WithClock replaces the clock, so a test can drive backoff without waiting.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// StartRequest is one operator decision to move the fleet to a release.
type StartRequest struct {
	Channel       string
	TargetVersion string
	TargetDigest  string
	PriorVersion  string
	Policy        Policy
	CreatedBy     string
	// Nodes is every host this rollout must converge, as the fleet stands now.
	//
	// SNAPSHOT AT START, and that is deliberate. A host that registers later is
	// running whatever it was installed with and is not part of a decision taken
	// before it existed; a host that has disappeared still holds the rollout open,
	// because its compute may be running. Both are wrong if the set is recomputed
	// on every pass.
	Nodes []string
}

// Start records one fleet decision, or reports the one already running.
//
// IDEMPOTENT ON THE TARGET. `billet rollout start` run twice against the same
// release must find the rollout that is running rather than creating a second
// one — the issue's requirement that a repeated instruction does not duplicate a
// decision. A DIFFERENT target while one is open is refused rather than silently
// retargeting: work is already underway against the first.
func (s *Store) Start(ctx context.Context, req StartRequest) (*Rollout, error) {
	if req.TargetDigest == "" || req.TargetVersion == "" {
		return nil, errors.New("rollout: a rollout needs a target version and its digest")
	}

	if len(req.Nodes) == 0 {
		// NOT A SILENT SUCCESS. A rollout covering no node converges the moment it
		// starts and reports that the fleet is up to date, which is exactly what an
		// operator whose nodes have not registered would be told — and would
		// believe.
		return nil, errors.New("rollout: this deployment has no registered nodes, so a " +
			"rollout would converge without updating anything. Register the fleet first, " +
			"or update the control plane by itself with `billet host-upgrade`")
	}

	if req.Policy.Cohort <= 0 {
		req.Policy.Cohort = DefaultPolicy().Cohort
	}

	var out *Rollout

	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		existing, err := readOpen(ctx, q)
		if err != nil && !errors.Is(err, ErrNoRollout) {
			return err
		}

		if existing != nil {
			// THE SAME DECISION IS THE SAME ROLLOUT. Compared on the DIGEST rather
			// than the version, because the digest is the identity: two releases
			// can carry one tag only if something moved it, and that is precisely
			// the case where continuing the first rollout is right.
			if existing.TargetDigest == req.TargetDigest {
				out = existing

				return nil
			}

			return fmt.Errorf("%w: %s is converging on %s. Abort it or let it finish before "+
				"starting one for %s", ErrOpen, existing.ID, existing.TargetVersion,
				req.TargetVersion)
		}

		policy, err := json.Marshal(req.Policy)
		if err != nil {
			return fmt.Errorf("rollout: render the policy: %w", err)
		}

		highest, err := q.HighestRolloutGeneration(ctx)
		if err != nil {
			return fmt.Errorf("rollout: read the rollout generation: %w", err)
		}

		id, err := newID()
		if err != nil {
			return err
		}

		out = &Rollout{
			ID:              id,
			Generation:      highest + 1,
			Channel:         req.Channel,
			TargetVersion:   req.TargetVersion,
			TargetDigest:    req.TargetDigest,
			PriorVersion:    req.PriorVersion,
			Policy:          req.Policy,
			ControllerPhase: PhasePending,
			State:           StateOpen,
			CreatedBy:       req.CreatedBy,
			CreatedAt:       ts(s.now()),
		}

		if err := q.InsertRollout(ctx, ledgerdb.InsertRolloutParams{
			ID:              out.ID,
			Generation:      out.Generation,
			Channel:         out.Channel,
			TargetVersion:   out.TargetVersion,
			TargetDigest:    out.TargetDigest,
			Policy:          string(policy),
			ControllerPhase: string(out.ControllerPhase),
			PriorVersion:    out.PriorVersion,
			State:           out.State,
			CreatedBy:       out.CreatedBy,
			CreatedAt:       out.CreatedAt,
		}); err != nil {
			return fmt.Errorf("rollout: record the rollout: %w", err)
		}

		for _, node := range req.Nodes {
			if err := q.InsertRolloutNode(ctx, ledgerdb.InsertRolloutNodeParams{
				RolloutID: out.ID,
				Node:      node,
				Phase:     string(PhasePending),
				UpdatedAt: out.CreatedAt,
			}); err != nil {
				return fmt.Errorf("rollout: record node %s: %w", node, err)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Open reads the rollout that is running, if any.
//
// ON THE READ-ONLY POOL, because a control plane asks this on a cadence and a
// question must not reserve the single writer slot to answer itself.
func (s *Store) Open(ctx context.Context) (*Rollout, error) {
	var out *Rollout

	err := s.db.View(ctx, func(q state.Querier) error {
		r, err := readOpen(ctx, state.ReadQueries(q))
		if err != nil {
			return err
		}

		out = r

		return nil
	})

	return out, err
}

// Nodes reads where every host in one rollout has got to, in a stable order.
func (s *Store) Nodes(ctx context.Context, rolloutID string) ([]Node, error) {
	var out []Node

	err := s.db.View(ctx, func(q state.Querier) error {
		out = nil

		rows, err := state.ReadQueries(q).ListRolloutNodes(ctx, rolloutID)
		if err != nil {
			return fmt.Errorf("rollout: list the nodes in %s: %w", rolloutID, err)
		}

		// INDEXED RATHER THAN RANGED BY VALUE: a row is 160 bytes and this list is
		// the whole fleet.
		for i := range rows {
			row := &rows[i]

			out = append(out, Node{
				Node:            row.Node,
				Phase:           Phase(row.Phase),
				Attempts:        int(row.Attempts),
				NextAttemptAt:   row.NextAttemptAt,
				Blocker:         row.Blocker,
				PriorRelease:    row.PriorRelease,
				RollbackResult:  row.RollbackResult,
				ExemptReason:    row.ExemptReason,
				UpdatedAt:       row.UpdatedAt,
				DispatchEpoch:   row.DispatchEpoch,
				ConvergedDigest: row.ConvergedDigest,
			})
		}

		return nil
	})

	return out, err
}

// AdvanceRequest is one component moving to a new phase.
type AdvanceRequest struct {
	RolloutID string
	// Node is the host, or empty for the controller itself.
	Node string
	To   Phase
	// Blocker explains a phase that needs explaining. Required for blocked,
	// because a cordoned host with no reason recorded is one nobody can clear.
	Blocker string
	// RollbackResult records what a rollback proved, or could not.
	RollbackResult string
	// ExemptReason is the operator's decision, required for exempt and
	// decommissioned.
	ExemptReason string
	// PriorRelease is what the component was running before this rollout touched
	// it, recorded once so a rollback has somewhere to go.
	PriorRelease string
	// Backoff, when positive, is how long before this component may be tried
	// again. It also increments the attempt count.
	Backoff time.Duration
	// DispatchEpoch, when positive, records the host's registration epoch at the
	// moment it was told to upgrade. See Node.DispatchEpoch.
	DispatchEpoch int64
	// ConvergedDigest, when non-empty, records the release manifest that proved a
	// host converged. See Node.ConvergedDigest.
	ConvergedDigest string
}

// Advance moves one component through the state machine.
//
// THE CURRENT PHASE IS READ INSIDE THE WRITE TRANSACTION, against the row the
// write acts on. A caller that read the phase, decided, and wrote would be
// deciding on a snapshot — and the thing that lands in between is another process
// blocking the host it is about to install on.
func (s *Store) Advance(ctx context.Context, req AdvanceRequest) error {
	if req.To == PhaseBlocked && req.Blocker == "" {
		// A CORDONED COMPONENT WITH NO REASON IS ONE NOBODY CAN CLEAR. Blocking is
		// billet saying it could not prove something, and the operator's only way
		// forward is knowing what.
		return errors.New("rollout: blocking a component needs a reason; a cordoned host " +
			"with nothing recorded is one nobody can clear")
	}

	if (req.To == PhaseExempt || req.To == PhaseDecommissioned) && req.ExemptReason == "" {
		return fmt.Errorf("rollout: recording a component as %s needs the operator's reason, "+
			"because it is what lets the rollout complete without that component "+
			"converging", req.To)
	}

	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		if req.Node == "" {
			return s.advanceController(ctx, q, req)
		}

		row, err := q.ReadRolloutNodeProgress(ctx, ledgerdb.ReadRolloutNodeProgressParams{
			RolloutID: req.RolloutID,
			Node:      req.Node,
		})

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("rollout: %s is not part of rollout %s", req.Node, req.RolloutID)
		case err != nil:
			return fmt.Errorf("rollout: read %s in %s: %w", req.Node, req.RolloutID, err)
		}

		if err := Transition(Phase(row.Phase), req.To); err != nil {
			return fmt.Errorf("%w (node %s)", err, req.Node)
		}

		attempts, next := row.Attempts, ""
		if req.Backoff > 0 {
			attempts++
			next = ts(s.now().Add(req.Backoff))
		}

		if err := q.AdvanceRolloutNode(ctx, ledgerdb.AdvanceRolloutNodeParams{
			Phase:           string(req.To),
			Attempts:        attempts,
			NextAttemptAt:   next,
			Blocker:         req.Blocker,
			RollbackResult:  req.RollbackResult,
			ExemptReason:    req.ExemptReason,
			PriorRelease:    req.PriorRelease,
			DispatchEpoch:   req.DispatchEpoch,
			ConvergedDigest: req.ConvergedDigest,
			UpdatedAt:       ts(s.now()),
			RolloutID:       req.RolloutID,
			Node:            req.Node,
			ExpectPhase:     row.Phase,
		}); err != nil {
			return fmt.Errorf("rollout: advance %s to %s: %w", req.Node, req.To, err)
		}

		return nil
	})
}

func (s *Store) advanceController(
	ctx context.Context, q state.WriteOps, req AdvanceRequest,
) error {
	phase, err := q.ReadRolloutControllerPhase(ctx, req.RolloutID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrNoRollout, req.RolloutID)
	case err != nil:
		return fmt.Errorf("rollout: read the controller phase of %s: %w", req.RolloutID, err)
	}

	if err := Transition(Phase(phase), req.To); err != nil {
		return fmt.Errorf("%w (controller)", err)
	}

	if err := q.AdvanceRolloutController(ctx, ledgerdb.AdvanceRolloutControllerParams{
		ControllerPhase: string(req.To),
		ID:              req.RolloutID,
		ExpectPhase:     phase,
	}); err != nil {
		return fmt.Errorf("rollout: advance the controller to %s: %w", req.To, err)
	}

	return nil
}

// Finish closes a rollout.
//
// COMPLETION IS NOT "NOTHING LEFT TO DO". A rollout is complete when every
// required component reports the exact target and a healthy contract, or an
// operator has recorded an exemption or a decommission after proving compute
// absence. This refuses to call a rollout complete while any component is still
// live and unconverged, because "most nodes updated" is the failure mode the
// issue names.
func (s *Store) Finish(ctx context.Context, rolloutID, outcome, reason string) error {
	if outcome != StateCompleted && outcome != StateAborted {
		return fmt.Errorf("rollout: %q is not a terminal rollout state", outcome)
	}

	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		if outcome == StateCompleted {
			controller, err := q.ReadRolloutControllerPhase(ctx, rolloutID)
			if err != nil {
				return fmt.Errorf("rollout: read the controller phase of %s: %w", rolloutID, err)
			}

			if !Phase(controller).Converged() {
				return fmt.Errorf("%w: the controller is %s", ErrOutstanding, controller)
			}

			outstanding, err := unresolved(ctx, q, rolloutID)
			if err != nil {
				return err
			}

			if len(outstanding) > 0 {
				return fmt.Errorf("%w: %d node(s) have neither converged nor been "+
					"explicitly exempted or decommissioned (%v); \"most nodes updated\" is "+
					"not a completed rollout", ErrOutstanding, len(outstanding), outstanding)
			}
		}

		if err := q.FinishRollout(ctx, ledgerdb.FinishRolloutParams{
			State:          outcome,
			FinishedAt:     ts(s.now()),
			TerminalReason: reason,
			ID:             rolloutID,
			ExpectState:    StateOpen,
		}); err != nil {
			return fmt.Errorf("rollout: finish %s: %w", rolloutID, err)
		}

		return nil
	})
}

// unresolved lists nodes that are neither converged nor decided about.
func unresolved(ctx context.Context, q state.ReadOps, rolloutID string) ([]string, error) {
	rows, err := q.ListRolloutNodePhases(ctx, rolloutID)
	if err != nil {
		return nil, fmt.Errorf("rollout: list the nodes in %s: %w", rolloutID, err)
	}

	var out []string

	for _, row := range rows {
		if !Phase(row.Phase).Terminal() {
			out = append(out, row.Node+" ("+row.Phase+")")
		}
	}

	return out, nil
}

// NewestForTarget is the newest rollout, in any state, to one manifest digest,
// and whether there is one.
func (s *Store) NewestForTarget(ctx context.Context, digest string) (*Rollout, bool, error) {
	var out *Rollout

	err := s.db.View(ctx, func(q state.Querier) error {
		out = nil

		row, err := state.ReadQueries(q).ReadNewestRolloutForTarget(ctx, digest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("rollout: read the newest rollout to %s: %w", digest, err)
		}

		out, err = rolloutFrom(&row)

		return err
	})

	return out, out != nil, err
}

// History lists finished rollouts, newest first.
func (s *Store) History(ctx context.Context, limit int) ([]Rollout, error) {
	if limit <= 0 {
		limit = 10
	}

	var out []Rollout

	err := s.db.View(ctx, func(q state.Querier) error {
		out = nil

		rows, err := state.ReadQueries(q).ListRolloutHistory(ctx, int64(limit))
		if err != nil {
			return fmt.Errorf("rollout: list rollouts: %w", err)
		}

		for i := range rows {
			r, err := rolloutFrom(&rows[i])
			if err != nil {
				return err
			}

			out = append(out, *r)
		}

		return nil
	})

	return out, err
}

// readOpen reads the one rollout that is running.
//
// ABSENCE IS READ FROM THE ERROR, never from the value: a :one query returns a
// zero-value row alongside sql.ErrNoRows, so a caller testing the struct would
// report a rollout with an empty id as a real one.
func readOpen(ctx context.Context, q state.ReadOps) (*Rollout, error) {
	row, err := q.ReadRolloutInState(ctx, StateOpen)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoRollout
	case err != nil:
		return nil, fmt.Errorf("rollout: read the open rollout: %w", err)
	}

	return rolloutFrom(&row)
}

// rolloutFrom maps one ledger row onto billet's own type.
func rolloutFrom(row *ledgerdb.Rollout) (*Rollout, error) {
	r := Rollout{
		ID:              row.ID,
		Generation:      row.Generation,
		Channel:         row.Channel,
		TargetVersion:   row.TargetVersion,
		TargetDigest:    row.TargetDigest,
		ControllerPhase: Phase(row.ControllerPhase),
		PriorVersion:    row.PriorVersion,
		State:           row.State,
		CreatedBy:       row.CreatedBy,
		CreatedAt:       row.CreatedAt,
		FinishedAt:      row.FinishedAt,
		TerminalReason:  row.TerminalReason,
	}

	if row.Policy != "" {
		if err := json.Unmarshal([]byte(row.Policy), &r.Policy); err != nil {
			return nil, fmt.Errorf("rollout: read the policy of %s: %w", r.ID, err)
		}
	}

	return &r, nil
}

func newID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("rollout: generate an id: %w", err)
	}

	return hex.EncodeToString(raw[:]), nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
