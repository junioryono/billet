package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// routerNames are the names one deployment's listeners answer to behind a router.
type routerNames struct{ nodes, enroll string }

var (
	deploymentANames = routerNames{"a.nodes.billet.example", "a.enroll.billet.example"}
	deploymentBNames = routerNames{"b.nodes.billet.example", "b.enroll.billet.example"}
)

// EVERY NAME THESE TESTS DIAL RESOLVES HERE, in-process, and only those.
//
// A CLIENT SENDS THE NAME IT DIALLED, so a test of routing by name has to dial a name.
// No production seam stands in for that: nodeclient and FetchCA build their own
// transports on purpose, so what these tests exercise is the transport a real node
// uses, and the one thing swapped is what net.DefaultResolver asks. A test on one
// loopback address needs every name to reach it, which no hosts file a test may edit
// can say.
var routerResolver sync.Once

func resolveRouterNames() {
	routerResolver.Do(func() {
		net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()

			go answerDNS(server)

			return &dnsPipeConn{client}, nil
		}}
	})
}

// dnsPipeConn makes a pipe read as a packet connection, which is what tells the
// resolver one read is one whole message.
type dnsPipeConn struct{ net.Conn }

func (dnsPipeConn) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, errors.ErrUnsupported }

func (dnsPipeConn) WriteTo([]byte, net.Addr) (int, error) { return 0, errors.ErrUnsupported }

// answerDNS answers one conversation on the UDP framing: A queries under the test's
// domain with 127.0.0.1, and everything else with no record.
func answerDNS(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	for {
		query := make([]byte, 512)

		n, err := conn.Read(query)
		if err != nil || n < 17 {
			return
		}

		if _, err := conn.Write(dnsAnswer(query[:n])); err != nil {
			return
		}
	}
}

// dnsAnswer builds the response to one question, keeping the query's id and question.
func dnsAnswer(query []byte) []byte {
	end := 12
	for end < len(query) && query[end] != 0 {
		end += int(query[end]) + 1
	}

	if end+5 > len(query) {
		return query
	}

	name := dnsName(query[12 : end+1])
	kind := binary.BigEndian.Uint16(query[end+1:])
	question := query[12 : end+5]

	response := make([]byte, 12, 64)
	copy(response, query[:2])
	binary.BigEndian.PutUint16(response[2:], 0x8180)
	binary.BigEndian.PutUint16(response[4:], 1)

	const typeA = 1

	if kind == typeA && strings.HasSuffix(name, ".billet.example.") {
		binary.BigEndian.PutUint16(response[6:], 1)

		response = append(response, question...)
		response = append(response, 0xc0, 12, 0, typeA, 0, 1, 0, 0, 0, 1, 0, 4, 127, 0, 0, 1)

		return response
	}

	return append(response, question...)
}

func dnsName(wire []byte) string {
	var out strings.Builder

	for i := 0; i < len(wire) && wire[i] != 0; {
		length := int(wire[i])
		out.Write(wire[i+1 : i+1+length])
		out.WriteByte('.')
		i += length + 1
	}

	return out.String()
}

// sniRouter is the rule the reference router states, in process: it reads the name from
// a connection's ClientHello, copies bytes to the deployment that name belongs to, and
// refuses a connection with no name or one naming no deployment. It holds no key: the
// parse uses a throwaway tls.Server that stops at the hello.
type sniRouter struct {
	ln     net.Listener
	routes map[string]string

	mu      sync.Mutex
	refused []string
	conns   []net.Conn
	wg      sync.WaitGroup
}

func newSNIRouter(t *testing.T, routes map[string]string) *sniRouter {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	r := &sniRouter{ln: ln, routes: routes}

	r.wg.Go(r.serve)
	t.Cleanup(r.close)

	return r
}

func (r *sniRouter) addr() string { return r.ln.Addr().String() }

func (r *sniRouter) close() {
	_ = r.ln.Close()

	r.mu.Lock()
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()

	r.wg.Wait()
}

func (r *sniRouter) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}

		r.mu.Lock()
		r.conns = append(r.conns, conn)
		r.mu.Unlock()

		r.wg.Go(func() { r.route(conn) })
	}
}

// errHelloRead stops the throwaway handshake once the name is known.
var errHelloRead = errors.New("the hello was read")

