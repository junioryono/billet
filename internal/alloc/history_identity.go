package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// HistoryJob is what one of GitHub's scale-set messages said about the job a
// lease ran, as the history row keeps it for people to read.
type HistoryJob struct {
	// JobID is GitHub's workflow-job id, not the runner request id.
	JobID       string
	Owner       string
	Repository  string
	WorkflowRef string
	Name        string
	Event       string
	// QueuedAt, ScaleSetAssignedAt and RunnerAssignedAt are GitHub's times for
	// the job, on GitHub's clock; zero is a time the message did not carry.
	QueuedAt           time.Time
	ScaleSetAssignedAt time.Time
	RunnerAssignedAt   time.Time
	// RunID is the workflow run of the job that started, which for a pooled
	// runner may differ from the run its lease was assigned; zero is unknown.
	// Only a JobStarted gives it.
	RunID int64
}

// Repo is the job's repository as owner/name, or whichever half is known.
func (j HistoryJob) Repo() string {
	switch {
	case j.Owner != "" && j.Repository != "":
		return j.Owner + "/" + j.Repository
	case j.Repository != "":
		return j.Repository
	default:
		return j.Owner
	}
}

// RecordJobIdentity fills in which job a lease ran on its history row.
//
// Each column is written once, and only while the row names no job or names
// this one, so a later message can complete the record but never rewrite it.
// A lease with no history row records nothing: the row opens at assignment,
// and a message about a lease the ledger never assigned is not evidence of a
// job this deployment ran. A write that completes the job's queue wait tells it,
// as the start does when it lands second.
func (a *Allocator) RecordJobIdentity(ctx context.Context, leaseID string, job HistoryJob) error {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(job.JobID) == "" {
		return nil
	}

	return a.txTelling(ctx, func(tx *sql.Tx, told *waitsToTell) error {
		before, found, err := readJobWait(ctx, tx, leaseID)
		if err != nil || !found {
			return err
		}

		if err := state.WriteQueries(tx).RecordJobIdentity(ctx, ledgerdb.RecordJobIdentityParams{
			GithubJobID:              job.JobID,
			Repo:                     job.Repo(),
			WorkflowRef:              job.WorkflowRef,
			JobName:                  job.Name,
			Event:                    job.Event,
			GithubQueuedAt:           stamp(job.QueuedAt),
			GithubScaleSetAssignedAt: stamp(job.ScaleSetAssignedAt),
			GithubRunnerAssignedAt:   stamp(job.RunnerAssignedAt),
			GithubRunID:              max(job.RunID, 0),
			LeaseID:                  leaseID,
		}); err != nil {
			return fmt.Errorf("alloc: record the job lease %s ran: %w", leaseID, err)
		}

		return told.ifCompleted(ctx, tx, leaseID, before)
	})
}

// readJobWait reads the two ends of one job's wait inside a write, so the
// decision to report it is the write's own.
func readJobWait(ctx context.Context, tx *sql.Tx, leaseID string) (ledgerdb.ReadJobWaitRow, bool, error) {
	row, err := state.ReadQueries(tx).ReadJobWait(ctx, leaseID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return row, false, nil
	case err != nil:
		return row, false, fmt.Errorf("alloc: read the queue wait of lease %s: %w", leaseID, err)
	}

	return row, true, nil
}

