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
	RemoveRunner(ctx context.Context, runnerID int64, runnerName string) error
	RecoverRunner(ctx context.Context, runnerName string) (JITRunnerRecovery, error)
}

type trustedRunnerGroupValidator interface {
	ValidateTrustedRunnerGroup(ctx context.Context, group string, wantWorkflows []string) error
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
	ID() int64
}

// JITRunnerRecovery reports whether an exact legacy registration is busy.
type JITRunnerRecovery struct {
	RunnerID int64
	Present  bool
	Busy     bool
}

type poolRunnerStore interface {
	RegisterPoolRunner(ctx context.Context, runner alloc.PoolRunner) error
	PoolRunnerByName(ctx context.Context, name string) (alloc.PoolRunner, error)
	PoolRunnerByLease(ctx context.Context, leaseID string) (alloc.PoolRunner, error)
	PreserveRecoveredBusyPoolRunner(ctx context.Context, runner alloc.PoolRunner) error
	RetireRecoveredPoolRunner(ctx context.Context, runner alloc.PoolRunner) (alloc.PoolRunner, error)
	RetirePoolRunner(ctx context.Context, leaseID string) error
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
	MarkFailure(ctx context.Context, leaseID string, epoch int64, reason string) error
	Resize(ctx context.Context, leaseID string, epoch int64, instanceType string,
		vcpu int, memory config.ByteSize) error
	Release(ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase) error
	Lease(ctx context.Context, leaseID string) (*alloc.Lease, error)
	// RecordCacheObservation writes what the node saw the cache do for a lease's
	// job, from alloc's closed vocabularies, fenced on the epoch.
	RecordCacheObservation(ctx context.Context, leaseID string, epoch int64,
		obs alloc.CacheObservation) error
	// MarkDeregistered records that a lease's GitHub runner registration has been
	// removed, so ActiveRunnerLeases stops counting it as a live runner. It is
	// monotonic and unfenced; deregistration is a fact about GitHub, not about who
	// holds the lease.
	MarkDeregistered(ctx context.Context, leaseID string) error
	LaunchedLeaseIDs(ctx context.Context, node string) (map[string]bool, error)
	// EndedLeaseNode is the host a lease's job was attributed to, from the
	// history that outlives the lease row; ErrLeaseNotFound when there is none.
	EndedLeaseNode(ctx context.Context, leaseID string) (string, error)
	// QuarantinedLeaseIDs are leases holding capacity for compute nobody has
	// accounted for. A node needs them to tell an orphan from a job whose
	// listener died while it was still running.
	QuarantinedLeaseIDs(ctx context.Context, node string) (map[string]bool, error)
}

// CachePolicy answers the kill switch for transparent Actions caching.
type CachePolicy interface {
	ActionsCacheAllowed(ctx context.Context, owner, repository string) (bool, error)
}

// maxBody bounds a request body.
//
// A node is authenticated but not trusted to be well-behaved — it may be an old
// build, or wedged. Without a limit one malformed content-length would let a
// single host exhaust the control plane's memory, and the control plane is the
// one process whose loss stops every job in the organisation.
const maxBody = 1 << 20

// maxEnrollBody bounds the ONE body a caller with no certificate can send.
//
// An ECDSA P-256 certificate request is well under a kilobyte, and the rest of
// the message is a node name and a join token, so maxBody is three orders of
// magnitude of slack on the only route a stranger reaches.
const maxEnrollBody = 16 << 10

// maxConcurrentEnrollments bounds how many enrollments are in flight at once.
//
// NOT ABOUT MEMORY. RequestEnrollmentWithToken begins IMMEDIATE, which takes
// SQLite's single writer connection -- the one the operational wire also needs --
// so the bootstrap listener's connection cap does not bound what this route can
// do to the rest of the control plane. alloc.plausibleEnrollment already keeps a
// caller with no usable token out of that transaction from the read-only pool;
// this bounds the ones that get past it.
//
// Refused rather than queued: waiting for a permit moves the queue instead of
// bounding it, and a node polling for approval is happy to be told to come back.
const maxConcurrentEnrollments = 8

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
	// CertRevokedFor answers by serial AND by the cutoff the node carries, so a
	// credential billet never recorded — one from before it tracked serials, or
	// an earlier certificate for a name that was issued twice — is still refused.
	CertRevokedFor(ctx context.Context, node, serial string, issuedAt time.Time) (bool, error)
	CertRevoked(ctx context.Context, serial string) (bool, error)
	RecordIssuedCert(ctx context.Context, cert alloc.IssuedCert) error

	// RecordRenewedCert records a renewal and refuses one whose parent has been
	// revoked since this request began, in one transaction. Two calls would let a
	// revocation commit between the check and the record and take back a
	// credential the machine had already stopped presenting.
	RecordRenewedCert(
		ctx context.Context, cert alloc.IssuedCert, parent string, parentIssuedAt time.Time,
	) error
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

// WithCachePolicy gives nodes the central interception kill switch.
func WithCachePolicy(policy CachePolicy) HandlerOption {
	return func(h *handler) { h.cachePolicy = policy }
}

