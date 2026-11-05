// Package hostsource is the contract a source of rented bare metal implements
// (ADR-025): order a host, describe it, prove it gone, release it, and say what its
// terms are. A pool on top of it grows and shrinks; a source only answers.
//
// A SIBLING OF internal/provider, BELOW THE SCHEDULER. A provider starts one job's
// compute and a source rents the machine a node runs on; neither imports the other,
// and neither imports server, node or cmd.
//
// EVERY ANSWER THAT RELEASES ANYTHING HAS THREE VALUES, and its zero value is the one
// that holds. A source that cannot tell whether a host is gone says so, and a release
// is refused without a proof the pool built from the node's own drained ending and a
// ledger holding no lease and no custody on the host.
package hostsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// ErrNoStock means the source said it has no host of that shape where it was asked.
// The pool reports it and asks elsewhere or later; it never waits on the order.
var ErrNoStock = errors.New("hostsource: the source has no host of that shape there")

// ErrQuota means the source said the account may not hold another host of that shape.
var ErrQuota = errors.New("hostsource: the account's quota is reached")

// ErrNoProof is a release refused because its proof is missing or names another host.
var ErrNoProof = errors.New("hostsource: a host is released only on proof it is drained and holds nothing")

// Request is one host the pool asks a source for.
type Request struct {
	// Order is the pool's own identifier for the order, which the source records on
	// the host where it can (a tag, a description) so a restarted pool finds it.
	Order string
	// Shape is the source's own name for the machine type.
	Shape string
	// Location is the source's own name for where, empty for the source's default.
	Location string
	// Arch is what the pool expects the shape to be, which the source refuses to
	// order when its catalogue says otherwise.
	Arch config.Arch
	// Boot is what the host runs on its first boot, carrying the order-bound join
	// token. It is a credential, so it is redacted wherever a Request is printed.
	Boot Secret
}

// State is what a source reports a host doing.
type State int

const (
	// StateUnknown is could-not-tell, and the zero value.
	StateUnknown State = iota
	// StateProvisioning is ordered and not yet running.
	StateProvisioning
	// StateRunning is running, which says nothing about its node.
	StateRunning
	// StateFailed is a host the source says will not come up.
	StateFailed
	// StateReleasing is a host the source is taking back.
	StateReleasing
)

func (s State) String() string {
	switch s {
	case StateProvisioning:
		return "provisioning"
	case StateRunning:
		return "running"
	case StateFailed:
		return "failed"
	case StateReleasing:
		return "releasing"
	case StateUnknown:
		return "unknown"
	}

	return fmt.Sprintf("state(%d)", int(s))
}

// Host is a source's description of one host.
type Host struct {
	// ID is the source's identifier, the one every later call names.
	ID string
	// Order is the pool's order the source recorded on the host, empty if it did not.
	Order    string
	Shape    string
	Location string
	State    State
	// PublicAddresses are what the source says the host answers on, which the join
	// compares with the address an enrollment came from where the source signs nothing.
	PublicAddresses []string
}

// Presence is the answer that releases capacity, so it has three values.
type Presence int

const (
	// PresenceUnknown is could-not-tell, and the zero value: nothing is released on it.
	PresenceUnknown Presence = iota
	// PresenceGone is proved gone.
	PresenceGone
	// PresencePresent is proved still there.
	PresencePresent
)

func (p Presence) String() string {
	switch p {
	case PresenceGone:
		return "gone"
	case PresencePresent:
		return "present"
	case PresenceUnknown:
		return "unknown"
	}

	return fmt.Sprintf("presence(%d)", int(p))
}

// Terms are what a source charges and how fast it answers, per shape.
type Terms struct {
	// LeadTime is how long an order takes to become a running host, as measured.
	LeadTime time.Duration
	// BillingUnit is the smallest period a host is billed for once ordered: a second,
	// an hour, a month.
	BillingUnit time.Duration
	// MinimumTerm is the shortest a host may be held, zero when there is none.
	MinimumTerm time.Duration
	// Preemptible is a host the source may take back with warning. A pool never runs
	// jobs on one (ADR-025), and a source that offers only these is refused.
	Preemptible bool
}

