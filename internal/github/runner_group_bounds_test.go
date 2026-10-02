package github

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// policyGuard is how long a test waits for a call whose bound under test is a
// fraction of it. It is a failure deadline, never a condition: a call that is
// bounded returns long before it, and one that is not never returns at all.
const policyGuard = 10 * time.Second

// returnsWithin runs call and fails the test if it has not returned by
// policyGuard.
func returnsWithin(t *testing.T, call func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- call() }()

	select {
	case err := <-done:
		return err
	case <-time.After(policyGuard):
		t.Fatalf("the call had not returned after %s; nothing bounds it", policyGuard)

		return nil
	}
}

// testBounds are the production bounds with the two timeouts a test exercises
// replaced, so the one under test fires and the other cannot.
func testBounds(responseHeader, request time.Duration) policyBounds {
	b := defaultPolicyBounds
	b.responseHeader = responseHeader
	b.request = request

	return b
}

// stallingServer answers the installation-token exchange unless stallToken is
// set and hands every other request to stall, which may block until the test
// ends. tokenSeen is signalled when a token request arrives.
func stallingServer(t *testing.T, stallToken bool,
	stall func(w http.ResponseWriter, r *http.Request, release <-chan struct{}),
) (string, <-chan struct{}) {
	t.Helper()

	release := make(chan struct{})
	tokenSeen := make(chan struct{}, 8)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		tokenSeen <- struct{}{}
		if stallToken {
			select {
			case <-release:
			case <-r.Context().Done():
			}

			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { stall(w, r, release) })

	srv := httptest.NewServer(mux)
	// LIFO: the handlers are released before Close waits for them.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	return srv.URL, tokenSeen
}

// policyOperations are every request-making method of the policy client, each
// asked of a group or runner the fake would otherwise answer for.
var policyOperations = []struct {
	name string
	call func(ctx context.Context, c *runnerGroupPolicyClient) error
}{
	{"FindRunnerGroupID", func(ctx context.Context, c *runnerGroupPolicyClient) error {
		_, _, err := c.FindRunnerGroupID(ctx, "trusted")

		return err
	}},
	{"ValidateTrustedRunnerGroup", func(ctx context.Context, c *runnerGroupPolicyClient) error {
		return c.ValidateTrustedRunnerGroup(ctx, 7, []string{"acme/api/.github/workflows/ci.yml@refs/heads/main"})
	}},
	{"ValidateRunnerGroupReach", func(ctx context.Context, c *runnerGroupPolicyClient) error {
		return c.ValidateRunnerGroupReach(ctx, 7)
	}},
	{"InspectScaleSetRunner", func(ctx context.Context, c *runnerGroupPolicyClient) error {
		got, err := c.InspectScaleSetRunner(ctx, "runner-1", 41)
		if err == nil {
			return fmt.Errorf("a stalled listing was read as %+v", got)
		}

		return err
	}},
	{"WorkflowRun", func(ctx context.Context, c *runnerGroupPolicyClient) error {
		_, err := c.WorkflowRun(ctx, "acme", "api", 31)

		return err
	}},
	{"DefaultBranch", func(ctx context.Context, c *runnerGroupPolicyClient) error {
		_, err := c.DefaultBranch(ctx, "acme", "api")

		return err
	}},
}

// assertUndecided holds that err is could-not-tell and neither of the verdicts
// a policy question can otherwise reach.
func assertUndecided(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("a request GitHub never answered returned no error")
	}

	if !Undecided(err) {
		t.Errorf("Undecided(%v) = false; GitHub not answering is could-not-tell, never a refusal", err)
	}

	if errors.Is(err, ErrRunnerGroupNotFound) {
		t.Errorf("a request GitHub never answered reported the group absent: %v", err)
	}
}

