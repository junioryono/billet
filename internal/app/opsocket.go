package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
)

// The operator socket: how a command run on the controller's host reaches what only the
// running control plane can do, which is ask a node for something. A Unix socket under the
// identity directory, so it reaches no network and adds no listener #28 would have to
// answer for, and it serves a connection only from root or the control plane's own user.
const (
	operatorSocketName = "operator.sock"
	// operatorReadLimit bounds what the socket reads of a request, and operatorWait how
	// long a request waits on a node.
	operatorReadLimit = 1 << 20
	operatorWait      = 30 * time.Second
	operatorLogPath   = "/v1/logs/read"
)

// ErrNoController says nothing answered on this host's operator socket: the control
// plane is not running here, or this is a standby. That is could-not-tell about whatever
// was asked, and never its absence.
var ErrNoController = errors.New("no control plane answered on this host's operator socket")

// operatorSocketPath is the socket's path.
func operatorSocketPath(cfg *config.Config) string {
	return filepath.Join(cfg.Server.IdentityDir, operatorSocketName)
}

// JobLogRead is a request for lines of a job's log.
type JobLogRead struct {
	Lease  string                 `json:"lease"`
	Ranges []nodeapi.LogLineRange `json:"ranges"`
}

// peerListener closes a connection from anyone but root or this process's own user, by
// what the kernel says of its peer.
type peerListener struct {
	net.Listener

	log *slog.Logger
}

// Accept returns the next connection the peer rule admits.
func (l *peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		unix, ok := conn.(*net.UnixConn)
		if ok {
			uid, known, err := peerUID(unix)
			if err == nil && known && (uid == 0 || uid == os.Geteuid()) {
				return conn, nil
			}

			l.log.Warn("refused a connection to the operator socket from another user", "known", known, "error", err)
		}

		_ = conn.Close()
	}
}

// listenOperator opens the socket. A failure is said and costs operators this one way in,
// never the control plane: nothing that schedules goes through it.
func listenOperator(cfg *config.Config) (*peerListener, error) {
	path := operatorSocketPath(cfg)

	// A SOCKET AN EARLIER CONTROLLER LEFT. Anything else at the name is not ours: Listen
	// refuses it, and that is said.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove the operator socket left behind: %w", err)
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()

		return nil, fmt.Errorf("set the operator socket's mode: %w", err)
	}

	return &peerListener{Listener: ln, log: slog.Default()}, nil
}

// operatorServer is what the socket serves.
type operatorServer struct {
	jobs  *alloc.Allocator
	plane *nodeplane.Plane
	log   *slog.Logger
}

// serveOperator serves the socket until ctx ends. A failure to listen is said and ends
// only this.
func serveOperator(ctx context.Context, cfg *config.Config, server *operatorServer) {
	ln, err := listenOperator(cfg)
	if err != nil {
		server.log.Warn("the operator socket is not served, so a command on this host cannot read a job's log "+
			"through this control plane", "error", err)

		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+operatorLogPath, server.readLog)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		server.log.Warn("the operator socket stopped serving", "error", err)
	}
}

// readLog asks the node that ran a job for lines of its log. A NODE THAT CANNOT BE ASKED
// IS SAID, in the status, since a reader must not take it for lines that do not exist.
func (s *operatorServer) readLog(w http.ResponseWriter, r *http.Request) {
	var req JobLogRead

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, operatorReadLimit))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		http.Error(w, "not a log read", http.StatusBadRequest)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operatorWait)
	defer cancel()

	job, err := s.jobs.Job(ctx, req.Lease)
	if err != nil {
		http.Error(w, "the ledger holds no such job", http.StatusNotFound)

		return
	}

	res, err := s.plane.ReadJobLog(ctx, job.Node, nodeapi.LogReadSpec{Lease: req.Lease, Ranges: req.Ranges,
		MaxBytes: nodeapi.MaxLogReadBytes})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(res); err != nil {
		s.log.Warn("could not answer a log read on the operator socket", "error", err)
	}
}

// operatorClient is an HTTP client whose every request goes to this host's socket.
func operatorClient(cfg *config.Config) *http.Client {
	path := operatorSocketPath(cfg)

	return &http.Client{
		Timeout: operatorWait + 5*time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

// ReadJobLogLines reads lines of a job's log through the control plane running on this
// host. ErrNoController says nothing answered; any other error is what it said.
func ReadJobLogLines(ctx context.Context, cfg *config.Config, read JobLogRead) (nodeapi.LogReadResult, error) {
	body, err := json.Marshal(read)
	if err != nil {
		return nodeapi.LogReadResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://operator"+operatorLogPath,
		strings.NewReader(string(body)))
	if err != nil {
		return nodeapi.LogReadResult{}, err
	}

	resp, err := operatorClient(cfg).Do(req)
	if err != nil {
		return nodeapi.LogReadResult{}, fmt.Errorf("%w: %w", ErrNoController, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10)) //nolint:errcheck // the status is the answer

		return nodeapi.LogReadResult{}, fmt.Errorf("the control plane answered %s: %s", resp.Status,
			strings.TrimSpace(string(text)))
	}

	var out nodeapi.LogReadResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, operatorReadLimit*2)).Decode(&out); err != nil {
		return nodeapi.LogReadResult{}, fmt.Errorf("the control plane's answer could not be read: %w", err)
	}

	return out, nil
}
