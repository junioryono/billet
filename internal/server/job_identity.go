package server

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/provider"
)

// actualJobIdentity contains only job aliases, never a pool launch identity.
type actualJobIdentity struct {
	requests []int64
	job      string
	run      int64
}

type resolvedJob struct {
	protocolID int64
	job        Job
	actual     actualJobIdentity
	cleanup    Job
	binding    *alloc.PoolRunner
}

type jobResolution int

const (
	resolveAcquisition jobResolution = iota
	resolveCompletion
	resolveCommitment
)

type resolvedMessage struct {
	available []resolvedJob
	assigned  []resolvedJob
	completed []resolvedJob
	committed []actualJobIdentity
	poisoned  []error
}

// sameActualJob implements the comparison rule documented at resolveActualJob.
func sameActualJob(a, b actualJobIdentity) bool {
	if a.run != 0 && b.run != 0 && a.run != b.run ||
		a.job != "" && b.job != "" && a.job != b.job {
		return false
	}
	return a.job != "" && a.job == b.job || slices.ContainsFunc(a.requests, func(id int64) bool {
		return slices.Contains(b.requests, id)
	})
}

// mergeActual retains the aliases accepted by resolveActualJob.
func mergeActual(a, b actualJobIdentity) actualJobIdentity {
	a.requests = slices.Clone(a.requests)
	for _, id := range b.requests {
		if !slices.Contains(a.requests, id) {
			a.requests = append(a.requests, id)
		}
	}
	if a.job == "" {
		a.job = b.job
	}
	if a.run == 0 {
		a.run = b.run
	}
	return a
}

// resolveActualJob is the sole actual-job identity rule: wire fields, an actual
// lease binding, and compatible identities established by this batch contribute
// positive request, durable negative direct, JobID and RunID aliases. Two jobs
// match only if a nonzero request alias or nonempty JobID matches and neither
// known JobID nor known RunID contradicts. RunID alone never identifies a job.
// Assignments establish aliases before offers, and both precede completions.
// Offers and assignments may mint direct identities; completions only read them.
// The runner's launch request is returned separately for cleanup and contributes
// no actual-job alias. Resolution runs outside l.mu, before message decisions;
// consumers use sameActualJob and never reconstruct aliases from cleanup IDs.
func (l *Listener) resolveActualJob(ctx context.Context, job Job, mode jobResolution,
	known []actualJobIdentity,
) (resolvedJob, error) {
	out := resolvedJob{job: job, cleanup: job, protocolID: job.RequestID}
	actual := job
	if mode != resolveAcquisition && job.RunnerName != "" && l.alloc != nil {
		binding, err := l.alloc.PoolRunnerByName(ctx, job.RunnerName)
		switch {
		case err == nil:
			if binding.Tier != l.tier || binding.LaunchRequestID == 0 {
				return out, fmt.Errorf("%w: completed runner %q belongs to tier %q",
					ErrUntrustworthySession, job.RunnerName, binding.Tier)
			}
			if mode == resolveCommitment && binding.ActualRequestID != 0 {
				actual = Job{RequestID: binding.ActualRequestID, JobID: binding.JobID, RunID: binding.RunID}
			} else if mode == resolveCommitment && job.RequestID < 0 {
				_, direct, err := l.alloc.DirectJobID(ctx, job.RequestID)
				if err != nil {
					return out, err
				}
				if !direct {
					actual = Job{}
				}
			}
			if mode == resolveCompletion && (job.JobID != "" && binding.JobID != "" && job.JobID != binding.JobID ||
				job.RunID != 0 && binding.RunID != 0 && job.RunID != binding.RunID) {
				return out, fmt.Errorf("%w: completed runner %q contradicts its actual job binding",
					ErrUntrustworthySession, job.RunnerName)
			}
			if actual.RequestID == 0 {
				actual.RequestID = binding.ActualRequestID
			}
			if actual.JobID == "" {
				actual.JobID = binding.JobID
			}
			if actual.RunID == 0 {
				actual.RunID = binding.RunID
			}
			out.cleanup.RequestID = binding.LaunchRequestID
			out.binding = &binding
		case !errors.Is(err, alloc.ErrLeaseNotFound):
			return out, fmt.Errorf("%w: cannot resolve completed runner %q: %w",
				ErrUntrustworthySession, job.RunnerName, err)
		default:
			if leaseID, ok := provider.LeaseOf(job.RunnerName); ok {
				identity, err := l.alloc.JobForLease(ctx, leaseID)
				switch {
				case err == nil:
					if identity.Tier != l.tier || identity.RequestID == 0 {
						return out, fmt.Errorf("%w: completed runner %q resolves outside tier %q",
							ErrUntrustworthySession, job.RunnerName, l.tier)
					}
					out.cleanup.RequestID = identity.RequestID
					if out.cleanup.RunID == 0 {
						out.cleanup.RunID = identity.RunID
					}
				case !errors.Is(err, alloc.ErrLeaseNotFound):
					return out, fmt.Errorf("%w: cannot resolve completed runner %q: %w",
						ErrUntrustworthySession, job.RunnerName, err)
				}
			}
		}
	}
	out.actual = actualJobIdentity{job: actual.JobID, run: actual.RunID}
	if actual.RequestID != 0 {
		out.actual.requests = []int64{actual.RequestID}
	}
	if actual.RequestID < 0 && l.alloc != nil {
		jobID, exists, err := l.alloc.DirectJobID(ctx, actual.RequestID)
		if err != nil {
			return out, err
		}
		if exists {
			if out.actual.job != "" && out.actual.job != jobID {
				return out, fmt.Errorf("%w: request %d contradicts direct job %q",
					ErrUntrustworthySession, actual.RequestID, jobID)
			}
			out.actual.job = jobID
		}
	}
	if out.actual.job != "" && l.alloc != nil {
		id, exists, err := l.alloc.DirectJobIdentity(ctx, out.actual.job)
		if err != nil {
			return out, err
		}
		if !exists && mode == resolveAcquisition && job.RequestID == 0 {
			id, err = l.alloc.IdentifyDirectJob(ctx, out.actual.job)
			if err != nil {
				return out, err
			}
			exists = true
		}
		if exists {
			out.actual = mergeActual(out.actual, actualJobIdentity{requests: []int64{id}})
		}
	}
	for _, prior := range known {
		if sameActualJob(out.actual, prior) {
			out.actual = mergeActual(out.actual, prior)
		}
	}
	if out.binding != nil && out.binding.ActualRequestID != 0 {
		bound := actualJobIdentity{requests: []int64{out.binding.ActualRequestID},
			job: out.binding.JobID, run: out.binding.RunID}
		if !sameActualJob(out.actual, bound) {
			return out, fmt.Errorf("%w: completed runner %q contradicts its actual request",
				ErrUntrustworthySession, job.RunnerName)
		}
		out.actual = mergeActual(out.actual, bound)
	}
	if out.job.RequestID == 0 && len(out.actual.requests) > 0 {
		out.job.RequestID = out.actual.requests[0]
	}
	if out.cleanup.RequestID == 0 {
		out.cleanup.RequestID = out.job.RequestID
	}
	if mode == resolveCompletion {
		if out.cleanup.RequestID == 0 {
			return out, fmt.Errorf("%w: %s completed runner %q without a request id or resolvable job or billet lease identity",
				errQuarantinableCompletion, l.tier, job.RunnerName)
		}
	} else if mode == resolveAcquisition && out.job.RequestID == 0 {
		return out, fmt.Errorf("%w: %s cannot identify directly assigned job %q",
			ErrUntrustworthySession, l.tier, job.JobID)
	}
	return out, nil
}

