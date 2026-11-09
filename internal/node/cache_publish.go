package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	storecontract "github.com/junioryono/billet/internal/store"
)

// publishWindow bounds how long a closed session keeps retrying a publication
// its completion authorised. Past it the writes are discarded: a cache that
// could not be published in half an hour is a miss, never a reason to hold a
// clone for ever.
const publishWindow = 30 * time.Minute

// CacheAuthorityReader asks the control plane what a lease's job may do with a
// cache now. The node client implements it.
type CacheAuthorityReader interface {
	CacheAuthority(ctx context.Context, leaseID string) (dispatch.CacheAuthority, error)
}

// SetAuthorityReader installs where a default-branch session asks what its job
// may do with the Actions cache while the job runs. Without one, every such
// call goes to GitHub.
func (s *CacheService) SetAuthorityReader(reader CacheAuthorityReader) { s.authority = reader }

// publishIntent is a completion's authority for one attachment, recorded
// durably by SettleCompleted and acted on by the cache loop after the compute
// is gone.
type publishIntent struct {
	LeaseID string    `json:"lease_id"`
	JobID   string    `json:"job_id"`
	RunID   int64     `json:"run_id"`
	At      time.Time `json:"at"`
}

// publishPhase is how far a deferred publication got, recorded BEFORE the
// remote call each phase guards, so a crash leaves a phase that names what may
// already have happened.
type publishPhase string

const (
	// phaseSnapshotting: Snapshot may have run. It consumes the clone, so a
	// crash here can neither snapshot again nor tell which candidate exists:
	// the publication is abandoned and the store's eviction takes the orphan.
	phaseSnapshotting publishPhase = "snapshotting"
	// phaseSnapshotted: the candidate exists and is recorded.
	phaseSnapshotted publishPhase = "snapshotted"
	// phasePublishing: PublishCAS may have run with the recorded writer.
	phasePublishing publishPhase = "publishing"
	phasePublished  publishPhase = "published"
	phaseAbandoned  publishPhase = "abandoned"
)

