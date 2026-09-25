package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/server"
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
	CacheAuthority(ctx context.Context, leaseID string) (server.CacheAuthority, error)
}

// SetAuthorityReader installs where a deferred publication re-checks its
// authority before it writes anything. Without one, nothing deferred publishes:
// a permission that cannot be re-checked is one that cannot be told.
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
) (done bool, err error) {
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
	if time.Since(attachment.Intent.At) > publishWindow {
		return abandon("the publication did not complete within its window")
	}

	// THE AUTHORITY IS ASKED AGAIN, NOW. The completion's grant was decided
	// before the compute was destroyed; a default branch renamed, a permission
	// withdrawn or a kill switch set since then stops the publication here.
	if allowed, reason, err := s.stillAuthorised(ctx, session, attachment); err != nil {
		journal.Attempts++

		return false, errors.Join(err, s.persistSession(session))
	} else if !allowed {
		return abandon(reason)
	}

	if journal.Phase == "" {
		journal.Phase = phaseSnapshotting
		if err := s.persistSession(session); err != nil {
			return false, err
		}
		candidate, err := s.store.Snapshot(ctx, attachment.Volume)
		if err != nil {
			// RETRIED ON THE NEXT PASS. A failed Snapshot leaves the pointer
			// unchanged, which is all the contract promises; if it also consumed
			// the clone, the retry fails the same way and the window's end
			// abandons it, and the discard that follows is idempotent.
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
	journal.Phase = phasePublishing
	if err := s.persistSession(session); err != nil {
		return false, errors.Join(err, s.releaseWriter(ctx, lease, fence))
	}

	candidate := *journal.Candidate
	current, currentErr := s.store.Current(ctx, key)
	if currentErr == nil && current == candidate.Generation {
		// A publication a crash interrupted after the pointer moved.
		journal.Phase = phasePublished

		return true, errors.Join(s.releaseWriter(ctx, lease, fence), s.persistSession(session))
	}
	if err := s.publish(ctx, attachment, candidate, lease, fence); err != nil {
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

// stillAuthorised asks the control plane again whether the session's job may
// publish, and checks the answer names the same job the completion did.
func (s *CacheService) stillAuthorised(
	ctx context.Context, session *cacheSession, attachment *cacheAttachment,
) (bool, string, error) {
	if s.authority == nil {
		return false, "no control plane to re-check the authority with", nil
	}
	authority, err := s.authority.CacheAuthority(ctx, session.leaseID)
	if err != nil {
		return false, "", fmt.Errorf("re-check the cache authority: %w", err)
	}
	if !authorisesPublication(session, authority) || authority.JobID != attachment.Intent.JobID ||
		authority.RunID != attachment.Intent.RunID {
		return false, "the control plane no longer authorises this publication", nil
	}
	if !s.kindAllowed(ctx, attachmentKind(attachment), session.cache.Owner, session.cache.Repository) {
		return false, "the cache is disabled for this repository", nil
	}

	return true, "", nil
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
// static scope, which is all a guest's first request can be judged by.
func (s *CacheService) sessionKindAllowed(ctx context.Context, session *cacheSession,
	kind config.CacheKind,
) bool {
	owner, repository := session.owner, session.repository
	if session.cache != nil && session.cache.Owner != "" {
		owner, repository = session.cache.Owner, session.cache.Repository
	}
	if owner == "" {
		return true
	}

	return s.kindAllowed(ctx, kind, owner, repository)
}

// volumeCeiling is the largest volume a cache setting admits.
func volumeCeiling(setting config.CacheSetting) int64 {
	if setting.MaxSize <= 0 || int64(setting.MaxSize) > cacheVolumeLimit {
		return cacheVolumeLimit
	}

	return int64(setting.MaxSize)
}

// cloneWithin clones a key's current generation, or reports a cold cache.
//
// A GENERATION LARGER THAN THE CEILING IS NOT HANDED OUT: its clone is
// discarded and the job starts cold at its own size, so lowering a tier's
// max_size re-seeds its caches rather than leaving them at the old size. A store
// that cannot say how large a clone is keeps it.
func (s *CacheService) cloneWithin(
	ctx context.Context, key string, ceiling int64,
) (storecontract.Volume, bool, error) {
	volume, err := s.store.Clone(ctx, key, "")
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
	size, err := sizer.SizeOf(ctx, volume)
	if err != nil || size <= ceiling {
		return volume, false, nil
	}
	s.log.Info("a cache generation is larger than its tier allows; starting cold",
		"key", key, "generation", volume.Generation, "size", size, "ceiling", ceiling)
	if err := s.store.Discard(ctx, volume); err != nil {
		return storecontract.Volume{}, false, fmt.Errorf("discard an oversized clone: %w", err)
	}

	return storecontract.Volume{}, true, nil
}

// kindAllowed asks the kill switch about one cache for a repository. Could not
// tell is refused: a publication the switch might forbid does not happen.
func (s *CacheService) kindAllowed(ctx context.Context, kind config.CacheKind,
	owner, repository string,
) bool {
	if s.policy == nil {
		return true
	}
	policyCtx, cancel := context.WithTimeout(ctx, actionsPolicyLimit)
	defer cancel()
	allowed, err := s.policy.CacheAllowed(policyCtx, kind, owner, repository)
	if err != nil {
		s.log.Warn("the cache kill switch could not be read; the cache is treated as disabled",
			"kind", kind, "owner", owner, "repository", repository, "error", err)

		return false
	}

	return allowed
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
// redelivered completion must not queue behind it, and nothing is recorded,
// which publishes nothing.
func (s *CacheService) SettleCompleted(
	ctx context.Context, instance string, succeeded bool, authority server.CacheAuthority,
) error {
	session := s.sessionOf(instance)
	if session == nil || session.closing.Load() {
		return nil
	}

	switch sessionPolicy(session) {
	case config.CachePublishTrustedOnly:
		return s.SettleDocker(ctx, instance, succeeded)
	case config.CachePublishOff:
		return nil
	}

	if !succeeded || !authorisesPublication(session, authority) {
		return nil
	}

	intent := &publishIntent{LeaseID: authority.LeaseID, JobID: authority.JobID,
		RunID: authority.RunID, At: s.now()}

	if err := lockCacheSession(ctx, session); err != nil {
		return err
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
	if err != nil || attachment == nil {
		return err
	}
	if err := lockCacheSession(ctx, session); err != nil {
		return err
	}
	defer session.mu.Unlock()
	if session.slots[0] == attachment && attachment.Ready {
		attachment.Intent = intent
	}

	return s.persistSession(session)
}
