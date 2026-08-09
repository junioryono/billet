package nodeplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
)

// LeaseStore is the ledger, as the node wire needs it.
//
// Declared here rather than imported from internal/node so the transport does
// not depend on the runtime it serves — the two are on opposite sides of a
// process boundary and coupling them would defeat the point of having one. The
// shapes match because both describe the same allocator.
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

// Handler serves the node wire.
//
// THE NODE NAMES ITSELF IN THE PATH, AND THAT IS NOT AUTHENTICATION. Until mTLS
// lands, any process that can reach this listener can claim to be any node. That
// is why the server refuses to serve this on anything but loopback without TLS —
// see the guard in the command wiring — and why this comment is here rather than
// in a design document: the next person to bind it to 0.0.0.0 should meet the
// problem, not discover it.
func Handler(log *slog.Logger, p *Plane, store LeaseStore) http.Handler {
	h := &handler{log: log, plane: p, store: store}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/register", h.register)
	mux.HandleFunc("POST /v1/nodes/{node}/poll", h.poll)
	mux.HandleFunc("POST /v1/nodes/{node}/result", h.result)
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/bind", h.bind)
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/advance", h.advance)
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/heartbeat", h.heartbeat)
	mux.HandleFunc("POST /v1/nodes/{node}/leases/{lease}/release", h.release)
	mux.HandleFunc("GET /v1/nodes/{node}/leases/{lease}", h.lease)
	mux.HandleFunc("GET /v1/nodes/{node}/launched", h.launched)

	return mux
}

type handler struct {
	log   *slog.Logger
	plane *Plane
	store LeaseStore
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.RegisterRequest
	if !decode(w, r, &req) {
		return
	}

	res, err := h.plane.Register(req)
	if err != nil {
		// REFUSED, not "try again". A version mismatch or a foreign deployment
		// cannot be fixed by retrying, and a node that retries forever against a
		// control plane that will never accept it is a node nobody notices is
		// broken.
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, err.Error())

		return
	}

	h.log.Info("node registered",
		"node", req.Node, "provider", req.Provider, "guest_os", req.GuestOS)

	writeJSON(w, http.StatusOK, res)
}

func (h *handler) poll(w http.ResponseWriter, r *http.Request) {
	cmd, ok, err := h.plane.Poll(r.Context(), r.PathValue("node"))
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

	if err := h.plane.Result(r.PathValue("node"), res); err != nil {
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

	if err := h.requireRegistered(r.PathValue("node")); err != nil {
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

	if err := h.requireRegistered(r.PathValue("node")); err != nil {
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

	if err := h.requireRegistered(r.PathValue("node")); err != nil {
		writeStoreErr(w, err)

		return
	}

	if err := h.store.Release(r.Context(), r.PathValue("lease"), req.Epoch, outcome); err != nil {
		writeStoreErr(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) lease(w http.ResponseWriter, r *http.Request) {
	if err := h.requireRegistered(r.PathValue("node")); err != nil {
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

// requireRegistered refuses lease work from a node the plane does not know.
//
// This is what a node meets after the CONTROL PLANE restarts: its commands and
// lease writes are rejected with a code that means "register again" rather than
// a generic failure it would retry forever without ever fixing.
func (h *handler) requireRegistered(node string) error {
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
// Until mTLS lands the node names itself in the path and nothing verifies that
// claim, so a listener reachable beyond this host would let anything on the
// network bind leases and take commands. Refusing is the only honest option; the
// alternative is a deployment that looks like it works and has no boundary at
// all.
func LoopbackOnly(addr string) bool {
	host := addr

	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}

	host = strings.Trim(host, "[]")

	switch host {
	case "localhost", "127.0.0.1", "::1", "":
		return true
	default:
		return strings.HasPrefix(host, "127.")
	}
}

var _ = time.Second
