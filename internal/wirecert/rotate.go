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

	// rolledBack records that this identity came from the generation a renewal
	// replaced, because the one on top of it did not load.
	rolledBack bool

	// staleCopies records a superseded generation that could not be removed. One
	// of those files is a private key, so it is worth an operator's attention
	// even though nothing is broken.
	staleCopies error

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

// prevSuffix names the generation a renewal replaced, kept until the new one is
// known to load.
const prevSuffix = ".prev"

// NewRotating builds a rotating identity from a bundle on disk, falling back to
// the generation a renewal replaced if the current one is incomplete.
//
// A GENERATION IS THREE FILES AND THREE RENAMES, and no amount of care makes
// that one operation. Between any two of them the process can die, and what is
// left is a new key beside an old certificate — a pair that verifies as nothing.
// The node then cannot start, and cannot renew its way out either, because
// renewal is authenticated by the certificate being renewed and the key that
// certificate belonged to has already been overwritten. That machine has to be
// enrolled again by hand, which is the outcome renewal exists to avoid.
//
// So the answer is not to make the write atomic — it cannot be — but to keep the
// predecessor until the successor is known to load, and to come back to it when
// the successor does not. Recovery is silent to the wire and loud in the log:
// RolledBack reports it so the caller can say so.
func NewRotating(certPath, keyPath, caPath string) (*Rotating, error) {
	r, err := loadRotating(certPath, keyPath, caPath, "")
	if err == nil {
		// The current generation is good, so the predecessor has done its job.
		if cleanErr := clearPrevious(certPath, keyPath, caPath); cleanErr != nil {
			r.staleCopies = cleanErr
		}

		return r, nil
	}

	prev, prevErr := loadRotating(certPath, keyPath, caPath, prevSuffix)
	if prevErr != nil {
		return nil, fmt.Errorf(
			"%w (and the generation it replaced does not load either: %w)", err, prevErr)
	}

	// PUT BACK, so the next renewal replaces a complete generation rather than
	// the wreckage of the last one.
	if err := restorePrevious(certPath, keyPath, caPath); err != nil {
		return nil, err
	}

	prev.rolledBack = true

	return prev, nil
}

// loadRotating reads one generation, optionally the superseded one.
func loadRotating(certPath, keyPath, caPath, suffix string) (*Rotating, error) {
	b, err := LoadBundle(certPath+suffix, keyPath+suffix, caPath+suffix)
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

	// The live paths, never the suffixed ones: a recovered identity must renew
	// into the real bundle.
	r := &Rotating{certPath: certPath, keyPath: keyPath, caPath: caPath}
	r.roots.Store(pool)
	r.current.Store(cert)

	return r, nil
}

// savePrevious keeps the generation about to be replaced.
func savePrevious(paths ...string) error {
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Nothing to keep. A first write has no predecessor.
				continue
			}

			return fmt.Errorf("wirecert: read %s before replacing it: %w", path, err)
		}

		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("wirecert: read the mode of %s: %w", path, err)
		}

		if err := WriteFileAtomic(path+prevSuffix, body, info.Mode().Perm()); err != nil {
			return err
		}
	}

	return nil
}

// restorePrevious puts a superseded generation back in place.
func restorePrevious(paths ...string) error {
	for _, path := range paths {
		body, err := os.ReadFile(path + prevSuffix)
		if err != nil {
			return fmt.Errorf("wirecert: read %s to recover it: %w", path+prevSuffix, err)
		}

		info, err := os.Stat(path + prevSuffix)
		if err != nil {
			return fmt.Errorf("wirecert: read the mode of %s: %w", path+prevSuffix, err)
		}

		if err := WriteFileAtomic(path, body, info.Mode().Perm()); err != nil {
			return err
		}
	}

	return clearPrevious(paths...)
}

// clearPrevious drops a superseded generation, reporting what it could not
// remove.
//
// NOT SWALLOWED, because one of these files is a private key. A removal that
// fails leaves a second copy of a node's identity on disk while the renewal it
// belonged to reports success, and nothing else would ever mention it. It is
// still not a failure of the renewal — the new generation is installed and
// working — so the caller warns rather than refusing.
func clearPrevious(paths ...string) error {
	var failures []error

	for _, path := range paths {
		if err := os.Remove(path + prevSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
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

// RolledBack reports whether this identity was recovered from the generation a
// renewal replaced — which means a renewal was interrupted partway through
// installing itself, and is worth an operator's attention even though nothing is
// broken.
func (r *Rotating) RolledBack() bool { return r.rolledBack }

// StaleCopies reports a superseded generation that could not be deleted — a
// second copy of this node's private key, left on disk.
func (r *Rotating) StaleCopies() error { return r.staleCopies }

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

	// THE GENERATION THIS REPLACES IS KEPT UNTIL THE NEW ONE LOADS. Three files
	// cannot be renamed as one operation, so a crash partway leaves a new key
	// beside an old certificate; NewRotating comes back to these.
	if err := savePrevious(r.keyPath, r.certPath, r.caPath); err != nil {
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

	// Complete and verified — the predecessor has nothing left to protect.
	if cleanErr := clearPrevious(r.keyPath, r.certPath, r.caPath); cleanErr != nil {
		r.staleCopies = cleanErr
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

// WriteFileAtomic replaces a file with data, giving it exactly mode, and leaves
// either the old contents or the new ones behind.
//
// NOT os.WriteFile, FOR TWO REASONS THAT BOTH MATTER FOR A PRIVATE KEY.
//
// It applies its mode only when it CREATES the file, so writing a fresh key over
// an existing node.key that happened to be 0644 left a new secret world-readable
// and reported success. And it FOLLOWS SYMLINKS: with a temporary name derived
// from the destination, anyone able to write the directory can plant that name
// pointing somewhere they can read, and the key is written there before the
// rename puts it back. The approval wait makes that window minutes long and
// entirely predictable.
//
// So the temporary is created with a random name by CreateTemp — which uses
// O_EXCL, so it cannot be an attacker's symlink — chmodded explicitly rather
// than inheriting anything, and fsynced before the rename. The directory is
// synced afterwards, so the rename survives a power cut rather than leaving a
// name pointing at nothing.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)

	f, err := os.CreateTemp(dir, ".billet-*")
	if err != nil {
		return fmt.Errorf("wirecert: stage a replacement for %s: %w", path, err)
	}

	tmp := f.Name()

	// Best effort on every failure path: a temporary left behind is litter, and
	// litter holding a private key is worse than that.
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	// EXPLICIT, because CreateTemp makes 0600 and a caller asking for 0644 must
	// get it — and because relying on the default would make the secret case
	// correct by accident rather than by instruction.
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("wirecert: set the mode on %s: %w", tmp, err)
	}

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("wirecert: write %s: %w", tmp, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("wirecert: flush %s: %w", tmp, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("wirecert: close %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("wirecert: replace %s: %w", path, err)
	}

	return syncDir(dir)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	return WriteFileAtomic(path, data, mode)
}
