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
	Sequence                                    int64
	LeaseID, Tier, Target, Repository           string
	Provider, InstanceType, Site                string
	VCPU, Memory, PriceMicrosPerHour            int64
	BoughtAt, StartedAt, FinishedAt             string
	Result, ResultAt, Conclusion, FailureReason string
}

// ErrNoUsageRecord means the ledger holds no finished lease at a sequence number.
var ErrNoUsageRecord = errors.New("state: no finished lease has that usage sequence number")

// UsageSince reads up to limit finished leases past a sequence number, in the
// order they finished.
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
				Sequence: r.UsageSeq, LeaseID: r.LeaseID, Tier: r.Tier, Target: r.GithubTarget,
				Repository: r.Repo, Provider: r.ChosenProvider, InstanceType: r.InstanceType,
				Site: r.Site, VCPU: r.Vcpu, Memory: r.Memory, PriceMicrosPerHour: r.PriceMicrosPerHour,
				BoughtAt: r.BoughtAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
				Result: r.Result, ResultAt: r.ResultAt, Conclusion: r.Conclusion,
				FailureReason: r.FailureReason,
			})
		}

		return nil
	})

	return out, err
}

// UsageLease names the lease at a sequence number, which is how a resuming
// exporter proves its cursor still means something on this ledger. ErrNoUsageRecord
// is a definite answer; any other error is one the ledger could not give.
func (db *DB) UsageLease(ctx context.Context, sequence int64) (string, error) {
	var lease string

	err := db.View(ctx, func(tx Querier) error {
		var err error

		lease, err = ReadQueries(tx).ReadUsageLease(ctx, sequence)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoUsageRecord, sequence)
		}
		if err != nil {
			return fmt.Errorf("state: read usage record %d: %w", sequence, err)
		}

		return nil
	})

	return lease, err
}