func (r *sniRouter) route(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var (
		seen bytes.Buffer
		name string
	)

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		r.refuse("no deadline")

		return
	}

	// NOTHING IS WRITTEN BACK. The throwaway server reads through a tee and gives up
	// the moment it has the hello, which is what "reads the name in the clear" means.
	hello := tls.Server(helloConn{Conn: conn, tee: &seen}, &tls.Config{
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			name = info.ServerName

			return nil, errHelloRead
		},
		MinVersion: tls.VersionTLS12,
	})

	if err := hello.HandshakeContext(context.Background()); !errors.Is(err, errHelloRead) {
		r.refuse("no hello")

		return
	}

	backend, known := r.routes[strings.ToLower(name)]
	if !known {
		r.refuse(name)

		return
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		r.refuse(name)

		return
	}

	var d net.Dialer

	upstream, err := d.DialContext(context.Background(), "tcp", backend)
	if err != nil {
		r.refuse(name)

		return
	}

	r.mu.Lock()
	r.conns = append(r.conns, upstream)
	r.mu.Unlock()

	defer func() { _ = upstream.Close() }()

	if _, err := upstream.Write(seen.Bytes()); err != nil {
		return
	}

	done := make(chan struct{}, 2)

	go func() {
		_, _ = io.Copy(upstream, conn) //nolint:errcheck // a copy ends when a side closes, which is how this one ends
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream) //nolint:errcheck // a copy ends when a side closes, which is how this one ends
		done <- struct{}{}
	}()

	<-done
}

func (r *sniRouter) refuse(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.refused = append(r.refused, name)
}

// refusals is what the router turned away, by the name each asked for.
func (r *sniRouter) refusals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.refused...)
}

// helloConn lets a throwaway handshake read without being able to answer.
type helloConn struct {
	net.Conn
	tee *bytes.Buffer
}

func (c helloConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.tee.Write(p[:n])

	return n, err
}

func (helloConn) Write(p []byte) (int, error) { return len(p), nil }

// routedDeployment is one control plane behind a router, and its enrolled node.
type routedDeployment struct {
	stack  *stack
	names  routerNames
	node   *nodeLoop
	bundle wirecert.Bundle
}

// withTLSRouter puts a stack's control plane behind a router under names, with an
// enrollment listener, and starts no node for it: its first node comes through the router.
func withTLSRouter(names routerNames) stackOpt {
	return func(c *stackConfig) {
		c.wire = true
		c.noFirstNode = true
		c.configure = func(cfg *config.Config) {
			cfg.Server.BootstrapListen = "127.0.0.1:0"
			cfg.Server.TLSRouter = &config.TLSRouterConfig{
				NodeAddr: names.nodes + ":443", EnrollAddr: names.enroll + ":443",
			}
		}
	}
}

// enrollThroughRouter takes a machine through what `billet node --enroll` does, through
// the router, by name: read the authority against the fingerprint an operator compared,
// ask to join with a token, wait to be approved.
func enrollThroughRouter(t *testing.T, routerPort string, d *routedDeployment, nodeName string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	base := "https://" + net.JoinHostPort(d.names.enroll, routerPort)

	deployment, err := state.DeploymentID(d.stack.dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	authority, err := wirecert.LoadServing(d.stack.dir, deployment)
	if err != nil {
		t.Fatalf("LoadServing: %v", err)
	}

	caPEM, _, err := nodeclient.FetchCA(ctx, base, authority.Issuing.Fingerprint())
	if err != nil {
		t.Fatalf("read the authority through the router: %v", err)
	}

	token, err := d.stack.alloc.NewJoinToken(ctx, time.Hour, 1, "router test")
	if err != nil {
		t.Fatalf("NewJoinToken: %v", err)
	}

	csrPEM, keyPEM, err := wirecert.NewNodeCSR(nodeName)
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}

	if _, _, err := nodeclient.Enroll(ctx, base, nodeName, token, caPEM, csrPEM); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("the first ask through the router answered %v, want it waiting for approval", err)
	}

	// THE OPERATOR'S APPROVAL, by the path `billet nodes approve` takes.
	signed, err := authority.Issuing.SignNodeCSR(nodeName, csrPEM)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}

	fingerprint, err := wirecert.FingerprintOfCSR(csrPEM)
	if err != nil {
		t.Fatalf("FingerprintOfCSR: %v", err)
	}

	if err := d.stack.alloc.DecideEnrollment(ctx, nodeName, fingerprint, alloc.EnrollApproved,
		string(signed.CertPEM)); err != nil {
		t.Fatalf("DecideEnrollment: %v", err)
	}

	certPEM, signedBy, err := nodeclient.Enroll(ctx, base, nodeName, token, caPEM, csrPEM)
	if err != nil {
		t.Fatalf("collect the certificate through the router: %v", err)
	}

	d.bundle = wirecert.Bundle{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: signedBy}
}