// publishJournal is a deferred publication's durable progress.
type publishJournal struct {
	Phase     publishPhase             `json:"phase,omitempty"`
	Candidate *storecontract.Candidate `json:"candidate,omitempty"`
	// Consumed says Snapshot took the clone, so there is no volume left to
	// discard whatever becomes of the candidate.
	Consumed bool   `json:"consumed,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// publishDeferred advances one authorised attachment of a closed session as
// far as it can. Called with session.mu held, from cleanupSession.
//
// done reports that the attachment needs nothing more from publication: it was
// published, or abandoned, and what remains of it (the clone, if Snapshot never
// consumed it) is discarded as any other. A false done keeps the attachment for
// the next pass.
func (s *CacheService) publishDeferred(
	ctx context.Context, session *cacheSession, slot int, attachment *cacheAttachment,
) (bool, error) {
	journal := attachment.Journal
	if journal == nil {
		journal = &publishJournal{}
		attachment.Journal = journal
	}

	abandon := func(reason string) (bool, error) {
		journal.Phase, journal.Reason = phaseAbandoned, reason
		s.log.Info("a cache publication was abandoned; the writes are discarded",
			"instance", session.instance, "slot", slot, "key", attachment.Volume.Key,
			"reason", reason)

		return true, s.persistSession(session)
	}

	switch journal.Phase {
	case phasePublished, phaseAbandoned:
		return true, nil
	case phaseSnapshotting:
		return abandon("a crash interrupted the snapshot, so which candidate exists cannot be told")
	}

	if !attachment.Ready || attachment.Intent == nil {
		return abandon("the attachment was never proved ready to publish")
	}
	deadline := attachment.Intent.At.Add(publishWindow)
	if !s.now().Before(deadline) {
		return abandon("the publication did not complete within its window")
	}
	// EVERY STORAGE STEP BELOW IS BOUNDED BY THE WINDOW, a writer's wait
	// included, so one contended key cannot hold the cache loop, and with it
	// every other session's cleanup, past the point this publication is owed.
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// THE AUTHORITY IS ASKED AGAIN, NOW. The completion's grant was decided
	// before the compute was destroyed; a default branch renamed, a permission
	// withdrawn or a kill switch set since then stops the publication here.
	if allowed, reason := s.stillAuthorised(ctx, session, attachment); !allowed {
		return abandon(reason)
	}

	// A STORE THAT HAS STOPPED ANSWERING IS ASKED AGAIN NEXT PASS, inside the window,
	// and nothing here holds the cache loop for it meanwhile. On probation it may be the
	// only thing that could settle it, on a node with no job left to ask for a volume.
	if !s.breaker.admit() {
		return false, nil
	}

	if journal.Phase == "" {
		journal.Phase = phaseSnapshotting
		if err := s.persistSession(session); err != nil {
			return false, err
		}
		candidate, err := s.store.Snapshot(ctx, attachment.Volume)
		s.countedPublication(session.instance, "a cache snapshot", err)
		if errors.Is(err, storecontract.ErrVolumeGone) {
			// NOTHING IS LEFT TO RETRY: an earlier attempt's failure took the clone,
			// and that failure is what was logged. Asking again until the window
			// ended held the session open for it, and for nothing else.
			return abandon("the volume is gone after an earlier failed snapshot")
		}
		if err != nil {
			// RETRIED ON THE NEXT PASS. A failed Snapshot leaves the pointer
			// unchanged, which is all the contract promises. If it also took the
			// clone, the retry says so with ErrVolumeGone, and the discard that
			// follows is idempotent.
			journal.Phase = ""
			journal.Attempts++

			return false, errors.Join(fmt.Errorf("snapshot: %w", err), s.persistSession(session))
		}
		journal.Phase, journal.Candidate, journal.Consumed = phaseSnapshotted, &candidate, true
		if err := s.persistSession(session); err != nil {
			return false, err
		}
	}

	if journal.Candidate == nil {
		return abandon("the journal names no candidate to publish")
	}

	key := attachment.Volume.Key
	lease, fence, err := s.acquireWriter(ctx, attachment, session.instance)
	if err != nil {
		journal.Attempts++

		return false, errors.Join(fmt.Errorf("acquire writer: %w", err), s.persistSession(session))
	}
	// ASKED AGAIN AFTER THE WAIT, which may have been long: a kill switch set
	// while this pass waited for the writer stops it before the pointer moves.
	if allowed, reason := s.stillAuthorised(ctx, session, attachment); !allowed {
		if err := s.releaseWriter(ctx, lease, fence); err != nil {
			s.log.Warn("could not release a cache writer", "key", key, "error", err)
		}

		return abandon(reason)
	}
	journal.Phase = phasePublishing
	if err := s.persistSession(session); err != nil {
		return false, errors.Join(err, s.releaseWriter(ctx, lease, fence))
	}

	candidate := *journal.Candidate
	current, currentErr := s.store.Current(ctx, key)
	s.countedPublication(session.instance, "a cache lookup", currentErr)
	if currentErr == nil && current == candidate.Generation {
		// A publication a crash interrupted after the pointer moved.
		journal.Phase = phasePublished

		return true, errors.Join(s.releaseWriter(ctx, lease, fence), s.persistSession(session))
	}
	err = s.publish(ctx, attachment, candidate, lease, fence)
	s.countedPublish(session.instance, err)
	if err != nil {
		journal.Phase = phaseSnapshotted
		journal.Attempts++

		return false, errors.Join(fmt.Errorf("publish: %w", err), s.releaseWriter(ctx, lease, fence),
			s.persistSession(session))
	}
	journal.Phase = phasePublished
	s.log.Info("published a cache a default-branch job wrote", "instance", session.instance,
		"slot", slot, "key", key, "generation", candidate.Generation,
		"job", attachment.Intent.JobID)

	return true, s.persistSession(session)
}

// stillAuthorised asks the kill switch again before anything is written.
//
// THE AUTHORITY ITSELF IS NOT RE-DECIDED. The control plane decided it at the
// job's completion, from GitHub's record of the run and the repository's
// default branch read fresh, and the binding it was decided from is settled
// and gone soon after. What can change between the completion and this pass
// (seconds, at most publishWindow) and must stop a publication is an
// operator's kill switch, which is asked here. A default branch renamed inside
// that window is the accepted residual: the writes are those of a run GitHub
// proved was on the then-default branch.
func (s *CacheService) stillAuthorised(
	ctx context.Context, session *cacheSession, attachment *cacheAttachment,
) (bool, string) {
	if !s.kindAllowed(ctx, attachmentKind(attachment), session.cache.Owner, session.cache.Repository) {
		return false, "the cache is disabled for this repository"
	}

	return true, ""
}

// pendingPublication reports whether an attachment still has a clone a
// publication is waiting on.
func pendingPublication(attachment *cacheAttachment) bool {
	if attachment.Intent == nil {
		return false
	}
	journal := attachment.Journal

	return journal == nil || (!journal.Consumed && journal.Phase != phaseAbandoned &&
		journal.Phase != phasePublished)
}

// sessionKindAllowed asks the kill switch about a cache for the session's
// static scope, as the plane answers now: what a publication is decided by.
// Serving a job asks sessionServingAllowed.
func (s *CacheService) sessionKindAllowed(ctx context.Context, session *cacheSession,
	kind config.CacheKind,
) bool {
	return s.sessionKindPolicy(ctx, session, kind) == policyAllowed
}

// sessionKindPolicy is sessionKindAllowed's answer with could-not-tell kept
// apart.
func (s *CacheService) sessionKindPolicy(ctx context.Context, session *cacheSession,
	kind config.CacheKind,
) policyAnswer {
	// A SESSION WITH NO REPOSITORY IS NOTHING A KILL SWITCH CAN NAME: blocks are
	// by repository or by its owner, and the ledger answers only for a
	// repository. A trusted-only tier without a cache scope is one.
	owner, repository := session.cacheOwner(), session.cacheRepository()
	if owner == "" || repository == "" {
		return policyAllowed
	}

	return s.kindPolicy(ctx, kind, owner, repository)
}

// sessionServingAllowed is sessionServingPolicy's answer as allowed or not.
func (s *CacheService) sessionServingAllowed(ctx context.Context, session *cacheSession,
	kind config.CacheKind,
) bool {
	return s.sessionServingPolicy(ctx, session, kind) == policyAllowed
}

// sessionServingPolicy is servingPolicy for the session's static scope: what
// serving the session's job from a cache asks, never a publication.
func (s *CacheService) sessionServingPolicy(ctx context.Context, session *cacheSession,
	kind config.CacheKind,
) policyAnswer {
	answer, _ := s.sessionServingPolicyAt(ctx, session, kind)

	return answer
}

// sessionServingPolicyAt is sessionServingPolicy with when its answer was
// given (servingPolicyAt).
func (s *CacheService) sessionServingPolicyAt(ctx context.Context, session *cacheSession,
	kind config.CacheKind,
) (policyAnswer, time.Time) {
	owner, repository := session.cacheOwner(), session.cacheRepository()
	if owner == "" || repository == "" {
		return policyAllowed, s.now()
	}

	return s.servingPolicyAt(ctx, kind, owner, repository)
}

// volumeCeiling is the largest volume a cache setting admits.
func volumeCeiling(setting config.CacheSetting) int64 {
	if setting.MaxSize <= 0 || int64(setting.MaxSize) > cacheVolumeLimit {
		return cacheVolumeLimit
	}

	return int64(setting.MaxSize)
}

// cloneOrCreate is the volume a job asked for: a clone of key's newest generation no
// larger than ceiling, or a fresh volume of size, which it also answers cold for.
func (s *CacheService) cloneOrCreate(
	ctx context.Context, session, key string, ceiling, size int64,
) (storecontract.Volume, bool, error) {
	volume, cold, err := s.cloneWithin(ctx, session, key, ceiling)
	if cold {
		volume, err = s.handOver(session, "a new volume", func() (storecontract.Volume, error) {
			return s.store.Create(ctx, key, size)
		})
	}

	return volume, cold, err
}

// cloneWithin clones a key's current generation, or reports a cold cache.
//
// A GENERATION LARGER THAN THE CEILING IS NOT HANDED OUT: its clone is
// discarded and the job starts cold at its own size, so lowering a tier's
// max_size re-seeds its caches rather than leaving them at the old size. A store
// that cannot say how large a clone is keeps it.
func (s *CacheService) cloneWithin(
	ctx context.Context, session, key string, ceiling int64,
) (storecontract.Volume, bool, error) {
	volume, err := s.handOver(session, "a clone", func() (storecontract.Volume, error) {
		return s.store.Clone(ctx, key, "")
	})
	if errors.Is(err, storecontract.ErrMiss) {
		return storecontract.Volume{}, true, nil
	}
	if err != nil {
		return storecontract.Volume{}, false, err
	}
	sizer, ok := s.store.(storecontract.VolumeSizer)
	if !ok {
		return volume, false, nil
	}
	size, known := s.volumeSize(ctx, sizer, key, volume)
	if !known || size <= ceiling {
		return volume, false, nil
	}
	// A CLONE THAT CANNOT BE DISCARDED IS KEPT AND USED, oversized, rather than
	// dropped: nothing has recorded it yet, so returning without it would lose
	// the only handle anything has on it. A discard unmaps first and may fail after
	// that, though, so its device is proved again before anything is given it: the
	// number may already be another job's disk. One that cannot be is not used, and
	// the store's eviction collects it once its lease runs out unrenewed.
	if err := s.store.Discard(ctx, volume); err != nil {
		device, proveErr := s.provenDevice(ctx, volume)
		if proveErr != nil {
			return storecontract.Volume{}, false, errors.Join(
				fmt.Errorf("discard a cache generation larger than its tier allows: %w", err), proveErr)
		}
		s.log.Warn("could not discard a cache generation larger than its tier allows; using it",
			"key", key, "generation", volume.Generation, "size", size, "ceiling", ceiling,
			"error", err)
		volume.Device = device

		return volume, false, nil
	}
	s.log.Info("a cache generation is larger than its tier allows; starting cold",
		"key", key, "generation", volume.Generation, "size", size, "ceiling", ceiling)

	return storecontract.Volume{}, true, nil
}

// volumeSize is how large a clone is, when the store can say. One it cannot
// size is used as it is, as a store with no sizer's always is.
func (s *CacheService) volumeSize(
	ctx context.Context, sizer storecontract.VolumeSizer, key string, volume storecontract.Volume,
) (int64, bool) {
	size, err := sizer.SizeOf(ctx, volume)
	if err != nil {
		s.log.Warn("could not read a cache generation's size; using it",
			"key", key, "generation", volume.Generation, "error", err)

		return 0, false
	}

	return size, true
}

// policyAnswer is what the kill switch said about a cache. The zero value is
// could-not-tell.
type policyAnswer int

const (
	policyUnknown policyAnswer = iota
	policyAllowed
	policyRefused
)

// kindAllowed asks the kill switch about one cache for a repository. Could not
// tell is refused: a publication the switch might forbid does not happen.
func (s *CacheService) kindAllowed(ctx context.Context, kind config.CacheKind,
	owner, repository string,
) bool {
	return s.kindPolicy(ctx, kind, owner, repository) == policyAllowed
}

// kindPolicy is kindAllowed's answer with could-not-tell kept apart, for a
// caller that records what the cache did: an unreadable switch is not one that
// refused. It is the plane's answer now and nothing else, which is what a
// publication is decided by.
func (s *CacheService) kindPolicy(ctx context.Context, kind config.CacheKind,
	owner, repository string,
) policyAnswer {
	answer, err := s.askKindPolicy(ctx, kind, owner, repository)
	if answer == policyUnknown {
		s.log.Warn("the cache kill switch could not be read; the cache is not used",
			"kind", kind, "owner", owner, "repository", repository, "error", err)
	}

	return answer
}

// servingPolicy is the kill switch's answer for serving a job from a cache:
// the plane's answer now, or, while the plane cannot be asked, its last
// "allowed" for that cache of that repository if that is younger than
// policyRememberedFor. A publication never uses it: what it writes is read by
// later jobs, and its own pass retries inside its window.
func (s *CacheService) servingPolicy(ctx context.Context, kind config.CacheKind,
	owner, repository string,
) policyAnswer {
	answer, _ := s.servingPolicyAt(ctx, kind, owner, repository)

	return answer
}

// servingPolicyAt is servingPolicy with when its answer was given: when the
// plane was asked, or when it gave the remembered answer. A session that skips
// asking for a while after an "allowed" counts that while from here
// (allowedAt), so it never serves past the remembered answer's age.
func (s *CacheService) servingPolicyAt(ctx context.Context, kind config.CacheKind,
	owner, repository string,
) (policyAnswer, time.Time) {
	asked := s.now()
	answer, err := s.askKindPolicy(ctx, kind, owner, repository)
	if answer != policyUnknown {
		return answer, asked
	}

	return s.rememberedPolicy(kind, owner, repository, err)
}

// rememberedPolicy is what serving falls back to when the plane could not be
// asked: allowed while the last "allowed" stands, could-not-tell otherwise,
// with when the remembered answer was given.
func (s *CacheService) rememberedPolicy(kind config.CacheKind, owner, repository string,
	err error,
) (policyAnswer, time.Time) {
	now := s.now()
	if answered, ok := s.policyMemo.standing(kind, owner, repository, now); ok {
		s.log.Warn("the cache kill switch could not be read; serving by its last answer, which allowed this cache",
			"kind", kind, "owner", owner, "repository", repository,
			"answered", now.Sub(answered).Round(time.Second), "error", err)

		return policyAllowed, answered
	}
	s.log.Warn("the cache kill switch could not be read; the cache is not used",
		"kind", kind, "owner", owner, "repository", repository, "error", err)

	return policyUnknown, now
}

// askKindPolicy asks the plane and records what it answered: an "allowed" is
// remembered and a refusal forgets it.
func (s *CacheService) askKindPolicy(ctx context.Context, kind config.CacheKind,
	owner, repository string,
) (policyAnswer, error) {
	if s.policy == nil {
		return policyAllowed, nil
	}
	ask := s.policyMemo.begin(s.now())
	policyCtx, cancel := context.WithTimeout(ctx, actionsPolicyLimit)
	defer cancel()
	allowed, err := s.policy.CacheAllowed(policyCtx, kind, owner, repository)

	return s.notePolicy(kind, owner, repository, allowed, err, ask), err
}

// notePolicy turns one answer of the plane, to an ask begun as ask, into a
// policyAnswer and records it.
func (s *CacheService) notePolicy(kind config.CacheKind, owner, repository string,
	allowed bool, err error, ask policyAsk,
) policyAnswer {
	switch {
	case err != nil:
		return policyUnknown
	case allowed:
		s.policyMemo.note(kind, owner, repository, true, ask)

		return policyAllowed
	default:
		s.policyMemo.note(kind, owner, repository, false, ask)

		return policyRefused
	}
}

// errActionsPublicationDisabled is a publication of an Actions cache entry the
// kill switch refused, as the plane answered at that moment: the operator's
// choice, recorded as disabled, never as billet's failure.
var errActionsPublicationDisabled = errors.New("the cache kill switch refused publishing this Actions cache entry")

// errActionsPublicationUnknown is a publication of an Actions cache entry whose
// kill switch could not be read at that moment.
var errActionsPublicationUnknown = errors.New("the cache kill switch could not be read, so this Actions cache entry is not published")

// actionsPublicationErr asks the plane now whether a session's Actions cache
// may publish what it saved, through the same reader actionsKeysFor asks for
// that session, and never by a remembered answer: a published entry is what
// later jobs restore. A refusal gives back a probation's trial, since the
// store was not asked.
func (s *CacheService) actionsPublicationErr(ctx context.Context, session *cacheSession) error {
	var answer policyAnswer
	switch {
	case sessionPolicy(session) == config.CachePublishDefaultBranch:
		answer = s.kindPolicy(ctx, config.CacheActions, session.cache.Owner, session.cache.Repository)
	case s.actionRule == nil:
		answer = policyUnknown
	default:
		ask := s.policyMemo.begin(s.now())
		policyCtx, cancel := context.WithTimeout(ctx, actionsPolicyLimit)
		allowed, err := s.actionRule.ActionsCacheAllowed(policyCtx, session.owner, session.repository)
		cancel()
		answer = s.notePolicy(config.CacheActions, session.owner, session.repository, allowed, err, ask)
		if answer == policyUnknown {
			s.log.Warn("the cache kill switch could not be read; this Actions cache entry is not published",
				"instance", session.instance, "owner", session.owner, "repository", session.repository,
				"error", err)
		}
	}
	switch answer {
	case policyAllowed:
		return nil
	case policyRefused:
		s.breaker.unused()

		return errActionsPublicationDisabled
	default:
		s.breaker.unused()

		return errActionsPublicationUnknown
	}
}

func attachmentKind(attachment *cacheAttachment) config.CacheKind {
	if attachment.Docker {
		return config.CacheDocker
	}

	return config.CacheSticky
}

// SettleCompleted records what a completed job's caches may publish, while the
// runner still holds its lease.
//
// NOTHING IS PUBLISHED HERE. A default-branch session's authorised attachments
// get a durable intent, and the cache loop publishes them after the compute is
// proved gone, off the node's command path; the one thing done in line is the
// Docker store's readiness handshake, because it needs the guest alive. A
// session already closing is not waited for: its storage is busy and a
// redelivered completion must not queue behind it, and it records nothing more.
func (s *CacheService) SettleCompleted(
	ctx context.Context, instance string, succeeded bool, authority dispatch.CacheAuthority,
) error {
	session := s.sessionOf(instance)
	if session == nil {
		return nil
	}
	// A DECISION TO RECORD NOTHING SAYS SO. A completed job's writes are
	// discarded with the session, so without these a publication that never
	// happened leaves no line at all: a default-branch job on an EC2 tier published
	// nothing on 2026-10-25, and its node's log could not say which of these held.
	//
	// A CLOSING SESSION IS NOT ONE THAT WILL NOT PUBLISH: an intent an earlier
	// completion recorded still publishes in its cleanup. Only this one adds nothing.
	if session.closing.Load() {
		s.log.Info("a completion recorded no publication: its session is already closing, "+
			"and what an earlier one recorded stands", "instance", instance)

		return nil
	}

	if !publicationAllowed(session, succeeded, authority) {
		s.log.Info("a completed job's caches will not publish", "instance", instance,
			"policy", sessionPolicy(session), "succeeded", succeeded, "proven", authority.Proven,
			"publish_default", authority.PublishDefault, "event", authority.Event, "ref", authority.Ref)

		return nil
	}
	intent := &publishIntent{LeaseID: authority.LeaseID, JobID: authority.JobID,
		RunID: authority.RunID, At: s.now()}

	if err := lockCacheSession(ctx, session); err != nil {
		return err
	}
	// THE CONTENT-ADDRESSED CACHES PUBLISH UNDER EITHER POLICY, always from the
	// cache loop after the compute is gone.
	for _, hv := range session.hosts {
		if hv.Dirty {
			hv.Intent = intent
		}
	}
	if sessionPolicy(session) == config.CachePublishTrustedOnly {
		err := s.persistSession(session)
		session.mu.Unlock()
		if err != nil {
			return err
		}

		return s.SettleDocker(ctx, instance, succeeded)
	}
	for _, attachment := range session.slots {
		if attachment != nil && !attachment.Docker && attachment.Deferred && attachment.Ready {
			attachment.Intent = intent
		}
	}
	err := s.persistSession(session)
	docker := session.slots[0] != nil && session.slots[0].Docker
	session.mu.Unlock()
	if err != nil || !docker {
		return err
	}

	attachment, err := s.awaitDockerReady(ctx, session)
	if errors.Is(err, errNoDockerStore) {
		s.log.Info("a completed job's Docker image store will not publish: it was discarded "+
			"before it was proved ready", "instance", instance)

		return nil
	}
	if err != nil {
		return err
	}
	defer session.mu.Unlock()
	attachment.Intent = intent

	return s.persistSession(session)
}
