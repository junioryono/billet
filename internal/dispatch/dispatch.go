// Package dispatch is the vocabulary the control plane and a compute host share
// to turn a lease into running compute and back: the Runner contract, the job a
// runner is launched for, what a completed job's caches may publish, and the two
// answers a runner gives that change who holds capacity.
//
// It sits below both halves so the node runtime names these without importing
// the scheduler. It imports nothing of billet's but internal/lease, the words a
// lease is described in, and the depguard rule `dispatch` keeps it that way.
package dispatch

import (
	"context"
	"errors"

	"github.com/junioryono/billet/internal/lease"
)

// Runner turns an assigned lease into running compute, and tears it down again.
// It is the seam between the control plane and a host.
//
// Both methods are called OUTSIDE the escrow mutex: launching pulls images and
// talks to a hypervisor, and holding the mutex across that would stall every
// heartbeat behind it.
type Runner interface {
	// The lease is already durable and counted against the budget, so a failure
	// here means capacity is held for something that is not running; the caller
	// releases it.
	Launch(ctx context.Context, lease *lease.Lease, job Job) error

	// MUST be idempotent: it runs on redelivered completions, on shutdown, and on
	// paths that have already failed once.
	Destroy(ctx context.Context, requestID int64) error
}

// CompletionAwareRunner receives GitHub's authoritative completed-job result.
// It is optional so runners that have no result-dependent teardown keep the
// smaller Runner contract.
//
// The authority is what the completed job's caches may publish; its zero value
// authorises nothing.
type CompletionAwareRunner interface {
	DestroyCompleted(ctx context.Context, requestID int64, result string,
		authority CacheAuthority) error
}

// BoundCompletionAwareRunner reconciles teardown with the node and lease that
// actually held compute before a control-plane restart erased live ownership.
type BoundCompletionAwareRunner interface {
	DestroyCompletedBound(
		ctx context.Context,
		requestID int64,
		result, leaseID, nodeName string,
		leaseEpoch int64,
		outcome lease.Phase,
		authority CacheAuthority,
	) error
}

// ErrCustody means the runner has taken responsibility for a lease's capacity,
// so the caller must NOT release it.
//
// Returned from Launch when compute may exist that could not be confirmed gone.
// Releasing then would hand the capacity back while a container is possibly
// still running on it.
var ErrCustody = errors.New("dispatch: the runner is holding this lease's capacity")

// ErrHolderUnavailable means result-dependent teardown has not reached the
// process that holds the compute, so its durable completion must be retried.
var ErrHolderUnavailable = errors.New("dispatch: the completion holder is unavailable")

// Job identifies one workflow job.
//
// RequestID is billet's numeric scheduler identity. GitHub's positive
// runnerRequestId is used unchanged; a direct assignment carrying zero receives
// a durable negative id keyed by JobID, so concurrent jobs never alias at zero.
type Job struct {
	RequestID int64
	RunID     int64
	// RunnerID and RunnerName identify the pool member GitHub actually bound.
	// They are authoritative only on JobStarted and JobCompleted messages.
	RunnerID int64
	// JobID is GitHub's stable workflow-job identity. It is required when the
	// direct-assignment path sends RequestID zero.
	JobID string
	// CompletionID is the scale-set message that delivered the result. A
	// redelivery keeps it; a later reuse of RequestID receives a different one.
	CompletionID int64
	// RunnerName is GitHub's name for the ephemeral runner. Completed messages
	// can omit RequestID, so the billet-issued name is the durable route back to
	// the lease and its assigned request.
	RunnerName string
	// Result is GitHub's conclusion on a completed-job message. It is empty on
	// available and assigned messages.
	Result string
	// The GitHub event that queued this job — retained for diagnostics only. A JIT
	// runner joins a pool before GitHub chooses its job, so event is not launch
	// authority; the tier's static trust policy is.
	Event string
	// Owner, Repository and WorkflowRef are GitHub's authenticated cache scope.
	// They come from the scale-set assignment, never from a workflow-controlled
	// environment variable or by decoding the Actions runtime token.
	Owner       string
	Repository  string
	WorkflowRef string
	// JobName is the job's display name, kept for people to read and never
	// consulted for a decision.
	JobName string
}

// CacheAuthority is what a cache may do for one job, decided from evidence
// GitHub produced and billet recorded. The zero value authorises nothing.
type CacheAuthority struct {
	LeaseID    string
	JobID      string
	RunID      int64
	Owner      string
	Repository string
	Event      string
	// Ref is the job's own cache scope: `refs/heads/<branch>`,
	// `refs/tags/<tag>` or `refs/pull/<n>/merge`.
	Ref string
	// BaseRef is a pull request's base branch, `refs/heads/<branch>`, which
	// GitHub also lets a pull request restore from. Empty for everything else.
	BaseRef string
	// DefaultRef is `refs/heads/<default branch>` when the authority was decided.
	DefaultRef string
	// Proven says every piece of evidence agreed. Nothing below it may be true
	// without it.
	Proven bool
	// WriteOwnRef says the job may write under Ref.
	WriteOwnRef bool
	// PublishDefault says the job may publish what the default branch reads.
	PublishDefault bool
}