// WithTargetJIT gives the handler one credential-holding source per GitHub
// target, keyed by the target's config name.
//
// A REGISTRATION IS MINTED WITH THE CREDENTIAL OF THE TIER'S TARGET, and the
// tier is what every route here already knows — from the lease it acts for, or
// the label it is asked about — so the target is resolved from the catalogue
// rather than taken from the request. With this set, the constructor's source
// serves only tiers the catalogue does not know; a tier whose target is not
// among these is refused rather than minted through some other owner's App.
func WithTargetJIT(sources map[string]JITSource) HandlerOption {
	return func(h *handler) { h.targets = sources }
}

// Handler serves the OPERATIONAL node wire: every route that acts for a node,
// and not one that a machine without a certificate can use.
//
// THERE IS NO UNAUTHENTICATED ROUTE HERE, and that absence is what lets the
// listener in front of this demand a certificate in the handshake
// (wirecert.ServerTLS). /v1/ca and /v1/enroll used to be registered below; they
// are BootstrapHandler's now, on a listener of their own, because a connection
// budget shared with callers who need not prove anything is a budget an anonymous
// caller can take from the fleet.
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

	// NOT METHOD-QUALIFIED, WHICH IS DELIBERATE AND IS THE ONLY ROUTE LIKE IT.
	//
	// `POST /v1/register` lets ServeMux answer any other method with its own 405
	// before this handler runs — and a registration is the one request whose mere
	// ARRIVAL is safety-relevant, because it contradicts whatever that host last
	// proved to a compute barrier. A caller holding a node certificate that
	// reaches this path has arrived, whatever verb it used, so the handler takes
	// the route and refuses the method itself.
	mux.HandleFunc("/v1/register", h.register)
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
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/failure", h.forOwnLease(h.markFailure))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/resize", h.forOwnLease(h.resize))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/cache", h.forOwnLease(h.cacheObservation))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/release", h.forOwnLease(h.release))
	mux.HandleFunc("GET /v1/nodes/{node}/leases/{lease}", h.forOwnLease(h.lease))
	mux.HandleFunc("GET /v1/nodes/{node}/launched", h.forNewWork(h.launched))
	mux.HandleFunc("POST /v1/nodes/{node}/describe", h.forNewWork(h.describe))
	mux.HandleFunc("POST /v1/nodes/{node}/trusted-runner-group", h.forNewWork(h.validateTrustedRunnerGroup))
	mux.HandleFunc("POST /v1/nodes/{node}/reconcile", h.forNewWork(h.reconcile))
	// forNewWork, BECAUSE A WITHDRAWAL IS ABOUT ELIGIBILITY FOR NEW WORK: it is the
	// current process's statement that the host is leaving, and a superseded
	// process making it would take its replacement out of the fleet.
	mux.HandleFunc("POST /v1/nodes/{node}/withdraw", h.forNewWork(h.withdraw))
	mux.HandleFunc("POST /v1/nodes/{node}/jit", h.forNode(h.jitConfig))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/runner/remove", h.forOwnOrEndedLease(h.removeRunner))
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/runner/recover", h.forOwnLease(h.recoverRunner))
	mux.HandleFunc("POST /v1/nodes/{node}/renew", h.forNode(h.renew))
	mux.HandleFunc("GET /v1/nodes/{node}/cache-policy", h.forNode(h.actionsCachePolicy))

	return mux
}

// BootstrapHandler serves the two routes a machine that has never enrolled needs,
// and nothing else.
//
// IT IS A SEPARATE HANDLER BECAUSE IT NEEDS A SEPARATE LISTENER. Neither route
// can require a certificate — a node deciding whether to trust this control plane
// must be able to read its authority first, and a machine asking to join has
// nothing to present — so the listener in front of this one admits strangers. Put
// that on the wire nodes work over and the two share a connection budget, which
// is a fleet an anonymous caller can take offline at a few requests a second.
// Here the blast radius of saturating it is that enrollment waits.
//
// THE AUTHORITY IS A PARAMETER RATHER THAN AN OPTION. Without one, both routes
// can only answer 404, so a bootstrap handler that has none should not be
// constructible at all.
//
// Nothing here grants anything. Reading the authority reveals what every
// handshake already presents; asking to join needs a join token, records a
// pending request, and waits for an operator to compare the fingerprint the node
// printed on its own console.
func BootstrapHandler(log *slog.Logger, p *Plane, ca *wirecert.CA, opts ...HandlerOption) http.Handler {
	h := &handler{log: log, plane: p, enrollSlots: make(chan struct{}, maxConcurrentEnrollments)}

	for _, opt := range opts {
		opt(h)
	}

	// AFTER THE OPTIONS, so the authority this listener was built for wins over
	// any option that also sets one. WithRenewal exists for the operational wire
	// and would otherwise let a caller serve a different authority here than the
	// certificate this listener presents.
	h.ca = ca

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enroll", h.enroll)
	mux.HandleFunc("GET /v1/ca", h.certificateAuthority)

	return mux
}

type handler struct {
	log         *slog.Logger
	plane       *Plane
	store       LeaseStore
	jit         JITSource
	targets     map[string]JITSource
	requireCert bool

	// revocations answers whether a credential has been withdrawn, ca signs
	// renewals and enrollments, and enrollments records who is asking to join.
	// All three are nil on a loopback wire, which has no certificates.
	revocations Revocations
	ca          *wirecert.CA
	trust       []byte
	enrollments Enrollments
	cachePolicy CachePolicy

	// enrollSlots bounds concurrent enrollments. Non-nil only on a bootstrap
	// handler; a nil channel means the route is not served at all.
	enrollSlots chan struct{}

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

	src, err := h.jitFor(tier)
	if err != nil {
		return 0, err
	}

	set, _, err := src.Describe(ctx, tier.Label, tier.RunnerGroup)
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
	return h.guardLease(next, false)
}