// JobRecord is one job's history row as an operator reads it. Every empty
// string is "not recorded".
type JobRecord struct {
	LeaseID   string
	Tier      string
	Node      string
	RunID     int64
	RequestID int64
	// Job's Owner and Repository are empty: the row keeps them joined, in Repo.
	Job            HistoryJob
	Repo           string
	Conclusion     string
	FailureReason  string
	Result         string
	Disruption     string
	QueuedAt       string
	AssignedAt     string
	StartedAt      string
	FinishedAt     string
	ChosenProvider string
	InstanceType   string
	VCPU           int64
	Memory         int64
	Site           string
	// GitHubQueuedAt, GitHubScaleSetAssignedAt and GitHubRunnerAssignedAt are
	// on GitHub's clock; BoughtAt is when billet bought the lease, on its own.
	GitHubQueuedAt           string
	GitHubScaleSetAssignedAt string
	GitHubRunnerAssignedAt   string
	BoughtAt                 string
	// DetailRetiredAt is when the job's series, destinations and guest report
	// were deleted past their retention, empty while they are kept or for a job
	// that never had them.
	DetailRetiredAt string
	// GitHubRunID is the run of the job that started, which for a pooled runner
	// may differ from RunID; zero is not recorded.
	GitHubRunID int64
	DisruptedAt string
	// Caches is what the node observed each cache do for the job, first
	// observation kept. An empty token is not observed, and a token this binary
	// does not know is a newer binary's, kept verbatim.
	Caches CacheObservation
	// LaunchedAt, RegisteredAt, CustodyAt, TeardownAt and QuarantinedAt are when
	// the lease first entered each part of its life, on billet's clock, and
	// RunnerID is GitHub's id for the runner registered; empty or zero is not
	// recorded (never entered, or a row from before migration 65).
	LaunchedAt    string
	RegisteredAt  string
	RunnerID      int64
	CustodyAt     string
	TeardownAt    string
	QuarantinedAt string
	// LaunchIncarnation is the node process that began the launch, and
	// HolderIncarnation the one that held the compute when the lease ended.
	LaunchIncarnation string
	HolderIncarnation string
}

// Job reads one lease's history row. A lease that was never assigned a job is
// ErrLeaseNotFound.
func (a *Allocator) Job(ctx context.Context, leaseID string) (JobRecord, error) {
	var out JobRecord
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJob(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no job was recorded for lease %s", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the job lease %s ran: %w", leaseID, err)
		}
		out = JobRecord{
			LeaseID: row.LeaseID, Tier: row.Tier, Node: row.Node, RunID: row.RunID,
			RequestID: row.RequestID,
			Job: HistoryJob{JobID: row.GithubJobID, WorkflowRef: row.WorkflowRef,
				Name: row.JobName, Event: row.Event},
			Repo: row.Repo, Conclusion: row.Conclusion, FailureReason: row.FailureReason,
			Result: row.Result, Disruption: row.Disruption, QueuedAt: row.QueuedAt,
			AssignedAt: row.AssignedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt,
			ChosenProvider: row.ChosenProvider, InstanceType: row.InstanceType,
			VCPU: row.Vcpu, Memory: row.Memory, Site: row.Site,
			GitHubQueuedAt: row.GithubQueuedAt, GitHubScaleSetAssignedAt: row.GithubScaleSetAssignedAt,
			GitHubRunnerAssignedAt: row.GithubRunnerAssignedAt, BoughtAt: row.BoughtAt,
			DetailRetiredAt: row.DetailRetiredAt,
			GitHubRunID:     row.GithubRunID, DisruptedAt: row.DisruptedAt,
			Caches: CacheObservation{
				ImageCache: ImageCache(row.ImageCache), CacheGeneration: row.CacheGeneration,
				ActionsCache: ActionsCache(row.ActionsCache),
				BuildCaches: BuildCaches{Sticky: BuildCache(row.StickyCache), Git: BuildCache(row.GitCache),
					Bazel: BuildCache(row.BazelCache), Go: BuildCache(row.GoCache), Npm: BuildCache(row.NpmCache)},
			},
			LaunchedAt: row.LaunchedAt, RegisteredAt: row.RegisteredAt, RunnerID: row.RunnerID,
			CustodyAt: row.CustodyAt, TeardownAt: row.TeardownAt, QuarantinedAt: row.QuarantinedAt,
			LaunchIncarnation: row.LaunchIncarnation, HolderIncarnation: row.HolderIncarnation,
		}
		return nil
	})
	return out, err
}
