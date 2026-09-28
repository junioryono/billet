// Package gocacheprog is the Go build cache helper billet installs in its guest
// images: a GOCACHEPROG program (cmd/go/internal/cacheprog's protocol) that
// keeps a local directory for the go command and shares objects through the
// node's content-addressed cache.
//
// A BUILD NEVER FAILS BECAUSE OF IT. Anything the node refuses, or any failure
// to reach it, turns the helper into a plain local cache for the rest of the
// process: the go command still gets every object it put, from local disk.
package gocacheprog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config says where the helper keeps its files and which node it asks.
type Config struct {
	// Dir holds the objects the go command reads through DiskPath.
	Dir string
	// Endpoint and Token are the node's cache endpoint and this guest's bearer.
	// Either empty means a local cache only.
	Endpoint string
	Token    string
	// Client carries the node requests; nil uses a client with a timeout.
	Client *http.Client
	// Log receives one line per condition worth knowing about.
	Log io.Writer
}

// request is cmd/go/internal/cacheprog.Request.
type request struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

// response is cmd/go/internal/cacheprog.Response.
type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// Run speaks the protocol on in and out until the go command closes it.
func Run(ctx context.Context, cfg Config, in io.Reader, out io.Writer) error {
	h, err := newHelper(cfg)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bufio.NewReader(in))
	encoder := json.NewEncoder(out)
	if err := encoder.Encode(response{KnownCommands: []string{"get", "put", "close"}}); err != nil {
		return fmt.Errorf("gocacheprog: announce capabilities: %w", err)
	}

	for {
		var req request
		if err := decoder.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("gocacheprog: read a request: %w", err)
		}
		var body []byte
		if req.Command == "put" && req.BodySize > 0 {
			if err := decoder.Decode(&body); err != nil {
				return fmt.Errorf("gocacheprog: read the body of request %d: %w", req.ID, err)
			}
			if int64(len(body)) != req.BodySize {
				return fmt.Errorf("gocacheprog: request %d announced %d bytes and carried %d",
					req.ID, req.BodySize, len(body))
			}
		}

		resp := response{ID: req.ID}
		switch req.Command {
		case "get":
			h.get(ctx, req, &resp)
		case "put":
			h.put(ctx, req, body, &resp)
		case "close":
			return encoder.Encode(resp)
		default:
			resp.Err = "unknown command " + strconv.Quote(req.Command)
		}
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("gocacheprog: answer request %d: %w", req.ID, err)
		}
	}
}

type helper struct {
	cfg    Config
	remote bool
}

func newHelper(cfg Config) (*helper, error) {
	if cfg.Dir == "" {
		return nil, errors.New("gocacheprog: no cache directory")
	}
	for _, sub := range []string{"a", "o"} {
		if err := os.MkdirAll(filepath.Join(cfg.Dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("gocacheprog: prepare %s: %w", cfg.Dir, err)
		}
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 2 * time.Minute}
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	cfg.Endpoint = endpoint

	return &helper{cfg: cfg, remote: endpoint != "" && cfg.Token != ""}, nil
}

// entry is what the helper records for one action: its output and size.
type entry struct {
	output string
	size   int64
	at     time.Time
}

func (e entry) encode() []byte {
	return fmt.Appendf(nil, "v1 %s %d %d\n", e.output, e.size, e.at.UnixNano())
}

func parseEntry(raw []byte) (entry, bool) {
	fields := strings.Fields(string(raw))
	if len(fields) != 4 || fields[0] != "v1" || !validDigest(fields[1]) {
		return entry{}, false
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return entry{}, false
	}
	nanos, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return entry{}, false
	}

	return entry{output: fields[1], size: size, at: time.Unix(0, nanos)}, true
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)

	return err == nil
}

func (h *helper) actionPath(action string) string { return filepath.Join(h.cfg.Dir, "a", action) }
func (h *helper) outputPath(output string) string { return filepath.Join(h.cfg.Dir, "o", output) }

func (h *helper) get(ctx context.Context, req request, resp *response) {
	action := hex.EncodeToString(req.ActionID)
	if found, ok := h.local(action); ok {
		answer(resp, found, h.outputPath(found.output))

		return
	}
	if found, ok := h.fetch(ctx, action); ok {
		answer(resp, found, h.outputPath(found.output))

		return
	}
	resp.Miss = true
}

// answer fills resp from found, whose output every reader checked is a digest;
// one that does not decode is a miss rather than a hit naming nothing.
func answer(resp *response, found entry, path string) {
	output, err := hex.DecodeString(found.output)
	if err != nil {
		resp.Miss = true

		return
	}
	at := found.at
	resp.OutputID, resp.Size, resp.Time, resp.DiskPath = output, found.size, &at, path
}

