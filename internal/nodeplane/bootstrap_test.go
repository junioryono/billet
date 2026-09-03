package nodeplane_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/wirecert"
)

// blockingEnrollments holds every request inside the handler until it is
// released, which is what makes the concurrency bound observable at all.
type blockingEnrollments struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingEnrollments) RequestEnrollmentWithToken(
	ctx context.Context, name, fingerprint, csrPEM, _ string,
) (alloc.Enrollment, error) {
	b.entered <- struct{}{}

	select {
	case <-b.release:
	case <-ctx.Done():
		return alloc.Enrollment{}, ctx.Err()
	}

	return alloc.Enrollment{
		Name: name, Fingerprint: fingerprint, CSRPEM: csrPEM, State: alloc.EnrollPending,
	}, nil
}

func (b *blockingEnrollments) LookupEnrollment(
	context.Context, string,
) (alloc.Enrollment, bool, error) {
	return alloc.Enrollment{}, false, nil
}

// testAuthority is an authority with nothing else attached to it.
func testAuthority(t *testing.T) *wirecert.CA {
	t.Helper()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	return ca
}

// THE NODE WIRE SERVES NO ROUTE A STRANGER CAN USE, and that absence is what
// lets the listener in front of it demand a certificate in the handshake.
//
// These two used to be registered here. While they were, an anonymous caller
// could complete a handshake and hold a connection out of the same budget real
// nodes draw on — a few requests a second and no node is accepted at all.
// Moving them was the fix, so a route creeping back is the regression.
func TestTheNodeWireServesNoEnrollmentRoute(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute)

	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil,
		nodeplane.WithRenewal(testAuthority(t)),
		nodeplane.WithEnrollment(&blockingEnrollments{})))
	t.Cleanup(srv.Close)

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/ca"},
		{http.MethodPost, "/v1/enroll"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), route.method, srv.URL+route.path,
			strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build %s: %v", route.path, err)
		}

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", route.path, err)
		}

		res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Errorf("the node wire answered %s to %s %s; it must not serve a route that "+
				"admits a caller with no certificate", res.Status, route.method, route.path)
		}
	}
}

// AND THE ENROLLMENT WIRE SERVES NOTHING ELSE.
//
// Its listener asks for no certificate, so anything reachable here is reachable
// by anyone who can open a socket. Two routes that grant nothing is the entire
// contract; a lease route or a JIT route appearing on this mux would hand the
// organisation away.
func TestTheEnrollmentWireServesOnlyItsTwoRoutes(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)
	p := nodeplane.New(log, deployment, time.Minute)

	srv := httptest.NewServer(nodeplane.BootstrapHandler(log, p, ca,
		nodeplane.WithEnrollment(&blockingEnrollments{
			entered: make(chan struct{}, 1), release: closedChan(),
		})))
	t.Cleanup(srv.Close)

	caReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.URL+"/v1/ca", http.NoBody)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	res, err := srv.Client().Do(caReq)
	if err != nil {
		t.Fatalf("read the authority: %v", err)
	}

	res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("the enrollment wire answered %s for its own authority", res.Status)
	}

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/register"},
		{http.MethodPost, "/v1/nodes/epyc-1/poll"},
		{http.MethodPost, "/v1/nodes/epyc-1/withdraw"},
		{http.MethodPost, "/v1/nodes/epyc-1/jit"},
		{http.MethodPost, "/v1/nodes/epyc-1/renew"},
		{http.MethodGet, "/v1/nodes/epyc-1/launched"},
		{http.MethodPost, "/v1/nodes/epyc-1/leases/l1/bind"},
		{http.MethodPost, "/v1/nodes/epyc-1/leases/l1/release"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), route.method, srv.URL+route.path,
			strings.NewReader(`{"node":"epyc-1"}`))
		if err != nil {
			t.Fatalf("build %s: %v", route.path, err)
		}

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", route.path, err)
		}

		res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Errorf("the enrollment wire answered %s to %s %s, on a listener that asks for "+
				"no certificate", res.Status, route.method, route.path)
		}
	}
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)

	return c
}

