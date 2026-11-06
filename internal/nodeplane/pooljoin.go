package nodeplane

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/wirecert"
)

// PoolJoins admits a pool host on its order's source's word (ADR-030).
type PoolJoins interface {
	// PoolJoinOrder is the pool order a join token was minted for, spent or not, and
	// empty for any other token; live is whether it could still be spent.
	PoolJoinOrder(ctx context.Context, token string) (order string, live bool, err error)
	// ReadPoolOrder reads the order, alloc.ErrPoolOrderClosed when there is none.
	ReadPoolOrder(ctx context.Context, id string) (alloc.PoolOrder, error)
	// JudgePoolJoin asks the order's source whether the host it placed is the one
	// asking with this key.
	JudgePoolJoin(ctx context.Context, claim hostsource.Claim) hostsource.Testimony
	// AdmitPoolEnrollment records the admission, the certificate and the link from the
	// order to its node in one transaction.
	AdmitPoolEnrollment(ctx context.Context, adm alloc.PoolAdmission) (alloc.Enrollment, error)
	// HoldPoolJoin holds an order's join for a person, as the order stood when it was
	// read (seen, its Updated then): alloc.ErrPoolOrderMoved when it has moved since.
	HoldPoolJoin(ctx context.Context, order, reason string, seen time.Time) error
	// HoldLapsedPoolJoin holds the order when nothing can join from it any more, and
	// answers whether it is held now.
	HoldLapsedPoolJoin(ctx context.Context, order string) (bool, error)
	// RecordHeldPoolRequest records a held order's request pending, spending nothing,
	// so a person has the host's key to approve.
	RecordHeldPoolRequest(ctx context.Context, order, fingerprint, csrPEM string) (alloc.Enrollment, error)
}

// WithPoolJoins lets a pool host join without a person when its source vouches for it.
// seesHostAddress says the enrollment listener sees a host's own address as the
// connection's peer: false behind a router, where the peer is the router, so the
// address is not checked and the source vouches on what else it can read.
func WithPoolJoins(p PoolJoins, seesHostAddress bool) HandlerOption {
	return func(h *handler) { h.poolJoins, h.seesHostAddress = p, seesHostAddress }
}

