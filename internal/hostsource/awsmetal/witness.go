package awsmetal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/joinconsole"
	"github.com/junioryono/billet/internal/provider/ec2"
)

var _ hostsource.Witness = (*Source)(nil)

// Witness says whether the order's host is the one asking to join with this key
// (ADR-030). It vouches only when all three agree with what EC2 says of that exact
// instance:
//
//   - its serial console, which only that instance writes and only this account reads,
//     names this key for the order, and no other;
//   - its identity document, signed by AWS, names that instance, this account and region,
//     its launch and its private address;
//   - the enrollment came from that private address, when the listener can see it.
//
// IT CONTRADICTS only on a positive answer about the host itself: the console names
// only other keys, a verified document names another instance, account or address, or
// the instance is ending. ANYTHING ELSE IS COULD-NOT-TELL: a failed read, a console that
// has not shown the line yet, two keys on it, a document no certificate here verifies
// (AWS rotates them), and an address or a launch time that does not match, which a NAT
// or a proxy in front of the listener, or a launch EC2 recorded differently, would
// explain without anything being wrong with the host.
//
// A REASON NEVER QUOTES AN AWS ANSWER: an EC2 or STS error's prose can name the caller's
// account and role, so a failure is said by its code.
func (s *Source) Witness(ctx context.Context, claim hostsource.Claim) hostsource.Testimony {
	unknown := func(format string, a ...any) hostsource.Testimony {
		return hostsource.Testimony{Verdict: hostsource.VerdictUnknown, Reason: fmt.Sprintf(format, a...)}
	}
	contradicts := func(format string, a ...any) hostsource.Testimony {
		return hostsource.Testimony{Verdict: hostsource.VerdictContradicts, Reason: fmt.Sprintf(format, a...)}
	}
	if claim.HostID == "" || claim.Order == "" || claim.Fingerprint == "" {
		return unknown("the claim names no host, order or key")
	}

	rec, err := s.hosts.Describe(ctx, claim.HostID)
	switch {
	case err != nil:
		return unknown("EC2 could not describe %s (%s)", claim.HostID, s.failure(err))
	case rec.State == "shutting-down" || rec.State == "terminated":
		return contradicts("instance %s is %s", claim.HostID, rec.State)
	case rec.State != "running":
		return unknown("instance %s is %q, not running", claim.HostID, rec.State)
	case rec.PrivateIP == "":
		return unknown("EC2 described %s with no private address", claim.HostID)
	case claim.Address != "" && claim.Address != rec.PrivateIP:
		return unknown("the enrollment came from %s and instance %s is at %s", claim.Address, claim.HostID, rec.PrivateIP)
	}

	// THE DOCUMENT, read only once the instance is known: a verified document naming
	// anything else is a contradiction, one that does not verify says nothing.
	if s.identity == nil || len(claim.Document) == 0 {
		return unknown("the enrollment carries no identity document this source can check")
	}
	doc, err := s.identity.Verify(s.region, claim.Document, claim.Signature)
	if err != nil {
		return unknown("the identity document does not verify under this region's certificate")
	}
	account, err := s.account(ctx)
	if err != nil || account == "" {
		return unknown("which account the credentials are in could not be read")
	}
	switch {
	case doc.InstanceID != claim.HostID:
		return contradicts("the identity document names instance %s, not %s", doc.InstanceID, claim.HostID)
	case doc.AccountID != account:
		return contradicts("the identity document names another account than this source's")
	case doc.PrivateIP != rec.PrivateIP:
		return contradicts("the identity document names address %s and instance %s is at %s", doc.PrivateIP,
			claim.HostID, rec.PrivateIP)
	case rec.LaunchTime.IsZero():
		return unknown("EC2 described %s with no launch time", claim.HostID)
	case !doc.PendingTime.Equal(rec.LaunchTime):
		return unknown("the identity document was signed for a launch at %s and EC2 says %s launched at %s",
			doc.PendingTime.Format(time.RFC3339), claim.HostID, rec.LaunchTime.Format(time.RFC3339))
	}

	// THE CONSOLE LAST, because it is the one read EC2 throttles.
	console, err := s.hosts.Console(ctx, claim.HostID)
	if err != nil {
		return unknown("EC2 could not read the console of %s (%s)", claim.HostID, s.failure(err))
	}
	keys := joinconsole.Fingerprints(console, claim.Order)
	switch {
	case len(keys) == 0:
		return unknown("the console of %s does not name a key for order %s yet", claim.HostID, claim.Order)
	case !slices.Contains(keys, claim.Fingerprint):
		return contradicts("the console of %s names %s for order %s, not %s", claim.HostID,
			strings.Join(keys, ", "), claim.Order, claim.Fingerprint)
	case len(keys) > 1:
		return unknown("the console of %s names %d keys for order %s", claim.HostID, len(keys), claim.Order)
	case claim.Address == "":
		// THE ADDRESS WAS NOT SEEN, AND THE DOCUMENT STANDS FOR IT: a router in front of
		// the listener hides the host's address, and the document is what binds the
		// request to the instance there.
		return hostsource.Testimony{Verdict: hostsource.VerdictVouches,
			Reason: "the console, the identity document and EC2 agree; the address was not seen"}
	}

	return hostsource.Testimony{Verdict: hostsource.VerdictVouches,
		Reason: "the console, the identity document, the address and EC2 agree"}
}

// failure names what went wrong with an EC2 call in billet's words or EC2's code.
func (s *Source) failure(err error) string {
	code, coded := s.code(err)
	switch {
	case errors.Is(err, ec2.ErrHostNotFound):
		return "EC2 has no such instance yet"
	case errors.Is(err, ec2.ErrNotAPoolHost):
		return "it is not this deployment's pool host"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "the call ran out of time"
	case coded:
		return "EC2 answered " + code
	}

	return "the call failed"
}