// forOwnOrEndedLease is forOwnLease for the one route a superseded process must
// still be able to COMPLETE about a lease the ledger has ended: withdrawing the
// runner registration.
//
// ROUTING BEFORE COMPUTE HOLDS EVEN AFTER THE LEASE IS OVER. A lease can be
// settled by a replacement's inventory while the superseded process still
// runs the guest — two hosts under one name — and that process must remove
// the registration GitHub can still route work to before it destroys the
// guest. Answering "not found" here would let it read the ledger's absence as
// a removal it never performed; the handler performs the durable removal, or
// says there is no durable registration to withdraw.
func (h *handler) forOwnOrEndedLease(next http.HandlerFunc) http.HandlerFunc {
	return h.guardLease(next, true)
}

func (h *handler) guardLease(next http.HandlerFunc, admitEnded bool) http.HandlerFunc {
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

		byOwner, err := h.plane.AuthorizeLease(node, incarnation, lease)
		if err != nil {
			// A SUPERSEDED PROCESS ASKING ABOUT A LEASE THAT IS OVER GETS THE
			// LEDGER'S ANSWER, not the plane's. The ownership record ends with the
			// lease, so a draining process whose custody outlived its lease's
			// settlement — inventory freed it, an operator forced it — would
			// otherwise be refused as superseded on every renew and every release,
			// and a refusal it cannot act on is a drain that never ends. "Not found"
			// is what the store would have said, and it is what the node's custody
			// already treats as done. Anything the ledger still holds open keeps the
			// refusal, because only the process given the work may touch a lease
			// that is still charged.
			if errors.Is(err, ErrSuperseded) && h.leaseIsOver(r, lease) {
				if admitEnded && h.endedOnThisNode(r, lease, node) {
					next(w, r)

					return
				}

				writeStoreErr(w, fmt.Errorf("%w: lease %s is no longer open", alloc.ErrLeaseNotFound, lease))

				return
			}

			writeStoreErr(w, err)

			return
		}

		// AND THE LEDGER ANSWERS FOR A LEASE NOTHING HAS CLAIMED YET. The owners
		// map holds only what this process has DELIVERED, so held escrow — and
		// every lease in the window after a restart, before the fleet re-adopts —
		// is legitimately missing from it, and the plane has nothing left to
		// admit on but fleet membership, which every registered node passes.
		// DECIDED FROM THE SAME SNAPSHOT AS THE ADMISSION: asking the plane again
		// whether an owner is recorded let one recorded in between — a
		// replacement adopting the lease — skip the ledger's check for a lease
		// another host's compute was still on.
		//
		// A LEASE THE LEDGER HAS ENDED HAS NO PLACEMENT TO AGREE WITH, so the
		// route that must still complete about one — the registration removal —
		// is admitted here too, for a CURRENT process whose record a restart or
		// an inventory dropped. Refusing it as not-found left custody retrying a
		// removal forever, with a routable registration in front of a guest it
		// would never destroy.
		if !byOwner {
			// THE WINDOW A TEST STANDS IN: admitted on membership, the ledger
			// not yet asked.
			if h.plane.afterAuthorizeForTest != nil {
				h.plane.afterAuthorizeForTest(node, lease)
			}

			if admitEnded && h.leaseIsOver(r, lease) && h.endedOnThisNode(r, lease, node) {
				next(w, r)

				return
			}

			if !h.ledgerAgrees(w, r, node, lease) {
				return
			}
		}

		next(w, r)
	}
}

// leaseIsOver reports whether the ledger holds no open row for a lease.
//
// ONLY A DEFINITE ABSENCE COUNTS. A store that could not answer is not a store
// that said the lease is gone, so an error keeps the refusal the caller was
// about to write — the could-not-tell/no collapse this codebase keeps removing.
func (h *handler) leaseIsOver(r *http.Request, leaseID string) bool {
	_, err := h.store.Lease(r.Context(), leaseID)

	return errors.Is(err, alloc.ErrLeaseNotFound)
}

// endedOnThisNode reports whether an ended lease's job was attributed to this
// node, from the history row that outlives the lease.
//
// MEMBERSHIP IS NOT PERMISSION. A lease id is not secret, and a registered
// node that could name any ended lease would withdraw another host's runner
// registration through the one route that admits ended leases. Only a definite
// match admits; an unattributed job, a missing row, or a store that could not
// answer refuses.
func (h *handler) endedOnThisNode(r *http.Request, leaseID, node string) bool {
	placed, err := h.store.EndedLeaseNode(r.Context(), leaseID)

	return err == nil && placed != "" && placed == node
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
	name, ok := h.authenticated(w, r)
	if !ok {
		return false
	}

	return h.claims(w, r, name, claimed)
}

