// Package releaseproof is the proof a rented host is released on (ADR-030): built by the
// ledger in the transaction that seals the host, and required by the source that takes
// the host back. A package of its own because both sides need it and neither may import
// the other: the allocator writes the ledger and reaches no network, and a host source
// calls one.
//
// Its zero value proves nothing, and the only ways to build one that does are the
// constructors here, from positive answers read in the sealing transaction or from the
// seal that transaction wrote.
//
// A leaf: the standard library only.
package releaseproof

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrNoProof is a release refused because its proof is missing or names another host.
var ErrNoProof = errors.New("hostsource: a host is released only on proof it holds nothing or never joined")

// Proof is what a release requires.
type Proof struct {
	host     string
	at       time.Time
	verified bool
}

// Holdings is what the ledger says a host holds, read in the transaction that sealed it
// against placement. A count the ledger could not read, or could not trust, is negative.
type Holdings struct {
	Host string
	// Sealed is when the host was sealed against placement, zero if it is not.
	Sealed     time.Time
	Leases     int
	Quarantine int
	Custody    int
	// Inventory is how many instances the host is proved to run, never an inventory's
	// count alone, which is not proof (ADR-030): zero only when its node never registered,
	// or was decommissioned on the compute barrier's proof with nothing it said since
	// contradicting that; the instances an inventory of its current registration reports
	// when there are any; negative, could-not-tell, otherwise. The caller derives it.
	Inventory int
}

// NewDrained builds the proof a drained host is released on: sealed against placement,
// and holding no lease, no quarantine and no custody, and proved to run nothing (see
// Holdings.Inventory for what proves it). A count that could not be read refuses like a
// holding.
func NewDrained(h Holdings) (Proof, error) {
	counts := []int{h.Leases, h.Quarantine, h.Custody, h.Inventory}
	switch {
	case h.Host == "":
		return Proof{}, fmt.Errorf("%w: no host named", ErrNoProof)
	case h.Sealed.IsZero():
		return Proof{}, fmt.Errorf("%w: %s is not sealed against placement", ErrNoProof, h.Host)
	case slices.ContainsFunc(counts, func(n int) bool { return n < 0 }):
		return Proof{}, fmt.Errorf("%w: what %s holds could not be read", ErrNoProof, h.Host)
	case slices.ContainsFunc(counts, func(n int) bool { return n > 0 }):
		return Proof{}, fmt.Errorf("%w: %s holds %d leases, %d quarantined, %d in custody and %d instances",
			ErrNoProof, h.Host, h.Leases, h.Quarantine, h.Custody, h.Inventory)
	}

	return Proof{host: h.Host, at: h.Sealed, verified: true}, nil
}

// Unjoined is what the ledger says of a host whose node never enrolled, read in the
// transaction that sealed its order against enrollment and revoked its token.
type Unjoined struct {
	Host string
	// OrderSealed is when the order was sealed against enrollment, zero if it is not.
	OrderSealed time.Time
	// Admitted is how many enrollments were ever admitted for the order; negative if it
	// could not be read.
	Admitted int
}

// NewUnjoined builds the proof a host that never joined is released on: it never held
// anything a job could use, so it has no drain to prove.
func NewUnjoined(u Unjoined) (Proof, error) {
	switch {
	case u.Host == "":
		return Proof{}, fmt.Errorf("%w: no host named", ErrNoProof)
	case u.OrderSealed.IsZero():
		return Proof{}, fmt.Errorf("%w: %s's order is not sealed against enrollment", ErrNoProof, u.Host)
	case u.Admitted < 0:
		return Proof{}, fmt.Errorf("%w: whether %s ever joined could not be read", ErrNoProof, u.Host)
	case u.Admitted > 0:
		return Proof{}, fmt.Errorf("%w: %s joined, so it is released by drain", ErrNoProof, u.Host)
	}

	return Proof{host: u.Host, at: u.OrderSealed, verified: true}, nil
}

// Sealed is the proof of a host the ledger recorded sealed. A seal is written only in
// the transaction that built one of the proofs above, and nothing moves a sealed order
// back, so the seal stands for the proof after that transaction: a release asked again,
// after a failed attempt or a restart, is the same release. The ledger alone calls it,
// from the row it reads.
func Sealed(host string, sealed time.Time) (Proof, error) {
	switch {
	case host == "":
		return Proof{}, fmt.Errorf("%w: no host named", ErrNoProof)
	case sealed.IsZero():
		return Proof{}, fmt.Errorf("%w: %s is not sealed", ErrNoProof, host)
	}

	return Proof{host: host, at: sealed, verified: true}, nil
}

// Host is the host the proof is for.
func (p Proof) Host() string { return p.host }

// For reports whether the proof releases id; a source calls it first in Release.
func (p Proof) For(id string) error {
	if !p.verified || p.host == "" {
		return ErrNoProof
	}
	if p.host != id {
		return fmt.Errorf("%w: the proof is for %q, not %q", ErrNoProof, p.host, id)
	}

	return nil
}