// poolEnroll answers a request whose token was minted for a pool order, and reports
// whether it answered: a request its source could not judge falls through to the
// ordinary path, which records it pending under its order for a person, spending the
// token, and asks the source again on the next poll.
//
// THE LEDGER IS ASKED BEFORE THE SOURCE, so an unauthenticated caller reaches no AWS
// call without a live token of an open order, or the request it already recorded: the
// name is the order's, a known decided key is the ordinary path's to answer, a closed
// order is refused, and a held one is answered pending without asking anything.
//
// A HELD ORDER, A CONTRADICTION AND A CONFLICT ARE ALL ANSWERED PENDING. The host keeps
// asking while a person looks, so a person's approval, or the release of a hold,
// reaches a host that is still there to collect it; and the answer carries none of
// what the source said, which is the ledger's and the log's. A contradiction holds the
// order and spends nothing (ADR-030).
//
// THE ADDRESS IS THE CONNECTION'S PEER, never a header a caller wrote: the enrollment
// listener is dialled directly unless seesHostAddress says otherwise.
func (h *handler) poolEnroll(
	w http.ResponseWriter, r *http.Request, req *nodeapi.EnrollRequest, fingerprint, order string, live bool,
) bool {
	ctx := r.Context()

	if req.Node != alloc.PoolNodeName(order) {
		h.log.Warn("a pool order's join token was presented under another name", "node", req.Node, "order", order)
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated, badJoinToken)

		return true
	}

	existing, found, err := h.enrollments.LookupEnrollment(ctx, req.Node)
	if err != nil {
		h.log.Error("could not read a pool host's enrollment", "node", req.Node, "error", err)
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, "billet could not read this request; try again")

		return true
	}

	mine := found && existing.Fingerprint == fingerprint
	if mine && existing.State != alloc.EnrollPending {
		return false
	}

	o, err := h.poolJoins.ReadPoolOrder(ctx, order)

	switch {
	case errors.Is(err, alloc.ErrPoolOrderClosed):
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused, poolOrderClosed)

		return true
	case err != nil:
		h.log.Error("could not read a pool order", "order", order, "error", err)
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, "billet could not read this request; try again")

		return true
	case o.State == alloc.PoolOrdering, o.State == alloc.PoolUncertain:
		// NOT YET PLACED, OR NOT KNOWN TO BE: an order whose answer was lost may yet
		// prove to hold this very host, so the host asks again rather than giving up.
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolOrderNotPlaced)

		return true
	case o.NodeName == req.Node && o.NodeFingerprint == fingerprint:
		// THIS KEY JOINED FROM THE ORDER while the request was on its way (a person's
		// approval, a concurrent admission): the ordinary path hands over what it was
		// given.
		return false
	case o.State != alloc.PoolPlaced || o.NodeName != "":
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused, poolOrderClosed)

		return true
	case o.JoinHold != "":
		h.log.Info("a pool host asked to join from an order held for a person", "node", req.Node, "order", order,
			"fingerprint", fingerprint, "held", o.JoinHold)
		h.recordHeld(w, r, req, fingerprint, order)

		return true
	case !live && (!mine || existing.PoolOrder != order):
		// A TOKEN OF THE ORDER THAT CAN NO LONGER BE SPENT, from a host with nothing
		// recorded: never refused. The order is held here when nothing can join from it
		// any more (no live token, no request waiting), as the pool's step would, so it
		// waits on no pool loop, and this host's request is then recorded for a person;
		// otherwise it asks again. Nothing is asked of the source or spent.
		held, err := h.poolJoins.HoldLapsedPoolJoin(ctx, order)
		switch {
		case err != nil:
			h.log.Error("could not hold a pool order nothing can join from", "order", order, "error", err)
			writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolJoinWaits)
		case held:
			h.recordHeld(w, r, req, fingerprint, order)
		default:
			writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolJoinWaits)
		}

		return true
	}

	claim := hostsource.Claim{
		Order: order, Fingerprint: fingerprint,
		Document: []byte(req.IdentityDocument), Signature: req.IdentitySignature,
	}
	if h.seesHostAddress {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			claim.Address = host
		}
	}

	t := h.poolJoins.JudgePoolJoin(ctx, claim)

	switch t.Verdict {
	case hostsource.VerdictContradicts:
		h.log.Warn("a pool host's source contradicted its enrollment; the order is held for a person",
			"node", req.Node, "order", order, "fingerprint", fingerprint, "why", t.Reason)
		if err := h.poolJoins.HoldPoolJoin(ctx, order, t.Reason, o.Updated); errors.Is(err, alloc.ErrPoolOrderMoved) {
			// THE ORDER MOVED SINCE IT WAS JUDGED (a person released a hold), so this
			// verdict is not acted on; the host asks again and is judged afresh.
			writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolJoinWaits)

			return true
		} else if err != nil {
			h.log.Error("could not hold a pool order's join", "order", order, "error", err)
			writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, "billet could not record this request; try again")

			return true
		}
		h.recordHeld(w, r, req, fingerprint, order)

		return true
	case hostsource.VerdictVouches:
		h.admitPoolHost(w, r, req, fingerprint, order, t.Reason, o.Updated)

		return true
	case hostsource.VerdictDeferred:
		// NOT ASKED, SO NOTHING RECORDED OR SPENT: another ask of this order is under way
		// or just answered, and this host asks again.
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolJoinWaits)

		return true
	}

	h.log.Info("a pool host's source could not yet vouch for it; its request waits, and is asked again",
		"node", req.Node, "order", order, "fingerprint", fingerprint, "why", t.Reason)

	return false
}

// recordHeld records a held order's request for a person and answers it pending: the
// host keeps asking, and its key is there to approve whether or not its token is still
// live. Another key already holding the name is answered pending too, recording
// nothing.
func (h *handler) recordHeld(w http.ResponseWriter, r *http.Request, req *nodeapi.EnrollRequest, fingerprint, order string) {
	e, err := h.poolJoins.RecordHeldPoolRequest(r.Context(), order, fingerprint, req.CSRPEM)

	switch {
	case err == nil && e.State == alloc.EnrollDenied:
		writeJSON(w, http.StatusOK, nodeapi.EnrollResponse{State: e.State, Fingerprint: e.Fingerprint})
	case err == nil, errors.Is(err, alloc.ErrEnrollmentConflict), errors.Is(err, alloc.ErrPoolJoinNotHeld):
		poolPending(w, fingerprint)
	case errors.Is(err, alloc.ErrPoolOrderClosed):
		// A NODE JOINED FROM THE ORDER while this was on its way, perhaps this very key
		// by a person's approval: the next ask finds out which.
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolJoinWaits)
	default:
		h.poolRefusal(w, req.Node, fingerprint, err)
	}
}

