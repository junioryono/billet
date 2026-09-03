package wiring

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/wirecert"
)

// NodeWire is the assembled node wire: what the listener serves and the handler
// behind it.
//
// THE HANDLER RATHER THAN THE OPTIONS THAT BUILD IT, and that is the second
// version of this type. Returning options left the caller to install them, which
// is a step no test could see — remove the append in cmd/billet and every test
// here stayed green, which is the same untested seam one level up from where it
// was found. The only way a test covers a handoff is to be handed the same
// finished thing production is.
type NodeWire struct {
	// Handler is the node wire's routes, with every option this deployment's
	// authority and policy imply already installed.
	Handler http.Handler
	// Bootstrap is the two routes a machine that has never enrolled needs, on a
	// handler of its own because they cannot require the certificate everything
	// else does — served on their own listener and budget. Nil on a loopback
	// wire, which has no certificates and so nothing to enroll into.
	Bootstrap http.Handler
	// TLS is the server side of the wire: what the control plane PRESENTS, and
	// which authorities a client certificate may come from. Nil on a loopback
	// wire, which has no certificates at all because the trust boundary there is
	// the machine.
	TLS *tls.Config

	// THE THREE BELOW ARE FOR THE SECOND LISTENER, and they come from this same
	// read for the reason the read exists. The two routes a machine with no
	// certificate needs are served on an address of their own, and what THAT
	// listener presents and what it hands an enrolling machine to trust have to
	// describe the same moment as this one — a `billet ca retire` landing between
	// two reads would admit a node against an authority the control plane has
	// stopped presenting. All three are zero on a loopback wire, which has no
	// certificates and so nothing to enroll into.
	//
	// Serving is the certificate BOTH listeners present, and IssuingExpiry below
	// is what the authority behind them expires. The handlers already carry the
	// rest of that read.
	Serving wirecert.Bundle
	// Rotating reports an overlap, and RotationAge how long ago it started, so
	// the caller can say so.
	Rotating    bool
	RotationAge time.Duration
	// IssuingExpiry is when the authority that signs node certificates stops
	// working, and Hosts what its certificate was minted for. Reported at
	// startup, because a CA is a slow cliff.
	IssuingExpiry time.Time
}

// NodeWireRequest is what a deployment brings to its own node wire.
type NodeWireRequest struct {
	// StateDir and Deployment locate and name the authority.
	StateDir, Deployment string
	// Hosts are the names a node may dial this control plane by. Empty on a
	// loopback wire.
	Hosts []string
	// Loopback says the wire has no certificates at all: two processes on one
	// machine, where the trust boundary is the machine. Config validation refuses
	// node.tls against such a server for the same reason.
	Loopback bool

	// The collaborators the routes need.
	Log         *slog.Logger
	Plane       *nodeplane.Plane
	Leases      nodeplane.LeaseStore
	JIT         nodeplane.JITSource
	Revocations nodeplane.Revocations
	Enrollments nodeplane.Enrollments
	CachePolicy nodeplane.CachePolicy
}

// BuildNodeWire assembles the authenticated node wire from a deployment's
// authority.
//
// HERE RATHER THAN IN cmd/billet, AND THAT IS THE WHOLE POINT OF THE FILE. This
// was eleven lines inside serveNodeWire, which lives in a package excluded from
// coverage and takes six collaborators to stand up — so nothing asserted the
// SEAM, only the pieces either side of it. A mutant replacing the trust bundle
// with the issuing authority alone survived the entire suite, and during a
// rotation that refuses every node holding a certificate from the previous
// authority, which is the whole fleet until it has renewed.
//
// The three answers it has to get right, each of which was reachable only from
// here:
//
//   - What the server PRESENTS is signed by the authority the fleet still
//     trusts — the PREVIOUS one during an overlap. Presenting the new one makes
//     the control plane unverifiable to every node that has not renewed, over
//     the wire it would need in order to renew.
//   - What the server ACCEPTS is every authority in the bundle, so a node
//     holding either generation is recognised while the overlap runs.
//   - What renewal SIGNS with is the ISSUING authority — the new one — because
//     renewal is the only way the new authority reaches the fleet at all.
//
// In wiring rather than wirecert because nodeplane imports wirecert, so the
// dependency only points this way.
func BuildNodeWire(req NodeWireRequest) (NodeWire, error) {
	// THE CACHE KILL SWITCH IS ON BOTH WIRES, authenticated or not: a loopback
	// deployment can still be told to stop intercepting.
	opts := []nodeplane.HandlerOption{nodeplane.WithCachePolicy(req.CachePolicy)}

	wire := NodeWire{}

	if !req.Loopback {
		// ONE READ FOR ALL OF IT. What is presented, what is trusted and which
		// authority issues are three answers that have to describe one moment:
		// read separately, a `billet ca retire` landing between two of them left
		// a control plane presenting a certificate from the retired authority
		// while trusting only the new one — healthy-looking, and unverifiable to
		// every node.
		authority, err := wirecert.LoadServing(req.StateDir, req.Deployment)
		if err != nil {
			return NodeWire{}, err
		}

		bundle, err := authority.Presents.IssueServer(req.Hosts)
		if err != nil {
			return NodeWire{}, err
		}

		// AND EVERY AUTHORITY IS TRUSTED FOR CLIENTS, so a node holding either
		// an old or a new certificate is recognised while the overlap runs.
		bundle.CAPEM = authority.Trust

		if wire.TLS, err = wirecert.ServerTLS(bundle); err != nil {
			return NodeWire{}, fmt.Errorf("wiring: build the node wire's TLS config: %w", err)
		}

		opts = append(opts,
			nodeplane.RequireClientCert(),
			// A CREDENTIAL CAN BE TAKEN BACK, and it is checked on every request
			// rather than only at registration: a node holds one long poll open
			// for the better part of a minute and re-registers rarely.
			nodeplane.WithRevocations(req.Revocations),
			// AND RENEWED BEFORE IT EXPIRES, by the node itself. Without this a
			// fleet enrolled on one afternoon expires on one afternoon a year
			// later. The ISSUING authority signs it — the new one during an
			// overlap, which is how a rotation reaches the fleet at all.
			nodeplane.WithRenewal(authority.Issuing),
			nodeplane.WithTrustBundle(authority.Trust),
			// AND A WAY IN FOR A MACHINE THAT HAS NOTHING YET. Asking grants
			// nothing: the request waits until an operator compares its
			// fingerprint against what the node printed.
			nodeplane.WithEnrollment(req.Enrollments))

		// BOTH HANDLERS FROM THE ONE READ, which is the reason this function
		// exists said a second time. The enrollment routes moved to a listener of
		// their own so an anonymous caller cannot spend the fleet's connection
		// budget — and that gave the authority a SECOND consumer, assembled at a
		// second call site, where nothing could assert that the two agree. What
		// this listener presents and what it hands an enrolling machine to trust
		// have to describe the same moment as the wire above, or a `billet ca
		// retire` landing between two reads admits a node against an authority
		// the control plane has stopped presenting.
		wire.Bootstrap = nodeplane.BootstrapHandler(req.Log, req.Plane, authority.Issuing,
			nodeplane.WithTrustBundle(authority.Trust),
			nodeplane.WithEnrollment(req.Enrollments))

		wire.Serving = bundle
		wire.Rotating = authority.Rotating
		wire.RotationAge = authority.RotationAge
		wire.IssuingExpiry = authority.Issuing.NotAfter()
	}

	wire.Handler = nodeplane.Handler(req.Log, req.Plane, req.Leases, req.JIT, opts...)

	return wire, nil
}
