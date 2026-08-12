package nodeplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/wirecert"
)

// JITSource mints runner registrations. Held by the control plane alone.
//
// The same shape internal/node.JITSource has, declared separately for the same
// reason LeaseStore is: the transport must not depend on the runtime it serves.
type JITSource interface {
	Describe(ctx context.Context, name, group string) (*JITSet, []string, error)
	JITConfig(ctx context.Context, scaleSetID int, runnerName, workFolder string) (JITRegistration, error)
}

// JITSet is a scale set, as the wire needs it.
type JITSet struct {
	ID   int
	Name string
}

// JITRegistration is a minted registration whose config is a credential.
type JITRegistration interface {
	Config() string
	RunnerName() string
}

// LeaseStore is the ledger, as the node wire needs it.
//
// Declared here rather than imported from internal/node so the transport does
// not depend on the runtime it serves — the two are on opposite sides of a
// process boundary. The shapes match because both describe the same allocator.
type LeaseStore interface {
	Bind(ctx context.Context, leaseID string, epoch int64, node string) error
	Advance(ctx context.Context, leaseID string, epoch int64, to alloc.Phase) error
	Heartbeat(ctx context.Context, leaseID string, epoch int64) error
	Release(ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase) error
	Lease(ctx context.Context, leaseID string) (*alloc.Lease, error)
	LaunchedLeaseIDs(ctx context.Context, node string) (map[string]bool, error)
	// QuarantinedLeaseIDs are leases holding capacity for compute nobody has
	// accounted for. A node needs them to tell an orphan from a job whose
	// listener died while it was still running.
	QuarantinedLeaseIDs(ctx context.Context, node string) (map[string]bool, error)
}

// maxBody bounds a request body.
//
// A node is authenticated but not trusted to be well-behaved — it may be an old
// build, or wedged. Without a limit one malformed content-length would let a
// single host exhaust the control plane's memory, and the control plane is the
// one process whose loss stops every job in the organisation.
const maxBody = 1 << 20

// HandlerOption configures the wire.
type HandlerOption func(*handler)

// RequireClientCert makes a verified certificate the source of a node's name.
//
// WITHOUT IT THE PATH IS THE ONLY AUTHORITY, which is not authentication: any
// process that can reach the listener claims to be any node, binds its leases,
// takes its commands, and asks for a JIT registration — a credential that registers
// a runner against the organisation. A wire served without this refuses to bind
// anywhere but loopback.
//
// With it the certificate decides, and a request whose path disagrees is rejected
// rather than reconciled.
func RequireClientCert() HandlerOption {
	return func(h *handler) { h.requireCert = true }
}

// Revocations answers whether a certificate has been withdrawn, and records the
// ones this wire hands out.
//
// AN ISSUE IS PART OF REVOCATION, not a separate concern. A credential billet
// never wrote down cannot be taken back: renewal mints a fresh key and serial
// over this wire, and without recording it the only serials an operator can name
// are the ones from bundles they issued by hand — which a node that has renewed
// is no longer presenting.
//
// An interface rather than the allocator, so the wire depends on the two
// questions it asks rather than on the ledger.
type Revocations interface {
	CertRevoked(ctx context.Context, serial string) (bool, error)
	RecordIssuedCert(ctx context.Context, cert alloc.IssuedCert) error

	// RecordRenewedCert records a renewal and refuses one whose parent has been
	// revoked since this request began, in one transaction. Two calls would let a
	// revocation commit between the check and the record and take back a
	// credential the machine had already stopped presenting.
	RecordRenewedCert(ctx context.Context, cert alloc.IssuedCert, parent string) error
}

// WithRevocations lets the wire refuse a credential an operator has taken back.
//
// Without it a certificate is good until it expires, which for a decommissioned
// machine or a leaked key means up to a year of a host that can still be handed
// work — including a JIT credential that registers a runner against the
// organisation.
func WithRevocations(r Revocations) HandlerOption {
	return func(h *handler) { h.revocations = r }
}

// Enrollments records machines asking to join and what was decided about them,
// and checks the credential that lets one ask at all.
type Enrollments interface {
	// RequestEnrollmentWithToken records the request AND spends the credential
	// that authorised it in one transaction. Two calls would let a crash between
	// them burn a single-use token with no request to show for it, stranding the
	// machine it was minted for.
	RequestEnrollmentWithToken(
		ctx context.Context, name, fingerprint, csrPEM, token string,
	) (alloc.Enrollment, error)
	LookupEnrollment(ctx context.Context, name string) (alloc.Enrollment, bool, error)
}

// WithEnrollment lets a machine ask to join without already holding a
// certificate.
//
// The alternative — and what billet did before — is that admission happens
// entirely out of band: an operator runs `billet ca issue` and copies a bundle
// to the machine. That works, and it is not discoverable: a node that is powered
// on and pointed at a control plane appears nowhere until somebody already knows
// it exists.
func WithEnrollment(e Enrollments) HandlerOption {
	return func(h *handler) { h.enrollments = e }
}

// WithRenewal lets a node replace its own certificate before it expires.
//
// AUTHENTICATED BY THE CERTIFICATE BEING REPLACED, so this grants nothing: a
// host that can already act as a node asks to keep doing so. What it prevents is
// the cliff — a fleet enrolled on one afternoon whose certificates all expire on
// the same day a year later, with no warning louder than a log line and no way
// back except re-enrolling every machine by hand.
func WithRenewal(ca *wirecert.CA) HandlerOption {
	return func(h *handler) { h.ca = ca }
}

