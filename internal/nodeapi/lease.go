package nodeapi

import "github.com/junioryono/billet/internal/alloc"

// The lease half of the wire.
//
// internal/node.Runner already takes its ledger through a LeaseStore interface
// — Bind, Advance, Heartbeat, Release, Lease, LaunchedLeaseIDs — which is what
// makes a remote node possible without touching the runner at all. These types
// are that interface written down as messages.
//
// THE SERVER REMAINS THE ONLY WRITER. A node does not keep lease state of its
// own and cannot reconstruct any: every transition goes through here and is
// durable on the server before the node acts on it. That is deliberate and it is
// the same rule that #14 exists to enforce one layer up — a commitment that
// lives only in a process is a commitment lost when that process dies.

// BindRequest claims a lease for this node.
//
// NO PROVIDER FIELD, deliberately. Bind chooses the provider itself, from the
// lease's acceptable list and the node's REGISTERED backend — both of which the
// server already holds. Letting the node name one would create a second
// authority for a fact the ledger owns, which is the recurring defect shape in
// this codebase: the snapshot and the live value disagree, and the disagreement
// is silent.
type BindRequest struct {
	Epoch int64  `json:"epoch"`
	Node  string `json:"node"`
}

// AdvanceRequest moves a lease to a new phase.
//
// The phase is sent as its string form rather than an integer: an enum whose
// numbering is a wire contract is one insertion away from silently remapping
// every in-flight lease during a rolling upgrade.
type AdvanceRequest struct {
	Epoch int64  `json:"epoch"`
	Phase string `json:"phase"`
}

// HeartbeatRequest renews a lease.
type HeartbeatRequest struct {
	Epoch int64 `json:"epoch"`
}

// ReleaseRequest ends a lease with a terminal outcome.
type ReleaseRequest struct {
	Epoch   int64  `json:"epoch"`
	Outcome string `json:"outcome"`
}

// LeaseResponse carries a lease back to the node.
type LeaseResponse struct {
	Lease *alloc.Lease `json:"lease"`
}

// LaunchedResponse lists the leases this node is believed to have launched.
//
// A map rather than a list, matching LeaseStore's own shape, because every
// caller is asking "is this one mine?" and a list would be turned into this on
// arrival anyway.
type LaunchedResponse struct {
	LeaseIDs map[string]bool `json:"lease_ids"`
	// Quarantined are leases whose holder stopped heartbeating while compute may
	// still exist for them. Something IS waiting for these — the capacity is
	// still charged — so a node must not treat their instances as orphans.
	Quarantined map[string]bool `json:"quarantined,omitempty"`
}

// ReconcileRequest tells the control plane what this host is actually running.
//
// SENT EVERY SWEEP, not only at registration, because quarantine happens on the
// REAPER's clock rather than the node's. A control plane that restarts sees its
// nodes re-register within seconds — before the leases they were holding have
// expired — so the inventory that arrives with a registration almost never
// covers the quarantine that follows it. Without a recurring report, capacity
// for a container that finished during the outage is held until an operator
// intervenes.
type ReconcileRequest struct {
	// Instances are the lease ids this host is running right now. An empty list
	// from a host that could read its provider is meaningful; a host that could
	// not read it does not send this request at all.
	Instances []string `json:"instances"`
}

// ReconcileResponse reports what the control plane let go of.
type ReconcileResponse struct {
	Freed int `json:"freed"`
}

// ErrorResponse is how a refusal crosses the wire.
//
// Code is a STABLE STRING the node may branch on; Message is for a human. The
// separation matters because the node genuinely must distinguish some failures:
// a fenced lease means stop and let go, while a transport error means retry.
// Matching that out of prose is how a reworded message becomes an outage.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes the node branches on. Anything else is "unknown, retry".
const (
	// CodeFenced means another holder has the lease at a higher epoch. The node
	// must stop working on it and must NOT destroy the compute on this basis
	// alone — something else already owns it.
	CodeFenced = "fenced"
	// CodeNotFound means the lease is gone. Terminal for the node's purposes.
	CodeNotFound = "not_found"
	// CodeRefused means the request was understood and rejected — a placement
	// the server will not allow. Retrying it unchanged cannot help.
	CodeRefused = "refused"
	// CodeCustody means the control plane stopped waiting for this command and
	// handed the node custody of its lease. The node must adopt the compute it
	// started and keep the lease renewed; nothing else will.
	//
	// IT ANSWERS A SUCCESSFUL REPORT, which is what makes it easy to miss. The
	// launch worked, the node is telling the truth, and the answer is still "too
	// late, it is yours now" — because the plane already told the listener to stop
	// heartbeating, and something has to be holding the lease.
	CodeCustody = "custody"
	// CodeSuperseded means another process has registered as this node.
	//
	// TERMINAL FOR THE NODE THAT RECEIVES IT. Re-registering would take the name
	// back from whoever holds it now, and the two hosts would trade it forever
	// while the control plane's accounting followed neither. Two hosts sharing a
	// node name is a configuration mistake — a copied certificate bundle, or the
	// same name written into two files — and it is fixed by an operator, not by
	// retrying.
	CodeSuperseded = "superseded"
	// CodeUnauthenticated means the request carried no identity the control plane
	// would accept. DISTINCT FROM CodeRefused on purpose: refused is a verdict on
	// a node the plane knows, and the node stops. This one says the connection
	// itself was not proven, which a certificate that has expired or been
	// replaced produces — an operator fixes it, so the node must not treat it as
	// its own permanent defeat.
	CodeUnauthenticated = "unauthenticated"
	// CodeUnavailable means the control plane could not answer, not that it said
	// no. The node retries: this is a database it could not read or a dependency
	// that is down, and a node that treated it as a verdict would take itself out
	// of a fleet that is merely having a bad minute.
	CodeUnavailable = "unavailable"
	// CodeUnregistered means the server does not know this node. The node must
	// register again before anything else it says will be accepted; this is what
	// a node sees after the server restarts.
	CodeUnregistered = "unregistered"
)

// ParsePhase turns a wire phase into an alloc.Phase, refusing anything else.
//
// alloc.Phase is a string type, so an unrecognised value would flow through
// untouched and be caught later by validTransitions — which has no edge to it
// and therefore fails closed. That is the right OUTCOME reached for the wrong
// REASON: the operator gets "invalid transition to \"lauching\"" from deep in
// the ledger instead of "that is not a phase" from the boundary that could see
// it. Parsing here keeps a typo in one place and keeps the ledger's error about
// the ledger.
//
// The terminal phases are accepted because Release carries one, and refusing
// them here would mean two spellings of the same enum.
func ParsePhase(s string) (alloc.Phase, bool) {
	switch alloc.Phase(s) {
	case alloc.PhaseCapacity,
		alloc.PhaseAssigned,
		alloc.PhaseLaunching,
		alloc.PhaseOnline,
		alloc.PhaseBusy,
		alloc.PhaseDone,
		alloc.PhaseFailed:
		return alloc.Phase(s), true
	default:
		return "", false
	}
}
