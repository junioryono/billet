package nodeclient_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// The record's drain, failpoint, structural and request-base fixtures of PR 6b
// commit 3 (fixtures-c3.md, R3, R4, R6, N3). Serial: package seams.

const pollPath = "/v1/nodes/n1/poll"

func (f *fakeCompute) setHolding(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.holding = v
}

// countingInstall wraps the production installation call and counts it.
func countingInstall(t *testing.T) *atomic.Int32 {
	t.Helper()

	var installs atomic.Int32

	production := nodeclient.DefaultInstallRecord()

	t.Cleanup(nodeclient.SetInstallRecordForTest(func(d, name string, write func(io.Writer) error) (string, error) {
		installs.Add(1)

		return production(d, name, write)
	}))

	return &installs
}

// answerUnregistered answers the next n polls with the plane's unregistered
// code, which is what sends a draining node back to registration.
func (h *recordHarness) answerPollsUnregistered(t *testing.T, n int32) {
	t.Helper()

	var left atomic.Int32

	left.Store(n)

	h.mu.Lock()
	defer h.mu.Unlock()

	h.answer[pollPath] = func(w http.ResponseWriter, _ *http.Request) bool {
		if left.Add(-1) < 0 {
			return false
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		writeBody(t, w, `{"code": "`+nodeapi.CodeUnregistered+`", "message": "who"}`)

		return true
	}
}

// R3: THE DRAIN BOUNDARY, ISOLATED. A drain's re-registration on unregistered
// publishes exactly once; a failed one publishes nothing; supersession
// answered by the registration itself, and by a poll, publishes nothing,
// hands custody over, ends the worker, and attempts nothing more.
func TestADrainsReRegistrationPublishesAndSupersessionDoesNot(t *testing.T) {
	for _, supersededBy := range []string{"the registration", "a poll"} {
		t.Run("superseded by "+supersededBy, func(t *testing.T) {
			h := newRecordHarness(t)
			_, path := recordDir(t)

			installs := countingInstall(t)

			var clock atomic.Int64

			clock.Store(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC).UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }

			done := make(chan struct{})
			t.Cleanup(nodeclient.SetDrainServingDoneForTest(func() { close(done) }))

			compute := &fakeCompute{}
			c := h.client(t)

			ctx, cancel := context.WithCancel(t.Context())
			loopDone := make(chan error, 1)

			go func() {
				loopDone <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
					VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
					GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
					Log: slog.New(slog.DiscardHandler), Backoff: 20 * time.Millisecond,
					RegistrationRecordPath: path, Now: now, DrainTimeout: time.Hour,
				})
			}()

			t.Cleanup(func() {
				cancel()
				compute.setHolding(false)
				<-loopDone
			})

			// The initial record, then the node holds compute and is told to stop:
			// it drains, polling for the destroy that would free it.
			initial := awaitRecord(t, path, "")
			compute.setHolding(true)

			pollGate := h.hold(pollPath)
			regGate := h.hold("/v1/register")

			// EVERY BASELINE PRECEDES THE ACTION THAT MOVES IT: the drain's first
			// poll can arrive before a count taken after the stop.
			polls := h.count(pollPath)
			cancel()

			// The drain's first poll waits at the gate.
			waitFor(t, func() bool { return h.count(pollPath) > polls })

			baseline := installs.Load()
			inode := inodeOf(t, path)

			// UNREGISTERED: released one poll answered unregistered; the worker
			// re-registers, held at the gate, released under an advanced clock;
			// exactly one publication, read once the worker is back at its next poll.
			h.answerPollsUnregistered(t, 1)
			clock.Store(now().Add(time.Minute).UnixNano())
			polls = h.count(pollPath)
			regs := h.count("/v1/register")
			pollGate <- struct{}{}
			waitFor(t, func() bool { return h.count("/v1/register") > regs })
			regGate <- struct{}{}

			waitFor(t, func() bool { return h.count(pollPath) > polls })

			if got := installs.Load(); got != baseline+1 {
				t.Fatalf("the drain's re-registration published %d times, want once", got-baseline)
			}

			rec := readRecord(t, path)
			if rec["incarnation"] != initial["incarnation"] || rec["endpoint"] != initial["endpoint"] ||
				rec["registered_at"] != now().Format(time.RFC3339Nano) || inodeOf(t, path) == inode {
				t.Errorf("the drain's publication reads %v (initial %v)", rec, initial)
			}

			baseline, inode = installs.Load(), inodeOf(t, path)
			body, err := os.ReadFile(path)
			mustOK(t, err)

			// A FAILED RE-REGISTRATION publishes nothing.
			h.answerRegister(func(w http.ResponseWriter, _ *http.Request) bool {
				http.Error(w, "not yet", http.StatusServiceUnavailable)

				return true
			})
			h.answerPollsUnregistered(t, 1)
			polls = h.count(pollPath)
			regs = h.count("/v1/register")
			pollGate <- struct{}{}
			waitFor(t, func() bool { return h.count("/v1/register") > regs })
			regGate <- struct{}{}

			// The failure backs off and polls again.
			waitFor(t, func() bool { return h.count(pollPath) > polls })

			if installs.Load() != baseline || inodeOf(t, path) != inode {
				t.Errorf("a failed drain registration published")
			}

			// SUPERSEDED, by the registration or by a poll: the worker ends with
			// custody handed over, nothing published, and no further attempt.
			superseded := func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				writeBody(t, w, `{"code": "`+nodeapi.CodeSuperseded+`", "message": "another"}`)
			}

			switch supersededBy {
			case "the registration":
				h.answerRegister(func(w http.ResponseWriter, _ *http.Request) bool {
					superseded(w)

					return true
				})
				h.answerPollsUnregistered(t, 1)
				regs = h.count("/v1/register")
				pollGate <- struct{}{}
				waitFor(t, func() bool { return h.count("/v1/register") > regs })
				regGate <- struct{}{}
			case "a poll":
				h.mu.Lock()
				h.answer[pollPath] = func(w http.ResponseWriter, _ *http.Request) bool {
					superseded(w)

					return true
				}
				h.mu.Unlock()
				pollGate <- struct{}{}
			}

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the drain's worker did not end on supersession")
			}

			regsAfter := h.count("/v1/register")

			if compute.supersededCount() != 1 {
				t.Errorf("custody was handed over %d times, want once", compute.supersededCount())
			}

			if installs.Load() != baseline || inodeOf(t, path) != inode {
				t.Errorf("supersession published")
			}

			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, body) {
				t.Errorf("the record's bytes changed: %q, %v", after, err)
			}

			// NO ADDITIONAL ATTEMPT after the worker ended, read with the poll
			// gate still closed.
			time.Sleep(50 * time.Millisecond)

			if h.count("/v1/register") != regsAfter {
				t.Errorf("a registration was attempted after supersession ended the worker")
			}
		})
	}
}

