package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// The evidence an acceptance run produces, and what makes it evidence.
//
// EVERY FIELD IS SOMETHING BILLET OBSERVED, not something this command asserts.
// The jobs come from the ledger's own history, which records GitHub's reported
// result beside billet's own conclusion; the outstanding holds come from
// alloc.Quiescence; and the fleet's clearance comes from alloc.ComputeClear,
// which is the barrier that ASKS EACH HOST what its provider is actually running
// rather than reading the ledger. A report that concluded "clean" from the ledger
// alone would be blind to exactly the class the barrier exists for: compute whose
// lease has already gone.
//
// IT IS WRITTEN BEFORE THE TEARDOWN, because the teardown destroys what it is
// about — and after the services stop, so nothing it reads is moving.

// acceptanceEvidenceDoc is the JSON a run leaves behind.
type acceptanceEvidenceDoc struct {
	Version      int    `json:"version"`
	WrittenAt    string `json:"written_at"`
	DeploymentID string `json:"deployment_id"`
	ConfigPath   string `json:"config_path"`
	Account      string `json:"account,omitempty"`
	CallerARN    string `json:"caller_arn,omitempty"`

	// Tiers are the scale sets this run owned. Recorded so a reader can tell what
	// the run was allowed to touch without re-deriving the config.
	Tiers []string `json:"tiers"`

	// Jobs is every job the ledger recorded, with BOTH verdicts. `github` is what
	// GitHub said and `billet` is what billet's own lifecycle concluded; a run
	// where those disagree is the finding, so neither is dropped.
	Jobs []acceptanceJob `json:"jobs"`

	// Outstanding is what the ledger still holds at the moment this was written.
	// Empty is the expected answer and is not by itself proof of anything — see
	// Clearance.
	Outstanding []acceptanceHold `json:"outstanding"`

	// Sealed says whether admission was closed when this was read.
	Sealed bool `json:"sealed"`

	// Clearance is the COMPUTE BARRIER's answer: what each host said its provider
	// was actually running, against a fence taken before the question. It is the
	// only field here that can see compute whose lease has already gone, which is
	// the class an acceptance run most needs to be sure about.
	Clearance acceptanceClearance `json:"clearance"`
}

