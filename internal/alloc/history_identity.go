package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
// job this deployment ran.
func (a *Allocator) RecordJobIdentity(ctx context.Context, leaseID string, job HistoryJob) error {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(job.JobID) == "" {
		return nil
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).RecordJobIdentity(ctx, ledgerdb.RecordJobIdentityParams{
			GithubJobID: job.JobID,
			Repo:        job.Repo(),
			WorkflowRef: job.WorkflowRef,
			JobName:     job.Name,
			Event:       job.Event,
			LeaseID:     leaseID,
		}); err != nil {
			return fmt.Errorf("alloc: record the job lease %s ran: %w", leaseID, err)
		}

		return nil
	})
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
		}
		return nil
	})
	return out, err
}