// shortWriter accepts half of what it is given and then fails, the way a full
// disk does, BENEATH the record's unchanged write callback.
type shortWriter struct {
	w   io.Writer
	max int
	n   int
}

func (s *shortWriter) Write(p []byte) (int, error) {
	if s.n >= s.max {
		return 0, syscall.ENOSPC
	}

	take := min(len(p), s.max-s.n)

	n, err := s.w.Write(p[:take])
	s.n += n

	if err != nil {
		return n, err
	}

	if take < len(p) {
		return n, syscall.ENOSPC
	}

	return n, nil
}

// capturingLog records every log line's message and attributes.
type capturingLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturingLog) Enabled(context.Context, slog.Level) bool { return true }
func (c *capturingLog) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capturingLog) WithGroup(string) slog.Handler            { return c }

func (c *capturingLog) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()

		return true
	})
	c.lines = append(c.lines, line)

	return nil
}

func (c *capturingLog) count(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0

	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}

	return n
}

// R4: FAILPOINTS, ONE PER OPERATION. A replacement that fails at any step up to
// the rename leaves the previous record and no temporary; a failed directory
// sync keeps the new file and reports; in every case the node keeps serving,
// the failure is logged with the path and the operation, and nothing retries
// without another registration.
func TestARecordThatCannotBeWrittenIsLoggedAndTheNodeKeepsServing(t *testing.T) {
	boom := errors.New("injected")

	type failpoint struct {
		install func(dir, name string, write func(io.Writer) error) (string, error)
		keeps   bool // the new file lands (the directory sync's failure)
	}

	installerFailing := func(set func(*durablefile.Installer)) func(dir, name string, write func(io.Writer) error) (string, error) {
		var inst durablefile.Installer

		set(&inst)

		return func(dir, name string, write func(io.Writer) error) (string, error) {
			return inst.Install(dir, name, 0o600, write)
		}
	}

	failpoints := map[string]failpoint{
		"the write": {install: func(dir, name string, write func(io.Writer) error) (string, error) {
			return durablefile.Installer{}.Install(dir, name, 0o600, func(w io.Writer) error {
				return write(&shortWriter{w: w, max: 40})
			})
		}},
		"the mode":      {install: installerFailing(func(i *durablefile.Installer) { i.SetMode = func(*os.File, os.FileMode) error { return boom } })},
		"the file sync": {install: installerFailing(func(i *durablefile.Installer) { i.SyncFile = func(*os.File) error { return boom } })},
		"the rename":    {install: installerFailing(func(i *durablefile.Installer) { i.Rename = func(string, string) error { return boom } })},
		"the close": {install: func(dir, name string, write func(io.Writer) error) (string, error) {
			// The close seam is the installer's own (its package proves it);
			// here the installation call returns the installer's close error.
			return "", fmt.Errorf("durablefile: cannot close the staged %s: %w", filepath.Join(dir, name), boom)
		}},
		"the create": {install: func(dir, name string, write func(io.Writer) error) (string, error) {
			return "", fmt.Errorf("durablefile: cannot stage a file in %s: %w", dir, syscall.ENOSPC)
		}},
		"the directory sync": {install: installerFailing(func(i *durablefile.Installer) { i.SyncDir = func(string) error { return boom } }), keeps: true},
	}

	for name, fp := range failpoints {
		t.Run(name, func(t *testing.T) {
			h := newRecordHarness(t)
			dir, path := recordDir(t)

			previous := []byte(`{"schema":1,"node":"n1","deployment":"x","incarnation":"old","invocation_id":"","endpoint":"http://x:1","registered_at":"2026-01-01T00:00:00Z"}` + "\n")
			mustOK(t, os.WriteFile(path, previous, 0o600))
			previousIno := inodeOf(t, path)

			var attempts atomic.Int32

			t.Cleanup(nodeclient.SetInstallRecordForTest(func(d, n string, write func(io.Writer) error) (string, error) {
				attempts.Add(1)

				return fp.install(d, n, write)
			}))

			log := &capturingLog{}
			regGate := h.hold("/v1/register")
			regArrived := h.arrivalOf("/v1/register")

			c := h.client(t)
			ctx, cancel := context.WithCancel(t.Context())
			loopDone := make(chan error, 1)

			go func() {
				loopDone <- nodeclient.Run(ctx, c, &fakeCompute{}, nodeclient.LoopOptions{
					VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
					GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
					Log: slog.New(log), Backoff: 20 * time.Millisecond, RegistrationRecordPath: path,
				})
			}()

			t.Cleanup(func() {
				cancel()
				<-loopDone
			})

			<-regArrived
			regGate <- struct{}{}

			waitFor(t, func() bool { return attempts.Load() == 1 })

			// THE NODE KEEPS SERVING: a poll lands after the failure.
			polls := h.count(pollPath)
			waitFor(t, func() bool { return h.count(pollPath) > polls })

			if fp.keeps {
				if body, err := os.ReadFile(path); err != nil || bytes.Equal(body, previous) {
					t.Errorf("a failed directory sync did not leave the new file: %q, %v", body, err)
				}
			} else {
				if body, err := os.ReadFile(path); err != nil || !bytes.Equal(body, previous) || inodeOf(t, path) != previousIno {
					t.Errorf("%s: the previous record was not left intact: %q, %v", name, body, err)
				}

				requireOnlyCurrent(t, dir)
			}

			if log.count("could not record this registration") != 1 || log.count("path="+path) != 1 {
				t.Errorf("%s: the failure was logged %d times naming the path %d times", name,
					log.count("could not record this registration"), log.count("path="+path))
			}

			// NO RETRY WITHOUT ANOTHER REGISTRATION: the registration gate is
			// closed, and time passes.
			time.Sleep(100 * time.Millisecond)

			if attempts.Load() != 1 {
				t.Errorf("%s: the write was retried %d times without a registration", name, attempts.Load()-1)
			}
		})
	}

	t.Run("a directory that is a symlink, a regular file, or missing", func(t *testing.T) {
		for name, plant := range map[string]func(t *testing.T, dir string) string{
			"a symlink": func(t *testing.T, dir string) string {
				t.Helper()

				realDir := filepath.Join(filepath.Dir(dir), "real")
				mustOK(t, os.Mkdir(realDir, 0o700))
				mustOK(t, os.WriteFile(filepath.Join(realDir, "current"), []byte("planted\n"), 0o600))
				mustOK(t, os.Symlink(realDir, dir))

				return "is a symlink"
			},
			"a regular file": func(t *testing.T, dir string) string {
				t.Helper()
				mustOK(t, os.WriteFile(dir, []byte("not a directory"), 0o600))

				return "is not a directory"
			},
			"missing": func(*testing.T, string) string { return "examine the registration directory" },
		} {
			t.Run(name, func(t *testing.T) {
				h := newRecordHarness(t)
				dir := filepath.Join(t.TempDir(), "registration")
				path := filepath.Join(dir, "current")
				want := plant(t, dir)

				// A missing directory is the third case, so its Lstat error is the
				// expected one and every other error is a broken fixture.
				before, err := os.Lstat(dir)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}

				log := &capturingLog{}
				c := h.client(t)
				ctx, cancel := context.WithCancel(t.Context())
				loopDone := make(chan error, 1)

				go func() {
					loopDone <- nodeclient.Run(ctx, c, &fakeCompute{}, nodeclient.LoopOptions{
						VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
						GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
						Log: slog.New(log), Backoff: 20 * time.Millisecond, RegistrationRecordPath: path,
					})
				}()

				t.Cleanup(func() {
					cancel()
					<-loopDone
				})

				waitFor(t, func() bool { return log.count("could not record this registration") >= 1 })

				if log.count(want) == 0 {
					t.Errorf("%s: the diagnostic does not say %q", name, want)
				}

				after, err := os.Lstat(dir)

				switch {
				case before == nil:
					if err == nil {
						t.Errorf("%s: the directory was created", name)
					}
				case err != nil || after.Mode() != before.Mode():
					t.Errorf("%s: the obstructing object changed: %v", name, err)
				}

				if name == "a symlink" {
					if body, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "real", "current")); err != nil || string(body) != "planted\n" {
						t.Errorf("the record behind the link was touched: %q, %v", body, err)
					}
				}
			})
		}
	})
}