// WithTrustBundle sets every authority a node should accept, which during a
// rotation is more than one.
func WithTrustBundle(pem []byte) HandlerOption {
	return func(h *handler) { h.trust = pem }
}

// Handler serves the node wire.
//
// Every route that acts for a node is wrapped in forNode, and that is deliberate
// rather than tidy: the enforcement is visible in the routing table, so a route
// added without it is missing something a reader can SEE, instead of missing a
// check buried in a handler nobody re-reads.
func Handler(log *slog.Logger, p *Plane, store LeaseStore, jit JITSource, opts ...HandlerOption) http.Handler {
	h := &handler{log: log, plane: p, store: store, jit: jit}

	for _, opt := range opts {
		opt(h)
	}

	mux := http.NewServeMux()

	// UNAUTHENTICATED, both of them, and deliberately outside forNode. A machine
	// that has never enrolled has no certificate to present, and a node deciding
	// whether to trust this control plane has to be able to read its authority
	// before it trusts anything it says.
	mux.HandleFunc("POST /v1/enroll", h.enroll)
	mux.HandleFunc("GET /v1/ca", h.certificateAuthority)

	mux.HandleFunc("POST /v1/register", h.register)
	// THREE CLASSES, and the distinction is what a request can REACH.
	//
	// forNewWork — poll, bind, and anything that enumerates the node — is the
	// current process's alone. Two hosts under one name cannot both be given work,
	// and a list of the node's leases is the first step of acting on them.
	//
	// forOwnLease — everything addressed to ONE lease, renewal included — is
	// permitted for a lease this process was given, or if it is the current one.
	//
	// forNode — the result route — stays open to any process holding the
	// certificate, because reporting is the handover itself: a superseded process
	// that cannot report is a container whose lease nobody ends up owning.
	mux.HandleFunc("POST /v1/nodes/{node}/poll", h.forNewWork(h.poll))
	mux.HandleFunc("POST /v1/nodes/{node}/result", h.forNode(h.result))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/bind", h.forNewWork(h.bind))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/advance", h.forOwnLease(h.advance))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/heartbeat", h.forOwnLease(h.heartbeat))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/release", h.forOwnLease(h.release))
	mux.HandleFunc("GET /v1/nodes/{node}/leases/{lease}", h.forOwnLease(h.lease))
	mux.HandleFunc("GET /v1/nodes/{node}/launched", h.forNewWork(h.launched))
	mux.HandleFunc("POST /v1/nodes/{node}/describe", h.forNewWork(h.describe))
	mux.HandleFunc("POST /v1/nodes/{node}/jit", h.forNode(h.jitConfig))
	mux.HandleFunc("POST /v1/nodes/{node}/renew", h.forNode(h.renew))

	return mux
}

type handler struct {
	log         *slog.Logger
	plane       *Plane
	store       LeaseStore
	jit         JITSource
	requireCert bool

	// revocations answers whether a credential has been withdrawn, ca signs
	// renewals and enrollments, and enrollments records who is asking to join.
	// All three are nil on a loopback wire, which has no certificates.
	revocations Revocations
	ca          *wirecert.CA
	trust       []byte
	enrollments Enrollments

	// sets caches the resolved scale set per tier.
	//
	// A JIT REQUEST IS ON THE PATH OF EVERY JOB, and resolving the set means two
	// GitHub calls — a runner-group lookup and a scale-set lookup. Making them per
	// job would double this control plane's API traffic against a rate limit it
	// shares with every listener, to answer a question whose answer changes only
	// when somebody edits the organisation.
	setsMu sync.Mutex
	sets   map[string]int
}

// forgetScaleSet drops a cached id so the next request resolves it again.
func (h *handler) forgetScaleSet(label string) {
	h.setsMu.Lock()
	defer h.setsMu.Unlock()

	delete(h.sets, label)
}

// scaleSetFor resolves a tier's scale set id, once.
func (h *handler) scaleSetFor(ctx context.Context, tier config.Tier) (int, error) {
	h.setsMu.Lock()
	id, cached := h.sets[tier.Label]
	h.setsMu.Unlock()

	if cached {
		return id, nil
	}

	set, _, err := h.jit.Describe(ctx, tier.Label, tier.RunnerGroup)
	if err != nil {
		return 0, err
	}

	if set == nil {
		return 0, fmt.Errorf("nodeplane: tier %q has no scale set in runner group %q",
			tier.Label, tier.RunnerGroup)
	}

	h.setsMu.Lock()

	if h.sets == nil {
		h.sets = make(map[string]int)
	}

	h.sets[tier.Label] = set.ID
	h.setsMu.Unlock()

	return set.ID, nil
}

// forNode admits a request only if the certificate agrees with the path.
//
// AFTER ROUTING, WHICH IS WHY IT IS NOT MIDDLEWARE AROUND THE MUX. The path
// variable does not exist until the mux has matched, so a wrapper outside it
// would read an empty node name and compare the certificate against nothing —
// passing every request, and looking exactly like a working check.
func (h *handler) forNode(next http.HandlerFunc) http.HandlerFunc {
	return h.guard(next, false)
}