// authenticated settles everything the CONNECTION proves, and nothing about what
// the request asks for.
//
// SPLIT FROM THE NAME COMPARISON so /v1/register can run this BEFORE it reads a
// body. That route names itself in the body rather than the path, so it used to
// decode up to a megabyte of JSON from an unproven caller and only then ask who
// they were. Nothing about the certificate, the revocation list, or the empty-CN
// case needs the body, so none of it waited for one.
//
// The empty name it returns on a wire without certificates means "this wire does
// not authenticate", never "an anonymous caller"; claims is what reads it, and it
// re-asks requireCert rather than inferring from the name.
func (h *handler) authenticated(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !h.requireCert {
		return "", true
	}

	// THE LOAD-BEARING CHECK when this handler is served without TLS of its own —
	// a loopback wire, and every handler-level test. In production the listener
	// requires a certificate in the handshake (wirecert.ServerTLS), so a request
	// reaching here has already presented one; this is what makes the rule true
	// for any way Handler is built rather than only for the way it is deployed.
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
			"this wire requires a client certificate issued by the deployment's authority")

		return "", false
	}

	name := r.TLS.PeerCertificates[0].Subject.CommonName
	if name == "" {
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
			"the client certificate names no node, so there is nothing to act as")

		return "", false
	}

	if !h.notRevoked(w, r, name) {
		return "", false
	}

	return name, true
}

// claims reports whether an authenticated connection may act for the name the
// request asks about.
func (h *handler) claims(w http.ResponseWriter, r *http.Request, name, claimed string) bool {
	if !h.requireCert {
		return true
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

	return true
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

	leaf := r.TLS.PeerCertificates[0]
	serial := wirecert.Serial(leaf)

	// THE MOMENT IT WAS MINTED, not the moment it became valid. Certificates are
	// dated an hour early for clock skew, and reading NotBefore as the issuance
	// time would place every one of them before a cutoff set within the hour —
	// refusing the replacement a revocation is supposed to allow.
	revoked, err := h.revocations.CertRevokedFor(
		r.Context(), name, serial, leaf.NotBefore.Add(wirecert.ClockSkew))
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

	// THE PERMIT COMES BEFORE THE PARSE, because everything below it — a CSR
	// parse and a ledger transaction — is the work being bounded.
	select {
	case h.enrollSlots <- struct{}{}:
		defer func() { <-h.enrollSlots }()
	default:
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable,
			"this control plane is already handling as many enrollments as it will at once; "+
				"ask again in a moment")

		return
	}

	var req nodeapi.EnrollRequest
	if !decodeLimited(w, r, &req, maxEnrollBody) {
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
		presented := r.TLS.PeerCertificates[0]
		parent := wirecert.Serial(presented)

		recErr := h.revocations.RecordRenewedCert(r.Context(), alloc.IssuedCert{
			Serial:   wirecert.Serial(leaf),
			Node:     node,
			Source:   alloc.CertRenewed,
			NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339),
		}, parent, presented.NotBefore.Add(wirecert.ClockSkew))

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

// certificateName is the verified common name, WITHOUT the revocation check.
//
// SEPARATE FROM authenticated ON PURPOSE, and the three duplicated lines are the
// price of the distinction. Revocation decides whether a request may PROCEED; it
// does not decide whether the host arrived, and arriving is the only fact a
// compute barrier needs from it. Folding the two together is what left a revoked
// host's proof standing while that host was refused permanently and stopped
// retrying.
//
// Empty on a wire that requires no certificate, which is the loopback shape.
func (h *handler) certificateName(r *http.Request) string {
	if !h.requireCert {
		return ""
	}

	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}

	return r.TLS.PeerCertificates[0].Subject.CommonName
}

// discardProof throws away what a host had proved to a compute barrier, and
// reports whether the request may continue.
//
// A FAILURE IS AN OUTAGE, NOT A VERDICT: the same node with the same config
// succeeds once the ledger answers again, so it is answered 503 and must retry
// rather than stop.
func (h *handler) discardProof(w http.ResponseWriter, r *http.Request, node string) bool {
	if err := h.plane.ArrivingForRegistration(r.Context(), node); err != nil {
		h.log.Warn("could not discard what this host had proved to a compute barrier; "+
			"refusing the registration rather than letting that proof outlive the "+
			"arrival that contradicts it", "node", node, "error", err)

		writeErr(w, http.StatusServiceUnavailable, "", err.Error())

		return false
	}

	return true
}

