package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// vmmAPI talks to one microVM's own API socket.
//
// THE VMM IS CONFIGURED THROUGH THIS RATHER THAN THROUGH --config-file, and that
// is the decision the rest of this package rests on. A config file is read at
// startup and the VM begins immediately, so there is no moment at which billet
// can hand it the runner registration before the guest could ask for it. Driving
// the API means every resource is placed while the machine is still `Not started`
// and the credential is in the metadata service BEFORE the first instruction
// executes.
//
// It buys two more things that turned out to matter. Each resource reports its own
// failure — a bad drive says so, rather than one opaque parse error covering the
// whole boot — and the final state read is what makes a launch answerable at all:
// `jailer --daemonize` exits 0 for a VM that died on startup, measured, so its exit
// code proves nothing and this does.
type vmmAPI struct {
	http *http.Client
	// socket is the host path of the VMM's unix socket, inside the jail.
	socket string
}

// apiTimeout bounds one call to a VMM.
//
// Short, because the peer is a process on this machine over a unix socket and
// every one of these calls is a local state change. A VMM that does not answer in
// this long is wedged rather than busy, and the launch path has a node command
// timeout above it that a stalled boot must not consume.
const apiTimeout = 10 * time.Second

// newVMMAPI returns a client for the socket at that path.
func newVMMAPI(socket string) *vmmAPI {
	var d net.Dialer

	return &vmmAPI{
		socket: socket,
		http: &http.Client{
			Timeout: apiTimeout,
			Transport: &http.Transport{
				// PROVIDER OPERATIONS CONSTRUCT INDEPENDENT CLIENTS for one VMM.
				// Retaining an idle connection in each short-lived transport eventually
				// reaches Firecracker's API connection limit, at which point a healthy
				// guest refuses cache detach and its completed update is discarded.
				// A fresh local unix connection per request is cheap and gives each
				// operation a connection lifetime it can actually close.
				DisableKeepAlives: true,
				// THE ADDRESS IS IGNORED, WHICH IS THE POINT. Every request is
				// written to `http://localhost/...` so net/http has a host to build a
				// request line from, and the dialer sends it to this socket whatever
				// that host says. Nothing here can reach the network.
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// instanceInfo is what the VMM says about itself.
type instanceInfo struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// stateRunning is the only state in which a guest is executing instructions.
//
// `Not started` is the one that looks alive and is not — the microVM equivalent of
// a docker container in `created`. It has an API socket, a pid and a jail, and it
// will never run anything, because whatever would have started it is gone.
const stateRunning = "Running"

// info reads the VMM's own account of itself.
//
// The ONLY authority for whether a microVM is running, and it is authoritative in
// a way nothing else here is: it carries the instance id, so a stale socket
// belonging to a different VM cannot be mistaken for this one.
func (a *vmmAPI) info(ctx context.Context) (instanceInfo, error) {
	var out instanceInfo

	if err := a.call(ctx, http.MethodGet, "/", nil, &out); err != nil {
		return instanceInfo{}, err
	}

	return out, nil
}

// put sends one configuration change.
func (a *vmmAPI) put(ctx context.Context, path string, body any) error {
	return a.call(ctx, http.MethodPut, path, body, nil)
}

// patch replaces the backing path of a drive the guest has left unmounted.
func (a *vmmAPI) patch(ctx context.Context, path string, body any) error {
	return a.call(ctx, http.MethodPatch, path, body, nil)
}

// call issues one request and decodes the answer.
func (a *vmmAPI) call(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("firecracker: encode the %s request: %w", path, err)
		}

		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, payload)
	if err != nil {
		return fmt.Errorf("firecracker: build the %s request: %w", path, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker: %s %s on %s: %w", method, path, a.socket, err)
	}

	defer resp.Body.Close()

	// BOUNDED, because this is another program's output on its way into an error
	// string, a log and eventually a terminal. The same rule the ceph client
	// follows, and for the same reason: parsing something does not make it safe to
	// echo, and a fault message is free text billet did not write.
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		return fmt.Errorf("firecracker: read the answer to %s %s: %w", method, path, err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("firecracker: %s %s: %s: %s", method, path, resp.Status,
			faultMessage(answer))
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("firecracker: %s %s answered %s, which is not the json this api "+
			"returns", method, path, bounded(string(answer)))
	}

	return nil
}

// maxAPIBody bounds what is read from a VMM's answer.
//
// Every response this package reads is a short object — an instance description or
// a fault message. The socket is inside a jail billet built, so this is a guard
// against a wedged or wrong process rather than against a hostile one.
const maxAPIBody = 8 << 10

// faultMessage renders the VMM's own explanation of a refusal.
//
// Firecracker answers a rejected request with `{"fault_message":"…"}`, and that
// sentence is the actionable half — `Invalid request method and/or path` versus
// `Open tap device failed`, which send an operator to completely different places.
// A body that is not that shape is rendered bounded rather than dropped, because a
// status code with nothing beside it sends the reader nowhere.
func faultMessage(body []byte) string {
	var fault struct {
		Message string `json:"fault_message"`
	}

	if err := json.Unmarshal(body, &fault); err == nil && fault.Message != "" {
		return bounded(fault.Message)
	}

	return bounded(string(body))
}

// gone reports whether an API error proves the VMM is not there.
//
// THE DISTINCTION THE Instance CONTRACT TURNS ON. `Running` must be reported true
// by a backend that cannot tell, because the caller destroys what is not running —
// so only an answer that PROVES absence may make it false. A socket nothing is
// listening on and a socket that is not there are proofs; a timeout, a permission
// error and a half-read response are not, and reading them as absence force-kills
// a live job.
//
// Measured: a VMM stopped with SIGTERM leaves its socket FILE in place, so the
// error is a refused connection rather than a missing file. Both are handled
// because a removed jail produces the other.
func gone(err error) bool {
	if err == nil {
		return false
	}

	var syscallErr *net.OpError
	if !errors.As(err, &syscallErr) {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such file or directory")
}