// forOwnLease wraps every route that touches ONE named lease.
//
// A superseded process may finish what it was given and nothing else. Between an
// incarnation and its replacement the node name and the certificate are identical,
// so a permission keyed on either lets one host release capacity a running
// container is using.
//
// RENEWAL IS HERE TOO: "a heartbeat only extends a lease" is true of one heartbeat
// and false of a process that keeps sending them. Repeated renewal does not hold
// capacity slightly longer, it denies it indefinitely, and the reaper is what
// reclaims when the current process dies. Reads are here for the same reason — a
// lease id and its epoch are what a release needs.
func (h *handler) forOwnLease(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node := r.PathValue("node")

		if !h.authorise(w, r, node) {
			return
		}

		incarnation := r.Header.Get(nodeapi.HeaderIncarnation)

		// STILL EVIDENCE OF LIFE, for the process that currently owns the name. A
		// node whose command loop is wedged keeps heartbeating from its janitor,
		// and that must keep it in the fleet — the alternative is the plane
		// expiring a node whose every heartbeat proves it is there. Seen itself
		// ignores a superseded process, so a drain does not vouch for anybody.
		h.plane.Seen(node, incarnation)

		// OWNERSHIP IS THE AUTHORISATION HERE, not fleet membership — which is why
		// these handlers do not also demand registration. A draining process
		// outlives its replacement on purpose, and once the replacement goes silent
		// the node is forgotten; requiring membership refused the drain its own
		// lease at exactly the moment nothing else was renewing it.
		lease := r.PathValue("lease")

		if err := h.plane.MayMutateLease(node, incarnation, lease); err != nil {
			writeStoreErr(w, err)

			return
		}

		// AND THE LEDGER ANSWERS FOR A LEASE NOTHING HAS CLAIMED YET. The owners
		// map holds only what this process has DELIVERED, so held escrow — and
		// every lease in the window after a restart, before the fleet re-adopts —
		// is legitimately missing from it, and MayMutateLease has nothing left to
		// refuse with but fleet membership, which every registered node passes.
		if !h.plane.LeaseOwnerRecorded(lease) && !h.ledgerAgrees(w, r, node, lease) {
			return
		}

		next(w, r)
	}
}

// ledgerAgrees reports whether the ledger places this lease on this node,
// answering the request itself when it does not.
//
// COALESCE(node, target_node), the way the rest of the arithmetic attributes a
// lease: escrow chose the machine long before a bind fills `node` in, and that
// choice is what billet advertised against. A lease the ledger has not placed at
// all is allowed through — that is the recovery path, where a node re-adopts
// what it is already running.
func (h *handler) ledgerAgrees(w http.ResponseWriter, r *http.Request, node, leaseID string) bool {
	lease, err := h.store.Lease(r.Context(), leaseID)
	if err != nil {
		writeStoreErr(w, err)

		return false
	}

	// A LEDGER THAT SAYS NOTHING CONTRADICTS NOTHING. A store may answer with no
	// lease and no error, and refusing on that would turn a lookup miss into a
	// node losing the right to maintain compute it is running.
	if lease == nil {
		return true
	}

	placed := lease.Node
	if placed == "" {
		placed = lease.TargetNode
	}

	if placed == "" || placed == node {
		return true
	}

	writeStoreErr(w, fmt.Errorf(
		"%w: the ledger places lease %s on node %q and this request came from %q. A node may "+
			"change the fate only of work placed on it",
		ErrSuperseded, leaseID, placed, node))

	return false
}

// forNewWork wraps a route by which a node ACQUIRES work or capacity.
//
// SUPERSEDING A NODE MUST NOT SILENCE IT. A superseded host may be holding a
// container right now: it is refused new work, because two hosts under one name
// cannot both be given work, but NOT the calls that maintain and conclude what it
// already has.
//
// Fencing those is worse than not fencing at all. Registration tells the listener
// the node has custody, so the listener stops heartbeating; if the superseded
// process can then neither renew nor report, the lease expires while its container
// runs and the capacity is resold.
//
// Eligibility for NEW work and permission to maintain EXISTING work have different
// lifetimes.
func (h *handler) forNewWork(next http.HandlerFunc) http.HandlerFunc {
	return h.guard(next, true)
}

func (h *handler) guard(next http.HandlerFunc, fenceSuperseded bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node := r.PathValue("node")

		// WHO is settled here. A certificate proves the host is entitled to the
		// name; it cannot say whether this is still the process that registered
		// under it, because a bundle copied to a second machine authenticates
		// exactly as well as the original.
		if !h.authorise(w, r, node) {
			return
		}

		if !fenceSuperseded {
			next(w, r)

			return
		}

		if err := h.plane.CheckIncarnation(node, r.Header.Get(nodeapi.HeaderIncarnation)); err != nil {
			writeErr(w, http.StatusConflict, nodeapi.CodeSuperseded, err.Error())

			return
		}

		next(w, r)
	}
}

