package awsmetal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/awsidentity"
	"github.com/junioryono/billet/internal/hostsource"
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
// IT CONTRADICTS only on a positive answer for something else: the console names only
// other keys, the address or a verified document names another instance, or the
// instance is ending. ANYTHING IT CANNOT READ IS COULD-NOT-TELL: a failed Describe, a
// console that has not shown the line yet, two keys on the console, or a document no
// certificate here verifies (AWS rotates them), and an empty address cannot vouch
// unless the document does.
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
		return unknown("EC2 could not describe %s: %v", claim.HostID, err)
	case rec.State == "shutting-down" || rec.State == "terminated":
		return contradicts("instance %s is %s", claim.HostID, rec.State)
	case rec.State != "running":
		return unknown("instance %s is %q, not running", claim.HostID, rec.State)
	case rec.PrivateIP == "":
		return unknown("EC2 described %s with no private address", claim.HostID)
	case claim.Address != "" && claim.Address != rec.PrivateIP:
		return contradicts("the enrollment came from %s and instance %s is at %s", claim.Address, claim.HostID, rec.PrivateIP)
	}

	// THE DOCUMENT, read only once the instance is known: a verified document naming
	// anything else is a contradiction, one that does not verify says nothing.
	if s.identity == nil || len(claim.Document) == 0 {
		return unknown("the enrollment carries no identity document this source can check")
	}
	doc, err := s.identity.Verify(s.region, claim.Document, claim.Signature)
	if errors.Is(err, awsidentity.ErrUnverified) || err != nil {
		return unknown("the identity document: %v", err)
	}
	account, err := s.account(ctx)
	if err != nil || account == "" {
		return unknown("which account the credentials are in: %v", err)
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
		return contradicts("the identity document was signed for a launch at %s and %s launched at %s",
			doc.PendingTime.Format("2006-01-02T15:04:05Z"), claim.HostID, rec.LaunchTime.Format("2006-01-02T15:04:05Z"))
	}

	// THE CONSOLE LAST, because it is the one read EC2 throttles.
	console, err := s.hosts.Console(ctx, claim.HostID)
	if err != nil {
		return unknown("EC2 could not read the console of %s: %v", claim.HostID, err)
	}
	keys := hostsource.ConsoleFingerprints(console, claim.Order)
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
