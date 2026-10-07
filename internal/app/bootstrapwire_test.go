package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// THE NODE WIRE SERVES THROUGH A DRAIN. SIGTERM cancels the context the wire was
// started with, and the wire stays up until its stop runs after the drain; a
// handshake run under that context refused every node for the whole drain
// (2026-10-03).
func TestTheNodeWireServesAfterItsContextIsCancelled(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()

	deploymentID, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{
		Listen: ":0", IdentityDir: stateDir, NodeTLSHosts: []string{"127.0.0.1"},
	}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	served, err := ServeNodeWire(ctx, cfg, noIdentityAccess,
		nodeplane.New(slog.New(slog.DiscardHandler), deploymentID, time.Minute),
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("serving the node wire: %v", err)
	}

	t.Cleanup(served.Stop)

	ca, err := wirecert.LoadOrCreateCA(stateDir, deploymentID)
	if err != nil {
		t.Fatalf("load the authority the wire minted: %v", err)
	}

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	// What SIGTERM does when a drain begins.
	cancel()

	node := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: clientConf},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://127.0.0.1"+portOf(t, served.Addr)+"/v1/register",
		strings.NewReader(`{"node":"epyc-1"}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	res, err := node.Do(req)
	if err != nil {
		t.Fatalf("an enrolled node could not reach the wire once its context was "+
			"cancelled, which is every node for the whole of a drain: %v", err)
	}

	defer res.Body.Close()

	if res.StatusCode == http.StatusUnauthorized {
		t.Errorf("the node wire told an enrolled node it had not authenticated: %s", res.Status)
	}
}

// splitWire starts a control plane with both listeners and returns their
// addresses and the authority they present.
func splitWire(t *testing.T, bootstrap string, opts ...wireOption) (string, string, *wirecert.CA) {
	t.Helper()

	stateDir := t.TempDir()

	deploymentID, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{
		// A WILDCARD, DELIBERATELY. Loopback is the one address billet serves
		// without certificates, so a loopback listen would exercise the plain-HTTP
		// path and none of this.
		Listen:          ":0",
		BootstrapListen: bootstrap,
		IdentityDir:     stateDir,
		NodeTLSHosts:    []string{"127.0.0.1"},
	}}

	wire, err := ServeNodeWire(t.Context(), cfg, noIdentityAccess,
		nodeplane.New(slog.New(slog.DiscardHandler), deploymentID, time.Minute),
		nil, nil, nil, nil, nil, opts...)
	if err != nil {
		t.Fatalf("serving the node wire: %v", err)
	}

	t.Cleanup(wire.Stop)

	ca, err := wirecert.LoadOrCreateCA(stateDir, deploymentID)
	if err != nil {
		t.Fatalf("load the authority the wire minted: %v", err)
	}

	// DIALLED BY NAME, NOT BY WILDCARD. The listen address below is ":0" so the
	// certificate rule applies at all, and the resolved address it reports is
	// ":<port>" — which names no host to connect to. 127.0.0.1 is the subject
	// name the certificate carries.
	return "127.0.0.1" + portOf(t, wire.Addr), bootstrapDialAddr(t, wire.Bootstrap), ca
}

// portOf is the ":<port>" half of a resolved listen address.
func portOf(t *testing.T, addr string) string {
	t.Helper()

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}

	return ":" + port
}

func bootstrapDialAddr(t *testing.T, addr string) string {
	t.Helper()

	if addr == "" {
		return ""
	}

	return "127.0.0.1" + portOf(t, addr)
}

// anonymousClient is one connection's worth of the shared-budget attack: a
// caller with no certificate that completes a handshake, asks for the authority,
// and then keeps its connection in the pool.
func anonymousClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// The fingerprint check is what an enrolling node verifies this
			// listener with, and this is not an enrolling node — it is a stranger.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
			// Held rather than reaped, so the slot stays occupied for the
			// assertions below.
			MaxIdleConnsPerHost: 1,
		},
	}
}

// readAuthority asks for /v1/ca and reports whether it was served, leaving the
// connection in the client's idle pool.
//
// THE BODY IS READ TO COMPLETION AND CLOSED, which is what returns the
// connection to the pool rather than tearing it down. A request abandoned
// mid-body occupies a slot in a different and shorter-lived way, and the point
// here is the keep-alive.
func readAuthority(t *testing.T, client *http.Client, addr string) bool {
	t.Helper()

	// Errorf rather than Fatalf: this runs on the goroutines that take the slots,
	// and FailNow from anywhere but the test's own goroutine is undefined.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://"+addr+"/v1/ca", http.NoBody)
	if err != nil {
		t.Errorf("build: %v", err)

		return false
	}

	res, err := client.Do(req)
	if err != nil {
		return false
	}

	defer res.Body.Close()

	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return false
	}

	return true
}

// SATURATING THE ENROLLMENT LISTENER LEAVES THE NODE WIRE SERVING. This is
// the property the split exists for, and the reason the two are separate
// listeners at all.
//
// It performs the actual attack rather than describing it: complete a handshake,
// ask /v1/ca, keep the connection. While the two routes were on the node wire,
// a few requests a second of exactly this took every node offline — the budget
// was shared, and once it is full Accept blocks before the kernel accept, so a
// healthy node's connection is never admitted at all.
//
// EQUAL, TINY BUDGETS, because at the production sizes this test cannot fail for
// the reason it is named. Filling the enrollment listener's 64 leaves 448 of a
// shared 512, so a node would be served whether the budgets were separate or not
// — the assertion would prove that 64 is a bound and nothing about independence.
// With both at the same small number, filling one exhausts a shared budget
// exactly, so the node-wire request below fails if anything is shared.
//
// Driven through serveNodeWire, because the property belongs to how the process
// assembles its listeners and a helper-level version would prove nothing about
// what billet serves.
func TestASaturatedEnrollmentListenerLeavesTheNodeWireServing(t *testing.T) {
	t.Parallel()

	const budget = 4

	wire, bootstrap, ca := splitWire(t, ":0", withConnectionLimits(budget, budget))

	// EACH CLIENT HOLDS ITS OWN CONNECTION, because one transport pools and
	// reuses a single one — which would occupy one slot however many requests it
	// made, and prove nothing about a budget.
	//
	// TAKEN CONCURRENTLY, so the whole set is established at once. Sequentially,
	// the first connection's idle window is already most of the way gone by the
	// time the last one lands, and a slow machine would start reaping slots out
	// from under the assertions below — a flake that reports the opposite of what
	// happened.
	served := make([]bool, budget)

	var takingSlots sync.WaitGroup

	// SHORTER THAN THE ENROLLMENT LISTENER'S 15s IdleTimeout, and that bound is
	// the point rather than impatience: a connection that is BLOCKED on a full
	// budget must fail here, not wait for a reap to free somebody else's slot and
	// then succeed. With a longer timeout the test still fails when the budgets
	// are shared, but it fails fifteen seconds later and for the wrong reason —
	// the classic wait satisfied by something other than the thing under test.
	for i := range served {
		client := anonymousClient(5 * time.Second)
		t.Cleanup(client.CloseIdleConnections)
		takingSlots.Add(1)

		go func() {
			defer takingSlots.Done()

			served[i] = readAuthority(t, client, bootstrap)
		}()
	}

	takingSlots.Wait()

	held := 0

	for _, ok := range served {
		if ok {
			held++
		}
	}

	if held != budget {
		t.Fatalf("only %d of %d anonymous connections were served; the listener refused one "+
			"before its own bound", held, budget)
	}

	// AND IT IS GENUINELY FULL. Without this the assertion below could pass
	// against a listener that was never saturated in the first place.
	if readAuthority(t, anonymousClient(2*time.Second), bootstrap) {
		t.Error("a connection beyond the enrollment listener's bound was still served, so " +
			"nothing here is saturated and the node-wire check proves nothing")
	}

	// THE NODE WIRE IS UNTOUCHED. Different listener, different budget — and with
	// the two budgets equal and exhausted above, this is the assertion that fails
	// if they are one.
	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	node := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: clientConf},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+wire+"/v1/register", strings.NewReader(`{"node":"epyc-1"}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	res, err := node.Do(req)
	if err != nil {
		t.Fatalf("a node could not reach the wire while anonymous traffic filled the "+
			"enrollment listener, which is the failure the split exists to prevent: %v", err)
	}

	defer res.Body.Close()

	// The registration itself is refused — this plane declares no tiers — and that
	// is fine. What is asserted is that the request was ACCEPTED and answered.
	if res.StatusCode == http.StatusUnauthorized {
		t.Errorf("the node wire told an enrolled node it had not authenticated: %s", res.Status)
	}
}