// authorise reports whether the connection may act for the node it claims.
func (h *handler) authorise(w http.ResponseWriter, r *http.Request, claimed string) bool {
	if !h.requireCert {
		return true
	}

	// THE LOAD-BEARING CHECK, not a formality. The listener verifies a
	// certificate IF one is given but does not require one, because an unenrolled
	// machine has none and still has to reach /v1/enroll and /v1/ca. So this is
	// what separates those two routes from every other: anything wrapped in a
	// guard refuses a connection with no verified chain.
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
			"this wire requires a client certificate issued by the deployment's authority")

		return false
	}

	name := r.TLS.PeerCertificates[0].Subject.CommonName
	if name == "" {
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
			"the client certificate names no node, so there is nothing to act as")

		return false
	}

	if name != claimed {
		// NAMES BOTH, because the two ways to arrive here need different fixes: a
		// node dialling with the wrong bundle after a rename, and something on the
		// network trying to act as a host it is not.
		h.log.Warn("a connection tried to act for a node it is not",
			"certificate", name, "claimed", claimed, "path", r.URL.Path)

		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, fmt.Sprintf(
			"this connection is authenticated as node %q and cannot act for %q", name, claimed))

		return false
	}

	return h.notRevoked(w, r, name)
}

// notRevoked refuses a credential that has been withdrawn.
//
// ON EVERY REQUEST, not only at registration. A node holds one long poll open
// for the better part of a minute and re-registers rarely, so a check at
// registration alone would leave a revoked host working until it happened to
// restart — which for a decommissioned machine an operator has just taken out of
// service is exactly the case that matters.
//
// AND IT FAILS CLOSED. An unreadable revocation list refuses the request rather
// than assuming nothing is revoked, because the alternative makes a transient
// database fault equivalent to switching the check off.
func (h *handler) notRevoked(w http.ResponseWriter, r *http.Request, name string) bool {
	if h.revocations == nil {
		return true
	}

	serial := wirecert.Serial(r.TLS.PeerCertificates[0])

	revoked, err := h.revocations.CertRevoked(r.Context(), serial)
	if err != nil {
		h.log.Error("could not check whether a node certificate has been revoked; refusing "+
			"the request rather than assuming it is good",
			"node", name, "serial", serial, "error", err)

		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable,
			"billet cannot reach its revocation list, so it cannot admit this connection")

		return false
	}

	if revoked {
		h.log.Warn("a node connected with a certificate that has been revoked",
			"node", name, "serial", serial, "path", r.URL.Path)

		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
			"this certificate has been revoked; ask an operator to issue a new one with "+
				"`billet ca issue`")

		return false
	}

	return true
}

// warnIfExpiring complains about a certificate while the node still works.
//
// The alternative is finding out on the day it stops: an expired node
// certificate is a TLS failure at dial time, so the node simply disappears from
// the fleet and the control plane has no way left to tell anyone why.
func (h *handler) warnIfExpiring(r *http.Request, node string) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return
	}

	if left, soon := wirecert.ExpiresSoon(r.TLS.PeerCertificates[0]); soon {
		h.log.Warn("a node certificate is close to expiry; re-issue it before it stops working",
			"node", node, "expires_in", left.Round(time.Hour),
			"not_after", r.TLS.PeerCertificates[0].NotAfter)
	}
}

// certificateAuthority serves the authority a node verifies this control plane
// against.
//
// PUBLIC BY CONSTRUCTION. A CA certificate is presented in every handshake and
// already sits on every enrolled node; serving it reveals nothing. The security
// is entirely in what the NODE does with it: compare the fingerprint against a
// value an operator read off the server and gave it out of band. A node that
// skips that comparison has trusted whatever answered, which is why the client
// refuses to enroll without one.
func (h *handler) certificateAuthority(w http.ResponseWriter, _ *http.Request) {
	if h.ca == nil {
		writeErr(w, http.StatusNotFound, nodeapi.CodeRefused,
			"this control plane has no certificate authority; it serves a loopback wire")

		return
	}

	writeJSON(w, http.StatusOK, nodeapi.CAResponse{
		CAPEM:       string(h.trustBundle()),
		Fingerprint: h.ca.Fingerprint(),
		Deployment:  h.plane.deployment,
	})
}

// enroll records a machine asking to join, and hands back a certificate once an
// operator has approved it.
//
// ASKING GRANTS NOTHING. The request sits as `pending` until an operator
// compares the fingerprint it reports against the one the node printed on its
// own console and approves that exact value. Until then this returns "pending"
// and the node keeps asking.
//
// A NAME IS CLAIMED BY THE FIRST KEY TO ASK. A second key wanting the same name
// is refused rather than replacing it, because overwriting would mean an
// operator who compared a fingerprint yesterday is approving a different machine
// today under a name they already trust.
func (h *handler) enroll(w http.ResponseWriter, r *http.Request) {
	if h.ca == nil || h.enrollments == nil {
		writeErr(w, http.StatusNotFound, nodeapi.CodeRefused,
			"this control plane does not enroll nodes; it serves a loopback wire, where there "+
				"are no certificates")

		return
	}

	var req nodeapi.EnrollRequest
	if !decode(w, r, &req) {
		return
	}

	if err := config.ValidateNodeName("node", req.Node); err != nil {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused, err.Error())

		return
	}

	fingerprint, err := wirecert.FingerprintOfCSR([]byte(req.CSRPEM))
	if err != nil {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused, err.Error())

		return
	}

	// THE REQUEST AND THE TOKEN IN ONE CALL, because they have to commit
	// together. The token is spent only for a request that is NEW: a node polls
	// this endpoint until a human decides, so charging every call would spend a
	// single-use token on the second poll.
	enrollment, err := h.enrollments.RequestEnrollmentWithToken(
		r.Context(), req.Node, fingerprint, req.CSRPEM, req.JoinToken)
	if err != nil {
		if errors.Is(err, alloc.ErrBadJoinToken) {
			h.log.Warn("an enrollment was attempted without a usable join token",
				"node", req.Node, "fingerprint", fingerprint)

			writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
				"enrolling needs a join token: run `billet ca token` on the control plane and "+
					"pass it as --join-token")

			return
		}

		if errors.Is(err, alloc.ErrEnrollmentConflict) {
			h.log.Warn("a second key asked to join under a name that is already claimed",
				"node", req.Node, "fingerprint", fingerprint)

			writeErr(w, http.StatusConflict, nodeapi.CodeRefused, err.Error())

			return
		}

		h.log.Error("could not record an enrollment request", "node", req.Node, "error", err)

		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable,
			"billet could not record this request; try again")

		return
	}

	res := nodeapi.EnrollResponse{State: enrollment.State, Fingerprint: enrollment.Fingerprint}

	if enrollment.State == alloc.EnrollApproved {
		res.CertPEM = enrollment.CertPEM
		res.CAPEM = string(h.trustBundle())
	}

	if enrollment.State == alloc.EnrollPending {
		h.log.Info("a node is asking to join and is waiting for approval",
			"node", req.Node, "fingerprint", fingerprint)
	}

	writeJSON(w, http.StatusOK, res)
}

