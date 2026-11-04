package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// UsageRow is what the ledger holds about one finished lease for a usage export.
// Every empty string and zero is "not recorded", which the reader decides how to
// say; nothing here is defaulted.
type UsageRow struct {
	Sequence                          int64
	Mark                              string
	LeaseID, Tier, Target, Repository string
	Provider, InstanceType, Site      string
	VCPU, Memory, PriceMicrosPerHour  int64
	BoughtAt, StartedAt, FinishedAt   string
	Result, ResultAt, Conclusion      string
	// GitHubRunnerAssignedAt and GitHubFinishedAt are on GitHub's clock. StartedAt and
	// ResultAt are when billet received the messages saying so, which can be far later.
	GitHubRunnerAssignedAt, GitHubFinishedAt string
	// FailureReason and Disruption say whether the lease ended cleanly, which is what
	// lets a missing start mean that no job ran.
	FailureReason, Disruption string
}

// ErrNoUsageRecord means the ledger holds no finished lease at a sequence number.
var ErrNoUsageRecord = errors.New("state: no finished lease has that usage sequence number")

// UsageSince reads up to limit finished leases past a sequence number, in the order
// they were numbered.
func (db *DB) UsageSince(ctx context.Context, after int64, limit int) ([]UsageRow, error) {
	var out []UsageRow

	err := db.View(ctx, func(tx Querier) error {
		rows, err := ReadQueries(tx).ListUsageSince(ctx, ledgerdb.ListUsageSinceParams{
			After: after, MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("state: read usage past %d: %w", after, err)
		}

		out = make([]UsageRow, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			out = append(out, UsageRow{
				Sequence: r.UsageSeq, Mark: r.UsageMark, LeaseID: r.LeaseID, Tier: r.Tier, Target: r.GithubTarget,
				Repository: r.Repo, Provider: r.ChosenProvider, InstanceType: r.InstanceType,
				Site: r.Site, VCPU: r.Vcpu, Memory: r.Memory, PriceMicrosPerHour: r.PriceMicrosPerHour,
				BoughtAt: r.BoughtAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
				Result: r.Result, ResultAt: r.ResultAt, Conclusion: r.Conclusion,
				GitHubRunnerAssignedAt: r.GithubRunnerAssignedAt, GitHubFinishedAt: r.GithubFinishedAt,
				FailureReason: r.FailureReason, Disruption: r.Disruption,
			})
		}

		return nil
	})

	return out, err
}

// UsagePlace is what took one place in the order: the lease, and the mark drawn when it
// was numbered.
type UsagePlace struct {
	LeaseID, Mark string
}

// UsageRecordAt reads what took a sequence number, which is how a resuming exporter
// proves its cursor still means something on this ledger. ErrNoUsageRecord is a
// definite answer; any other error is one the ledger could not give.
func (db *DB) UsageRecordAt(ctx context.Context, sequence int64) (UsagePlace, error) {
	var out UsagePlace

	err := db.View(ctx, func(tx Querier) error {
		row, err := ReadQueries(tx).ReadUsageRecord(ctx, sequence)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoUsageRecord, sequence)
		}
		if err != nil {
			return fmt.Errorf("state: read usage record %d: %w", sequence, err)
		}

		out = UsagePlace{LeaseID: row.LeaseID, Mark: row.UsageMark}

		return nil
	})

	return out, err
}

// UsageStillNumbering reports whether a lease that finished before there were numbers
// is still waiting for one, which a reader caught up to the end would otherwise take
// for the whole history.
func (db *DB) UsageStillNumbering(ctx context.Context) (bool, error) {
	var waiting bool

	err := db.View(ctx, func(tx Querier) error {
		var err error

		waiting, err = ReadQueries(tx).ReadAnyUnnumberedUsage(ctx)
		if err != nil {
			return fmt.Errorf("state: read whether usage is still being numbered: %w", err)
		}

		return nil
	})

	return waiting, err
}
