package wirecert_test

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/wirecert"
)

const rotDeployment = "0123456789abcdef0123456789abcdef"

// A ROTATION IS AN OVERLAP, AND BOTH KINDS OF NODE KEEP WORKING THROUGH IT.
//
// This is the whole property. A node trusts the authority it was given, so the
// moment the control plane presents a certificate from a new one, every node
// that has not yet heard about it fails to verify the server and drops out —
// over the wire it would need in order to recover. There is no way back from
// that remotely.
//
// So during the overlap: the OLD authority signs what the server presents, the
// NEW one issues node certificates, and both are trusted for clients.
func TestARotationKeepsBothGenerationsWorking(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	old, err := wirecert.LoadOrCreateCA(dir, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A node enrolled before the rotation.
	before, err := old.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	fresh, err := wirecert.Rotate(dir, rotDeployment)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if fresh.Fingerprint() == old.Fingerprint() {
		t.Fatal("rotating produced the same authority")
	}

	// A node enrolled after it.
	after, err := fresh.IssueNode("epyc-2")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// THROUGH THE ONE READ THE CONTROL PLANE ACTUALLY MAKES. What is presented
	// and what is trusted have to describe the same moment, so asserting them
	// through separate helpers would be asserting something startup never does.
	authority, err := wirecert.LoadServing(dir, rotDeployment)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}

	if authority.Issuing.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("the issuing authority is %s; after a rotation it must be the new one, %s",
			authority.Issuing.Fingerprint(), fresh.Fingerprint())
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(authority.Trust) {
		t.Fatal("the trust bundle could not be parsed")
	}

	// BOTH ARE RECOGNISED AS CLIENTS. This is what stops the pre-rotation fleet
	// being locked out the moment the new authority starts issuing.
	for name, bundle := range map[string]wirecert.Bundle{"before": before, "after": after} {
		leaf, lerr := wirecert.LeafOf(bundle)
		if lerr != nil {
			t.Fatalf("%s: parse: %v", name, lerr)
		}

		if _, verr := leaf.Verify(x509.VerifyOptions{
			Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); verr != nil {
			t.Errorf("a node enrolled %s the rotation is not recognised during the overlap: %v",
				name, verr)
		}
	}

	// AND THE SERVER STILL PRESENTS SOMETHING THE OLD FLEET TRUSTS. A node that
	// has not renewed knows only the old authority, so a serving certificate from
	// the new one would make the control plane unverifiable to it.
	serving := authority.Presents

	if serving.Fingerprint() != old.Fingerprint() {
		t.Errorf("during the overlap the server signs with %s; it must be the authority the "+
			"un-renewed fleet still trusts, %s", serving.Fingerprint(), old.Fingerprint())
	}

	oldOnly := x509.NewCertPool()
	if !oldOnly.AppendCertsFromPEM(old.CertPEM()) {
		t.Fatal("parse the old authority")
	}

	servingBundle, err := serving.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue serving: %v", err)
	}

	servingLeaf, err := wirecert.LeafOf(servingBundle)
	if err != nil {
		t.Fatalf("parse serving: %v", err)
	}

	if _, err := servingLeaf.Verify(x509.VerifyOptions{
		Roots: oldOnly, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("a node that has not renewed cannot verify the control plane during the "+
			"overlap, so it drops out over the wire it would need to recover: %v", err)
	}
}

// RETIRING ENDS IT, and only then does the old authority stop being trusted.
func TestRetiringDropsTheOldAuthority(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	old, err := wirecert.LoadOrCreateCA(dir, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	fresh, err := wirecert.Rotate(dir, rotDeployment)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if err := wirecert.Retire(dir, rotDeployment); err != nil {
		t.Fatalf("retire: %v", err)
	}

	authority, err := wirecert.LoadServing(dir, rotDeployment)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}

	if authority.Rotating {
		t.Error("the deployment still reports a rotation running after retiring it")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(authority.Trust) {
		t.Fatal("parse")
	}

	before, err := old.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	leaf, err := wirecert.LeafOf(before)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Error("a certificate from the retired authority is still accepted; retiring did nothing")
	}

	// And the server now presents the new one.
	if authority.Presents.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("after retiring, the server still signs with %s",
			authority.Presents.Fingerprint())
	}
}

// A SECOND ROTATION WHILE ONE IS RUNNING IS REFUSED. There is one previous
// authority, so starting another would drop the one the un-renewed fleet still
// trusts — a rotation that locks out exactly the nodes it was meant to carry.
func TestASecondRotationIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, rotDeployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := wirecert.Rotate(dir, rotDeployment); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if _, err := wirecert.Rotate(dir, rotDeployment); err == nil {
		t.Fatal("a second rotation started while the first was still running")
	}
}