// trustBundle is every authority a node should accept.
//
// Set by the caller, because only it knows where the state directory is. Falls
// back to the issuing authority alone, which is the ordinary case: no rotation
// is running and there is nothing else to trust.
func (h *handler) trustBundle() []byte {
	if len(h.trust) > 0 {
		return h.trust
	}

	return h.ca.CertPEM()
}

// renew signs a new certificate for a node that already has a valid one.
//
// THE AUTHENTICATED NAME IS THE SUBJECT, never the one in the request. A CSR's
// subject is whatever the requester typed; the wire has already proved who this
// is. Signing the CSR's own name would let any node with a valid certificate
// mint one for any other name — every node able to impersonate every other,
// through the endpoint meant to keep them working.
//
// A REVOKED CERTIFICATE CANNOT RENEW, which forNode has already settled by the
// time this runs: revocation is checked on every authenticated request, so a
// withdrawn credential cannot extend itself.
func (h *handler) renew(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")

	if h.ca == nil {
		writeErr(w, http.StatusNotImplemented, nodeapi.CodeRefused,
			"this control plane does not sign renewals; it serves a loopback wire, where "+
				"there are no certificates")

		return
	}

	var req nodeapi.RenewRequest
	if !decode(w, r, &req) {
		return
	}

	bundle, err := h.ca.SignNodeCSR(node, []byte(req.CSRPEM))
	if err != nil {
		h.log.Warn("could not sign a node's renewal request", "node", node, "error", err)

		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused,
			fmt.Sprintf("this certificate request cannot be signed: %v", err))

		return
	}

	// RECORDED BEFORE IT IS HANDED OVER, because the alternative is a live
	// credential the control plane cannot name. A failure here refuses the
	// renewal: the node keeps the certificate it already has — which is recorded,
	// revocable, and good for months — and tries again on its next pass.
	if h.revocations != nil {
		leaf, leafErr := wirecert.LeafOf(bundle)
		if leafErr != nil {
			h.log.Error("could not read a renewed certificate back", "node", node, "error", leafErr)

			writeErr(w, http.StatusInternalServerError, nodeapi.CodeUnavailable,
				"billet could not read the certificate it just signed; the node keeps the one "+
					"it has")

			return
		}

		// AGAINST THE CERTIFICATE THAT ASKED, so a revocation racing this request
		// wins. notRevoked checked at the start of the request and this signs some
		// milliseconds later; without naming the parent here, a revocation
		// committing in between takes back a credential the machine has already
		// stopped presenting and reports success.
		parent := wirecert.Serial(r.TLS.PeerCertificates[0])

		recErr := h.revocations.RecordRenewedCert(r.Context(), alloc.IssuedCert{
			Serial:   wirecert.Serial(leaf),
			Node:     node,
			Source:   alloc.CertRenewed,
			NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339),
		}, parent)

		if errors.Is(recErr, alloc.ErrParentRevoked) {
			h.log.Warn("a revoked certificate tried to renew itself", "node", node)

			writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
				"this certificate has been revoked and cannot renew")

			return
		}

		if recErr != nil {
			h.log.Error("could not record a renewed certificate; refusing the renewal so the "+
				"credential stays one an operator can revoke", "node", node, "error", recErr)

			writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable,
				"billet could not record this renewal; the node keeps the certificate it has")

			return
		}
	}

	h.log.Info("renewed a node certificate", "node", node)

	// THE BUNDLE, NOT ONE CERTIFICATE. A renewal during a rotation is how the new
	// authority reaches a node at all: it adopts what this carries, so carrying
	// only the issuing one would leave it trusting nothing else and it would stop
	// the moment the old authority is retired.
	writeJSON(w, http.StatusOK, nodeapi.RenewResponse{
		CertPEM: string(bundle.CertPEM),
		CAPEM:   string(h.trustBundle()),
	})
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.RegisterRequest
	if !decode(w, r, &req) {
		return
	}

	// REGISTER NAMES ITSELF IN THE BODY, not the path, so it cannot use forNode.
	// It is also the request that matters most: it is what puts a node in the
	// fleet, and everything downstream trusts that membership.
	if !h.authorise(w, r, req.Node) {
		return
	}

	h.warnIfExpiring(r, req.Node)

	// READ BEFORE COMMITTING, because Register is not a question — it supersedes
	// whatever process held the name, resolves its in-flight commands, and makes
	// the new one current. Doing the ledger read afterwards and answering 503
	// produced the worst of both: the caller believed it had failed and backed
	// off, while the plane had already replaced a healthy process with one that
	// never considered itself registered.
	ids, err := h.store.LaunchedLeaseIDs(r.Context(), req.Node)
	if err != nil {
		h.log.Warn("could not read which leases this node already holds; refusing the "+
			"registration rather than accepting a node whose existing compute the plane "+
			"cannot attribute", "node", req.Node, "error", err)

		writeErr(w, http.StatusServiceUnavailable, "", fmt.Sprintf(
			"could not read the leases already placed on %s: %v", req.Node, err))

		return
	}

	res, err := h.plane.Register(r.Context(), req)
	if err != nil {
		// TWO KINDS OF NO, and conflating them was the bug. A verdict — wrong
		// protocol version, foreign deployment — cannot be fixed by retrying, and
		// a node that retried forever against it is a node nobody notices is
		// broken. An OUTAGE, such as a ledger that cannot write, heals; telling
		// that node to give up leaves it down after the database comes back.
		if errors.Is(err, ErrRefused) {
			writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, err.Error())

			return
		}

		writeErr(w, http.StatusServiceUnavailable, "", err.Error())

		return
	}

	// OWNERSHIP IS REBUILT FROM WHAT WAS READ ABOVE, because the plane's copy does
	// not survive a restart and a superseded process cannot finish without it: a
	// node holding compute, a plane that restarts, a re-registration, and then a
	// second host — the new plane never saw those launches, so it would refuse the
	// draining process its own release.
	open := make([]string, 0, len(ids))
	for id := range ids {
		open = append(open, id)
	}

	h.plane.AdoptOwnership(req.Node, req.Incarnation, open)

	h.log.Info("node registered",
		"node", req.Node, "provider", req.Provider, "guest_os", req.GuestOS)

	writeJSON(w, http.StatusOK, res)
}