// discardEveryProof throws away what EVERY host had proved, for an arrival this
// wire cannot attribute to one, and reports whether the request may continue.
func (h *handler) discardEveryProof(w http.ResponseWriter, r *http.Request) bool {
	if err := h.plane.ArrivalWasUnreadable(r.Context()); err != nil {
		h.log.Warn("could not discard the fleet's idle proofs for a registration this wire "+
			"cannot attribute; refusing it rather than letting those proofs outlive an "+
			"arrival that contradicts them", "error", err)

		writeErr(w, http.StatusServiceUnavailable, "", err.Error())

		return false
	}

	return true
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	// REGISTER NAMES ITSELF IN THE BODY, not the path, so it cannot use forNode.
	// It is also the request that matters most: it is what puts a node in the
	// fleet, and everything downstream trusts that membership.
	//
	// THE CONNECTION IS SETTLED BEFORE THE BODY IS READ. Only the name comparison
	// needs the body; the certificate, the chain and the revocation list do not,
	// and running all of it after the decode meant an unproven caller could make
	// this handler read up to maxBody first.
	//
	// WHAT THIS HOST HAD PROVED IS DISCARDED FIRST OF ALL — before the body is
	// read, and before the certificate is even AUTHORIZED.
	//
	// A host arriving to register is a host contradicting whatever it last proved
	// to a compute barrier, and a PERMANENTLY refused registration is one it does
	// not retry, so a proof left standing here stays standing. Every refusal ahead
	// of the semantic checks is one of those. `decode` is STRICT, so an unknown
	// field from a node rolled ahead of this control plane is answered CodeRefused
	// — the documented node-first-upgrade case. And `authenticated` checks
	// REVOCATION, which is an operator taking a machine's credentials back and
	// says nothing whatever about whether that machine is still running compute.
	//
	// So this reads the verified common name WITHOUT the revocation check, and
	// the authorization decision follows it. A revoked node that keeps arriving
	// can therefore keep its own host unprovable — which is the honest answer
	// rather than a denial of service: something is presenting a certificate this
	// deployment withdrew, and billet cannot prove that machine idle.
	//
	// A LOOPBACK WIRE HAS NO CERTIFICATE AT ALL, so nothing here can name the host
	// — and rather than waiting for the body to say, EVERY host's proof goes.
	// That is what closes the rest of the class in one place: the method check
	// below, the strict decoder, and `maxBody` all refuse before any body has been
	// read, all of them permanently, and each one is an arrival billet cannot
	// attribute. On loopback the fleet is one machine, so "every host" is that
	// host and the cost is a single barrier round.
	//
	// BOTH FORMS ANSWER 503 ON FAILURE, which is the reason they are ahead of
	// everything rather than tucked into the refusal paths: `decode` writes its
	// own 400 before returning, so an invalidation attempted after it could only
	// be LOGGED — and a node reads that 400 as permanent and stops retrying,
	// leaving a proof standing on the strength of a ledger blip.
	//
	// See Plane.ArrivingForRegistration for why this is exempt from the "a refusal
	// changes nothing" rule that keeps checkRegistration pure.
	if certName := h.certificateName(r); certName != "" {
		if !h.discardProof(w, r, certName) {
			return
		}
	} else if !h.discardEveryProof(w, r) {
		return
	}

	// THE METHOD IS REFUSED HERE RATHER THAN BY THE ROUTER, so the arrival above
	// is recorded first. See the route's own comment.
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "",
			"a registration is a POST; this wire does not answer "+r.Method+" here")

		return
	}

	name, ok := h.authenticated(w, r)
	if !ok {
		return
	}

	var req nodeapi.RegisterRequest
	if !decode(w, r, &req) {
		return
	}

	if !h.claims(w, r, name, req.Node) {
		return
	}

	h.warnIfExpiring(r, req.Node)

	// JUDGED BEFORE ANYTHING IS TOUCHED. beginRegistration below clears the
	// incumbent process's inventory, and only a SUCCESSFUL registration restores
	// it — so a permanently refused request that got that far left a live host's
	// inventory unknown until it registered again, and a completion cannot settle
	// a lease from absence without it. Every check here is pure.
	negotiated, nodeWire, err := h.plane.checkRegistration(req)
	if err != nil {
		if errors.Is(err, ErrRefused) {
			writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, err.Error())

			return
		}

		writeErr(w, http.StatusServiceUnavailable, "", err.Error())

		return
	}

	// INVALIDATE THE PRIOR INVENTORY BEFORE THE STORE READ. A replacement has
	// already reported its live instances in this request; letting completion
	// consume the old process's absence while this read or the epoch write blocks
	// would release capacity under compute the replacement is about to adopt.
	//
	intent := h.plane.beginRegistration(req.Node)
	defer h.plane.finishRegistration(req.Node, intent)

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

	res, err := h.plane.register(r.Context(), req, intent, negotiated, nodeWire)
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
	// Keep both durable launched leases and every instance the exact registering
	// process reported. The latter includes live quarantined compute, which the
	// launched query deliberately omits; dropping it here would leave a charged,
	// running lease with no owner and make completion recovery unable to address
	// its destroy.
	openSet := make(map[string]bool, len(ids)+len(req.Instances))
	for id := range ids {
		openSet[id] = true
	}
	if req.InventoryKnown {
		for _, id := range req.Instances {
			openSet[id] = true
		}
	}
	open := make([]string, 0, len(openSet))
	for id := range openSet {
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

func (h *handler) actionsCachePolicy(w http.ResponseWriter, r *http.Request) {
	if h.cachePolicy == nil {
		writeErr(w, http.StatusServiceUnavailable, "", "Actions cache policy is unavailable")

		return
	}
	owner := r.URL.Query().Get("owner")
	repository := r.URL.Query().Get("repository")
	allowed, err := h.cachePolicy.ActionsCacheAllowed(r.Context(), owner, repository)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "", err.Error())

		return
	}
	writeJSON(w, http.StatusOK, nodeapi.CachePolicyResponse{Allowed: allowed})
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