// AN UNSET ENROLLMENT ADDRESS SERVES NOTHING, which is what makes the absence a
// refusal rather than a default.
//
// A control plane with no server.bootstrap_listen does not admit machines over
// the network at all: an operator issues the bundle here and copies it out of
// band. Nothing should be listening for them to find.
func TestWithoutAnEnrollmentAddressNothingServesEnrollment(t *testing.T) {
	t.Parallel()

	wire, bootstrap, ca := splitWire(t, "")

	if bootstrap != "" {
		t.Fatalf("the fixture invented an enrollment address: %q", bootstrap)
	}

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	// ASKED WITH A CERTIFICATE, deliberately: a caller without one cannot get
	// through the handshake, so this is the only way to see what the node wire
	// actually routes. It must not be enrollment.
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: clientConf},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://"+wire+"/v1/ca", http.NoBody)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("read the authority: %v", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("the node wire answered %s for /v1/ca; enrollment must live on its own "+
			"listener or nowhere", res.Status)
	}
}

// THE NODE WIRE BOUNDS A HANDSHAKE.
//
// A caller that opens a socket and sends nothing occupies one of the listener's
// pending-handshake slots until something ends it. That bound used to be DERIVED
// — Go's Server.tlsHandshakeTimeout is the minimum of the POSITIVE
// ReadHeaderTimeout, ReadTimeout and WriteTimeout, and the node wire sets no
// WriteTimeout because a command poll is a long poll, so zeroing the other two
// would have made it unlimited. handshakingListener sets handshakeTimeout
// explicitly instead, because a number that decides how expensive an attack is
// should not be an emergent property of three unrelated settings.
//
// Asserted by observation, on the listener the process really builds: a socket
// that says nothing is closed rather than held.
func TestTheNodeWireBoundsATLSHandshake(t *testing.T) {
	t.Parallel()

	wire, _, _ := splitWire(t, "", withHandshakeTimeout(testHandshakeTimeout))

	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp", wire)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	defer conn.Close()

	// MANY TIMES THE BOUND, so a pass means the server ended the connection
	// rather than the test giving up — and short enough that a hang is reported
	// promptly. At the production five seconds this waited thirty, and MEASURED,
	// that was not enough headroom under a full -race suite: it failed there and
	// passed in five seconds on its own, which is a test reporting the machine's
	// load rather than the code.
	if err := conn.SetReadDeadline(time.Now().Add(60 * testHandshakeTimeout)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	// NOTHING IS SENT. The read returns when the server gives up on the
	// handshake and closes; a timeout here means it never would have.
	buf := make([]byte, 1)

	start := time.Now()

	if _, err := conn.Read(buf); err == nil {
		t.Fatal("a connection that sent nothing was answered")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a connection that sent nothing held its slot for %s; the node wire's "+
			"handshake bound is gone", time.Since(start).Round(time.Millisecond))
	}
}

