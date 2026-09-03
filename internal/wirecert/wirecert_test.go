package wirecert_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

			if err := os.Remove(filepath.Join(wirecert.CADir(dir), missing)); err != nil {
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

	info, err := os.Stat(filepath.Join(wirecert.CADir(dir), "ca.key"))
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

	// REQUIRE, not verify-if-given. This was the relaxed form so that an
	// unenrolled machine could reach /v1/ca and /v1/enroll on this listener, with
	// the handler refusing it everywhere else — which authenticated correctly and
	// let an anonymous caller hold connections out of the budget real nodes need.
	// Those two routes are BootstrapHandler's now, on a listener of their own, so
	// nothing certless has business here and the handshake says so first.
	if conf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("client auth is %v; want RequireAndVerifyClientCert, so a caller with no "+
			"certificate cannot occupy a slot an enrolled node needs", conf.ClientAuth)
	}

	if conf.ClientCAs == nil {
		t.Error("no client authority is configured, so any certificate would verify")
	}
}

// THE ENROLLMENT LISTENER ASKS FOR NOTHING, and that is the point of it being a
// second listener rather than a relaxation of the first.
//
// Every caller here is a machine that has no certificate yet, so asking for one
// can only make this listener parse and verify a chain a stranger chose. What
// secures these two routes is the fingerprint the operator compared out of band,
// the join token, and an approval that waits for a human.
func TestTheBootstrapConfigAsksForNoClientCertificate(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.BootstrapTLS(bundle)
	if err != nil {
		t.Fatalf("bootstrap tls: %v", err)
	}

	if conf.ClientAuth != tls.NoClientCert {
		t.Errorf("client auth is %v; want NoClientCert", conf.ClientAuth)
	}

	if conf.ClientCAs != nil {
		t.Error("a client authority is configured on a listener that does not ask for a " +
			"certificate, which is verification work no caller can ever benefit from")
	}

	if conf.MinVersion != tls.VersionTLS13 {
		t.Errorf("min version is %x; want TLS 1.3", conf.MinVersion)
	}

	// THE SAME CERTIFICATE THE NODE WIRE PRESENTS. An enrolling node verifies this
	// listener by the hostname it dialled, so a different or missing leaf here is
	// a handshake failure at exactly the moment a machine is trying to join.
	operational, err := wirecert.ServerTLS(bundle)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	if len(conf.Certificates) != 1 || len(operational.Certificates) != 1 {
		t.Fatalf("each config must carry exactly one certificate, got %d and %d",
			len(conf.Certificates), len(operational.Certificates))
	}

	if !bytes.Equal(conf.Certificates[0].Certificate[0],
		operational.Certificates[0].Certificate[0]) {
		t.Error("the enrollment listener presents a different certificate than the node wire")
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

// LOSING BOTH CA FILES IS NOT A FIRST RUN, and nothing could tell the difference
// until something remembered.
//
// A restored backup that omitted the CA directory, a state directory recreated
// by a provisioning script, an operator clearing what they took for cache — each
// leaves an empty directory that looks exactly like day one. Minting a new
// authority there invalidates every node certificate this deployment ever
// issued: the whole fleet drops off at once, and the control plane looks
// perfectly healthy while it happens.
func TestAnAuthorityThatVanishedIsNotSilentlyReplaced(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := wirecert.LoadOrCreateCA(dir, deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, name := range []string{"ca.crt", "ca.key"} {
		if err := os.Remove(filepath.Join(wirecert.CADir(dir), name)); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
	}

	second, err := wirecert.LoadOrCreateCA(dir, deployment)
	if !errors.Is(err, wirecert.ErrAuthorityLost) {
		t.Fatalf("a deployment whose authority disappeared minted a new one (%v); every "+
			"bundle it ever issued now fails to verify", err)
	}

	if second != nil && !bytes.Equal(second.CertPEM(), first.CertPEM()) {
		t.Error("a replacement authority was returned alongside the error")
	}
}

// The marker is what an operator deletes to start over deliberately.
func TestAuthorityLossIsRecoverableByChoice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, path := range []string{
		filepath.Join(wirecert.CADir(dir), "ca.crt"),
		filepath.Join(wirecert.CADir(dir), "ca.key"),
		filepath.Join(dir, "authority-created"),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove %s: %v", path, err)
		}
	}

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Errorf("an operator who removed the marker deliberately could not start over: %v", err)
	}
}

// AN AUTHORITY FROM ANOTHER DEPLOYMENT IS REFUSED.
//
// Verifying against the CA is what decides which nodes may connect at all, so a
// CA restored from somewhere else silently re-points that decision: a holder of
// the other installation's node certificate connects, names this deployment in
// its registration body, and is accepted.
func TestAForeignAuthorityIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := wirecert.LoadOrCreateCA(dir, deployment)
	if !errors.Is(err, wirecert.ErrForeignAuthority) {
		t.Errorf("a control plane loaded another deployment's authority (%v); its nodes could "+
			"then drive this installation", err)
	}
}

// A KEY THAT ANY LOCAL USER CAN READ IS NOT A KEY.
//
// Creation writes 0600, and that says nothing about what is on disk now. A
// backup that restored ca.key as 0644 starts billet perfectly happily while
// anyone on the host copies the authority and mints node identities at will.
func TestAWorldReadableAuthorityKeyIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	key := filepath.Join(wirecert.CADir(dir), "ca.key")
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := wirecert.LoadOrCreateCA(dir, deployment)
	if err == nil {
		t.Fatal("a CA key readable by every local user was accepted")
	}

	if !strings.Contains(err.Error(), "chmod") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// A key reached through a symlink is refused: billet reads the path it was