// startRoutedNode runs a real node loop over the router, on its enrolled bundle, and
// waits for its control plane to say it registered.
func startRoutedNode(t *testing.T, routerPort string, d *routedDeployment, nodeName string) {
	t.Helper()

	tlsConf, err := wirecert.ClientTLS(d.bundle)
	if err != nil {
		t.Fatalf("ClientTLS: %v", err)
	}

	deployment, err := state.DeploymentID(d.stack.dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	d.node = startNodeLoop(t, slog.Default(), "https://"+net.JoinHostPort(d.names.nodes, routerPort), nodeProcess{
		host: nodeName, deployment: deployment, provider: d.stack.provider, kind: d.stack.kind,
		hurry: d.stack.hurry, tls: tlsConf,
	})
	d.stack.stopNode = d.node.stop
	d.stack.assembled.awaitRegistered(t, 1)
}

// runRoutedJob takes one job through a deployment behind the router, from offer to the
// lease's end, and answers the lease.
func runRoutedJob(t *testing.T, d *routedDeployment, request int64) string {
	t.Helper()

	s := d.stack

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", request, "push", simulatedTier))

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", request, "push", simulatedTier))

	names := s.awaitOneRunning(t)

	leaseID, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("instance %q does not carry a lease", names[0])
	}

	started := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0]}
	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		started.Apply(fakeactions.JobJSON("JobStarted", request, "push", simulatedTier)))

	completed := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0], Result: "succeeded"}
	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		completed.Apply(fakeactions.JobJSON("JobCompleted", request, "push", simulatedTier)))

	s.awaitGone(t)

	deadline = time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), leaseID)
		if err == nil && rec.Conclusion != "" {
			if rec.Conclusion != "done" {
				t.Fatalf("the job concluded %q", rec.Conclusion)
			}

			return leaseID
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's lease never ended: %+v, err %v", rec, err)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// dialThrough opens a mutual TLS connection through the router, as the node holding
// bundle would, and says whether its control plane took it. What refuses the stranger
// is the deployment's own handshake, seen on the first read after it, because TLS 1.3
// finishes the client's side before the server has judged its certificate.
func dialThrough(t *testing.T, routerAddr, serverName string, conf *tls.Config) error {
	t.Helper()

	conf = conf.Clone()
	conf.ServerName = serverName

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: conf,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, "tcp", routerAddr)
		}}}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(serverName, "443")+"/v1/ca", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err //nolint:wrapcheck // the test reads it whole.
	}

	_ = resp.Body.Close()

	return nil
}

// insecureHello is a client that verifies nothing, for asking what a router does with a
// name: the router is judged here, not the deployment behind it.
func insecureHello() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}
}

// exerciseTwoDeployments is the whole scenario, on whatever router it is handed: nodes
// enroll and run jobs against each of two deployments behind one address, and a node of
// one cannot reach the other's wire.
func exerciseTwoDeployments(t *testing.T, a, b *routedDeployment, routerAddr string) {
	t.Helper()

	_, port, err := net.SplitHostPort(routerAddr)
	if err != nil {
		t.Fatalf("router address: %v", err)
	}

	enrollThroughRouter(t, port, a, "router-node-a")
	enrollThroughRouter(t, port, b, "router-node-b")

	refuseAnothersJoinToken(t, port, a, b)

	startRoutedNode(t, port, a, "router-node-a")
	startRoutedNode(t, port, b, "router-node-b")

	stopA := a.stack.run(t)
	defer stopA()

	stopB := b.stack.run(t)
	defer stopB()

	leaseA := runRoutedJob(t, a, 6001)
	leaseB := runRoutedJob(t, b, 6002)

	// EACH JOB RAN ON ITS OWN DEPLOYMENT'S NODE, and its ledger knows nothing of the
	// other's: the router sent each node home.
	if rec, err := a.stack.alloc.Job(t.Context(), leaseA); err != nil || rec.Node != "router-node-a" {
		t.Errorf("deployment A's job ran on %q, %v", rec.Node, err)
	}
	if rec, err := b.stack.alloc.Job(t.Context(), leaseB); err != nil || rec.Node != "router-node-b" {
		t.Errorf("deployment B's job ran on %q, %v", rec.Node, err)
	}
	if rec, err := a.stack.alloc.Job(t.Context(), leaseB); err == nil {
		t.Errorf("deployment A's ledger holds deployment B's lease: %+v", rec)
	}

	// A NODE OF ONE DEPLOYMENT CANNOT REACH THE OTHER'S WIRE. Pointed at the other's name
	// it verifies neither way: its own bundle's authority does not sign B's certificate,
	// and with that check off, B's handshake refuses a certificate A's authority signed.
	nodeA, err := wirecert.ClientTLS(a.bundle)
	if err != nil {
		t.Fatalf("ClientTLS: %v", err)
	}

	if err := dialThrough(t, routerAddr, b.names.nodes, nodeA); err == nil {
		t.Error("a node of deployment A reached deployment B's wire, verifying its certificate")
	}

	blind := nodeA.Clone()
	blind.InsecureSkipVerify = true

	if err := dialThrough(t, routerAddr, b.names.nodes, blind); err == nil {
		t.Error("deployment B's wire served a certificate deployment A's authority signed")
	}

	// THE PROOF THE ABOVE WAS REFUSED FOR THE RIGHT REASON: the same client reaches its
	// own wire by the same path.
	if err := dialThrough(t, routerAddr, a.names.nodes, nodeA); err != nil {
		t.Errorf("a node reaching its own wire the same way failed, so the refusals prove nothing: %v", err)
	}

	// NO NAME, AND A NAME NOBODY HOLDS, ARE REFUSED AT THE ROUTER.
	for _, name := range []string{"", "nobody.billet.example"} {
		conf := insecureHello()
		conf.ServerName = name

		var d net.Dialer

		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)

		raw, err := d.DialContext(ctx, "tcp", routerAddr)
		if err != nil {
			cancel()
			t.Fatalf("dial the router: %v", err)
		}

		conn := tls.Client(raw, conf)
		err = conn.HandshakeContext(ctx)

		_ = conn.Close()

		cancel()

		if err == nil {
			t.Errorf("a connection naming %q completed a handshake through the router", name)
		}
	}
}