// A ROTATION REACHES A CONNECTION THE NODE HAS ALREADY BUILT.
//
// Every part of the overlap is on disk and in memory before this matters, and
// none of it helps if the live transport cannot see it. A node calls ClientTLS
// exactly once, at startup, and the config it gets is installed in an
// http.Transport for the life of the process. A renewal that widens the trust
// bundle is the whole propagation mechanism for a CA rotation — so if that
// widening does not reach the config already handed out, the rotation is
// invisible to every node that has not restarted.
//
// The failure is the outage the two-phase design exists to prevent, arriving on
// the operator's schedule: rotate, watch nodes renew, retire, restart the
// control plane so it presents the new authority, and every node that has been
// up the whole time cannot verify it. Nothing in the procedure says to restart
// the fleet, because on paper nothing needs to.
func TestARenewalWidensTrustForAConfigAlreadyHandedOut(t *testing.T) {
	t.Parallel()

	// Two independent authorities: what the node starts out trusting, and what a
	// rotation moves it to.
	oldDir, newDir := t.TempDir(), t.TempDir()

	oldCA, err := wirecert.LoadOrCreateCA(oldDir, rotDeployment)
	if err != nil {
		t.Fatalf("create the old authority: %v", err)
	}

	newCA, err := wirecert.LoadOrCreateCA(newDir, rotDeployment)
	if err != nil {
		t.Fatalf("create the new authority: %v", err)
	}

	node := writeBundleTo(t, oldCA, "epyc-1")

	id, err := wirecert.NewRotating(node.cert, node.key, node.ca)
	if err != nil {
		t.Fatalf("rotating identity: %v", err)
	}

	// TAKEN ONCE, AT STARTUP, exactly as `billet node` takes it.
	clientTLS := id.ClientTLS("127.0.0.1")

	// A control plane presenting a certificate from the NEW authority — which is
	// what it does the moment the old one is retired.
	srv := serveWith(t, newCA)

	if err := dialWith(t, srv, clientTLS); err == nil {
		t.Fatal("the node verified an authority it had never been told about")
	}

	// The renewal: a new leaf from the new authority, and a bundle carrying both.
	renewed, err := newCA.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue the renewed certificate: %v", err)
	}

	bundle := append(append([]byte(nil), newCA.CertPEM()...), oldCA.CertPEM()...)

	if err := id.Replace(renewed.CertPEM, renewed.KeyPEM, bundle); err != nil {
		t.Fatalf("replace: %v", err)
	}

	// THE SAME CONFIG, not a fresh one. A node has no reason to ask for another
	// and no code path that does.
	if err := dialWith(t, srv, clientTLS); err != nil {
		t.Fatalf("the renewal did not reach the connection the node already had: %v", err)
	}
}

// nodeBundle is a bundle written to disk, which is what NewRotating reads.
type nodeBundle struct{ cert, key, ca string }

func writeBundleTo(t *testing.T, ca *wirecert.CA, name string) nodeBundle {
	t.Helper()

	b, err := ca.IssueNode(name)
	if err != nil {
		t.Fatalf("issue %s: %v", name, err)
	}

	dir := t.TempDir()
	out := nodeBundle{
		cert: filepath.Join(dir, "node.crt"),
		key:  filepath.Join(dir, "node.key"),
		ca:   filepath.Join(dir, "ca.crt"),
	}

	for path, body := range map[string][]byte{out.cert: b.CertPEM, out.key: b.KeyPEM, out.ca: b.CAPEM} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	return out
}

// serveWith starts a TLS server presenting a certificate from ca.
func serveWith(t *testing.T, ca *wirecert.CA) *httptest.Server {
	t.Helper()

	b, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(b)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	// Clients here are testing SERVER verification, so a missing client
	// certificate must not be what fails the handshake.
	conf.ClientAuth = tls.NoClientCert

	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	return srv
}

// dialWith makes one request through a transport built from conf, the way
// nodeclient builds one.
func dialWith(t *testing.T, srv *httptest.Server, conf *tls.Config) error {
	t.Helper()

	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig:     conf,
		TLSHandshakeTimeout: 5 * time.Second,
		DisableKeepAlives:   true,
	}}
	defer c.CloseIdleConnections()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	resp, err := c.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	return err
}

// A RENEWAL THAT DOES NOT CHAIN IS NOT WRITTEN DOWN EITHER.
//
// Verifying in memory and persisting first is the wrong order and the failure is
// delayed: the node keeps running on the certificate it already had, logs that
// it kept it, and looks healthy — while the bundle on disk is the bad one. It is
// the restart that fails, and by then nothing on the node can fix it, because
// renewal is authenticated by the certificate being renewed. The machine has to
// be re-enrolled by hand, which is exactly what renewal exists to avoid.
func TestARejectedRenewalLeavesTheBundleOnDiskAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	ca, err := wirecert.LoadOrCreateCA(dir, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	node := writeBundleTo(t, ca, "epyc-1")

	id, err := wirecert.NewRotating(node.cert, node.key, node.ca)
	if err != nil {
		t.Fatalf("rotating identity: %v", err)
	}

	// ALL THREE FILES AND THEIR MODES. Snapshotting only the certificate would
	// pass a regression that wrote the key and the authority before discovering
	// the leaf was bad — the node's identity would be corrupt and this would say
	// nothing.
	before := snapshotBundle(t, node)

	// A certificate from an authority this node has never heard of, offered
	// without a bundle that would explain it — a control plane that has gone
	// wrong, which is the case worth surviving.
	stranger, err := wirecert.LoadOrCreateCA(t.TempDir(), rotDeployment)
	if err != nil {
		t.Fatalf("create the stranger: %v", err)
	}

	bad, err := stranger.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if err := id.Replace(bad.CertPEM, bad.KeyPEM, nil); err == nil {
		t.Fatal("a certificate from an unknown authority was accepted")
	}

	if after := snapshotBundle(t, node); !reflect.DeepEqual(before, after) {
		t.Error("the rejected renewal changed the bundle on disk; this node keeps working " +
			"until it restarts and then cannot start at all")
	}

	// AND WHAT IS THERE STILL LOADS, which is the property that actually matters.
	if _, err := wirecert.NewRotating(node.cert, node.key, node.ca); err != nil {
		t.Errorf("the identity on disk no longer loads after a rejected renewal: %v", err)
	}
}

// fileState is a file's contents and mode, which is what "unchanged" has to mean
// for a bundle holding a private key.
type fileState struct {
	body string
	mode os.FileMode
}

func snapshotBundle(t *testing.T, node nodeBundle) map[string]fileState {
	t.Helper()

	out := map[string]fileState{}

	for _, path := range []string{node.cert, node.key, node.ca} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}

		out[path] = fileState{body: string(body), mode: info.Mode().Perm()}
	}

	return out
}