func (h *handler) poll(w http.ResponseWriter, r *http.Request) {
	cmd, ok, err := h.plane.Poll(r.Context(), r.PathValue("node"),
		r.Header.Get(nodeapi.HeaderIncarnation))
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	if !ok {
		// 204 IS THE IDLE ANSWER, matching the scale-set API's own 202-for-nothing
		// convention that billet already speaks to GitHub. A node treats it as
		// "poll again", not as a failure.
		w.WriteHeader(http.StatusNoContent)

		return
	}

	writeJSON(w, http.StatusOK, cmd)
}

func (h *handler) result(w http.ResponseWriter, r *http.Request) {
	var res nodeapi.CommandResult
	if !decode(w, r, &res) {
		return
	}

	if err := h.plane.Result(r.PathValue("node"),
		r.Header.Get(nodeapi.HeaderIncarnation), res); err != nil {
		writeStoreErr(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) bind(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.BindRequest
	if !decode(w, r, &req) {
		return
	}

	// THE PATH DECIDES WHICH NODE, NOT THE BODY. They can disagree, and a node
	// that binds a lease under another node's name would place compute where the
	// ledger does not expect it. The body's field exists for readability of a
	// captured request; it is not consulted.
	node := r.PathValue("node")

	if err := h.requireRegistered(node); err != nil {
		writeStoreErr(w, err)

		return
	}

	if err := h.store.Bind(r.Context(), r.PathValue("lease"), req.Epoch, node); err != nil {
		writeStoreErr(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) advance(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.AdvanceRequest
	if !decode(w, r, &req) {
		return
	}

	phase, ok := nodeapi.ParsePhase(req.Phase)
	if !ok {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused,
			fmt.Sprintf("%q is not a phase", req.Phase))

		return
	}

	// NO MEMBERSHIP CHECK: forOwnLease has already established that this process
	// owns the lease, and a draining process outlives the node record on purpose.

	if err := h.store.Advance(r.Context(), r.PathValue("lease"), req.Epoch, phase); err != nil {
		writeStoreErr(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.HeartbeatRequest
	if !decode(w, r, &req) {
		return
	}

	// NO MEMBERSHIP CHECK: forOwnLease has already established that this process
	// owns the lease, and a draining process outlives the node record on purpose.

	if err := h.store.Heartbeat(r.Context(), r.PathValue("lease"), req.Epoch); err != nil {
		writeStoreErr(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) release(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.ReleaseRequest
	if !decode(w, r, &req) {
		return
	}

	outcome, ok := nodeapi.ParsePhase(req.Outcome)
	if !ok {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused,
			fmt.Sprintf("%q is not a phase", req.Outcome))

		return
	}

	// NO MEMBERSHIP CHECK: forOwnLease has already established that this process
	// owns the lease, and a draining process outlives the node record on purpose.

	if err := h.store.Release(r.Context(), r.PathValue("lease"), req.Epoch, outcome); err != nil {
		// TERMINAL FOR THE NODE IS TERMINAL FOR THE RECORD. A lease that is gone or
		// fenced will never be released successfully — the node stops holding it on
		// exactly these answers — so returning without dropping the ownership left
		// an entry no later event could ever remove.
		if errors.Is(err, alloc.ErrLeaseNotFound) || errors.Is(err, alloc.ErrFenced) {
			h.plane.ForgetLease(r.PathValue("node"), r.PathValue("lease"))
		}

		writeStoreErr(w, err)

		return
	}

	// THE OWNERSHIP RECORD ENDS WITH THE LEASE. Kept only while somebody might
	// still need to prove they hold it.
	h.plane.ForgetLease(r.PathValue("node"), r.PathValue("lease"))

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) lease(w http.ResponseWriter, r *http.Request) {
	// NO MEMBERSHIP CHECK: forOwnLease has already established that this process
	// owns the lease, and a draining process outlives the node record on purpose.

	lease, err := h.store.Lease(r.Context(), r.PathValue("lease"))
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	writeJSON(w, http.StatusOK, nodeapi.LeaseResponse{Lease: lease})
}

func (h *handler) launched(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")

	if err := h.requireRegistered(node); err != nil {
		writeStoreErr(w, err)

		return
	}

	ids, err := h.store.LaunchedLeaseIDs(r.Context(), node)
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	held, err := h.store.QuarantinedLeaseIDs(r.Context(), node)
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	writeJSON(w, http.StatusOK, nodeapi.LaunchedResponse{LeaseIDs: ids, Quarantined: held})
}

func (h *handler) describe(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.DescribeRequest
	if !decode(w, r, &req) {
		return
	}

	if err := h.registered(r); err != nil {
		writeStoreErr(w, err)

		return
	}

	if h.jit == nil {
		writeErr(w, http.StatusServiceUnavailable, "",
			"this control plane has no GitHub client, so it cannot describe scale sets")

		return
	}

	set, names, err := h.jit.Describe(r.Context(), req.Name, req.Group)
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	res := nodeapi.DescribeResponse{Names: names}

	// FOUND IS A FIELD BECAUSE ABSENCE IS NOT ID ZERO. A missing scale set and a
	// scale set numbered nothing are different answers, and the runner stops on
	// one and launches on the other.
	if set != nil {
		res.Found = true
		res.ID = set.ID
		res.Name = set.Name
	}

	writeJSON(w, http.StatusOK, res)
}

func (h *handler) jitConfig(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.JITRequest
	if !decode(w, r, &req) {
		return
	}

	if err := h.registered(r); err != nil {
		writeStoreErr(w, err)

		return
	}

	if h.jit == nil {
		writeErr(w, http.StatusServiceUnavailable, "",
			"this control plane has no GitHub client, so it cannot mint registrations")

		return
	}

	// BOUND TO A COMMAND, not merely to a registration. A JIT config registers a
	// runner against the organisation; a node that could ask for one whenever it
	// liked could start runners billet never escrowed capacity for, never
	// tracked, and never tears down.
	//
	// The runner name billet chooses carries the lease id, so the entitlement is
	// already in the request: the node may mint exactly the registration for the
	// launch it was told to perform, and nothing else.
	leaseID, named := provider.LeaseOf(req.RunnerName)

	// FOR THE MESSAGE, NOT FOR THE DECISION. A name billet did not assign yields
	// no lease id, and no in-flight launch matches an empty one, so the check
	// below refuses it either way. Saying which of the two things went wrong is
	// worth a branch: "that is not a name I assign" and "you were not given that
	// launch" send an operator to different places.
	if !named {
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, fmt.Sprintf(
			"runner name %q was not assigned by billet, so no command entitles this node to "+
				"a registration for it", req.RunnerName))

		return
	}

	tier, err := h.plane.EntitledToLaunch(r.PathValue("node"),
		r.Header.Get(nodeapi.HeaderIncarnation), leaseID)
	if err != nil {
		h.log.Warn("refused a runner registration a node was not entitled to",
			"node", r.PathValue("node"), "lease", leaseID, "scale_set", req.ScaleSetID)

		writeStoreErr(w, err)

		return
	}

	// AND THE SCALE SET IS PART OF THE ENTITLEMENT. Checking only the lease left a
	// substitution open: a node holding an ordinary launch for a low-privilege
	// tier could ask for a registration in ANOTHER set, and the runner it started
	// would join a tier with different labels, different jobs and possibly
	// different secrets. The set is resolved here, from the lease's own tier,
	// rather than taken from the request.
	known, ok := h.plane.tierFor(tier)
	if !ok {
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, fmt.Sprintf(
			"lease %s names tier %q, which is not in this control plane's catalogue", leaseID, tier))

		return
	}

	setID, err := h.scaleSetFor(r.Context(), known)
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	// A RECREATED SCALE SET MUST NOT WEDGE THE TIER. The cached id is a fact about
	// somebody else's system, and deleting and recreating a scale set changes it.
	// The node re-resolves on its own failure and arrives with the NEW id; a cache
	// that never re-checks refuses it against the old one forever, and every
	// launch for that tier fails until the control plane is restarted.
	//
	// So a mismatch is a reason to look again, once, before refusing.
	if setID != req.ScaleSetID {
		h.forgetScaleSet(known.Label)

		if setID, err = h.scaleSetFor(r.Context(), known); err != nil {
			writeStoreErr(w, err)

			return
		}
	}

	if setID != req.ScaleSetID {
		h.log.Warn("refused a runner registration for a scale set the launch does not name",
			"node", r.PathValue("node"), "lease", leaseID, "tier", tier,
			"asked_for", req.ScaleSetID, "belongs_to", setID)

		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, fmt.Sprintf(
			"lease %s belongs to tier %q, so a registration may be minted in that tier's "+
				"scale set and no other", leaseID, tier))

		return
	}

	reg, err := h.jit.JITConfig(r.Context(), req.ScaleSetID, req.RunnerName, req.WorkFolder)
	if err != nil {
		// NOT LOGGED WITH THE REQUEST. A failure to mint can carry the runner name
		// and the scale set, which are harmless, but this path is one edit away
		// from carrying the config itself into a log line that outlives the job.
		writeStoreErr(w, err)

		return
	}

	writeJSON(w, http.StatusOK, nodeapi.JITResponse{
		Config:     reg.Config(),
		RunnerName: reg.RunnerName(),
	})
}

// registered is requireRegistered for a request, carrying its incarnation so
// only the CURRENT process refreshes the node's scheduling liveness.
func (h *handler) registered(r *http.Request) error {
	return h.requireRegisteredAs(r.PathValue("node"), r.Header.Get(nodeapi.HeaderIncarnation))
}

// requireRegistered refuses lease work from a node the plane does not know.
//
// This is what a node meets after the CONTROL PLANE restarts: its commands and
// lease writes are rejected with a code that means "register again" rather than
// a generic failure it would retry forever without ever fixing.
func (h *handler) requireRegistered(node string) error {
	return h.requireRegisteredAs(node, "")
}

func (h *handler) requireRegisteredAs(node, incarnation string) error {
	// RECORDED BEFORE THE QUESTION IS ASKED, because asking it runs expiry. A
	// custody heartbeat arriving from a healthy janitor used to expire the very
	// node it came from — the node had not polled, and nothing else counted as
	// life — and then be refused as unregistered.
	h.plane.Seen(node, incarnation)

	for _, known := range h.plane.Nodes() {
		if known == node {
			return nil
		}
	}

	return fmt.Errorf("%w: %s", ErrUnregistered, node)
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))

	// UNKNOWN FIELDS ARE AN ERROR, which is the strict half of being liberal in
	// what you accept. A node from a newer build sending a field this server does
	// not understand is a version mismatch, and silently dropping it would let the
	// two sides disagree about what was asked for. Register's version check is
	// meant to catch that first; this is what happens when it did not.
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused, err.Error())

		return false
	}

	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// A WRITE THAT FAILS IS A CONNECTION THAT IS GONE. The status line is already
	// out, so there is no second way to report this and nothing the node could do
	// with it; the poll it was answering simply ends and it polls again.
	if err := json.NewEncoder(w).Encode(body); err != nil {
		_ = err
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, nodeapi.ErrorResponse{Code: code, Message: msg})
}

