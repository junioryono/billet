package server

import (
	"context"
	"time"

	"github.com/junioryono/billet/internal/config"
)

const (
	// retentionEvery is how often the control plane deletes job detail past its
	// retention.
	retentionEvery = 10 * time.Minute
	// A pass deletes at most retentionBatches batches of retentionBatchJobs jobs,
	// each its own transaction, so the writer slot is held for one batch at a
	// time and a backlog (a ledger's first pass after an upgrade) is worked
	// through over several passes rather than in one. A job holds at most 257
	// destination rows, so a batch deletes at most 12,850 of them.
	retentionBatchJobs = 50
	retentionBatches   = 100
)

// retireJobDetail deletes the series, destinations and guest report of every
// job recorded longer ago than the retention, until ctx ends. It decides
// nothing: a pass that fails is logged and the next one tries again.
func (s *Server) retireJobDetail(ctx context.Context) {
	tick := time.NewTicker(retentionEvery)
	defer tick.Stop()

	for {
		s.retireJobDetailOnce(ctx, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// retireJobDetailOnce is one pass at now.
func (s *Server) retireJobDetailOnce(ctx context.Context, now time.Time) {
	retention := s.detailRetention
	if retention <= 0 {
		retention = config.DefaultJobDetailRetention
	}
	before := now.Add(-retention)

	var jobs, series, destinations, guest int
	for range retentionBatches {
		if ctx.Err() != nil {
			return
		}
		batch, err := s.alloc.RetireJobDetail(ctx, before, retentionBatchJobs)
		if err != nil {
			s.log.Warn("could not delete job detail past its retention; trying again", "error", err)
			break
		}
		jobs, series, destinations, guest = jobs+batch.Jobs, series+batch.Series,
			destinations+batch.Destinations, guest+batch.GuestReports
		if batch.Jobs < retentionBatchJobs {
			break
		}
	}
	if jobs > 0 {
		s.log.Info("deleted job detail past its retention", "jobs", jobs, "series", series,
			"destinations", destinations, "guest_reports", guest, "retention", retention)
	}
}