// refuseAnothersJoinToken presents A's join token at B's enrollment: the one credential a
// machine with no certificate has, at the deployment it was not issued for.
func refuseAnothersJoinToken(t *testing.T, routerPort string, a, b *routedDeployment) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	deployment, err := state.DeploymentID(b.stack.dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	authority, err := wirecert.LoadServing(b.stack.dir, deployment)
	if err != nil {
		t.Fatalf("LoadServing: %v", err)
	}

	base := "https://" + net.JoinHostPort(b.names.enroll, routerPort)

	caPEM, _, err := nodeclient.FetchCA(ctx, base, authority.Issuing.Fingerprint())
	if err != nil {
		t.Fatalf("read B's authority through the router: %v", err)
	}

	token, err := a.stack.alloc.NewJoinToken(ctx, time.Hour, 1, "deployment A's token")
	if err != nil {
		t.Fatalf("NewJoinToken: %v", err)
	}

	csrPEM, _, err := wirecert.NewNodeCSR("crossing-node")
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}

	_, _, err = nodeclient.Enroll(ctx, base, "crossing-node", token, caPEM, csrPEM)
	if err == nil || errors.Is(err, nodeclient.ErrNotApproved) {
		t.Errorf("deployment B took deployment A's join token: %v", err)
	}
}

// TWO DEPLOYMENTS BEHIND ONE PASSTHROUGH ROUTER, with the routing rule in process, in the
// ordinary suite. TestTheReferenceRouterConfigurationRoutesTwoDeployments runs the same
// scenario on HAProxy with the file billet ships.
func TestTwoDeploymentsServeTheirOwnNodesBehindOneRouter(t *testing.T) {
	resolveRouterNames()

	a, b := twoRoutedDeployments(t)

	router := newSNIRouter(t, map[string]string{
		a.names.nodes: a.stack.assembled.wire.Addr, a.names.enroll: a.stack.assembled.wire.Bootstrap,
		b.names.nodes: b.stack.assembled.wire.Addr, b.names.enroll: b.stack.assembled.wire.Bootstrap,
	})

	exerciseTwoDeployments(t, a, b, router.addr())

	refused := router.refusals()
	if !containsAll(refused, "", "nobody.billet.example") {
		t.Errorf("the router's refusals are %q, want the nameless and the unknown connection among them", refused)
	}
}

func containsAll(have []string, want ...string) bool {
	seen := map[string]bool{}
	for _, h := range have {
		seen[h] = true
	}

	for _, w := range want {
		if !seen[w] {
			return false
		}
	}

	return true
}

// twoRoutedDeployments is two whole control planes, each behind router names of its own.
func twoRoutedDeployments(t *testing.T) (*routedDeployment, *routedDeployment) {
	t.Helper()

	stackA, _, _ := simulatedStack(t, withTLSRouter(deploymentANames))
	stackB, _, _ := simulatedStack(t, withTLSRouter(deploymentBNames))

	return &routedDeployment{stack: stackA, names: deploymentANames},
		&routedDeployment{stack: stackB, names: deploymentBNames}
}

