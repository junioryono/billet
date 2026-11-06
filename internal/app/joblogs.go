package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/joblog"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
)

// logIndexDirName is the log index's directory under the identity directory: a cache of
// what the nodes hold, kept where the controller's own files are.
const logIndexDirName = "logindex"

// logIndexEvery is how often the index gives up what is past its bounds, and
// logIndexSlots how many chunks it decodes at once.
const (
	logIndexEvery = 10 * time.Minute
	logIndexSlots = 4
)

// LogIndexDir is where this control plane's log index lives.
func LogIndexDir(cfg *config.Config) string {
	return filepath.Join(cfg.Server.IdentityDir, logIndexDirName)
}

// logIndexSink is what the node wire hands a chunk of a job's log to.
type logIndexSink struct {
	index *state.LogIndex
	jobs  *alloc.Allocator
	slots chan struct{}
	now   func() time.Time
}

func newLogIndexSink(index *state.LogIndex, jobs *alloc.Allocator) *logIndexSink {
	return &logIndexSink{index: index, jobs: jobs, slots: make(chan struct{}, logIndexSlots), now: time.Now}
}

// IndexLogChunk indexes one chunk for the node that says it holds it. A LEASE IS ONLY A
// NAME UNTIL THE LEDGER SAYS THE JOB RAN THERE, so a node cannot write into another
// node's job, which would make a search read a line from a node that never held it.
func (s *logIndexSink) IndexLogChunk(ctx context.Context, nodeName, leaseID string, firstLine int64, data []byte) error {
	job, err := s.jobs.Job(ctx, leaseID)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return fmt.Errorf("%w: this ledger recorded no such job", nodeplane.ErrRefused)
	}

	if err != nil {
		return err
	}

	if job.Node != nodeName {
		return fmt.Errorf("%w: this job did not run on this node", nodeplane.ErrRefused)
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return errors.New("the log index is decoding other nodes' chunks")
	}

	chunk, err := joblog.Decode(data)
	if err != nil {
		return fmt.Errorf("%w: %w", nodeplane.ErrRefused, err)
	}

	err = s.index.Add(ctx, state.LogIndexChunk{Lease: leaseID, FirstLine: firstLine, Chunk: &chunk,
		ReceivedAt: s.now()})
	if errors.Is(err, state.ErrLogLineRange) {
		return fmt.Errorf("%w: %w", nodeplane.ErrRefused, err)
	}

	return err
}

// retainLogIndex gives up what is past the index's bounds, until ctx ends. It decides
// nothing: a pass that fails is said and the next tries again.
func retainLogIndex(ctx context.Context, index *state.LogIndex, limits config.ServerLogLimits, log *slog.Logger) {
	tick := time.NewTicker(logIndexEvery)
	defer tick.Stop()

	for {
		removed, err := index.Retain(time.Now().Add(-limits.Retention), int64(limits.MaxBytes))
		switch {
		case err != nil:
			log.Warn("could not remove what is past the log index's bounds; trying again", "error", err)
		case removed > 0:
			log.Info("removed days of the log index past its bounds", "days", removed,
				"retention", limits.Retention, "max_bytes", limits.MaxBytes)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// startJobLogs builds a node's store of its jobs' step logs and offers it to its guests.
func startJobLogs(cfg *config.Config, service *node.CacheService) error {
	limits, err := cfg.Node.Logs.Limits()
	if err != nil {
		return err
	}

	store, err := node.NewJobLogStore(cfg.Node.StateDir, node.JobLogLimits{
		Retention: limits.Retention, MaxBytes: int64(limits.MaxBytes), MaxJobBytes: int64(limits.MaxJobBytes),
	}, slog.Default())
	if err != nil {
		return err
	}

	service.KeepGuestLogs(store)

	return nil
}

// shipJobLogs ships what a node keeps of its jobs' logs until stopped, and waits for the
// pass in flight. A node with no store has nothing to stop.
func shipJobLogs(ctx context.Context, service *node.CacheService, client *nodeclient.Client) func() {
	if service == nil || service.JobLogs() == nil {
		return func() {}
	}

	shipCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		service.JobLogs().Run(shipCtx, client)
	}()

	return func() {
		cancel()
		<-done
	}
}