type acceptanceJob struct {
	LeaseID string `json:"lease_id"`
	Tier    string `json:"tier"`
	Node    string `json:"node,omitempty"`
	RunID   int64  `json:"run_id,omitempty"`
	// GitHub is GitHub's own reported result for the job.
	GitHub string `json:"github,omitempty"`
	// Billet is what billet's lifecycle concluded about the lease.
	Billet string `json:"billet,omitempty"`
	// FailureReason is billet's own account of a failure, empty otherwise.
	FailureReason string `json:"failure_reason,omitempty"`
	// Disruption is billet's own infrastructure getting in the way — a spot
	// interruption, a host that went. Reported because a failed job during one is
	// a different fact from a failed job.
	Disruption string `json:"disruption,omitempty"`
	QueuedAt   string `json:"queued_at,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type acceptanceHold struct {
	ID    string `json:"id"`
	Tier  string `json:"tier"`
	Node  string `json:"node,omitempty"`
	Phase string `json:"phase"`
	RunID string `json:"run_id,omitempty"`
	Since string `json:"since,omitempty"`
}

type acceptanceClearance struct {
	// Requested says a durable barrier was in force. WITHOUT ONE NOTHING ELSE
	// HERE IS EVIDENCE: no host was asked, so `clear: false` would mean "not
	// asked" rather than "something is running", and reporting the boolean alone
	// would collapse those.
	Requested bool `json:"requested"`
	Clear     bool `json:"clear"`
	// Sealed and AdmissionGeneration are what the barrier was taken under, read in
	// the SAME snapshot. Without them a proof outlives the seal it was taken
	// under: between somebody resuming, taking work and resealing, every host's
	// stored run still reads as clear.
	Sealed              bool  `json:"sealed"`
	AdmissionGeneration int64 `json:"admission_generation"`

	// Nodes is EVERY expected host with the state it is actually in, rather than
	// two buckets. billet distinguishes six — proved, running, settling, has not
	// answered, unreachable, and too old to be asked — and folding them into
	// "blocked" and "unprovable" would let "could not tell" read as one of the two
	// answers it is not. A report about a teardown has no business collapsing that.
	Nodes []acceptanceNodeClearance `json:"nodes,omitempty"`

	// Excluded is every host a person removed from the expected set, with whether
	// that removal was PROVED. An unproven exclusion is billet admitting it does
	// not know what is on a machine, and it must not read as a clean one.
	Excluded []acceptanceExclusion `json:"excluded,omitempty"`

	Explanation string `json:"explanation,omitempty"`
}

type acceptanceNodeClearance struct {
	Node  string `json:"node"`
	State string `json:"state"`
	// EmptySince and ClearAt are the two OBSERVATIONS a proof is made of. They are
	// here because "clear" is a claim about a continuous run spanning a grace
	// rather than about a moment, and a reader checking this report's arithmetic
	// needs both ends of it.
	EmptySince string `json:"empty_since,omitempty"`
	ClearAt    string `json:"clear_at,omitempty"`
}

type acceptanceExclusion struct {
	Node   string `json:"node"`
	Proven bool   `json:"proven"`
	Actor  string `json:"actor,omitempty"`
	At     string `json:"at,omitempty"`
}

const acceptanceEvidenceVersion = 1

func cmdAcceptanceEvidence(ctx context.Context, args []string) error {
	fs := newFlagSet("billet acceptance evidence")
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	out := fs.String("out", "", "where to write it (default: <workspace>/"+acceptanceEvidence+")")

	if err := parse(fs, args); err != nil {
		return err
	}

	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}

	path := *out
	if path == "" {
		path = filepath.Join(filepath.Dir(ws.ConfigPath), acceptanceEvidence)
	}

	return writeAcceptanceEvidence(ctx, ws, path)
}

func writeAcceptanceEvidence(ctx context.Context, ws acceptanceWorkspace, path string) error {
	doc, err := collectAcceptanceEvidence(ctx, ws)
	if err != nil {
		return err
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("render the evidence: %w", err)
	}

	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	fmt.Printf("evidence written to %s (%d job(s), %d outstanding)\n",
		path, len(doc.Jobs), len(doc.Outstanding))

	return nil
}

func collectAcceptanceEvidence(
	ctx context.Context, ws acceptanceWorkspace,
) (acceptanceEvidenceDoc, error) {
	db, cfg, err := openLedgerForAdmission(ctx, ws.ConfigPath)
	if err != nil {
		return acceptanceEvidenceDoc{}, err
	}

	defer db.Close()

	doc := acceptanceEvidenceDoc{
		Version:      acceptanceEvidenceVersion,
		WrittenAt:    time.Now().UTC().Format(time.RFC3339),
		DeploymentID: ws.DeploymentID,
		ConfigPath:   ws.ConfigPath,
		Account:      ws.Account,
		CallerARN:    ws.CallerARN,
		Tiers:        ws.Tiers,
	}

	jobs, err := readJobHistory(ctx, db)
	if err != nil {
		return acceptanceEvidenceDoc{}, err
	}

	doc.Jobs = jobs

	allocator, err := acceptanceAllocator(db, cfg)
	if err != nil {
		return acceptanceEvidenceDoc{}, err
	}

	q, err := allocator.Quiescence(ctx)
	if err != nil {
		return acceptanceEvidenceDoc{}, fmt.Errorf("read what the deployment is holding: %w", err)
	}

	doc.Sealed = q.Sealed

	for _, o := range q.Outstanding {
		doc.Outstanding = append(doc.Outstanding, acceptanceHold{
			ID:    o.ID,
			Tier:  o.Tier,
			Node:  o.Node,
			Phase: string(o.Phase),
			RunID: o.RunID,
			Since: o.Since,
		})
	}

	clearance, err := allocator.ComputeClear(ctx)
	if err != nil {
		// REPORTED, NOT FATAL, and reported as unexplained rather than as clear.
		// The barrier is the strongest thing in this document and the least
		// likely to be available — it needs a durable request the control plane
		// has observed — so a run that could not read it must produce evidence
		// saying so rather than no evidence at all.
		doc.Clearance.Explanation = "the compute barrier could not be read: " + err.Error()

		return doc, nil
	}

	doc.Clearance = acceptanceClearance{
		Requested:           clearance.Requested,
		Clear:               clearance.Clear(),
		Sealed:              clearance.AdmissionSealed,
		AdmissionGeneration: clearance.AdmissionGeneration,
	}

	for _, n := range clearance.Nodes {
		doc.Clearance.Nodes = append(doc.Clearance.Nodes, acceptanceNodeClearance{
			Node:       n.Node,
			State:      n.State.String(),
			EmptySince: n.EmptySince,
			ClearAt:    n.ClearAt,
		})
	}

	for _, e := range clearance.Excluded {
		doc.Clearance.Excluded = append(doc.Clearance.Excluded, acceptanceExclusion{
			Node:   e.Node,
			Proven: e.Proven,
			Actor:  e.Actor,
			At:     e.At,
		})
	}

	if !clearance.Requested {
		doc.Clearance.Explanation = "no compute barrier was in force, so no host was asked; " +
			"`clear` here is the absence of a question rather than an answer"
	}

	return doc, nil
}

// acceptanceAllocator builds the allocator the evidence and the teardown read
// through, from the derived config's own catalogue.
func acceptanceAllocator(db *state.DB, cfg *config.Config) (*alloc.Allocator, error) {
	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		return nil, fmt.Errorf("capacity allocator: %w", err)
	}

	return allocator, nil
}

// readJobHistory reads every job this deployment recorded.
func readJobHistory(ctx context.Context, db *state.DB) ([]acceptanceJob, error) {
	rows, err := state.ReadQueries(db.Reader()).ListJobHistory(ctx, acceptanceJobLimit)
	if err != nil {
		return nil, fmt.Errorf("read this deployment's job history: %w", err)
	}

	jobs := make([]acceptanceJob, 0, len(rows))

	for i := range rows {
		r := &rows[i]

		jobs = append(jobs, acceptanceJob{
			LeaseID:       r.LeaseID,
			Tier:          r.Tier,
			Node:          r.Node,
			RunID:         r.RunID,
			GitHub:        r.Result,
			Billet:        r.Conclusion.String,
			FailureReason: r.FailureReason,
			Disruption:    r.Disruption,
			QueuedAt:      r.QueuedAt,
			StartedAt:     r.StartedAt,
			FinishedAt:    r.FinishedAt,
		})
	}

	return jobs, nil
}

// readAcceptanceJobs is what the wait loop asks.
func readAcceptanceJobs(ctx context.Context, ws acceptanceWorkspace) ([]acceptanceJob, error) {
	db, _, err := openLedgerForAdmission(ctx, ws.ConfigPath)
	if err != nil {
		return nil, err
	}

	defer db.Close()

	return readJobHistory(ctx, db)
}

// newTerminalJobs are the ones that reached an outcome and were not already
// finished when this run started waiting.
//
// THE BASELINE IS WHAT MAKES A RESUMED RUN PROVE ANYTHING. A workspace can be
// re-run, so its ledger may already hold finished jobs — and counting those let a
// second `run --jobs 1` succeed on its first poll with no workflow having reached
// the deployment at all.
func newTerminalJobs(jobs []acceptanceJob, baseline map[string]bool) []acceptanceJob {
	var out []acceptanceJob

	for i := range jobs {
		if jobs[i].Billet != "" && !baseline[jobs[i].LeaseID] {
			out = append(out, jobs[i])
		}
	}

	return out
}