// TestTheServedWireCarriesTheFleetOntoTheNewAuthority closes the last seam
// in serving the fleet from one read of the authority, and the only one that
// can be closed here.
//
// BuildNodeWire's tests prove the handler it BUILDS is right. What it cannot prove is
// that ServeNodeWire INSTALLS it: rebuilding a bare nodeplane.Handler at the
// http.Server would drop the certificate guard, revocation, renewal and the trust
// bundle in one line, and every test in that package would stay green. This
// drives the real ServeNodeWire, over a real handshake, on a deployment mid
// rotation.
//
// RENEWAL IS THE ASSERTION because it is the only way a node ever adopts the new
// authority — the server keeps presenting the old one until every host has, and
// `billet ca retire` is safe only once they all did. A renewal signed by the
// authority being retired means no node ever moves.
func TestTheServedWireCarriesTheFleetOntoTheNewAuthority(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()

	deploymentID, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	old, err := wirecert.LoadOrCreateCA(stateDir, deploymentID)
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	// The node as it stands before renewing: enrolled under the old authority.
	enrolled, err := old.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	fresh, err := wirecert.Rotate(t.Context(), stateDir, deploymentID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{
		Listen:       ":0",
		IdentityDir:  stateDir,
		NodeTLSHosts: []string{"127.0.0.1"},
	}}

	wire, err := ServeNodeWire(t.Context(), cfg, noIdentityAccess,
		nodeplane.New(slog.New(slog.DiscardHandler), deploymentID, time.Minute),
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("serving the node wire: %v", err)
	}

	t.Cleanup(wire.Stop)

	// WHAT THE NODE ALREADY TRUSTS, which during an overlap is the old authority
	// alone — it has not renewed yet, so it has never been handed the new one.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(old.CertPEM()) {
		t.Fatal("parse the old authority")
	}

	cert, err := tls.X509KeyPair(enrolled.CertPEM, enrolled.KeyPEM)
	if err != nil {
		t.Fatalf("load the node keypair: %v", err)
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		MinVersion:   tls.VersionTLS13,
	}}

	t.Cleanup(transport.CloseIdleConnections)

	csrPEM, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}

	body, err := json.Marshal(nodeapi.RenewRequest{CSRPEM: string(csrPEM)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	url := "https://127.0.0.1" + portOf(t, wire.Addr) + "/v1/nodes/epyc-1/renew"

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatalf("POST renew: %v", err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST renew = %d; a node holding a certificate from the previous authority "+
			"could not renew over the served wire", res.StatusCode)
	}

	var got struct {
		CertPEM string `json:"cert_pem"`
		CAPEM   string `json:"ca_pem"`
	}

	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	renewed, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: []byte(got.CertPEM)})
	if err != nil {
		t.Fatalf("parse the renewed certificate: %v", err)
	}

	// AGAINST THE NEW AUTHORITY ALONE. Against the bundle it would pass either
	// way, which is the assertion that proves nothing.
	newOnly := x509.NewCertPool()
	if !newOnly.AppendCertsFromPEM(fresh.CertPEM()) {
		t.Fatal("parse the new authority")
	}

	if _, err := renewed.Verify(x509.VerifyOptions{
		Roots: newOnly, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("the served wire signed a renewal with an authority the fleet is supposed to "+
			"be moving OFF, so the overlap can never end: %v", err)
	}

	// AND IT HANDS BACK BOTH, because the node writes this over its own ca.crt:
	// with the new authority alone it can no longer verify a control plane that
	// is still presenting the old one.
	for name, ca := range map[string]*wirecert.CA{"previous": old, "new": fresh} {
		if !strings.Contains(got.CAPEM, string(ca.CertPEM())) {
			t.Errorf("the renewal answer omits the %s authority", name)
		}
	}
}

