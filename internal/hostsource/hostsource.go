// Package hostsource is the contract a source of rented bare metal implements
// (ADR-030): order a host, describe it, prove it gone, release it, and say what its
// terms are. A pool on top of it grows and shrinks; a source only answers.
//
// BESIDE internal/provider, BELOW THE SCHEDULER. A provider starts one job's compute
// and a source rents the machine a node runs on. A source may use a provider's client
// (awsmetal uses the ec2 provider's, so there is one EC2 client), a provider never
// imports a source, and neither imports server, node or cmd (depguard: provider,
// hostsource). Lifting the EC2 client below both is owed with the pool.
//
// EVERY ANSWER THAT RELEASES ANYTHING HAS THREE VALUES, and its zero value is the one
// that holds. A source that cannot tell whether a host is gone says so, says gone only
// on its own terminal answer for that host, and a release is refused without a proof
// the pool built from the ledger in the transaction that sealed the host.
package hostsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/releaseproof"
)

// ErrNoStock means the source said it has no host of that shape where it was asked.
// The pool reports it and asks elsewhere or later; it never waits on the order.
var ErrNoStock = errors.New("hostsource: the source has no host of that shape there")

// ErrQuota means the source said the account may not hold another host of that shape.
var ErrQuota = errors.New("hostsource: the account's quota is reached")

// ErrRefused means the source refused the request for a reason an operator has to act
// on (a credential without the right to order, an account that may not order yet). It
// is an answer, not could-not-tell: the pool stops ordering from that source and says
// why, rather than reading the order back and placing it again.
var ErrRefused = errors.New("hostsource: the source refused the request")

// ErrNotSent means the order failed before anything was sent to the source: a local
// refusal, or a read the order depended on (a quota, a stock list) that failed. No
// host can exist for it, so it is not could-not-tell.
var ErrNotSent = errors.New("hostsource: the order was not sent")

// ErrNoProof is a release refused because its proof is missing or names another host.
var ErrNoProof = releaseproof.ErrNoProof

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
	// IdempotentFor is how long after an order's first attempt the same order sent
	// again cannot place a second host, because the source deduplicates it (EC2's
	// ClientToken). A source does not keep that record forever, so a pool may order
	// again after Find listed no host only inside this window from the first attempt it
	// recorded; past it, or on a source with none, an order whose answer was lost waits
	// for a person, because an order still in flight can land after a list that did not
	// show it. Zero, the zero value, is not idempotent.
	IdempotentFor time.Duration
	// Preemptible is a host the source may take back with warning. A pool never runs
	// jobs on one (ADR-030), and a source that offers only these is refused.
	Preemptible bool
}

// Source is one kind of rented bare metal.
type Source interface {
	// Name is the source's kind, for logs and the ledger: "aws-metal", "phoenixnap".
	Name() string
	// Scope is where this source orders (an AWS account, region, subnet and zone; a
	// phoenixNAP account), recorded with each order, so a lost order is read back and
	// sent again only where it was placed. An error is could-not-tell: nothing is
	// ordered, and no order is resolved, on it.
	Scope(ctx context.Context) (string, error)
	// Order asks for one host. ErrNoStock and ErrQuota are the source's own refusals;
	// any other error is could-not-tell, and the pool reads the order back by its
	// identifier before it orders again.
	Order(ctx context.Context, req Request) (Host, error)
	// Describe reads one host as the source sees it.
	Describe(ctx context.Context, id string) (Host, error)
	// Find reads back the host the source recorded an order on, so an order whose
	// answer was lost is found rather than placed twice. ok is false only when the
	// source listed none, which licenses ordering again only inside the source's
	// IdempotentFor window from the order's first attempt.
	Find(ctx context.Context, order string) (host Host, ok bool, err error)
	// Gone is the one answer that frees a host's row. It says gone only on the source's
	// own terminal answer for that host, never on not-found or on elapsed time.
	Gone(ctx context.Context, id string) (Presence, error)
	// Release asks the source to take the host back, on proof.
	Release(ctx context.Context, proof ReleaseProof) error
	// Terms says what a shape costs and how fast it comes.
	Terms(shape string) (Terms, error)
}

// ReleaseProof is what Release requires: the proof the ledger built in the transaction
// that sealed the host (internal/releaseproof, which the allocator can import and a
// source cannot be imported by). Its zero value proves nothing.
type ReleaseProof = releaseproof.Proof

// Holdings and Unjoined are what a proof is built from.
type (
	Holdings = releaseproof.Holdings
	Unjoined = releaseproof.Unjoined
)

// NewDrainedProof and NewUnjoinedProof are releaseproof's constructors.
var (
	NewDrainedProof  = releaseproof.NewDrained
	NewUnjoinedProof = releaseproof.NewUnjoined
)

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