// THE POLICY CLIENT IS BUILT ON A CLIENT OF ITS OWN. http.DefaultClient has no
// timeout and its transport is shared by the whole process, so a half-open
// connection on it stalls every policy read, and with it every JIT mint.
func TestThePolicyClientIsBuiltOnItsOwnBoundedTransport(t *testing.T) {
	t.Parallel()

	build := func() *runnerGroupPolicyClient {
		c, ok := NewRunnerGroupPolicyClientAt("", OrganizationTarget("acme"), 1, 2, nil).(*runnerGroupPolicyClient)
		if !ok {
			t.Fatal("NewRunnerGroupPolicyClientAt no longer returns the concrete client")
		}

		return c
	}

	c := build()
	if c.client == nil || c.client == http.DefaultClient {
		t.Fatalf("the policy client runs on %p, the process-wide default client is %p", c.client, http.DefaultClient)
	}

	if c.client.Timeout != defaultPolicyBounds.request || c.request != defaultPolicyBounds.request {
		t.Errorf("overall bound %s and per-request bound %s, want both %s",
			c.client.Timeout, c.request, defaultPolicyBounds.request)
	}

	transport, ok := c.client.Transport.(*http.Transport)
	if !ok || transport == http.DefaultTransport {
		t.Fatalf("transport = %T %p; want an *http.Transport of its own", c.client.Transport, c.client.Transport)
	}

	if transport.DialContext == nil {
		t.Error("no dialer is set, so a dial takes the default transport's bound or none")
	}

	if transport.TLSHandshakeTimeout != defaultPolicyBounds.tlsHandshake ||
		transport.ResponseHeaderTimeout != defaultPolicyBounds.responseHeader {
		t.Errorf("TLS handshake %s and response header %s, want %s and %s",
			transport.TLSHandshakeTimeout, transport.ResponseHeaderTimeout,
			defaultPolicyBounds.tlsHandshake, defaultPolicyBounds.responseHeader)
	}

	for name, d := range map[string]time.Duration{
		"dial": defaultPolicyBounds.dial, "TLS handshake": defaultPolicyBounds.tlsHandshake,
		"response header": defaultPolicyBounds.responseHeader, "request": defaultPolicyBounds.request,
		"ping after": defaultPolicyBounds.pingAfter, "ping timeout": defaultPolicyBounds.pingTimeout,
	} {
		if d <= 0 {
			t.Errorf("the %s bound is %s, which bounds nothing", name, d)
		}
	}

	if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout != defaultPolicyBounds.pingAfter ||
		transport.HTTP2.PingTimeout != defaultPolicyBounds.pingTimeout {
		t.Errorf("HTTP/2 health check = %+v, want a ping after %s answered within %s",
			transport.HTTP2, defaultPolicyBounds.pingAfter, defaultPolicyBounds.pingTimeout)
	}

	if other := build(); other.client == c.client || other.client.Transport == c.client.Transport {
		t.Error("two policy clients share an HTTP client or transport, so one target's stalled " +
			"connection is another's")
	}
}

// A SERVER THAT NEVER ANSWERS. The connection is accepted and nothing is ever
// written back, so only the request's overall bound can end the exchange.
func TestAPolicyServerThatNeverAnswersIsUndecided(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		var held []net.Conn
		defer func() {
			for _, conn := range held {
				_ = conn.Close()
			}
		}()

		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()

	key, _ := testKeyPKCS1(t)
	c := newRunnerGroupPolicyClient(testBounds(time.Hour, 200*time.Millisecond), "http://"+listener.Addr().String(),
		OrganizationTarget("acme"), 11, 22, key)

	err = returnsWithin(t, func() error {
		_, _, err := c.FindRunnerGroupID(t.Context(), "trusted")

		return err
	})
	assertUndecided(t, err)
}

// A SLOW HEADER. The token is answered and every policy read is accepted and
// never given a status line; the overall bound is an hour, so only the
// transport's response-header bound can end it.
func TestASlowPolicyHeaderIsUndecided(t *testing.T) {
	t.Parallel()

	for _, op := range policyOperations {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			base, _ := stallingServer(t, false, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})

			key, _ := testKeyPKCS1(t)
			c := newRunnerGroupPolicyClient(testBounds(200*time.Millisecond, time.Hour), base,
				OrganizationTarget("acme"), 11, 22, key)

			err := returnsWithin(t, func() error { return op.call(t.Context(), c) })
			assertUndecided(t, err)

			if err != nil && !strings.Contains(err.Error(), "awaiting response headers") {
				t.Errorf("err = %v; want the response-header bound to be what ended it", err)
			}
		})
	}
}

