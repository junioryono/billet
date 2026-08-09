package wirecert_test

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/wirecert"
)

const deployment = "0123456789abcdef0123456789abcdef"

// THE CA IS MINTED ONCE AND ONLY ONCE.
//
// A second authority in the same directory would invalidate every node
// certificate issued from the first, and the fleet would drop off together.
func TestTheAuthorityIsStable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := wirecert.LoadOrCreateCA(dir, deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	second, err := wirecert.LoadOrCreateCA(dir, deployment)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !bytes.Equal(first.CertPEM(), second.CertPEM()) {
		t.Error("loading an existing CA directory produced a different authority, so every " +
			"certificate issued before this call has stopped verifying")
	}
}

// A HALF-INITIALISED CA DIRECTORY IS REFUSED, NOT REPAIRED.
//
// Minting a replacement for a missing key is the tempting repair and the
// catastrophic one: it is a new authority, and every node certificate the old
// one signed stops verifying at once.
func TestAHalfInitialisedAuthorityIsRefused(t *testing.T) {
	t.Parallel()

	for _, missing := range []string{"ca.key", "ca.crt"} {
		t.Run("without "+missing, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
				t.Fatalf("create: %v", err)
			}

			if err := os.Remove(filepath.Join(dir, missing)); err != nil {
				t.Fatalf("remove %s: %v", missing, err)
			}

			_, err := wirecert.LoadOrCreateCA(dir, deployment)
			if !errors.Is(err, wirecert.ErrHalfInitialised) {
				t.Errorf("a CA directory missing %s was not refused: %v", missing, err)
			}
		})
	}
}

// The CA key is a secret and is written like one.
func TestTheAuthorityKeyIsNotReadableByOthers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the CA key is mode %o, want 600: it signs every node's identity", perm)
	}
}

// A NODE CERTIFICATE'S COMMON NAME IS THE NODE'S IDENTITY.
//
// The wire reads the name from here and acts for that node, so anything else in
// this field is a host acting as somebody else.
func TestANodeCertificateCarriesItsName(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	cert := parse(t, bundle.CertPEM)

	if cert.Subject.CommonName != "epyc-1" {
		t.Errorf("common name is %q, want epyc-1 — the wire authorises by this field",
			cert.Subject.CommonName)
	}

	// CLIENT AUTH ONLY. A node certificate that also carried server auth would be
	// usable to impersonate the control plane to another node.
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("a node certificate carries extended key usage %v; it must be client auth "+
			"alone, or a node could present it as the control plane", cert.ExtKeyUsage)
	}
}

// A LEAF MAY NOT OUTLIVE ITS AUTHORITY.
//
// Verification fails on the CA's expiry regardless, so a leaf dated past it
// prints a lie about when the node stops working.
func TestALeafIsCappedAtTheAuthoritysExpiry(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueNode("n1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if leaf := parse(t, bundle.CertPEM); leaf.NotAfter.After(ca.NotAfter()) {
		t.Errorf("a node certificate expires at %s, after its authority at %s",
			leaf.NotAfter, ca.NotAfter())
	}
}

// A SERVER CERTIFICATE WITHOUT NAMES IS REFUSED.
//
// A node verifies the address it dialled against the certificate's names, so one
// with none matches nothing and fails on the node, a machine away from the
// config that caused it.
func TestAServerCertificateNeedsNames(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := ca.IssueServer(nil); err == nil {
		t.Error("a server certificate with no hosts was issued; no node could verify it")
	}
}

// Addresses and names land in the right fields, or verification fails on one of
// the two forms an operator can legitimately write.
func TestAServerCertificateCoversNamesAndAddresses(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueServer([]string{"billet.example", "10.0.0.4"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	cert := parse(t, bundle.CertPEM)

	if err := cert.VerifyHostname("billet.example"); err != nil {
		t.Errorf("the certificate does not cover the DNS name it was issued for: %v", err)
	}

	if err := cert.VerifyHostname("10.0.0.4"); err != nil {
		t.Errorf("the certificate does not cover the IP it was issued for: %v", err)
	}

	if err := cert.VerifyHostname("elsewhere.example"); err == nil {
		t.Error("the certificate verified a name it was never issued for")
	}
}

// A BUNDLE IS NEVER WRITTEN OVER.
//
// Re-issuing into a live node's directory leaves that host with a key it never
// loaded and a certificate it cannot use until someone restarts it — and, if the
// write half-fails, with neither.
func TestABundleWillNotOverwrite(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueNode("n1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "out")

	if err := bundle.Write(dir); err != nil {
		t.Fatalf("write: %v", err)
	}

	before, err := os.ReadFile(filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	next, err := ca.IssueNode("n1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if err := next.Write(dir); err == nil {
		t.Error("a second bundle was written over the first")
	}

	after, err := os.ReadFile(filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Error("the refused write still replaced the key that was there")
	}
}

// The bundle's key is a secret and is written like one.
func TestABundleKeyIsNotReadableByOthers(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueNode("n1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "out")

	if err := bundle.Write(dir); err != nil {
		t.Fatalf("write: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("a node key is mode %o, want 600", perm)
	}
}

// The server's config demands a certificate rather than merely offering to check
// one, which is the difference between authentication and decoration.
func TestTheServerConfigRequiresAClientCertificate(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ServerTLS(bundle)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	if conf.ClientAuth != 4 { // tls.RequireAndVerifyClientCert
		t.Errorf("client auth is %v; anything short of require-and-verify lets an "+
			"unauthenticated connection reach a handler", conf.ClientAuth)
	}

	if conf.ClientCAs == nil {
		t.Error("no client authority is configured, so any certificate would verify")
	}
}

// The warning fires while the node still works, which is the only time it can
// help: an expired certificate is a handshake failure, and the node is simply
// gone.
func TestAnExpiringCertificateIsFlaggedBeforeItFails(t *testing.T) {
	t.Parallel()

	soon := &x509.Certificate{NotAfter: time.Now().Add(wirecert.ExpiryWarning / 2)}
	if _, ok := wirecert.ExpiresSoon(soon); !ok {
		t.Error("a certificate inside the warning window was not flagged")
	}

	later := &x509.Certificate{NotAfter: time.Now().Add(2 * wirecert.ExpiryWarning)}
	if _, ok := wirecert.ExpiresSoon(later); ok {
		t.Error("a certificate well outside the warning window was flagged, which is how a " +
			"warning becomes noise nobody reads")
	}
}

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("the certificate is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	return cert
}