// ENROLLMENT IS BOUNDED BY WORK IN FLIGHT, NOT ONLY BY CONNECTIONS.
//
// Recording a request begins an IMMEDIATE transaction, which takes SQLite's
// single writer connection — the one the operational wire needs too. So the
// bootstrap listener's connection cap does not, on its own, bound what this
// route can do to the rest of the control plane.
//
// REFUSED RATHER THAN QUEUED, because waiting for a permit moves the queue
// instead of bounding it, and a node polling for approval is happy to come back.
func TestEnrollmentIsBoundedByConcurrentRequests(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)

	list := &blockingEnrollments{
		entered: make(chan struct{}, 64),
		release: make(chan struct{}),
	}

	srv := httptest.NewServer(nodeplane.BootstrapHandler(log, nodeplane.New(log, deployment,
		time.Minute), ca, nodeplane.WithEnrollment(list)))
	t.Cleanup(srv.Close)

	// JOINED BEFORE THE TEST RETURNS, and released before that. Cleanups run
	// last in, first out, so the release comes before the wait and the wait
	// before httptest's own Close — otherwise the holders sit on a channel
	// nobody closes and nothing that follows can finish. A goroutine still
	// calling t methods after its test has ended is its own bug.
	var inFlight sync.WaitGroup

	t.Cleanup(inFlight.Wait)
	t.Cleanup(func() { close(list.release) })

	// CAPTURED, not asked for inside the goroutine: t.Context is a t method, and
	// these goroutines are still running while the test is being torn down.
	ctx := t.Context()

	// Each request needs its own name, or the ledger stand-in would be answering
	// about one machine asking repeatedly rather than many machines at once.
	held := 0

	for i := range nodeplaneMaxConcurrentEnrollments {
		inFlight.Add(1)

		go func() {
			defer inFlight.Done()

			res, err := enrollOnce(ctx, srv, fmt.Sprintf("epyc-%d", i))
			if err == nil {
				res.Body.Close()
			}
		}()
	}

	// EVERY PERMIT IS PROVABLY TAKEN BEFORE THE NEXT REQUEST IS MADE. Firing the
	// ninth without waiting would race the eight and pass whichever way the
	// scheduler went.
	for held < nodeplaneMaxConcurrentEnrollments {
		select {
		case <-list.entered:
			held++
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d enrollments reached the ledger",
				held, nodeplaneMaxConcurrentEnrollments)
		}
	}

	res, err := enrollOnce(t.Context(), srv, "epyc-overflow")
	if err != nil {
		t.Fatalf("the overflowing enrollment: %v", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an enrollment beyond the bound answered %s; want 503, so a caller is told "+
			"to come back rather than queued against the ledger's single writer", res.Status)
	}

	// AND IT NEVER REACHED THE LEDGER. A 503 produced after the transaction had
	// already started would be a status code with no bound behind it.
	select {
	case <-list.entered:
		t.Error("the refused enrollment still reached the ledger, so the bound is decoration")
	default:
	}
}

// nodeplaneMaxConcurrentEnrollments mirrors the package's own bound.
//
// Not exported, because nothing outside the package has a decision to make with
// it; asserted here by driving the route rather than by reading the constant, so
// a change to the number is a change to this test's arithmetic and not to what
// it proves.
const nodeplaneMaxConcurrentEnrollments = 8

func enrollOnce(ctx context.Context, srv *httptest.Server, name string) (*http.Response, error) {
	csrPEM, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		return nil, err
	}

	body := fmt.Sprintf(`{"node":%q,"csr_pem":%q,"join_token":"a-token"}`, name, string(csrPEM))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/enroll",
		strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	return srv.Client().Do(req)
}

// countingBody records how much of the request body something read.
type countingBody struct {
	io.ReadCloser
	reads *atomic.Int64
}

func (c countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.reads.Add(int64(n))

	return n, err
}

