package node

import (
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// errWritesLost is a publication refused because writes to its volume never
// reached the volume's storage: the volume is discarded, never published.
var errWritesLost = errors.New("writes to the volume were lost, so it is discarded rather than published")

// writesLost says err reports writes to a volume that never reached its storage:
// a detach after a failed write-back (provider.ErrWritesLost), a snapshot's
// write-back or a mounted filesystem's syncfs (store.ErrWritesLost).
//
// THE KERNEL SAYS IT ONCE. A failed write-back is reported to the first fsync or
// syncfs that asks after it and to none after, so the attempt that hears it is the
// only one that will: a retry is told the volume is fine. Whoever hears it marks the
// volume before doing anything else, in memory and in the session's record, and
// every publication refuses a marked volume (noteWritesLost).
func writesLost(err error) bool {
	return errors.Is(err, storecontract.ErrWritesLost) || errors.Is(err, provider.ErrWritesLost)
}

// noteWritesLost marks a volume whose writes were lost, through mark, and records
// the session: the answer is errWritesLost with what reported it, and with the
// record's failure if it could not be made, which is also logged. The mark is set
// first, so a failed record still refuses every publication this process tries.
func (s *CacheService) noteWritesLost(session *cacheSession, mark *bool, what string, err error) error {
	*mark = true
	s.log.Warn("writes to a cache volume never reached its storage; it is discarded, never published",
		"instance", session.instance, "volume", what, "error", err)
	persistErr := s.persistSession(session)
	if persistErr != nil {
		s.log.Warn("could not record a cache volume's lost writes; this process still refuses to publish it",
			"instance", session.instance, "volume", what, "error", persistErr)
	}

	return errors.Join(fmt.Errorf("%w: %w", errWritesLost, err), persistErr)
}