// noIdentityAccess is an IdentityAccess that holds nothing: these tests are
// about the listeners, and the host's exclusion around the authority read is
// cmd/billet's, tested there.
func noIdentityAccess(context.Context, string) (func() error, error) {
	return func() error { return nil }, nil
}

// A REFUSAL RELEASES THE IDENTITY ACCESS IT TOOK. A wildcard listen address
// with no node_tls_hosts says nothing about the name a node dials, so the wire
// refuses to start; the exclusion it took around the authority read is given
// back on that path too, or the next command on this host waits on a lock no
// process will release.
func TestARefusedNodeWireReleasesItsIdentityAccess(t *testing.T) {
	t.Parallel()

	var taken, released int

	access := func(context.Context, string) (func() error, error) {
		taken++

		return func() error {
			released++

			return nil
		}, nil
	}

	cfg := &config.Config{Server: &config.ServerConfig{Listen: "0.0.0.0:0", IdentityDir: t.TempDir()}}

	wire, err := ServeNodeWire(t.Context(), cfg, access,
		nodeplane.New(slog.New(slog.DiscardHandler), "0123456789abcdef0123456789abcdef", time.Minute),
		nil, nil, nil, nil, nil)
	if err == nil {
		wire.Stop()
		t.Fatal("a wildcard listen address with no node_tls_hosts was served")
	}

	if !strings.Contains(err.Error(), "node_tls_hosts") {
		t.Errorf("the refusal does not name what to set: %v", err)
	}

	if taken != 1 || released != 1 {
		t.Errorf("the identity access was taken %d times and released %d, want once each", taken, released)
	}
}
