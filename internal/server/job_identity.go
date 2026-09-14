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
	held       []actualJobIdentity
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
	committed []jobCommitment
	poisoned  []error
	held      []actualJobIdentity
}

// jobCommitment keeps scheduler aliases separate from their renewal ownership.
type jobCommitment struct {
	actual  actualJobIdentity
	key     int64
	lease   *alloc.Lease
	promise *promise
}

// heldBy requires l.mu. A reused ownership key cannot revive an old commitment.
func (c jobCommitment) heldBy(l *Listener) bool {
	if c.promise != nil {
		return l.acquiring[c.key] == c.promise
	}
	lease := l.running[c.key]
	return lease != nil && lease.ID == c.lease.ID && lease.Epoch == c.lease.Epoch
}

// currentCommitments drops discharged ownership without re-deriving aliases.
func (l *Listener) currentCommitments(commitments []jobCommitment) []actualJobIdentity {
	l.mu.Lock()
	defer l.mu.Unlock()

	var actual []actualJobIdentity
	for _, c := range commitments {
		if c.heldBy(l) {
			actual = append(actual, c.actual)
		}
	}
	return actual
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

// actualJobCandidates coalesces consistent aliases before using a unique wire
// request to distinguish conflicting jobs. A wildcard shared by contradictory
// candidates must not be assigned to whichever candidate happens to come first.
func actualJobCandidates(actual actualJobIdentity, requestID int64, known []actualJobIdentity) []actualJobIdentity {
	var candidates []actualJobIdentity
	for _, prior := range known {
		if sameActualJob(actual, prior) {
			candidates = append(candidates, prior)
		}
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); {
			a, b := candidates[i], candidates[j]
			consistent := sameActualJob(a, b)
			for _, other := range candidates {
				if sameActualJob(a, other) != sameActualJob(b, other) {
					consistent = false
					break
				}
			}
			if consistent {
				candidates[i] = mergeActual(a, b)
				candidates = slices.Delete(candidates, j, j+1)
			} else {
				j++
			}
		}
	}
	if len(candidates) > 1 && requestID != 0 {
		var named []actualJobIdentity
		for _, candidate := range candidates {
			if slices.Contains(candidate.requests, requestID) {
				named = append(named, candidate)
			}
		}
		if len(named) == 1 && !slices.ContainsFunc(candidates, func(candidate actualJobIdentity) bool {
			return sameActualJob(named[0], candidate) &&
				(named[0].run == 0 && candidate.run != 0 || named[0].job == "" && candidate.job != "")
		}) {
			return named
		}
	}
	return candidates
}

