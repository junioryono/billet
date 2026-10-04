package lease

// Disruption names something billet's OWN infrastructure did to a lease while
// the job on it may still have been running.
//
// A CLOSED VOCABULARY, and only billet's control plane ever writes one. It is an
// OBSERVATION rather than a verdict: nothing here says a job failed because of
// it, and nothing derives a conclusion from it on its own. What makes a
// disruption interesting is reading it beside GitHub's own result for the same
// job — see alloc.Allocator.AttributedFailures — which is why the two are
// recorded separately and neither is stored as an answer.
type Disruption string

const (
	// DisruptionNodeForgotten means this control plane stopped hearing from the
	// host the lease was running on and gave up on it.
	//
	// THE WEAKEST OF THE THREE, and it is here because nothing else covers the
	// case that motivated any of this: a host that vanishes mid-job never lets
	// its lease expire — the listener goes on renewing it — so it is never
	// quarantined and no inventory ever reports it absent. The bar is not a
	// blip: nodeplane forgets a host only after four consecutive poll windows of
	// silence, and never while it has a command in flight.
	DisruptionNodeForgotten Disruption = "node-forgotten"
	// DisruptionGuestAbsent means the host's own inventory, taken under the
	// registration this deployment is talking to, did not contain the lease's
	// guest after the quarantine grace. The compute is gone and billet did not
	// remove it.
	DisruptionGuestAbsent Disruption = "guest-absent"
	// DisruptionReclaimed means an external party told billet the machine was
	// being taken — today an EC2 Spot interruption warning. The strongest of the
	// three: billet was informed, about this exact fenced lease, before it began
	// tearing the guest down.
	DisruptionReclaimed Disruption = "reclaimed"
	// DisruptionHeldPastLimit means billet itself destroyed the job, because an
	// operator bounded how long compute may be held (node.WithMaxCustody) and
	// this job outlived the bound. The one disruption billet chooses rather
	// than observes, and it is recorded for the same reason the others are: the
	// build went red, and only billet knows why.
	DisruptionHeldPastLimit Disruption = "held-past-limit"
)

// Valid reports whether this is a token billet may write.
//
// EVERY NEW OBSERVATION GOES THROUGH alloc's disruptableTx and its callers,
// which check this, so the closed set is enforced in one place rather than at
// each call site. The
// database carries no CHECK for it — see migration 35 — so this is the only
// thing standing between a typo and a token no report knows how to render.
//
// AN ARCHIVE CARRY IS NOT A NEW OBSERVATION and is deliberately not checked:
// alloc.archive copies whatever the lease row already holds, which may be a
// token a NEWER binary wrote. Refusing it there would drop an observation on the
// floor to protect a vocabulary that is already on disk, so the reader is total
// instead.
func (d Disruption) Valid() bool {
	switch d {
	case DisruptionNodeForgotten, DisruptionGuestAbsent, DisruptionReclaimed,
		DisruptionHeldPastLimit:
		return true
	}

	return false
}