// R6: STRUCTURAL. The node command builds its client through the one shared
// constructor and passes the loop the one record path; no second spelling of
// the path exists; the record writer installs through the zero-value Installer.
func TestTheNodeCommandAndTheRecordWriterUseOneConstructionEach(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	main, err := parser.ParseFile(fset, "../../cmd/billet/main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var cmdNode *ast.FuncDecl

	for _, d := range main.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "cmdNode" {
			cmdNode = fn
		}
	}

	if cmdNode == nil {
		t.Fatal("cmdNode is not in cmd/billet/main.go")
	}

	var newCalls, runWithPath int

	ast.Inspect(cmdNode.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		if fn, ok := call.Fun.(*ast.SelectorExpr); ok {
			if id, ok := fn.X.(*ast.Ident); ok && id.Name == "nodeclient" && fn.Sel.Name == "New" {
				newCalls++
			}

			if id, ok := fn.X.(*ast.Ident); ok && id.Name == "nodeclient" && fn.Sel.Name == "Run" {
				for _, arg := range call.Args {
					lit, ok := arg.(*ast.CompositeLit)
					if !ok {
						continue
					}

					for _, elt := range lit.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}

						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "RegistrationRecordPath" {
							if c, ok := kv.Value.(*ast.CallExpr); ok {
								if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "nodeRegistrationRecordPath" {
									runWithPath++
								}
							}
						}
					}
				}
			}
		}

		return true
	})

	if newCalls != 0 {
		t.Errorf("cmdNode calls nodeclient.New %d times beside newNodeClientFor", newCalls)
	}

	if runWithPath != 1 {
		t.Errorf("cmdNode passes RegistrationRecordPath: nodeRegistrationRecordPath(...) to Run %d times, want once", runWithPath)
	}

	// ONE SPELLING OF THE PATH, in cmd/billet and here.
	spellings := 0

	for _, dir := range []string{"../../cmd/billet", "."} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}

			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}

			spellings += bytes.Count(body, []byte(`"/run/billet/registration/current"`))
		}
	}

	if spellings != 1 {
		t.Errorf("the record's path is spelled %d times in production code, want once", spellings)
	}

	// THE WRITER INSTALLS THROUGH THE ZERO-VALUE INSTALLER, no seam set.
	record, err := parser.ParseFile(fset, "record.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	installers := 0

	ast.Inspect(record, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}

		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Installer" {
			return true
		}

		installers++

		if len(lit.Elts) != 0 {
			t.Errorf("record.go constructs an Installer with %d fields set; the seams are the tests'", len(lit.Elts))
		}

		return true
	})

	if installers != 1 {
		t.Errorf("record.go constructs %d Installers, want one", installers)
	}
}