// Source is one kind of rented bare metal.
type Source interface {
	// Name is the source's kind, for logs and the ledger: "aws-metal", "phoenixnap".
	Name() string
	// Order asks for one host. ErrNoStock and ErrQuota are the source's own refusals;
	// any other error is could-not-tell, and the pool reads the order back by its
	// identifier before it orders again.
	Order(ctx context.Context, req Request) (Host, error)
	// Describe reads one host as the source sees it.
	Describe(ctx context.Context, id string) (Host, error)
	// Find reads back the host the source recorded an order on, so an order whose
	// answer was lost is found rather than placed twice. ok is false only when the
	// source proved there is none.
	Find(ctx context.Context, order string) (host Host, ok bool, err error)
	// Gone is the one answer that frees anything.
	Gone(ctx context.Context, id string) (Presence, error)
	// Release asks the source to take the host back, on proof.
	Release(ctx context.Context, proof ReleaseProof) error
	// Terms says what a shape costs and how fast it comes.
	Terms(shape string) (Terms, error)
}

// ReleaseProof is what Release requires. Its zero value proves nothing, and the only
// way to build one that does is NewReleaseProof, from positive answers.
type ReleaseProof struct {
	host     string
	drained  time.Time
	verified bool
}

// Drained says the node on a host ended drained, from its own ending record, and when.
type Drained struct {
	Host string
	At   time.Time
}

// NewReleaseProof builds a proof from what the pool read: the node's drained ending,
// and the ledger's count of leases and custody records still on the host. Anything
// but a drained ending for this host and two zero counts is refused, and so is a
// count the caller could not read, which it passes as negative.
func NewReleaseProof(host string, drained Drained, leases, custody int) (ReleaseProof, error) {
	switch {
	case host == "":
		return ReleaseProof{}, fmt.Errorf("%w: no host named", ErrNoProof)
	case drained.Host != host:
		return ReleaseProof{}, fmt.Errorf("%w: the drained ending is %q's, not %q's", ErrNoProof, drained.Host, host)
	case drained.At.IsZero():
		return ReleaseProof{}, fmt.Errorf("%w: %s has no drained ending", ErrNoProof, host)
	case leases < 0 || custody < 0:
		return ReleaseProof{}, fmt.Errorf("%w: what %s holds could not be read", ErrNoProof, host)
	case leases > 0 || custody > 0:
		return ReleaseProof{}, fmt.Errorf("%w: %s holds %d leases and %d custody records", ErrNoProof, host, leases, custody)
	}

	return ReleaseProof{host: host, drained: drained.At, verified: true}, nil
}

// Host is the host the proof is for.
func (p ReleaseProof) Host() string { return p.host }

// For reports whether the proof releases id; a source calls it first in Release.
func (p ReleaseProof) For(id string) error {
	if !p.verified || p.host == "" {
		return ErrNoProof
	}
	if p.host != id {
		return fmt.Errorf("%w: the proof is for %q, not %q", ErrNoProof, p.host, id)
	}

	return nil
}

// Secret is a first-boot payload or a credential: it reaches the source's API and
// nothing else, and every way of printing it prints that it is redacted.
type Secret struct{ value []byte }

// NewSecret wraps a value.
func NewSecret(v []byte) Secret { return Secret{value: append([]byte(nil), v...)} }

// Reveal is the value, for the one call that sends it.
func (s Secret) Reveal() []byte { return append([]byte(nil), s.value...) }

// Empty reports whether there is no value.
func (s Secret) Empty() bool { return len(s.value) == 0 }

func (s Secret) String() string   { return "[redacted]" }
func (s Secret) GoString() string { return s.String() }

// Format catches every verb, %x and %q included.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a secret out of anything that serialises it structurally.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// LogValue is what slog consults, which never asks fmt.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }
