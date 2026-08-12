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

	roots *x509.CertPool
}

// NewRotating builds a rotating identity from a bundle on disk.
func NewRotating(certPath, keyPath, caPath string) (*Rotating, error) {
	b, err := LoadBundle(certPath, keyPath, caPath)
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CAPEM) {
		return nil, errors.New("wirecert: the CA certificate could not be parsed for verification")
	}

	r := &Rotating{certPath: certPath, keyPath: keyPath, caPath: caPath, roots: pool}

	if err := r.set(b.CertPEM, b.KeyPEM); err != nil {
		return nil, err
	}

	return r, nil
}

// set parses a keypair and makes it the live one.
func (r *Rotating) set(certPEM, keyPEM []byte) error {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("wirecert: load the node certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("wirecert: parse the node certificate: %w", err)
	}

	// VERIFIED BEFORE IT IS INSTALLED. A renewal that does not chain to the
	// authority this node verifies the control plane against would replace a
	// working identity with one that cannot connect — and the node would discover
	// that on its next handshake, having already overwritten the good one.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     r.roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("wirecert: this certificate was not issued by the authority this "+
			"node trusts, so the control plane would reject it: %w", err)
	}

	cert.Leaf = leaf
	r.current.Store(&cert)

	return nil
}

// Leaf is the certificate in force right now.
func (r *Rotating) Leaf() *x509.Certificate { return r.current.Load().Leaf }

// ClientTLS is this identity as a dialable config.
func (r *Rotating) ClientTLS() *tls.Config {
	return &tls.Config{
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return r.current.Load(), nil
		},
		RootCAs:    r.roots,
		MinVersion: tls.VersionTLS13,
	}
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
func (r *Rotating) Replace(certPEM, keyPEM []byte) error {
	if err := writeAtomic(r.keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	if err := writeAtomic(r.certPath, certPEM, 0o644); err != nil {
		return err
	}

	return r.set(certPEM, keyPEM)
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