// recordingTransport records every request's URL and answers through the
// plane's handler in memory.
type recordingTransport struct {
	handler http.Handler
	mu      sync.Mutex
	urls    []string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.urls = append(rt.urls, req.URL.String())
	rt.mu.Unlock()

	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)

	return rec.Result(), nil
}

func (rt *recordingTransport) first() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if len(rt.urls) == 0 {
		return ""
	}

	return rt.urls[0]
}

// N3: THE REQUEST BASE IS OBSERVED, not the accessor: a client built with each
// accepted spelling registers through a recording transport, and the COMPLETE
// recorded URL is the vector table's canonical text plus /v1/register, as is
// Endpoint(); every spelling the representation refuses New refuses too.
func TestTheRequestBaseIsTheCanonicalEndpoint(t *testing.T) {
	h := newRecordHarness(t)
	handler := h.srv.Config.Handler

	for _, v := range []struct {
		input, canonical string
		tls              bool
	}{
		{"control.example:8443", "https://control.example:8443", true},
		{"control.example.:8443", "https://control.example.:8443", true},
		{"CONTROL.Example:8443", "https://control.example:8443", true},
		{"control.example", "https://control.example:443", true},
		{"control.example", "http://control.example:80", false},
		{"control.example:08443", "https://control.example:8443", true},
		{"http://control.example:443", "http://control.example:443", false},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"[2001:DB8::1]:8443", "https://[2001:db8::1]:8443", true},
		{"[::ffff:192.0.2.1]:8443", "https://192.0.2.1:8443", true},
		{"192.0.2.1.:8443", "https://192.0.2.1.:8443", true},
		{"xn--bcher-kva.example:8443", "https://xn--bcher-kva.example:8443", true},
	} {
		t.Run(v.input, func(t *testing.T) {
			opts := nodeclient.Options{Base: v.input, Node: "n1"}
			if v.tls {
				opts.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
			}

			c, err := nodeclient.New(opts)
			if err != nil {
				t.Fatalf("New(%q): %v", v.input, err)
			}

			if got := c.Endpoint().String(); got != v.canonical {
				t.Errorf("Endpoint() = %q, want %q", got, v.canonical)
			}

			rt := &recordingTransport{handler: handler}
			nodeclient.ReplaceTransportForTest(c, rt)

			// The registration is answered by the plane's handler whatever host
			// the URL names; what is observed is the URL the client built.
			_ = c.Register(t.Context(), nodeclient.Registration{ //nolint:errcheck // the plane's answer is not what this fixture reads; the URL is
				Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
				Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
			})

			if got := rt.first(); got != v.canonical+"/v1/register" {
				t.Errorf("the request went to %q, want %q", got, v.canonical+"/v1/register")
			}
		})
	}

	for _, bad := range []struct {
		input string
		tls   bool
	}{
		{"https://control.example:8443/v1", true}, {"https://u:p@control.example:8443", true},
		{"https://control.example:8443?x", true}, {"https://control.example:8443#f", true},
		{"https://control.example:8443?", true}, {"https://control.example:8443#", true},
		{"https://control.example:8443/", true}, {"[fe80::1%25eth0]:8443", true},
		{"control.example:0", true}, {"under_score.example:8443", true}, {"http://x:1", true}, {"https://x:1", false},
	} {
		opts := nodeclient.Options{Base: bad.input, Node: "n1"}
		if bad.tls {
			opts.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
		}

		if _, err := nodeclient.New(opts); err == nil {
			t.Errorf("New(%q, tls=%v) accepted a spelling the representation refuses", bad.input, bad.tls)
		}
	}
}

func mustOK(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