// writeStoreErr maps a ledger error onto a code the node can branch on.
//
// THE CODE IS THE CONTRACT, not the message. A node must tell a fenced lease
// (stop, something else owns this) from a transport failure (retry) from an
// unregistered node (register again), and matching that out of prose is how a
// reworded error becomes an outage.
func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSuperseded):
		writeErr(w, http.StatusConflict, nodeapi.CodeSuperseded, err.Error())
	case errors.Is(err, ErrNotEntitled):
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, err.Error())
	case errors.Is(err, ErrTakeCustody):
		// 409, NOT AN ERROR STATUS THE NODE WILL RETRY. The report was accepted;
		// what is being returned is an instruction about who now holds the lease.
		writeErr(w, http.StatusConflict, nodeapi.CodeCustody, err.Error())
	case errors.Is(err, ErrUnregistered):
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnregistered, err.Error())
	case errors.Is(err, alloc.ErrFenced):
		writeErr(w, http.StatusConflict, nodeapi.CodeFenced, err.Error())
	case errors.Is(err, alloc.ErrLeaseNotFound):
		writeErr(w, http.StatusNotFound, nodeapi.CodeNotFound, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The node hung up or the poll window closed. Nothing to say, and saying
		// it into a closed connection achieves nothing.
		return
	default:
		writeErr(w, http.StatusInternalServerError, "", err.Error())
	}
}

