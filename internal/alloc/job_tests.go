package alloc

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RecordedTestResults is a job's test suites and cases as the ledger kept them, with
// the job they came from. Codec and Data are THE GUEST'S OWN, UNVERIFIED VIEW, opaque
// to the control plane, for a reader to decode and label as the guest's; nothing
// decides from them. Unreadable says the ledger's row could not be read back, which
// is never no results.
type RecordedTestResults struct {
	LeaseID     string
	RecordedAt  string
	Codec       int
	Data        []byte
	Unreadable  bool
	Tier        string
	Repo        string
	WorkflowRef string
	JobName     string
	HeadSHA     string
	HeadBranch  string
	Result      string
	RunID       int64
	RunAttempt  int64
	// Cursor is where the page after this row begins.
	Cursor TestResultsCursor
}

// TestResultsCursor is a position in the newest-first order of kept test results;
// the zero cursor is the newest.
type TestResultsCursor struct {
	recordedAt string
	leaseID    string
}

// LeaseTestResults reads a lease's test results as the ledger kept them. A lease with
// none is ErrLeaseNotFound, which says nothing was kept and is never an empty result.
func (a *Allocator) LeaseTestResults(ctx context.Context, leaseID string) (RecordedTestResults, error) {
	var out RecordedTestResults

	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJobTestResults(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no test results were recorded for lease %s", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the test results of lease %s: %w", leaseID, err)
		}
		data, err := base64.StdEncoding.DecodeString(row.Data)
		if err != nil {
			return fmt.Errorf("alloc: the test results of lease %s are not base64: %w", leaseID, err)
		}
		out = RecordedTestResults{LeaseID: row.LeaseID, RecordedAt: row.RecordedAt, Codec: int(row.Codec), Data: data}

		return nil
	})

	return out, err
}

// TestResultsSince reads at most limit jobs' test results recorded at or after since,
// newest first, from after; an empty repository is every repository. A short page is
// the last.
func (a *Allocator) TestResultsSince(ctx context.Context, since time.Time, repository string,
	after TestResultsCursor, limit int,
) ([]RecordedTestResults, error) {
	var out []RecordedTestResults

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListJobTestResults(ctx, ledgerdb.ListJobTestResultsParams{
			Since: ts(since), Repo: repository,
			BeforeRecorded: after.recordedAt, BeforeLease: after.leaseID, MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list test results: %w", err)
		}

		out = make([]RecordedTestResults, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			rec := RecordedTestResults{LeaseID: r.LeaseID, RecordedAt: r.RecordedAt, Codec: int(r.Codec),
				Tier: r.Tier, Repo: r.Repo, WorkflowRef: r.WorkflowRef, JobName: r.JobName,
				HeadSHA: r.HeadSha, HeadBranch: r.HeadBranch, Result: r.Result, RunID: r.RunID,
				RunAttempt: r.RunAttempt,
				Cursor:     TestResultsCursor{recordedAt: r.RecordedAt, leaseID: r.LeaseID}}
			if rec.Data, err = base64.StdEncoding.DecodeString(r.Data); err != nil {
				rec.Data, rec.Unreadable = nil, true
			}
			out = append(out, rec)
		}

		return nil
	})

	return out, err
}

// TestResultsPastRetention lists at most limit leases whose test results were recorded
// before before, oldest first. It is a read, so the deletes alone take the writer slot.
func (a *Allocator) TestResultsPastRetention(ctx context.Context, before time.Time, limit int) ([]string, error) {
	var out []string

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListJobTestResultsPastRetention(ctx,
			ledgerdb.ListJobTestResultsPastRetentionParams{
				Before: ts(before), MaxRows: int64(limit),
			})
		if err != nil {
			return fmt.Errorf("alloc: list test results past their retention: %w", err)
		}
		out = rows

		return nil
	})

	return out, err
}

// TestResultsPastBound lists at most limit leases whose test results lie past maxBytes
// of them in all, newest kept, oldest listed first.
func (a *Allocator) TestResultsPastBound(ctx context.Context, maxBytes int64, limit int) ([]string, error) {
	var out []string

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListJobTestResultsPastBound(ctx,
			ledgerdb.ListJobTestResultsPastBoundParams{MaxBytes: maxBytes, MaxRows: int64(limit)})
		if err != nil {
			return fmt.Errorf("alloc: list test results past their bound: %w", err)
		}
		out = rows

		return nil
	})

	return out, err
}

// DeleteTestResults deletes the test results of leaseIDs, in one transaction, and says
// how many it deleted.
func (a *Allocator) DeleteTestResults(ctx context.Context, leaseIDs []string) (int, error) {
	if len(leaseIDs) == 0 {
		return 0, nil
	}

	packed, err := packLeaseIDs(leaseIDs)
	if err != nil {
		return 0, err
	}

	var deleted int64

	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := state.WriteQueries(tx).DeleteJobTestResults(ctx, packed)
		if err != nil {
			return fmt.Errorf("alloc: delete %d jobs' test results: %w", len(leaseIDs), err)
		}
		deleted = n

		return nil
	})

	return int(deleted), err
}