// local is the action's entry when this machine already has its output.
func (h *helper) local(action string) (entry, bool) {
	raw, err := os.ReadFile(h.actionPath(action))
	if err != nil {
		return entry{}, false
	}
	found, ok := parseEntry(raw)
	if !ok {
		return entry{}, false
	}
	info, err := os.Stat(h.outputPath(found.output))
	if err != nil || !info.Mode().IsRegular() || info.Size() != found.size {
		return entry{}, false
	}

	return found, true
}

// fetch asks the node for an action's entry and output, and keeps both.
func (h *helper) fetch(ctx context.Context, action string) (entry, bool) {
	if !h.remote {
		return entry{}, false
	}
	raw, ok := h.call(ctx, http.MethodGet, "ac/"+action, nil)
	if !ok {
		return entry{}, false
	}
	found, ok := parseEntry(raw)
	if !ok {
		return entry{}, false
	}
	// AN OUTPUT ALREADY ON DISK IS REUSED ONLY WHEN IT IS THE ENTRY'S: a damaged
	// one, which local() refused, is fetched again rather than named.
	if !h.holds(found) {
		body, ok := h.call(ctx, http.MethodGet, "cas/"+found.output, nil)
		if !ok || int64(len(body)) != found.size || digest(body) != found.output {
			return entry{}, false
		}
		if err := writeFile(h.outputPath(found.output), body); err != nil {
			return entry{}, false
		}
	}
	if err := writeFile(h.actionPath(action), found.encode()); err != nil {
		return entry{}, false
	}

	return found, true
}

// holds reports whether the local output is exactly the one found names.
func (h *helper) holds(found entry) bool {
	body, err := os.ReadFile(h.outputPath(found.output))

	return err == nil && int64(len(body)) == found.size && digest(body) == found.output
}

func (h *helper) put(ctx context.Context, req request, body []byte, resp *response) {
	action := hex.EncodeToString(req.ActionID)
	output := hex.EncodeToString(req.OutputID)
	if !validDigest(action) || !validDigest(output) {
		resp.Err = "a put needs a 32-byte action and output id"

		return
	}
	now := time.Now()
	record := entry{output: output, size: int64(len(body)), at: now}
	if err := writeFile(h.outputPath(output), body); err != nil {
		resp.Err = err.Error()

		return
	}
	if err := writeFile(h.actionPath(action), record.encode()); err != nil {
		resp.Err = err.Error()

		return
	}
	resp.DiskPath = h.outputPath(output)

	// SHARED ONLY WHEN THE BYTES ARE WHAT THE OUTPUT ID SAYS, which is how the
	// go command computes one; the node refuses anything else anyway.
	if h.remote && digest(body) == output {
		if _, ok := h.call(ctx, http.MethodPut, "cas/"+output, body); ok {
			h.call(ctx, http.MethodPut, "ac/"+action, record.encode())
		}
	}
}

// call makes one node request. A refusal or a failure to reach the node turns
// the helper local for the rest of the process; a miss does not.
func (h *helper) call(ctx context.Context, method, path string, body []byte) ([]byte, bool) {
	if !h.remote {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, method, h.cfg.Endpoint+"/v1/cas/go/"+path,
		bytes.NewReader(body))
	if err != nil {
		h.disable(err.Error())

		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	resp, err := h.cfg.Client.Do(req)
	if err != nil {
		h.disable("the node cache is unreachable")

		return nil, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<31))
	switch {
	case err != nil:
		return nil, false
	case resp.StatusCode == http.StatusOK:
		return raw, true
	// A MISS, A FULL VOLUME, OR A NODE BUSY WITH THE JOB'S OTHER TRANSFERS is
	// this request's answer, not the node's verdict on the job: the next one
	// may well be served.
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusInsufficientStorage ||
		resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		return nil, false
	default:
		h.disable(fmt.Sprintf("the node cache answered %d", resp.StatusCode))

		return nil, false
	}
}

func (h *helper) disable(reason string) {
	if !h.remote {
		return
	}
	h.remote = false
	fmt.Fprintf(h.cfg.Log, "billet go cache: %s; continuing with a local cache only\n", reason)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

// writeFile installs a file through a temporary name, so the go command never
// reads a partial object at a DiskPath.
func writeFile(path string, body []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.Write(body)
	if err := errors.Join(writeErr, temporary.Close()); err != nil {
		return err
	}

	return os.Rename(temporary.Name(), path)
}
