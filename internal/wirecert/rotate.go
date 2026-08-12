package wirecert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Rotating is a node's TLS identity that can be replaced without a restart.
//
// A CALLBACK RATHER THAN A tls.Config FIELD, because a Config's Certificates
// slice is read when a handshake starts and a node holds long-lived connections
// either side of a renewal. Swapping the slice under a live Config is a data
// race; GetClientCertificate is the supported way to answer "what am I, right
// now" per handshake.
type Rotating struct {
	// current is the live keypair. Held as a value so a reader never sees a
	// half-written one.
	current atomic.Pointer[tls.Certificate]

	// paths are where the bundle lives, so a renewal survives a restart.
	certPath, keyPath, caPath string

	// roots is what this node verifies the control plane against. Replaced by a
	// renewal that carries a wider bundle, which is how a CA rotation propagates.
	//
	// A POINTER READ AT EVERY HANDSHAKE, not a tls.Config field. Config.RootCAs is
	// captured when the config is built, and a node builds its config once at
	// startup and keeps it in an http.Transport for the life of the process — so
	// widening this pool would reach the disk, reach memory, and never reach a
	// single connection. The rotation would be invisible to every node that had
	// not restarted, and retiring the old authority would take the fleet down on
	// the operator's schedule while every check said the overlap had worked.
	roots atomic.Pointer[x509.CertPool]
}

// NewRotating builds a rotating identity from a bundle on disk.
func NewRotating(certPath, keyPath, caPath string) (*Rotating, error) {
	b, err := LoadBundle(certPath, keyPath, caPath)
	if err != nil {
		return nil, err
	}

	pool, err := poolFrom(b.CAPEM)
	if err != nil {
		return nil, err
	}

	cert, err := verifiedKeyPair(b.CertPEM, b.KeyPEM, pool)
	if err != nil {
		return nil, err
	}

	r := &Rotating{certPath: certPath, keyPath: keyPath, caPath: caPath}
	r.roots.Store(pool)
	r.current.Store(cert)

	return r, nil
}

// poolFrom builds a verification pool from a PEM bundle.
func poolFrom(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("wirecert: the CA certificate could not be parsed for verification")
	}

	return pool, nil
}

// verifiedKeyPair parses a keypair and refuses one the given authority did not
// issue.
//
// PURE, so a caller can check a renewal before it commits to it anywhere.
func verifiedKeyPair(certPEM, keyPEM []byte, roots *x509.CertPool) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("wirecert: load the node certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("wirecert: parse the node certificate: %w", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("wirecert: this certificate was not issued by the authority this "+
			"node trusts, so the control plane would reject it: %w", err)
	}

	cert.Leaf = leaf

	return &cert, nil
}

// Leaf is the certificate in force right now.
func (r *Rotating) Leaf() *x509.Certificate { return r.current.Load().Leaf }

// ClientTLS is this identity as a dialable config, verifying the control plane
// against serverName.
//
// BOTH HALVES ARE ANSWERED PER HANDSHAKE, because a node builds this once and
// keeps it for the life of the process. GetClientCertificate does that for what
// the node presents; RootCAs cannot, because it is a value captured when the
// config is built — so verification is done here instead, against the pool as it
// is at the handshake. That is what lets a renewal widen a running node's trust,
// which is the entire mechanism by which a CA rotation reaches the fleet.
//
// THE NAME IS PASSED IN RATHER THAN READ OFF THE CONNECTION. tls.ConnectionState
// carries only what went out in SNI, and SNI is not sent for an IP literal — so
// a node dialling its control plane by address would arrive here with nothing to
// check the certificate against, and billet supports exactly that.
func (r *Rotating) ClientTLS(serverName string) *tls.Config {
	return &tls.Config{
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return r.current.Load(), nil
		},
		// The verification below replaces it and is stricter in no way: same roots,
		// same hostname, same usage. Skipping the built-in check is what makes the
		// roots late-bound.
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection does the full check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			return r.verifyServer(serverName, cs)
		},
		MinVersion: tls.VersionTLS13,
	}
}

// verifyServer is the check crypto/tls would have done, against the authority
// this node trusts RIGHT NOW.
func (r *Rotating) verifyServer(serverName string, cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("wirecert: the control plane presented no certificate")
	}

	// FAILS CLOSED ON A MISSING NAME, because an empty one turns the hostname
	// check into a no-op — after which any host holding a certificate from this
	// authority could answer for the control plane.
	if serverName == "" {
		return errors.New(
			"wirecert: this node was given no control-plane name to verify against, so the " +
				"certificate cannot be checked against the address it dialled")
	}

	intermediates := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		intermediates.AddCert(c)
	}

	if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Roots:         r.roots.Load(),
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("wirecert: the control plane's certificate was not issued by the "+
			"authority this node trusts: %w", err)
	}

	return nil
}

// Replace installs a renewed certificate, writing it down before using it.
//
// TO DISK FIRST, AND ATOMICALLY. A node that adopted a renewal in memory and
// then failed to persist it would keep working until it restarted and then come
// back with the old certificate — which by then may be closer to expiry, or past
// it. Writing first means the worst case is a renewal that is durable but not
// yet live, which the next start picks up.
//
// The key is written 0600 and the certificate 0644: one is a secret and the
// other is public, and giving them the same mode teaches the wrong lesson to
// whoever copies this next.
func (r *Rotating) Replace(certPEM, keyPEM, caPEM []byte) error {
	// THE AUTHORITY FIRST, and this is what makes a CA rotation reach a node at
	// all. A renewal during an overlap carries a trust bundle holding both the new
	// authority and the old one; adopting the certificate without the bundle would
	// leave this node trusting only what it already had, so it would keep working
	// right up until the old authority is retired and then stop.
	//
	// Widened before it is narrowed: the pool is built from the bundle BEFORE the
	// new leaf is checked against it, because a leaf issued by the new authority
	// does not chain to the old one alone.
	pool := r.roots.Load()

	if len(caPEM) > 0 {
		widened, err := poolFrom(caPEM)
		if err != nil {
			return errors.New("wirecert: the renewed authority could not be parsed")
		}

		pool = widened
	}

	// VERIFIED BEFORE ANY OF IT IS WRITTEN DOWN. Installing on disk first and
	// checking afterwards leaves the node running on the certificate it already
	// had — correctly, and with a log line saying so — while the bundle on disk is
	// the bad one. Nothing is wrong until the process restarts, at which point it
	// cannot start at all and has to be re-enrolled by hand, which is the outcome
	// renewal exists to avoid.
	cert, err := verifiedKeyPair(certPEM, keyPEM, pool)
	if err != nil {
		return err
	}

	if len(caPEM) > 0 {
		if err := writeAtomic(r.caPath, caPEM, 0o644); err != nil {
			return err
		}
	}

	if err := writeAtomic(r.keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	if err := writeAtomic(r.certPath, certPEM, 0o644); err != nil {
		return err
	}

	// THE POOL BEFORE THE LEAF. Between the two stores a handshake sees the new
	// roots with the old certificate, which still verifies — the bundle carries
	// both authorities. The other order shows the new certificate to a pool that
	// cannot chain it.
	r.roots.Store(pool)
	r.current.Store(cert)

	return nil
}

// LeafOf parses the leaf certificate out of a bundle.
func LeafOf(b Bundle) (*x509.Certificate, error) {
	block, _ := pem.Decode(b.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("wirecert: the bundle carries no PEM certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("wirecert: parse the certificate: %w", err)
	}

	return cert, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")

	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("wirecert: write %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("wirecert: replace %s: %w", path, err)
	}

	return nil
}