// resolveActualJob is the sole actual-job identity rule: wire fields, an actual
// lease binding, and compatible identities established by this batch contribute
// positive request, durable negative direct, JobID and RunID aliases. Two jobs
// match only if a nonzero request alias or nonempty JobID matches and neither
// known JobID nor known RunID contradicts. RunID alone never identifies a job.
// Assignments establish aliases before offers, and both precede completions.
// Distinct compatible candidates require one unique match to the wire request;
// otherwise resolution refuses the ambiguity instead of depending on order.
// Offers and assignments may mint direct identities; completions only read them.
// The runner's launch request is returned separately for cleanup and contributes
// no actual-job alias. Resolution runs outside l.mu, before message decisions;
// consumers use sameActualJob and never reconstruct aliases from cleanup IDs.
// Measured on v0.10.0, 2026-08-31 through 2026-09-14 (entire controller journal):
// all 860 pooled starts and every logged assignment had request 0, using a direct
// ID from JobID. 114/860 runners (13%) completed another job than their launch;
// no JobID started twice, no runner started two jobs, and every completion named
// its runner. Cross-run JobID ambiguity and request-only assignments were unseen.
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
				return out, fmt.Errorf("%w: runner %q belongs to tier %q",
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
			return out, fmt.Errorf("%w: cannot resolve runner %q: %w",
				ErrUntrustworthySession, job.RunnerName, err)
		default:
			if leaseID, ok := provider.LeaseOf(job.RunnerName); ok {
				identity, err := l.alloc.JobForLease(ctx, leaseID)
				switch {
				case err == nil:
					if identity.Tier != l.tier || identity.RequestID == 0 {
						return out, fmt.Errorf("%w: runner %q resolves outside tier %q",
							ErrUntrustworthySession, job.RunnerName, l.tier)
					}
					out.cleanup.RequestID = identity.RequestID
					if out.cleanup.RunID == 0 {
						out.cleanup.RunID = identity.RunID
					}
				case !errors.Is(err, alloc.ErrLeaseNotFound):
					return out, fmt.Errorf("%w: cannot resolve runner %q: %w",
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
	candidates := actualJobCandidates(out.actual, job.RequestID, known)
	if len(candidates) > 1 {
		cause := ErrUntrustworthySession
		if mode == resolveCompletion {
			// Every candidate stays held until the completion can be settled.
			out.held = candidates
			cause = errQuarantinableCompletion
		}
		return out, fmt.Errorf("%w: %s request %d job %q matches several actual jobs",
			cause, l.tier, job.RequestID, job.JobID)
	}
	if len(candidates) == 1 {
		out.actual = mergeActual(out.actual, candidates[0])
	}
	if out.binding != nil && out.binding.ActualRequestID != 0 {
		bound := actualJobIdentity{requests: []int64{out.binding.ActualRequestID},
			job: out.binding.JobID, run: out.binding.RunID}
		if !sameActualJob(out.actual, bound) {
			return out, fmt.Errorf("%w: runner %q contradicts its actual request",
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
	var err error
	out.committed, err = l.resolveCommitments(ctx)
	if err != nil {
		return out, err
	}
	known := l.currentCommitments(out.committed)
	for _, jobs := range []struct {
		wire []Job
		dest *[]resolvedJob
	}{
		{msg.Assigned, &out.assigned}, {msg.Available, &out.available},
	} {
		for i := range jobs.wire {
			resolved, err := l.resolveActualJob(ctx, jobs.wire[i], resolveAcquisition, known)
			if err != nil {
				return out, err
			}
			*jobs.dest = append(*jobs.dest, resolved)
			known = append(known, resolved.actual)
		}
	}
	for i := range msg.Completed {
		job := msg.Completed[i]
		job.CompletionID = msg.MessageID
		resolved, err := l.resolveActualJob(ctx, job, resolveCompletion, known)
		if err != nil {
			if errors.Is(err, errQuarantinableCompletion) {
				out.poisoned = append(out.poisoned, err)
				out.held = append(out.held, resolved.held...)
				continue
			}
			return out, err
		}
		out.completed = append(out.completed, resolved)
	}

	return out, nil
}

// resolveCommitments snapshots ownership under l.mu and resolves outside it.
func (l *Listener) resolveCommitments(ctx context.Context) ([]jobCommitment, error) {
	l.mu.Lock()
	commitments := make([]jobCommitment, 0, len(l.acquiring)+len(l.running))
	jobs := make([]Job, 0, len(l.acquiring)+len(l.running))
	for id, p := range l.acquiring {
		job := p.job
		job.RequestID = id
		jobs = append(jobs, job)
		commitments = append(commitments, jobCommitment{actual: p.actual, key: id, lease: p.lease, promise: p})
	}
	for id, lease := range l.running {
		actual := l.runningJobs[id]
		job := Job{RequestID: id, JobID: actual.job, RunID: actual.run}
		if job.RunID == 0 {
			job.RunID = lease.RunID
		}
		job.RunnerName = provider.InstanceName(lease.ID)
		jobs = append(jobs, job)
		commitments = append(commitments, jobCommitment{actual: actual, key: id, lease: lease})
	}
	l.mu.Unlock()
	for i := range jobs {
		resolved, err := l.resolveActualJob(ctx, jobs[i], resolveCommitment, nil)
		if err != nil {
			kind := "running"
			if commitments[i].promise != nil {
				kind = "promised"
			}
			return nil, fmt.Errorf("server: cannot resolve listener's %s commitment for request %d: %w",
				kind, commitments[i].key, err)
		}
		// A busy pool binding names the actual job and supersedes launch intent.
		if resolved.binding == nil || resolved.binding.ActualRequestID == 0 {
			if sameActualJob(resolved.actual, commitments[i].actual) {
				resolved.actual = mergeActual(resolved.actual, commitments[i].actual)
			}
		}
		commitments[i].actual = resolved.actual
	}
	return commitments, nil
}

// containsActual uses resolveActualJob's rule for every acquisition consumer.
func containsActual(jobs []actualJobIdentity, actual actualJobIdentity) bool {
	return slices.ContainsFunc(jobs, func(prior actualJobIdentity) bool {
		return sameActualJob(prior, actual)
	})
}