// given, so that what it loads is what an operator secured.
func TestASymlinkedAuthorityKeyIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	key := filepath.Join(wirecert.CADir(dir), "ca.key")

	elsewhere := filepath.Join(t.TempDir(), "real.key")

	body, err := os.ReadFile(key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := os.WriteFile(elsewhere, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Remove(key); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := os.Symlink(elsewhere, key); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err == nil {
		t.Error("a CA key reached through a symlink was accepted")
	}
}

// MISMATCHED HALVES FAIL AT STARTUP, not days later on a node.
//
// Two unrelated files load happily and then sign leaves that nothing can verify.
// The failure surfaces as a handshake error on a machine that names neither
// file.
func TestAKeyThatDoesNotMatchItsCertificateIsRefused(t *testing.T) {
	t.Parallel()

	a, b := t.TempDir(), t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(a, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(b, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	// B's key beside A's certificate, which is what half a restore looks like.
	body, err := os.ReadFile(filepath.Join(wirecert.CADir(b), "ca.key"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wirecert.CADir(a), "ca.key"), body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(a, deployment); err == nil {
		t.Error("a certificate and an unrelated key were loaded as an authority; every " +
			"certificate signed with them would fail on the node presenting it")
	}
}

// A node's key is held to the same standard, for the same reason: it is the
// credential that lets a host act as that node.
func TestAWorldReadableNodeKeyIsRefused(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bundle, err := ca.IssueNode("n1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "bundle")

	if err := bundle.Write(dir); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Chmod(filepath.Join(dir, "node.key"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err = wirecert.LoadBundle(
		filepath.Join(dir, "node.crt"),
		filepath.Join(dir, "node.key"),
		filepath.Join(wirecert.CADir(dir), "ca.crt"))
	if err == nil {
		t.Error("a node key readable by every local user was accepted; any local user could " +
			"act as this node")
	}
}

// THE WHOLE ca DIRECTORY GOING MISSING IS THE FAILURE THIS EXISTS FOR, and the
// first version could not survive it: the marker lived inside that directory, so
// a backup or a provisioning script that omitted it took the witness along and
// hadAuthority answered "day one". The test deleted two files and kept the
// marker, which modelled a case that does not happen.
func TestLosingTheWholeAuthorityDirectoryIsDetected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := os.RemoveAll(wirecert.CADir(dir)); err != nil {
		t.Fatalf("remove the ca directory: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); !errors.Is(err, wirecert.ErrAuthorityLost) {
		t.Errorf("a deployment whose entire ca directory was dropped minted a new authority "+
			"(%v); every bundle it ever issued now fails to verify", err)
	}
}

// AN INSTALLATION OLDER THAN THE MARKER IS PROTECTED FROM ITS SECOND BOOT.
//
// Without a backfill the upgrade does nothing for exactly the deployments that
// have the most to lose: the ones that have been issuing certificates for
// longest.
func TestAnAuthorityPredatingTheMarkerIsBackfilled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	// An installation created before the marker existed.
	if err := os.Remove(filepath.Join(dir, "authority-created")); err != nil {
		t.Fatalf("remove the marker: %v", err)
	}

	// A boot on the new code, which loads the existing authority happily.
	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "authority-created")); err != nil {
		t.Fatalf("the marker was not backfilled, so this deployment keeps the old behaviour "+
			"forever: %v", err)
	}

	// And from here it is protected like any other.
	if err := os.RemoveAll(wirecert.CADir(dir)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); !errors.Is(err, wirecert.ErrAuthorityLost) {
		t.Errorf("a backfilled installation still minted a replacement: %v", err)
	}
}

// A BUNDLE WHOSE HALVES COME FROM DIFFERENT DEPLOYMENTS IS REFUSED.
//
// The leaf and key being a matched pair says nothing about whether the CA beside
// them issued that leaf. A node adopts its DEPLOYMENT from the leaf, so a mixed
// bundle wrote the wrong identity permanently, trusted a server that would
// reject it, and then refused the correct bundle as a conflict.
func TestABundleWhoseCertificateDoesNotChainToItsCAIsRefused(t *testing.T) {
	t.Parallel()

	a, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	b, err := wirecert.LoadOrCreateCA(t.TempDir(), "ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mine, err := a.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	mixed := wirecert.Bundle{
		CertPEM: mine.CertPEM,
		KeyPEM:  mine.KeyPEM,
		CAPEM:   b.CertPEM(), // somebody else's authority
	}

	if _, err := wirecert.ClientTLS(mixed); err == nil {
		t.Error("a node certificate was accepted beside an authority that did not issue it; " +
			"the node would adopt the wrong deployment permanently and still be rejected")
	}
}

// AN AUTHORITY RUNNING OUT SHORTENS EVERY CERTIFICATE IT ISSUES, silently, and
// that is what Capping exists to say out loud.
//
// A leaf may not outlive its authority, so once the CA has less than a leaf's
// life left every certificate is quietly shorter than the last. Renewals keep
// working — faster and faster — and then the whole fleet expires on the day the
// authority does. Nothing errors before that, which is exactly why an operator
// has to be told while there is still time to rotate.
func TestAnExpiringAuthorityIsReported(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A fresh authority has years left and is not capping anything.
	if left, capping := ca.Capping(); capping {
		t.Errorf("a new authority reports that it is shortening certificates (%s left)", left)
	}

	// And what it issues gets the full life rather than a truncated one.
	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := time.Until(leaf.NotAfter); got < wirecert.LeafLifetime-48*time.Hour {
		t.Errorf("a certificate from a fresh authority is only %s long, want about %s; it is "+
			"being capped, which is the state that ends with the whole fleet stopping at once",
			got.Round(time.Hour), wirecert.LeafLifetime)
	}
}
