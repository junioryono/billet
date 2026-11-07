package server

import (
	"context"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// endedHoldsKept is how long an ended hold's row stays: past any retry of the completion that
// reads it, which an ended row is what sends on to the destroy.
const endedHoldsKept = 7 * 24 * time.Hour

// retireTestResults deletes test results past their retention and past the bound on
// how much of them is kept, on the job detail retention's schedule and batches, until
// ctx ends. It decides nothing: a pass that fails is logged and the next one tries
// again.
func (s *Server) retireTestResults(ctx context.Context) {
	tick := time.NewTicker(retentionEvery)
	defer tick.Stop()

	for {
		s.retireTestResultsOnce(ctx, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// retireTestResultsOnce is one pass at now.
func (s *Server) retireTestResultsOnce(ctx context.Context, now time.Time) {
	retention, bound := s.testRetention, s.testBound
	if retention <= 0 {
		retention = config.DefaultTestResultsRetention
	}
	if bound <= 0 {
		bound = int64(config.DefaultTestResultsMaxBytes)
	}

	expired, err := s.alloc.TestResultsPastRetention(ctx, now.Add(-retention), retentionBatchJobs*retentionBatches)
	if err != nil {
		s.log.Warn("could not list test results past their retention; trying again", "error", err)
	} else if n := s.deleteTestResults(ctx, expired); n > 0 {
		s.log.Info("deleted test results past their retention", "jobs", n, "retention", retention)
	}

	// WHAT A HOLD DID IS IN THE LOG; its row is only what a hold reads while it lasts.
	if _, err := s.alloc.EndLapsedLeaseHolds(ctx); err != nil {
		s.log.Warn("could not end holds past their bound; trying again", "error", err)
	}

	if _, err := s.alloc.ForgetEndedLeaseHolds(ctx, now.Add(-endedHoldsKept)); err != nil {
		s.log.Warn("could not forget ended holds; trying again", "error", err)
	}

	// AFTER THE RETENTION, so a pass does not count what it has just deleted.
	past, err := s.alloc.TestResultsPastBound(ctx, bound, retentionBatchJobs*retentionBatches)
	if err != nil {
		s.log.Warn("could not list test results past their bound; trying again", "error", err)

		return
	}
	if n := s.deleteTestResults(ctx, past); n > 0 {
		s.log.Info("deleted the oldest test results past their bound", "jobs", n, "bytes", bound)
	}
}

// deleteTestResults deletes leaseIDs' test results a batch to a transaction, so the
// writer slot is held for one batch at a time, and says how many it deleted.
func (s *Server) deleteTestResults(ctx context.Context, leaseIDs []string) int {
	deleted := 0

	for start := 0; start < len(leaseIDs); start += retentionBatchJobs {
		if ctx.Err() != nil {
			return deleted
		}
		n, err := s.alloc.DeleteTestResults(ctx, leaseIDs[start:min(start+retentionBatchJobs, len(leaseIDs))])
		if err != nil {
			s.log.Warn("could not delete test results; trying again", "error", err)

			return deleted
		}
		deleted += n
	}

	return deleted
}
