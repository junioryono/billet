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

// LeaseStore is the ledger, as the node wire needs it.
//
// Declared here rather than imported from internal/node so the transport does
// not depend on the runtime it serves — the two are on opposite sides of a
// process boundary and coupling them would defeat the point of having one. The
// shapes match because both describe the same allocator.
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

type LeaseStore interface {
	Bind(ctx context.Context, leaseID string, epoch int64, node string) error
	Advance(ctx context.Context, leaseID string, epoch int64, to alloc.Phase) error
	Heartbeat(ctx context.Context, leaseID string, epoch int64) error
	Release(ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase) error
	Lease(ctx context.Context, leaseID string) (*alloc.Lease, error)
	LaunchedLeaseIDs(ctx context.Context, node string) (map[string]bool, error)
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

// WithTiers gives the wire the catalogue it needs to check a JIT request.
//
// A TIER'S RUNNER GROUP IS PART OF ITS ADDRESS, and resolving a scale set
// without one silently means "the default group". A tier deliberately placed in
// another group — which is how an operator stops every repository in the
// organisation from reaching it — would then have its legitimate registrations
// refused, because the set looked for is not the set that exists.
func WithTiers(tiers []config.Tier) HandlerOption {
	return func(h *handler) {
		h.tiers = make(map[string]config.Tier, len(tiers))
		for i := range tiers {
			h.tiers[tiers[i].Label] = tiers[i]
		}
	}
}

// RequireClientCert makes a verified certificate the source of a node's name.
//
// WITHOUT IT THE PATH IS THE ONLY AUTHORITY, which is not authentication at all:
// any process that can reach the listener claims to be any node, binds its
// leases, takes its commands, and asks for a JIT registration — a credential
// that registers a runner against the organisation. That is why a wire served
// without this refuses to bind anywhere but loopback.
//
// With it, the name in the certificate decides, and a request whose path
// disagrees is rejected rather than reconciled. Two authorities for one fact is
// how this codebase's worst bugs have started, and an authenticated identity is
// the one place it must not happen.
func RequireClientCert() HandlerOption {
	return func(h *handler) { h.requireCert = true }
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

	return mux
}

type handler struct {
	log         *slog.Logger
	plane       *Plane
	store       LeaseStore
	jit         JITSource
	requireCert bool

	// tiers is the catalogue, by label, used to resolve a lease's scale set.
	tiers map[string]config.Tier

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
// incarnation and its replacement the node name and the certificate are
// identical, so a permission keyed on either lets one host act on the other's
// work: releasing capacity a running container is using, or advancing a lease it
// never received.
//
// RENEWAL IS HERE TOO, and the argument for leaving it out was wrong. "A
// heartbeat only extends a lease" is true of one heartbeat and false of a
// process that keeps sending them: repeated renewal does not hold capacity
// slightly longer, it denies it indefinitely. If the current process dies before
// releasing, the reaper is the mechanism that reclaims — and a superseded
// process renewing that lease forever is precisely what stops it.
//
// Reads are here for the same reason: a lease id and its epoch are what a
// release needs, and handing them out defeats the check on the release itself.
func (h *handler) forOwnLease(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node := r.PathValue("node")

		if !h.authorise(w, r, node) {
			return
		}

		err := h.plane.MayMutateLease(node,
			r.Header.Get(nodeapi.HeaderIncarnation), r.PathValue("lease"))
		if err != nil {
			writeStoreErr(w, err)

			return
		}

		next(w, r)
	}
}

// forNewWork wraps a route by which a node ACQUIRES work or capacity.
//
// SUPERSEDING A NODE MUST NOT SILENCE IT, and treating every route alike was a
// way to lose a lease outright. A host that is superseded — a copied bundle
// arriving on a second machine — may well be holding a container right now. It
// is refused new work, because two hosts under one name cannot both be given
// work. It is NOT refused the calls that maintain and conclude what it already
// has: the heartbeat that keeps its lease alive, and the result that hands
// custody over.
//
// Fencing those was worse than not fencing at all. Registration tells the
// listener the node has custody, so the listener stops heartbeating; if the
// superseded process can then neither renew nor report, the lease expires while
// its container runs and the capacity is resold. The tombstone recorded for
// exactly that report becomes unreachable by the only process that could consume
// it.
//
// The same distinction the expiry rules needed, one layer over: eligibility for
// NEW work and permission to maintain EXISTING work have different lifetimes.
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

	// BELT AND BRACES. tls.RequireAndVerifyClientCert means an unverified
	// connection never reaches a handler, so this branch should be unreachable —
	// which is precisely why it is here. If some future wiring serves this mux
	// over a listener that does not require certificates, the failure must be a
	// refusal rather than every request silently authenticating as whatever the
	// path says.
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

	// OWNERSHIP IS REBUILT FROM THE LEDGER, because the plane's copy does not
	// survive a restart and a superseded process cannot finish without it. The
	// sequence that breaks otherwise: a node is holding compute, the control plane
	// restarts, the node re-registers and adopts what it finds, a second host
	// supersedes it — and the new plane never saw those launches, so it refuses
	// the draining process its own release and the drain never ends.
	//
	// Best effort: a ledger that cannot answer is not a reason to refuse a
	// registration, and the consequence of missing it is a drain that needs an
	// operator, not compute that runs unaccounted for.
	if ids, err := h.store.LaunchedLeaseIDs(r.Context(), req.Node); err != nil {
		h.log.Warn("could not rebuild which leases this node already holds; a process that is "+
			"later superseded may be unable to release them",
			"node", req.Node, "error", err)
	} else {
		open := make([]string, 0, len(ids))
		for id := range ids {
			open = append(open, id)
		}

		h.plane.AdoptOwnership(req.Node, req.Incarnation, open)
	}

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

	if err := h.registered(r); err != nil {
		writeStoreErr(w, err)

		return
	}

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

	if err := h.registered(r); err != nil {
		writeStoreErr(w, err)

		return
	}

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

	if err := h.registered(r); err != nil {
		writeStoreErr(w, err)

		return
	}

	if err := h.store.Release(r.Context(), r.PathValue("lease"), req.Epoch, outcome); err != nil {
		writeStoreErr(w, err)

		return
	}

	// THE OWNERSHIP RECORD ENDS WITH THE LEASE. Kept only while somebody might
	// still need to prove they hold it; one entry per historical job would grow
	// for the life of the installation, since a node that never goes quiet is
	// never expired.
	h.plane.ForgetLease(r.PathValue("node"), r.PathValue("lease"))

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) lease(w http.ResponseWriter, r *http.Request) {
	if err := h.registered(r); err != nil {
		writeStoreErr(w, err)

		return
	}

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

	writeJSON(w, http.StatusOK, nodeapi.LaunchedResponse{LeaseIDs: ids})
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
	known, ok := h.tiers[tier]
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