// The answers an unauthenticated caller reads, in billet's words only.
const (
	badJoinToken = "enrolling needs a join token: run `billet ca token` on the control plane and pass it as " +
		"--join-token"
	poolOrderClosed    = "this pool order admits no node"
	poolOrderNotPlaced = "this pool host's order is not yet recorded as placed; ask again in a moment"
	poolJoinWaits      = "this pool host's join waits; ask again in a moment"
)

// poolPending answers a pool host that waits for a person.
func poolPending(w http.ResponseWriter, fingerprint string) {
	writeJSON(w, http.StatusOK, nodeapi.EnrollResponse{State: alloc.EnrollPending, Fingerprint: fingerprint})
}

// admitPoolHost signs the host's request and admits it in one transaction. THE
// CERTIFICATE IS HANDED OVER ONLY ONCE THE ADMISSION HAS COMMITTED, the order the
// renewal keeps: one signed for a request the ledger then refused is never sent.
func (h *handler) admitPoolHost(
	w http.ResponseWriter, r *http.Request, req *nodeapi.EnrollRequest, fingerprint, order, why string, seen time.Time,
) {
	bundle, err := h.ca.SignNodeCSR(req.Node, []byte(req.CSRPEM))
	if err != nil {
		writeErr(w, http.StatusBadRequest, nodeapi.CodeRefused, "this certificate request cannot be signed: "+err.Error())

		return
	}
	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		h.log.Error("could not read a signed pool host certificate back", "node", req.Node, "error", err)
		writeErr(w, http.StatusInternalServerError, nodeapi.CodeUnavailable, "billet could not read the certificate it signed")

		return
	}

	enrollment, err := h.poolJoins.AdmitPoolEnrollment(r.Context(), alloc.PoolAdmission{
		Order: order, Fingerprint: fingerprint, CSRPEM: req.CSRPEM, Token: req.JoinToken,
		CertPEM: string(bundle.CertPEM), Serial: wirecert.Serial(leaf), NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339),
		Seen: seen,
	})
	if err != nil {
		h.poolRefusal(w, req.Node, fingerprint, err)

		return
	}

	res := nodeapi.EnrollResponse{State: enrollment.State, Fingerprint: enrollment.Fingerprint}
	if enrollment.State == alloc.EnrollApproved {
		res.CertPEM, res.CAPEM = enrollment.CertPEM, string(h.trustBundle())
		h.log.Info("a pool host was admitted on its source's word", "node", req.Node, "order", order,
			"fingerprint", fingerprint, "why", why)
	}

	writeJSON(w, http.StatusOK, res)
}

// isPoolRefusal is an answer only a request under a pool order's token gets.
func isPoolRefusal(err error) bool {
	return errors.Is(err, alloc.ErrPoolOrderNotPlaced) || errors.Is(err, alloc.ErrPoolJoinHeld) ||
		errors.Is(err, alloc.ErrPoolOrderClosed)
}

// poolRefusal answers what the ledger said of a pool host's enrollment, on either path:
// an order not yet placed is asked again, a held order or a conflict waits for a person
// as pending, a closed order is refused, and a token of the order that could not be
// spent is asked again. Nothing the ledger said is quoted to the caller.
func (h *handler) poolRefusal(w http.ResponseWriter, node, fingerprint string, err error) {
	switch {
	case errors.Is(err, alloc.ErrBadJoinToken):
		// A TOKEN OF THIS ORDER THAT COULD NOT BE SPENT lapsed, or was spent, while the
		// request was on its way: not a bad credential, so the host asks again, and the
		// pool holds an order nothing can join from, after which it is recorded for a
		// person.
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolJoinWaits)
	case errors.Is(err, alloc.ErrPoolOrderNotPlaced):
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolOrderNotPlaced)
	case errors.Is(err, alloc.ErrPoolOrderMoved):
		// THE ORDER MOVED WHILE ITS SOURCE WAS ASKED, so the verdict is not acted on: the
		// host asks again and is judged afresh.
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, poolJoinWaits)
	case errors.Is(err, alloc.ErrPoolJoinHeld), errors.Is(err, alloc.ErrEnrollmentConflict):
		h.log.Warn("a pool host's enrollment waits for a person", "node", node, "fingerprint", fingerprint, "why", err)
		poolPending(w, fingerprint)
	case errors.Is(err, alloc.ErrPoolOrderClosed):
		h.log.Warn("a pool host's enrollment was refused", "node", node, "fingerprint", fingerprint, "error", err)
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused, poolOrderClosed)
	default:
		h.log.Error("could not record a pool host's enrollment", "node", node, "error", err)
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, "billet could not record this request; try again")
	}
}
