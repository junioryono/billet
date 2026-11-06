package hostsource

import "context"

// Verdict is what a source says of a pool host's enrollment (ADR-030). The zero value
// is could-not-tell, which admits nothing and holds nothing.
type Verdict int

const (
	// VerdictUnknown is could-not-tell: the channel was not read, said nothing yet, or
	// said something these checks cannot vouch for.
	VerdictUnknown Verdict = iota
	// VerdictVouches is every check the source makes positive for this key.
	VerdictVouches
	// VerdictContradicts is a positive answer for something else: another key, another
	// instance, another address, or a host that is ending.
	VerdictContradicts
)

func (v Verdict) String() string {
	switch v {
	case VerdictVouches:
		return "vouches"
	case VerdictContradicts:
		return "contradicts"
	}

	return "unknown"
}

// Claim is what a pool host's enrollment says about the host it comes from.
type Claim struct {
	// Order is the pool order the enrollment's token was minted for; the source checks
	// that order's host and no other.
	Order string
	// HostID is the host the source named for the order.
	HostID string
	// Fingerprint is the key the enrollment asks a certificate for.
	Fingerprint string
	// Address is the enrollment's source address, or empty when the listener cannot
	// see the host's own (a router in front of it): an empty address is not checked,
	// and a source that needs it cannot vouch.
	Address string
	// Document and Signature are what the host's own platform signed about it, as the
	// host read them (an EC2 instance identity document), or empty.
	Document  []byte
	Signature string
}

// Testimony is a source's verdict on a claim, and why, for a person to read.
type Testimony struct {
	Verdict Verdict
	Reason  string
}

// Witness is a source that can read, through a channel it authenticates for one exact
// host, which key that host enrolls with. A source with no such channel is not one,
// and its hosts wait for a person to compare a fingerprint.
type Witness interface {
	Witness(ctx context.Context, claim Claim) Testimony
}
