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
	// empty for any other token.
	PoolJoinOrder(ctx context.Context, token string) (string, error)
	// JudgePoolJoin asks the order's source whether the host it placed is the one
	// asking with this key.
	JudgePoolJoin(ctx context.Context, claim hostsource.Claim) hostsource.Testimony
	// AdmitPoolEnrollment records the admission, the certificate and the link from the
	// order to its node in one transaction.
	AdmitPoolEnrollment(ctx context.Context, adm alloc.PoolAdmission) (alloc.Enrollment, error)
	// HoldPoolJoin holds an order's join for a person.
	HoldPoolJoin(ctx context.Context, order, reason string) error
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
// THE NAME IS THE ORDER'S, and a request naming anything else is refused before any
// source is asked. A KNOWN, DECIDED KEY asks nothing either: the ordinary path answers
// its approval or denial without a read. A CONTRADICTION holds the order and spends
// nothing (ADR-030): the host's own request then admits nothing until a person acts.
//
// THE ADDRESS IS THE CONNECTION'S PEER, never a header a caller wrote: the enrollment
// listener is dialled directly unless seesHostAddress says otherwise.
func (h *handler) poolEnroll(
	w http.ResponseWriter, r *http.Request, req *nodeapi.EnrollRequest, fingerprint, order string,
) bool {
	if req.Node != alloc.PoolNodeName(order) {
		h.log.Warn("a pool order's join token was presented under another name", "node", req.Node, "order", order)
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
			"enrolling needs a join token: run `billet ca token` on the control plane and pass it as --join-token")

		return true
	}

	existing, found, err := h.enrollments.LookupEnrollment(r.Context(), req.Node)
	if err != nil {
		h.log.Error("could not read a pool host's enrollment", "node", req.Node, "error", err)
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, "billet could not read this request; try again")

		return true
	}
	if found && existing.Fingerprint == fingerprint && existing.State != alloc.EnrollPending {
		return false
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

	t := h.poolJoins.JudgePoolJoin(r.Context(), claim)

	switch t.Verdict {
	case hostsource.VerdictContradicts:
		h.log.Warn("a pool host's source contradicted its enrollment; the order is held for a person",
			"node", req.Node, "order", order, "fingerprint", fingerprint, "why", t.Reason)
		if err := h.poolJoins.HoldPoolJoin(r.Context(), order, t.Reason); err != nil {
			h.log.Error("could not hold a pool order's join", "order", order, "error", err)
		}
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused,
			"this pool host's join is held for a person: its source said "+t.Reason)

		return true
	case hostsource.VerdictVouches:
		h.admitPoolHost(w, r, req, fingerprint, order, t.Reason)

		return true
	}

	h.log.Info("a pool host's source could not yet vouch for it; its request waits, and is asked again",
		"node", req.Node, "order", order, "fingerprint", fingerprint, "why", t.Reason)

	return false
}

// admitPoolHost signs the host's request and admits it in one transaction. THE
// CERTIFICATE IS HANDED OVER ONLY ONCE THE ADMISSION HAS COMMITTED, the order the
// renewal keeps: one signed for a request the ledger then refused is never sent.
func (h *handler) admitPoolHost(
	w http.ResponseWriter, r *http.Request, req *nodeapi.EnrollRequest, fingerprint, order, why string,
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

// poolRefusal answers what the ledger said of a pool host's enrollment, on either path.
func (h *handler) poolRefusal(w http.ResponseWriter, node, fingerprint string, err error) {
	switch {
	case errors.Is(err, alloc.ErrBadJoinToken):
		writeErr(w, http.StatusUnauthorized, nodeapi.CodeUnauthenticated,
			"enrolling needs a join token: run `billet ca token` on the control plane and pass it as --join-token")
	case errors.Is(err, alloc.ErrPoolOrderNotPlaced):
		// NOT A VERDICT: the order's answer is still being read back, and the host asks
		// again.
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable,
			"this pool host's order is not yet recorded as placed; ask again in a moment")
	case errors.Is(err, alloc.ErrPoolJoinHeld):
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, err.Error())
	case errors.Is(err, alloc.ErrPoolOrderClosed), errors.Is(err, alloc.ErrEnrollmentConflict):
		h.log.Warn("a pool host's enrollment was refused", "node", node, "fingerprint", fingerprint, "error", err)
		writeErr(w, http.StatusConflict, nodeapi.CodeRefused, err.Error())
	default:
		h.log.Error("could not record a pool host's enrollment", "node", node, "error", err)
		writeErr(w, http.StatusServiceUnavailable, nodeapi.CodeUnavailable, "billet could not record this request; try again")
	}
}