// A DEPLOYMENT BEHIND A ROUTER PRESENTS THE NAMES NODES DIAL. Its certificate names the
// router's two addresses and nothing else, and its loopback listener serves TLS rather
// than the plain HTTP a loopback wire otherwise does.
func TestABehindARouterDeploymentPresentsTheNamesNodesDial(t *testing.T) {
	resolveRouterNames()

	s, _, _ := simulatedStack(t, withTLSRouter(deploymentANames))

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var d net.Dialer

	raw, err := d.DialContext(ctx, "tcp", s.assembled.wire.Bootstrap)
	if err != nil {
		t.Fatalf("dial the enrollment listener: %v", err)
	}
	defer func() { _ = raw.Close() }()

	conn := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         deploymentANames.enroll, MinVersion: tls.VersionTLS13,
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		t.Fatalf("handshake on the loopback enrollment listener: %v", err)
	}

	leaf := conn.ConnectionState().PeerCertificates[0]

	for _, name := range []string{deploymentANames.nodes, deploymentANames.enroll} {
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("the certificate does not name %s: %v", name, err)
		}
	}

	if err := leaf.VerifyHostname("127.0.0.1"); err == nil {
		t.Error("the certificate names the loopback address a node never dials")
	}

	// AND THE NODE WIRE ON LOOPBACK SERVES TLS AND NOTHING ELSE: the listener handshakes before
	// anything reads a request, so plain HTTP gets no answer at all, where a plain wire would
	// route it and answer something.
	plain, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.assembled.wire.Addr+"/v1/register", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	resp, err := http.DefaultClient.Do(plain)
	if err == nil {
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		t.Errorf("plain HTTP to a loopback wire behind a router answered %s", resp.Status)
	}
}

// EVERY BILLET CLIENT SENDS THE NAME IT WAS CONFIGURED WITH, because a router has nothing
// else to go on. The wire's client, and both of enrollment's: FetchCA is the one that
// verifies nothing in the handshake, and a router still has to route it.
func TestEveryNodeWireClientSendsTheConfiguredName(t *testing.T) {
	resolveRouterNames()

	const name = "named.billet.example"

	seen := make(chan string, 8)

	cert, err := selfSigned(name)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
			GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
				seen <- info.ServerName

				return nil, nil
			},
		},
	}

	go func() { _ = srv.ServeTLS(ln, "", "") }() //nolint:errcheck // it ends with this test's Close, and says so

	t.Cleanup(func() { _ = srv.Close() })

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("address: %v", err)
	}

	base := "https://" + net.JoinHostPort(name, port)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	roots := x509.NewCertPool()

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	roots.AddCert(leaf)

	caPEM := pemCertificate(leaf)

	nc, err := app.NewNodeClient(&config.Config{Node: &config.NodeConfig{Name: "n", ServerAddr: base}},
		&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatalf("NewNodeClient: %v", err)
	}

	for what, ask := range map[string]func() error{
		"FetchCA": func() error {
			_, _, err := nodeclient.FetchCA(ctx, base, "SHA256:whatever")

			return err //nolint:wrapcheck // the answer is not the subject
		},
		"Enroll": func() error {
			_, _, err := nodeclient.Enroll(ctx, base, "n", "token", caPEM, []byte("csr"))

			return err //nolint:wrapcheck // the answer is not the subject
		},
		"the wire": func() error {
			return nc.Register(ctx, nodeclient.Registration{Provider: config.ProviderSimulated, VCPU: 1, Memory: config.GiB}) //nolint:wrapcheck // the answer is not the subject
		},
	} {
		_ = ask() //nolint:errcheck // what the server saw is the subject, not what it answered

		select {
		case got := <-seen:
			if got != name {
				t.Errorf("%s sent SNI %q, want %q", what, got, name)
			}
		case <-ctx.Done():
			t.Fatalf("%s reached no handshake", what)
		}

		for len(seen) > 0 {
			if got := <-seen; got != name {
				t.Errorf("%s sent SNI %q, want %q", what, got, name)
			}
		}
	}
}

// selfSigned is a certificate naming one host, its own root.
func selfSigned(name string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err //nolint:wrapcheck // a test helper's one failure
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err //nolint:wrapcheck // a test helper's one failure
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func pemCertificate(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// routerTestFile is where the reference configuration is, from this package.
func routerTestFile() string { return filepath.Join("..", "..", "deploy", "router", "haproxy.cfg") }

// readRouterFile reads the shipped file.
func readRouterFile(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile(routerTestFile())
	if err != nil {
		t.Fatalf("read the reference configuration: %v", err)
	}

	return string(body)
}