// A BODY THAT STALLS after the status line. The client's overall bound covers
// the read, and a body cut off is not an answer.
func TestAStalledPolicyBodyIsUndecided(t *testing.T) {
	t.Parallel()

	for _, op := range policyOperations {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			base, _ := stallingServer(t, false, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"runner_groups":[`)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})

			key, _ := testKeyPKCS1(t)
			c := newRunnerGroupPolicyClient(testBounds(time.Hour, 200*time.Millisecond), base,
				OrganizationTarget("acme"), 11, 22, key)

			err := returnsWithin(t, func() error { return op.call(t.Context(), c) })
			assertUndecided(t, err)

			if !errors.Is(err, errNoAnswer) {
				t.Errorf("err = %v; want the broken-off body named as no answer", err)
			}
		})
	}
}

// AN OVERSIZED BODY is refused whole. Its first megabyte is a valid listing
// naming the group, followed by padding and a second document, so a reader that
// truncated at the bound and decoded what it kept would accept an answer the
// whole response does not give.
func TestAnOversizedPolicyBodyIsUndecided(t *testing.T) {
	t.Parallel()

	listing := `{"runner_groups":[{"id":7,"name":"trusted","default":false}]}`
	body := listing + strings.Repeat(" ", maxPolicyResponse) + `{"runner_groups":[]}`

	base, _ := stallingServer(t, false, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		fmt.Fprint(w, body)
	})

	key, _ := testKeyPKCS1(t)
	c := newRunnerGroupPolicyClient(defaultPolicyBounds, base, OrganizationTarget("acme"), 11, 22, key)

	id, _, err := c.FindRunnerGroupID(t.Context(), "trusted")
	if err == nil {
		t.Fatalf("an oversized listing was read as group %d", id)
	}

	assertUndecided(t, err)

	if !errors.Is(err, errNoAnswer) {
		t.Errorf("err = %v; want the oversized body named as no answer", err)
	}
}

// A CALLER WAITING BEHIND A STALLED TOKEN EXCHANGE gives up when its own
// context does. The exchange is admitted one at a time; under a mutex the second
// caller waited out the first one's whole bound, whatever its own deadline.
func TestAWaitForTheTokenExchangeEndsWithTheCallersContext(t *testing.T) {
	t.Parallel()

	base, tokenSeen := stallingServer(t, true, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	key, _ := testKeyPKCS1(t)
	c := newRunnerGroupPolicyClient(testBounds(time.Hour, time.Hour), base, OrganizationTarget("acme"), 11, 22, key)

	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, _, err := c.FindRunnerGroupID(ctx, "trusted")
		first <- err
	}()
	t.Cleanup(func() {
		cancel()
		<-first
	})

	select {
	case <-tokenSeen:
	case <-time.After(policyGuard):
		t.Fatal("the first caller's token exchange never reached the server")
	}

	err := returnsWithin(t, func() error {
		waiting, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer stop()

		_, _, err := c.FindRunnerGroupID(waiting, "trusted")

		return err
	})
	assertUndecided(t, err)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want the waiting caller's own deadline", err)
	}
}

// NO PRODUCTION CODE IN THIS PACKAGE REACHES THE PROCESS-WIDE DEFAULTS, and the
// policy client sends every request through exchange, the one place its bounds
// are applied. doWithTimeout is the shape that is refused: given a client it
// adds nothing, and given none it builds one on the default transport.
func TestThePolicyClientReachesNoProcessWideDefault(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	methods := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "http" && (sel.Sel.Name == "DefaultClient" || sel.Sel.Name == "DefaultTransport") {
				t.Errorf("%s reaches http.%s", fset.Position(sel.Pos()), sel.Sel.Name)
			}

			return true
		})

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if recv, ok := star.X.(*ast.Ident); !ok || recv.Name != "runnerGroupPolicyClient" {
				continue
			}
			methods++

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch f := call.Fun.(type) {
				case *ast.Ident:
					if f.Name == "doWithTimeout" {
						t.Errorf("%s: %s sends through doWithTimeout, outside the policy client's bounds",
							fset.Position(call.Pos()), fn.Name.Name)
					}
				case *ast.SelectorExpr:
					if f.Sel.Name == "Do" && fn.Name.Name != "exchange" {
						t.Errorf("%s: %s sends a request itself rather than through exchange",
							fset.Position(call.Pos()), fn.Name.Name)
					}
				}

				return true
			})
		}
	}

	// The walk found the client: a rename must not turn this into a test of nothing.
	if methods < len(policyOperations) {
		t.Fatalf("found %d methods on runnerGroupPolicyClient; the walk no longer sees the client", methods)
	}
}
