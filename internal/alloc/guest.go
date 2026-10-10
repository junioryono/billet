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

// RecordGuestReport keeps what the agent inside a lease's guest told the node,
// the guest's own unverified view, beside the host's measurement and never in
// it.
//
// FENCED ON THE EPOCH, in the same transaction, as RecordLeaseUsage is: a
// superseded holder cannot report, and a lease that has ended is refused
// (ErrLeaseNotFound), because the node reports before it destroys the compute.
// The first report is kept and a later one is ignored.
func (a *Allocator) RecordGuestReport(ctx context.Context, leaseID string, epoch int64,
	report GuestReport,
) error {
	if err := report.Validate(); err != nil {
		return err
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}
		if _, err := state.WriteQueries(tx).RecordJobGuestReport(ctx, ledgerdb.RecordJobGuestReportParams{
			LeaseID: lease.ID, Node: lease.Node, RecordedAt: nowStamp(),
			AgentVersion: report.AgentVersion, AgentSchema: int64(report.Schema),
			Codec: int64(report.Codec), Data: base64.StdEncoding.EncodeToString(report.Data),
			Accepted: report.Accepted, Refused: report.Refused, DroppedBytes: report.DroppedBytes,
			Hello: flag(report.Hello), FinalSeen: flag(report.FinalSeen),
			NodeRestarted:   flag(report.NodeRestarted),
			FirstReceivedAt: arrival(report.FirstReceived), LastReceivedAt: arrival(report.LastReceived),
		}); err != nil {
			return fmt.Errorf("alloc: record the guest report of lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// RecordedGuestReport is a lease's guest report as the ledger kept it: the
// guest's own unverified view, with the host it came through and when the
// control plane kept it.
type RecordedGuestReport struct {
	GuestReport
	Node       string
	RecordedAt string
}

// LeaseGuestReport reads what the agent inside a lease's guest told the node.
// A lease with no report is ErrLeaseNotFound, which says nothing was kept and
// is never an empty report; a row the ledger cannot read back is an error of
// its own, never either of those.
func (a *Allocator) LeaseGuestReport(ctx context.Context, leaseID string) (RecordedGuestReport, error) {
	var out RecordedGuestReport
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJobGuestReport(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no guest report was recorded for lease %s", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the guest report of lease %s: %w", leaseID, err)
		}
		data, err := base64.StdEncoding.DecodeString(row.Data)
		if err != nil {
			return fmt.Errorf("alloc: the guest report of lease %s is not base64: %w", leaseID, err)
		}
		first, err := arrivalOf(row.FirstReceivedAt)
		if err != nil {
			return fmt.Errorf("alloc: the guest report of lease %s: %w", leaseID, err)
		}
		last, err := arrivalOf(row.LastReceivedAt)
		if err != nil {
			return fmt.Errorf("alloc: the guest report of lease %s: %w", leaseID, err)
		}
		out = RecordedGuestReport{Node: row.Node, RecordedAt: row.RecordedAt, GuestReport: GuestReport{
			AgentVersion: row.AgentVersion, Schema: int(row.AgentSchema), Codec: int(row.Codec), Data: data,
			Accepted: row.Accepted, Refused: row.Refused, DroppedBytes: row.DroppedBytes,
			Hello: row.Hello == 1, FinalSeen: row.FinalSeen == 1, NodeRestarted: row.NodeRestarted == 1,
			FirstReceived: first, LastReceived: last,
		}}

		return nil
	})

	return out, err
}

// flag is a boolean as a 0-or-1 column holds it.
func flag(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

// arrival is a node-clock arrival time as its column holds it: NULL where no
// batch arrived, which is not a time.
func arrival(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}

	return sql.NullString{String: t.UTC().Format(time.RFC3339Nano), Valid: true}
}

// arrivalOf reads an arrival time back: the zero time where none was kept.
func arrivalOf(s sql.NullString) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("arrival time %q is not RFC 3339: %w", s.String, err)
	}

	return t, nil
}