// resolveMessage applies resolveActualJob once to each entry before any consumer.
func (l *Listener) resolveMessage(ctx context.Context, msg *Message) (resolvedMessage, error) {
	var out resolvedMessage
	var known []actualJobIdentity
	for _, jobs := range []struct {
		wire []Job
		dest *[]resolvedJob
	}{
		{msg.Assigned, &out.assigned}, {msg.Available, &out.available},
	} {
		for _, job := range jobs.wire {
			resolved, err := l.resolveActualJob(ctx, job, resolveAcquisition, known)
			if err != nil {
				return out, err
			}
			*jobs.dest = append(*jobs.dest, resolved)
			known = append(known, resolved.actual)
		}
	}
	for _, job := range msg.Completed {
		job.CompletionID = msg.MessageID
		resolved, err := l.resolveActualJob(ctx, job, resolveCompletion, known)
		if err != nil {
			if errors.Is(err, errQuarantinableCompletion) {
				out.poisoned = append(out.poisoned, err)
				continue
			}
			return out, err
		}
		out.completed = append(out.completed, resolved)
	}

	// Snapshot ownership only; resolve its aliases after releasing the mutex.
	l.mu.Lock()
	var commitments []Job
	for id, p := range l.acquiring {
		commitments = append(commitments, p.job)
		commitments[len(commitments)-1].RequestID = id
	}
	for id, lease := range l.running {
		job := l.runningJobs[id]
		job.RequestID = id
		if job.RunID == 0 {
			job.RunID = lease.RunID
		}
		job.RunnerName = provider.InstanceName(lease.ID)
		commitments = append(commitments, job)
	}
	l.mu.Unlock()
	for _, job := range commitments {
		resolved, err := l.resolveActualJob(ctx, job, resolveCommitment, nil)
		if err != nil {
			return out, err
		}
		out.committed = append(out.committed, resolved.actual)
	}
	return out, nil
}

// containsActual uses resolveActualJob's rule for every acquisition consumer.
func containsActual(jobs []actualJobIdentity, actual actualJobIdentity) bool {
	return slices.ContainsFunc(jobs, func(prior actualJobIdentity) bool {
		return sameActualJob(prior, actual)
	})
}