func (h *handler) markFailure(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.MarkFailureRequest
	if !decode(w, r, &req) {
		return
	}

	if err := h.store.MarkFailure(r.Context(), r.PathValue("lease"), req.Epoch, req.Reason); err != nil {
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

// cacheObservation records what the node saw the cache do for a lease's job.
//
// THE VOCABULARY IS CHECKED AT THE BOUNDARY, the way a phase is, so a token this
// control plane does not record is refused as such rather than surfacing from
// the ledger as a validation error the node would read as transient.
func (h *handler) cacheObservation(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.CacheObservationRequest
	if !decode(w, r, &req) {
		return
	}

	obs := alloc.CacheObservation{
		ImageCache:      alloc.ImageCache(req.ImageCache),
		CacheGeneration: req.CacheGeneration,
		ActionsCache:    alloc.ActionsCache(req.ActionsCache),
	}
	if err := obs.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused, err.Error())

		return
	}

	if err := h.store.RecordCacheObservation(r.Context(), r.PathValue("lease"), req.Epoch, obs); err != nil {
		writeStoreErr(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) resize(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.ResizeRequest
	if !decode(w, r, &req) {
		return
	}

	if err := h.store.Resize(r.Context(), r.PathValue("lease"), req.Epoch,
		req.InstanceType, req.VCPU, req.Memory); err != nil {
		writeStoreErr(w, err)

		return
	}

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

// reconcile frees capacity held for compute this host says it is not running.
func (h *handler) reconcile(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")

	var req nodeapi.ReconcileRequest
	if !decode(w, r, &req) {
		return
	}

	freed, err := h.plane.ReconcileInventory(
		r.Context(), node, r.Header.Get(nodeapi.HeaderIncarnation), req.Instances)
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	if freed > 0 {
		h.log.Info("freed capacity held for compute this host is no longer running",
			"node", node, "leases", freed)
	}

	writeJSON(w, http.StatusOK, nodeapi.ReconcileResponse{Freed: freed})
}

// withdraw takes a node out of placement because it said it will not poll again.
//
// THREE ANSWERS, AND THE NODE BRANCHES ON THEM. Superseded and unregistered are
// verdicts the node reads as "nothing of mine to withdraw" and stops; anything
// else is the ledger not answering, which is 503 so the node tries again rather
// than exiting with the host still placeable. See Plane.Withdraw.
func (h *handler) withdraw(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.WithdrawRequest
	if !decode(w, r, &req) {
		return
	}

	node := r.PathValue("node")

	err := h.plane.Withdraw(r.Context(), node, r.Header.Get(nodeapi.HeaderIncarnation))

	switch {
	case errors.Is(err, ErrSuperseded), errors.Is(err, ErrUnregistered):
		writeStoreErr(w, err)

		return

	case err != nil:
		h.log.Warn("could not record a node's withdrawal; it stays placeable until this "+
			"succeeds or the node goes silent", "node", node, "error", err)

		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, err.Error())

		return
	}

	w.WriteHeader(http.StatusNoContent)
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

	if !h.hasJIT() {
		writeErr(w, http.StatusServiceUnavailable, "",
			"this control plane has no GitHub client, so it cannot describe scale sets")

		return
	}

	src, err := h.jitForLabel(req.Name)
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	set, names, err := src.Describe(r.Context(), req.Name, req.Group)
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

	if !h.hasJIT() {
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

	// THE TIER'S OWN TARGET MINTS, resolved here beside the scale-set check so a
	// launch on one owner's tier can never be registered through another's App.
	src, err := h.jitFor(known)
	if err != nil {
		writeStoreErr(w, err)

		return
	}

	if known.Trust == config.WorkloadTrusted {
		validator, ok := src.(trustedRunnerGroupValidator)
		if !ok {
			writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
				"trusted runner-group policy cannot be verified before registration")
			return
		}
		if err := validator.ValidateTrustedRunnerGroup(r.Context(), known.RunnerGroup,
			known.Workflows); err != nil {
			h.log.Error("refused a trusted runner registration after runner-group policy drift",
				"node", r.PathValue("node"), "lease", leaseID, "tier", tier, "error", err)
			writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
				"trusted runner-group policy no longer matches billet.yaml")
			return
		}
	}

	reg, err := src.JITConfig(r.Context(), req.ScaleSetID, req.RunnerName, req.WorkFolder)
	if err != nil {
		// NOT LOGGED WITH THE REQUEST. A failure to mint can carry the runner name
		// and the scale set, which are harmless, but this path is one edit away
		// from carrying the config itself into a log line that outlives the job.
		writeStoreErr(w, err)

		return
	}
	pool, ok := h.store.(poolRunnerStore)
	if !ok {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if removeErr := src.RemoveRunner(cleanupCtx, reg.ID(), reg.RunnerName()); removeErr != nil {
			err = errors.Join(errors.New("nodeplane: the lease store cannot preserve runner identity"),
				fmt.Errorf("remove unjournaled runner %q: %w", reg.RunnerName(), removeErr))
		} else {
			err = errors.New("nodeplane: the lease store cannot preserve runner identity")
		}
		writeStoreErr(w, err)

		return
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	lease, err := h.store.Lease(persistCtx, leaseID)
	if err == nil && lease == nil {
		err = fmt.Errorf("nodeplane: lease %s disappeared before runner identity was journaled", leaseID)
	}
	if err == nil {
		err = pool.RegisterPoolRunner(persistCtx, alloc.PoolRunner{
			LeaseID: leaseID, Tier: tier, LaunchRequestID: lease.RequestID,
			RunnerID: reg.ID(), RunnerName: reg.RunnerName(),
		})
	}
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cleanupCancel()
		if removeErr := src.RemoveRunner(cleanupCtx, reg.ID(), reg.RunnerName()); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove unjournaled runner %q: %w",
				reg.RunnerName(), removeErr))
		}
		writeStoreErr(w, err)

		return
	}

	writeJSON(w, http.StatusOK, nodeapi.JITResponse{
		Config:     reg.Config(),
		RunnerID:   reg.ID(),
		RunnerName: reg.RunnerName(),
	})
}