// LoopbackOnly reports whether an address is safe to serve the node wire on
// without TLS.
//
// config.LoopbackAddr answers the same question for validation, and the two must
// agree: whether a node needs a certificate is decided by whether the listener
// will ask for one.
//
// Until mTLS lands the node names itself in the path and nothing verifies that
// claim, so a listener reachable beyond this host would let anything on the
// network bind leases and take commands. Refusing is the only honest option; the
// alternative is a deployment that looks like it works and has no boundary at
// all.
func LoopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Not an address billet can reason about, so not one it will serve on.
		return false
	}

	// AN EMPTY HOST IS THE WILDCARD, NOT LOOPBACK, and the first version of this
	// function said the opposite. net.Listen("tcp", ":7717") binds every
	// interface, so treating ":7717" as safe served the unauthenticated wire to
	// the whole network while reporting that it had not — with a JIT credential
	// endpoint behind it. The test asserted that behaviour too, which pinned the
	// hole rather than catching it.
	if host == "" {
		return false
	}

	// The one hostname whose meaning is fixed by convention everywhere billet
	// runs. Anything else that might resolve to loopback is not trusted: what it
	// resolves to is not knowable here and can change under the process.
	if host == "localhost" {
		return true
	}

	// ASKED, not pattern-matched. 127.0.0.1, ::1 and the rest of 127/8 are all
	// loopback and a prefix test on the string got some of them by accident.
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