// REGISTER SETTLES THE CONNECTION BEFORE IT READS A BODY.
//
// It is the one route that names itself in the body rather than the path, so it
// cannot use forNode — and it used to decode up to a megabyte of JSON from a
// caller that had proved nothing, then ask who they were. Nothing about the
// certificate, the chain or the revocation list needs the body.
//
// COUNTED, NOT TIMED, and the obvious experiment is the one that does not work.
// Announcing a Content-Length, sending none of it and seeing whether an answer
// comes back reads as decisive and is not: MEASURED, Go's http.Server withholds
// the entire response until an announced body arrives, even for a handler that
// never touches it, so both orderings look identical from a socket. Wrapping the
// body and asking whether the HANDLER read it observes the property directly.
//
// BOTH DIRECTIONS, because a counter that can never count would make the first
// half vacuous: an authenticated registration does decode, and its read has to
// show up.
func TestRegisterRefusesAnUnauthenticatedCallerBeforeReadingItsBody(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	// Relaxed so a certless client reaches the handler at all. Production refuses
	// it in the handshake; what is under test is what the handler does with a
	// request it can see.
	conf.ClientAuth = tls.VerifyClientCertIfGiven

	var reads atomic.Int64

	wire := nodeplane.Handler(log, nodeplane.New(log, deployment, time.Minute),
		&fakeStore{}, nil, nodeplane.RequireClientCert())

	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// REPLACED ON THE REQUEST THE HANDLER SEES, not on the server's own
			// copy: http.Server keeps the original to drain after the handler
			// returns, so this counts handler reads and nothing else.
			r.Body = countingBody{ReadCloser: r.Body, reads: &reads}
			wire.ServeHTTP(w, r)
		}))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("parse the authority")
	}

	certless := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
	}}

	res, err := postRegister(t, certless, srv.URL, "epyc-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	res.Body.Close()

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a caller with no certificate got %s; want 401", res.Status)
	}

	if n := reads.Load(); n != 0 {
		t.Errorf("register read %d bytes of an unproven caller's body before refusing it", n)
	}

	// AND THE COUNTER WORKS. Without this the assertion above passes against a
	// body nothing could have read either way.
	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	enrolled := &http.Client{Transport: &http.Transport{TLSClientConfig: clientConf}}

	accepted, err := postRegister(t, enrolled, srv.URL, "epyc-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	accepted.Body.Close()

	if reads.Load() == 0 {
		t.Error("an authenticated registration read nothing, so the count above proves nothing")
	}
}

func postRegister(t *testing.T, c *http.Client, base, node string) (*http.Response, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/register",
		strings.NewReader(fmt.Sprintf(`{"node":%q}`, node)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	return c.Do(req)
}

// A GUARD DELETED IS NOT A GUARD MOVED.
//
// authenticated and claims are one check split in two so register can run half
// of it early. Both halves still have to refuse: a valid certificate naming a
// different node is not permission to act as that node, and splitting the
// function is exactly the edit that could lose the second half.
func TestAValidCertificateStillCannotActForAnotherNode(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	srv := httptest.NewUnstartedServer(nodeplane.Handler(log,
		nodeplane.New(log, deployment, time.Minute), &fakeStore{}, nil,
		nodeplane.RequireClientCert()))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientConf}}

	// THE BODY NAMES SOMEBODY ELSE, which is the half of the check that needs the
	// body and therefore runs after the decode.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/register",
		strings.NewReader(`{"node":"epyc-2"}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a certificate for epyc-1 registered as epyc-2 and got %s; want 403", res.Status)
	}

	// AND THE PATH-NAMED ROUTES TOO, which go through the unsplit authorise.
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/nodes/epyc-2/poll", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	polled, err := client.Do(req)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	defer polled.Body.Close()

	if polled.StatusCode != http.StatusForbidden {
		t.Errorf("a certificate for epyc-1 polled as epyc-2 and got %s; want 403", polled.Status)
	}
}