func (h *handler) removeRunner(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.RemoveRunnerRequest
	if !decode(w, r, &req) {
		return
	}
	byLease := req.RunnerID == 0 && req.RunnerName == ""
	if !byLease && (req.RunnerID < 0 || req.RunnerName == "") {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused,
			"runner removal needs both an identity or neither when recovering by lease")
		return
	}
	pool, ok := h.store.(poolRunnerStore)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "", "runner identity storage is unavailable")
		return
	}
	var binding alloc.PoolRunner
	var err error
	if byLease {
		binding, err = pool.PoolRunnerByLease(r.Context(), r.PathValue("lease"))
	} else {
		binding, err = pool.PoolRunnerByName(r.Context(), req.RunnerName)
	}
	if err != nil {
		if byLease && errors.Is(err, alloc.ErrLeaseNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeStoreErr(w, err)
		return
	}
	if binding.LeaseID != r.PathValue("lease") {
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
			"runner registration belongs to another lease")
		return
	}
	if !byLease && binding.RunnerID != req.RunnerID {
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
			"runner identity does not match the durable registration")
		return
	}
	// A LEASE THE LEDGER HAS ENDED STILL OWES ITS REGISTRATION'S REMOVAL. The
	// guard admitted this process on the strength of the lease it was given;
	// the durable binding names the registration; and a registration GitHub can
	// route work to is exactly what must go before the guest does, whatever the
	// ledger says about the capacity. Only an OPEN lease is checked for the node
	// it is placed on, because an ended one has no placement left to compare.
	lease, err := h.store.Lease(r.Context(), binding.LeaseID)
	ended := errors.Is(err, alloc.ErrLeaseNotFound)
	if err != nil && !ended {
		writeStoreErr(w, err)
		return
	}
	if !ended && lease == nil {
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused,
			"runner registration no longer has a lease")
		return
	}
	if !ended && lease.Node != r.PathValue("node") {
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
			"runner registration belongs to another node")
		return
	}
	// DURABLE BEFORE REMOTE. A node can disappear after GitHub refuses this
	// removal. The retiring row is what makes the control plane and a restarted
	// node retry deregistration before either is allowed to tear compute down.
	if err := pool.RetirePoolRunner(r.Context(), binding.LeaseID); err != nil {
		writeStoreErr(w, err)
		return
	}
	src, err := h.jitForLabel(binding.Tier)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if err := src.RemoveRunner(r.Context(), binding.RunnerID, binding.RunnerName); err != nil {
		writeStoreErr(w, err)
		return
	}
	// The registration is gone from GitHub, so stop counting this verified lease
	// as a live runner even if its custody teardown lingers — otherwise a node
	// that removed a runner during ambiguous-launch cleanup leaves the lease
	// counted and suppresses its replacement. Best-effort: a failure only leaves
	// it counted, which is the safe direction.
	if err := h.store.MarkDeregistered(r.Context(), binding.LeaseID); err != nil {
		h.log.Warn("could not record runner deregistration on removal",
			"lease", binding.LeaseID, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) recoverRunner(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.RecoverRunnerRequest
	if !decode(w, r, &req) {
		return
	}
	pool, ok := h.store.(poolRunnerStore)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "", "runner identity storage is unavailable")
		return
	}
	leaseID := r.PathValue("lease")
	lease, err := h.store.Lease(r.Context(), leaseID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if lease == nil {
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused,
			"quarantined runner registration no longer has a lease")
		return
	}
	if lease.Phase != alloc.PhaseQuarantine {
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused,
			"runner recovery is only allowed for quarantined compute")
		return
	}
	expectedName := provider.InstanceName(leaseID)
	if req.RunnerName != expectedName {
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
			"runner recovery name does not match its lease")
		return
	}
	src, err := h.jitForLabel(lease.Tier)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	binding, err := pool.PoolRunnerByLease(r.Context(), leaseID)
	if err == nil {
		if binding.Tier != lease.Tier || binding.LaunchRequestID != lease.RequestID {
			writeErr(w, http.StatusConflict, nodeapi.CodeRefused,
				"durable runner identity does not match its lease")
			return
		}
		switch binding.Status {
		case alloc.PoolRunnerBusy:
			if binding.ActualRequestID != 0 || binding.RunID != 0 || binding.JobID != "" {
				writeJSON(w, http.StatusOK, nodeapi.RecoverRunnerResponse{State: nodeapi.RunnerRecoveryTracked})
				return
			}
			req.RunnerName = binding.RunnerName
		case alloc.PoolRunnerIdle:
			req.RunnerName = binding.RunnerName
		case alloc.PoolRunnerRetiring:
			if err := src.RemoveRunner(r.Context(), binding.RunnerID, binding.RunnerName); err != nil {
				writeStoreErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, nodeapi.RecoverRunnerResponse{State: nodeapi.RunnerRecoveryRetired})
			return
		case alloc.PoolRunnerRetired:
			writeJSON(w, http.StatusOK, nodeapi.RecoverRunnerResponse{State: nodeapi.RunnerRecoveryRetired})
			return
		default:
			writeErr(w, http.StatusConflict, nodeapi.CodeRefused,
				"durable runner identity has an unknown status")
			return
		}
	}
	if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) {
		writeStoreErr(w, err)
		return
	}
	recovery, err := src.RecoverRunner(r.Context(), req.RunnerName)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if recovery.Present && binding.LeaseID != "" && binding.RunnerID != 0 &&
		binding.RunnerID != recovery.RunnerID {
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused,
			"recovered runner id changed; refusing replacement identity")
		return
	}
	if recovery.Busy {
		if err := pool.PreserveRecoveredBusyPoolRunner(r.Context(), alloc.PoolRunner{
			LeaseID: leaseID, Tier: lease.Tier, LaunchRequestID: lease.RequestID,
			RunnerID: recovery.RunnerID, RunnerName: req.RunnerName,
		}); err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, nodeapi.RecoverRunnerResponse{State: nodeapi.RunnerRecoveryBusy})
		return
	}
	// JobStarted can bind this runner while the remote lookup or deletion is in
	// flight. Delete first, then atomically claim only idle recovery state; an
	// authoritative busy binding wins and keeps its compute. A crash between the
	// operations leaves the charged quarantine and is retried safely.
	if recovery.Present {
		if err := src.RemoveRunner(r.Context(), recovery.RunnerID, req.RunnerName); err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	retiringID := int64(0)
	if recovery.Present {
		retiringID = recovery.RunnerID
	}
	settled, err := pool.RetireRecoveredPoolRunner(r.Context(), alloc.PoolRunner{
		LeaseID: leaseID, Tier: lease.Tier, LaunchRequestID: lease.RequestID,
		RunnerID: retiringID, RunnerName: req.RunnerName,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if settled.Status == alloc.PoolRunnerBusy &&
		(settled.ActualRequestID != 0 || settled.RunID != 0 || settled.JobID != "") {
		writeJSON(w, http.StatusOK, nodeapi.RecoverRunnerResponse{State: nodeapi.RunnerRecoveryTracked})
		return
	}
	writeJSON(w, http.StatusOK, nodeapi.RecoverRunnerResponse{State: nodeapi.RunnerRecoveryRetired})
}

func (h *handler) validateTrustedRunnerGroup(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.TrustedRunnerGroupRequest
	if !decode(w, r, &req) {
		return
	}

	if err := h.registered(r); err != nil {
		writeStoreErr(w, err)

		return
	}

	// THE TIER NAMES THE TARGET, AND AN UNTARGETED REQUEST IS ANSWERED ONLY WHERE
	// THERE IS NOTHING TO CHOOSE. A node below VersionTargetedRunnerGroup sends
	// no tier; on a plane serving one target that is the same question it always
	// asked, and on one serving several it is refused, because validating one
	// owner's group with another owner's App is the substitution this route
	// exists to prevent.
	var (
		src JITSource
		err error
	)

	switch {
	case req.Tier != "":
		src, err = h.jitForLabel(req.Tier)
	case len(h.targets) > 1:
		err = fmt.Errorf("this control plane serves %d GitHub targets and the request names no "+
			"tier, so it cannot choose which target's credential to validate with; a node from "+
			"wire version %d names its tier", len(h.targets), nodeapi.VersionTargetedRunnerGroup)
	default:
		src, err = h.jitForLabel("")
	}

	if err != nil {
		writeStoreErr(w, err)

		return
	}

	validator, ok := src.(trustedRunnerGroupValidator)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "",
			"this control plane cannot validate trusted runner-group policy")

		return
	}

	if err := validator.ValidateTrustedRunnerGroup(r.Context(), req.Group, req.Workflows); err != nil {
		writeStoreErr(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
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
	return decodeLimited(w, r, into, maxBody)
}

func decodeLimited(w http.ResponseWriter, r *http.Request, into any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))

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
	case errors.Is(err, alloc.ErrNoCapacity):
		writeErr(w, http.StatusConflict, nodeapi.CodeNoCapacity, err.Error())
	case errors.Is(err, alloc.ErrForceRelease):
		writeErr(w, http.StatusConflict, nodeapi.CodeForceRelease, err.Error())
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

// hasJIT reports whether any credential-holding source is attached.
func (h *handler) hasJIT() bool { return h.jit != nil || len(h.targets) > 0 }

// jitFor is the source that holds the credential of a tier's target.
//
// With no per-target sources every tier goes through the constructor's; with
// them, a tier naming no target resolves to the only one, and a tier naming a
// target that is not attached is refused rather than served by another owner's
// App. The constructor's source, when both are given, serves only a tier whose
// target is unresolvable — the single-source assembly a test builds.
func (h *handler) jitFor(tier config.Tier) (JITSource, error) {
	if len(h.targets) == 0 {
		if h.jit == nil {
			return nil, errors.New("nodeplane: this control plane has no GitHub client")
		}

		return h.jit, nil
	}

	name := tier.Target
	if name == "" && len(h.targets) == 1 {
		for only := range h.targets {
			name = only
		}
	}

	if src, ok := h.targets[name]; ok {
		return src, nil
	}

	if h.jit != nil && name == "" {
		return h.jit, nil
	}

	return nil, fmt.Errorf("nodeplane: tier %q names GitHub target %q, which this control plane "+
		"has no credential for", tier.Label, tier.Target)
}

// jitForLabel resolves a tier by its label and then its source. A label the
// catalogue does not know resolves as a tier naming no target, which is served
// only where there is one source to serve it.
func (h *handler) jitForLabel(label string) (JITSource, error) {
	tier, ok := h.plane.tierFor(label)
	if !ok {
		tier = config.Tier{Label: label}
	}

	return h.jitFor(tier)
}
