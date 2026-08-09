// Package wirecert issues the certificates the node wire authenticates with.
//
// THE PROBLEM IT SOLVES: a node names itself in the request path and, without
// this, nothing verifies the claim. Anything that could reach the listener could
// call itself any node, bind leases, take commands, and ask for a JIT
// registration — a credential that registers a runner against the organisation.
// That is why the wire refused to serve on anything but loopback until now.
//
// The design is deliberately small. One CA per deployment, held by the control
// plane, and one certificate per node. There is no revocation list, no OCSP, and
// no intermediate: a deployment with a compromised node re-issues its CA and its
// node certificates, which is a real cost and an honest one at this size. What
// there IS, and what matters, is that the authenticated name in the certificate
// is the ONLY thing that decides which node a request is from.
package wirecert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CALifetime is how long a deployment's certificate authority is good for.
//
// Long, because rotating it means re-issuing every node certificate by hand and
// a deployment that has to do that yearly will instead do it never. The CA key
// is the thing to protect; its lifetime is not the control that protects it.
const CALifetime = 10 * 365 * 24 * time.Hour

// LeafLifetime is how long an issued node certificate is good for.
//
// A YEAR IS A DEADLINE, NOT A DEFAULT. An expired node certificate takes that
// host out of the fleet with a TLS error, so the control plane warns while one
// is still working — see ExpiresSoon — rather than letting the first symptom be
// a node that cannot connect.
const LeafLifetime = 365 * 24 * time.Hour

// ExpiryWarning is how long before expiry the control plane starts complaining.
const ExpiryWarning = 30 * 24 * time.Hour

// ErrHalfInitialised means a CA directory holds one of its two files.
//
// REFUSED RATHER THAN REPAIRED, and this is the most important error in the
// package. A missing key next to a present certificate looks like "just create
// it again", and creating it again mints a DIFFERENT authority: every node
// certificate ever issued stops verifying, the whole fleet drops off at once,
// and the operator is left with a control plane that looks healthy and a fleet
// that cannot reach it.
var ErrHalfInitialised = errors.New("wirecert: the CA directory holds only one of its two files")

// CA is a deployment's certificate authority.
type CA struct {
	deployment string
	cert       *x509.Certificate
	key        *ecdsa.PrivateKey
	certPEM    []byte
}

// Bundle is everything one party needs to speak on the wire.
type Bundle struct {
	// CertPEM is the holder's certificate.
	CertPEM []byte
	// KeyPEM is the holder's private key. A secret: written 0600, never logged.
	KeyPEM []byte
	// CAPEM is the authority both sides verify against.
	CAPEM []byte
}

// LoadOrCreateCA reads the deployment's authority, minting it on first use.
//
// The deployment id is recorded in the CA's subject so a certificate found on a
// host can be attributed to the installation that issued it. It is NOT a second
// authority for which deployment a node belongs to — verifying against this CA
// is that answer, and registration checks the id it was told for the same reason
// it always did.
func LoadOrCreateCA(dir, deployment string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)

	switch {
	case certErr == nil && keyErr == nil:
		return parseCA(certPEM, keyPEM, deployment)

	case os.IsNotExist(certErr) && os.IsNotExist(keyErr):
		return createCA(dir, certPath, keyPath, deployment)

	case certErr != nil && !os.IsNotExist(certErr):
		return nil, fmt.Errorf("wirecert: read %s: %w", certPath, certErr)

	case keyErr != nil && !os.IsNotExist(keyErr):
		return nil, fmt.Errorf("wirecert: read %s: %w", keyPath, keyErr)
	}

	present, missing := certPath, keyPath
	if certErr != nil {
		present, missing = keyPath, certPath
	}

	return nil, fmt.Errorf(
		"%w: %s exists but %s does not. Billet will not mint a replacement, because a new "+
			"authority invalidates every node certificate this deployment ever issued and the "+
			"whole fleet would stop connecting at once. Restore the missing file from backup, or "+
			"move both aside deliberately and re-issue every node",
		ErrHalfInitialised, present, missing)
}

func createCA(dir, certPath, keyPath, deployment string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wirecert: create %s: %w", dir, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("wirecert: generate a CA key: %w", err)
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "billet node wire CA",
			Organization: []string{deployment},
		},
		NotBefore:             now.Add(-time.Hour), // clock skew between hosts
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("wirecert: sign the CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("wirecert: encode the CA key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// THE KEY FIRST, AND EXCLUSIVELY. O_EXCL so two processes initialising the
	// same directory cannot each mint an authority and have the loser's node
	// certificates verify against nothing. The key leads because a crash between
	// the two writes must leave the half-initialised state that REFUSES above,
	// rather than a certificate whose key was never written.
	if err := writeSecret(keyPath, keyPEM); err != nil {
		if os.IsExist(err) {
			return LoadOrCreateCA(dir, deployment)
		}

		return nil, err
	}

	if err := writePublic(certPath, certPEM); err != nil {
		return nil, err
	}

	if err := syncDir(dir); err != nil {
		return nil, err
	}

	// PARSED RATHER THAN ASSUMED. This is the DER x509.CreateCertificate just
	// produced, so a failure here should be impossible — which is not a reason to
	// panic. This runs inside the control plane, and a control plane that panics
	// drops every in-flight lease it was holding.
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("wirecert: parse the CA certificate just created: %w", err)
	}

	return &CA{deployment: deployment, cert: cert, key: key, certPEM: certPEM}, nil
}

func parseCA(certPEM, keyPEM []byte, deployment string) (*CA, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("wirecert: the CA certificate is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("wirecert: parse the CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("wirecert: the CA key is not PEM")
	}

	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("wirecert: parse the CA key: %w", err)
	}

	if !cert.IsCA {
		return nil, errors.New("wirecert: the CA certificate is not a certificate authority")
	}

	return &CA{deployment: deployment, cert: cert, key: key, certPEM: certPEM}, nil
}

// CertPEM is the authority nodes verify the control plane against.
func (c *CA) CertPEM() []byte { return append([]byte(nil), c.certPEM...) }

// NotAfter is when this authority stops working.
func (c *CA) NotAfter() time.Time { return c.cert.NotAfter }

// IssueServer mints the certificate the control plane serves with.
//
// KEPT IN MEMORY BY ITS CALLER, deliberately. Nothing verifies the control
// plane's certificate except against this CA, so re-minting it every boot costs
// nothing and removes a whole failure mode: a server certificate on disk is one
// more thing that expires, and its expiry takes the entire fleet offline at an
// hour nobody chose.
func (c *CA) IssueServer(hosts []string) (Bundle, error) {
	if len(hosts) == 0 {
		return Bundle{}, errors.New(
			"wirecert: a server certificate needs at least one host, because a node verifies " +
				"the address it dialled against the certificate's names")
	}

	tmpl, err := c.leafTemplate("billet control plane")
	if err != nil {
		return Bundle{}, err
	}

	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)

			continue
		}

		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}

	return c.sign(tmpl)
}

// IssueNode mints a node's certificate.
//
// THE COMMON NAME IS THE NODE'S IDENTITY, and it is the only one. The wire's
// handlers take the name from the verified certificate rather than from the
// request path, so a host holding this certificate can act as this node and as
// nothing else.
func (c *CA) IssueNode(name string) (Bundle, error) {
	if name == "" {
		return Bundle{}, errors.New("wirecert: a node certificate needs a node name")
	}

	tmpl, err := c.leafTemplate(name)
	if err != nil {
		return Bundle{}, err
	}

	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}

	return c.sign(tmpl)
}

func (c *CA) leafTemplate(cn string) (*x509.Certificate, error) {
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()

	notAfter := now.Add(LeafLifetime)
	if notAfter.After(c.cert.NotAfter) {
		// A LEAF MAY NOT OUTLIVE ITS AUTHORITY. Verification fails on the CA's
		// expiry regardless, so issuing past it would hand an operator a
		// certificate whose printed dates lie about when their node stops working.
		notAfter = c.cert.NotAfter
	}

	return &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{c.deployment},
		},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  notAfter,
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}, nil
}

func (c *CA) sign(tmpl *x509.Certificate) (Bundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: generate a key: %w", err)
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: sign a certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: encode a key: %w", err)
	}

	return Bundle{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		CAPEM:   c.CertPEM(),
	}, nil
}

// Write puts a bundle on disk for an operator to copy to a node.
//
// REFUSES TO OVERWRITE. Re-issuing over a live node's directory would leave that
// host with a key it never loaded and a certificate it cannot use until someone
// restarts it — and if the write half-fails, with neither.
func (b Bundle) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("wirecert: create %s: %w", dir, err)
	}

	keyPath := filepath.Join(dir, "node.key")

	if err := writeSecret(keyPath, b.KeyPEM); err != nil {
		if os.IsExist(err) {
			// SAYS WHAT TO DO. writeSecret returns the bare os error so its other
			// caller can detect the race that means "someone else initialised this
			// authority first"; here there is no race, only an operator pointing at a
			// directory that already holds a bundle.
			return fmt.Errorf(
				"wirecert: %s already exists and billet will not overwrite it. That node is "+
					"probably already enrolled — re-issuing would leave it holding a key this "+
					"bundle does not match until someone restarts it. Write to a new directory "+
					"with --out, or move this one aside deliberately", keyPath)
		}

		return err
	}

	if err := writePublic(filepath.Join(dir, "node.crt"), b.CertPEM); err != nil {
		return err
	}

	if err := writePublic(filepath.Join(dir, "ca.crt"), b.CAPEM); err != nil {
		return err
	}

	return syncDir(dir)
}

// Deployment reads the installation a bundle was issued for.
//
// THE CERTIFICATE IS WHERE A NODE'S DEPLOYMENT COMES FROM, and that removes the
// only step of enrollment an operator could not perform. A node's state
// directory mints a random identity when it has none — right for a control
// plane, which is where an installation BEGINS, and wrong for a node, which
// joins one. A freshly enrolled node invented an identity, the control plane
// compared it with its own and refused the registration, and no amount of
// copying certificates around could have fixed it.
//
// It is also one authority rather than two. The certificate already decides
// which deployment may connect at all — it is verified against that deployment's
// CA — so reading the identity from the same object cannot disagree with it.
func (b Bundle) Deployment() (string, error) {
	block, _ := pem.Decode(b.CertPEM)
	if block == nil {
		return "", errors.New("wirecert: the certificate is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("wirecert: parse the certificate: %w", err)
	}

	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] == "" {
		return "", errors.New(
			"wirecert: this certificate names no deployment. It was issued by a billet older " +
				"than the one reading it; re-issue the bundle with `billet ca issue`")
	}

	return cert.Subject.Organization[0], nil
}

// NodeName reads the node a bundle was issued for.
func (b Bundle) NodeName() (string, error) {
	block, _ := pem.Decode(b.CertPEM)
	if block == nil {
		return "", errors.New("wirecert: the certificate is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("wirecert: parse the certificate: %w", err)
	}

	return cert.Subject.CommonName, nil
}

// ServerTLS is the control plane's side of the wire.
//
// RequireAndVerifyClientCert is the whole point: a connection without a
// certificate this CA signed never reaches a handler, so no handler has to
// remember to check.
func ServerTLS(b Bundle) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("wirecert: load the server certificate: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CAPEM) {
		return nil, errors.New("wirecert: the CA certificate could not be parsed for verification")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLS is a node's side of the wire.
func ClientTLS(b Bundle) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("wirecert: load the node certificate: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CAPEM) {
		return nil, errors.New("wirecert: the CA certificate could not be parsed for verification")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// LoadBundle reads a bundle a node was given.
func LoadBundle(certPath, keyPath, caPath string) (Bundle, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: read %s: %w", certPath, err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: read %s: %w", keyPath, err)
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: read %s: %w", caPath, err)
	}

	return Bundle{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caPEM}, nil
}

// ExpiresSoon reports whether a certificate is close enough to expiry to
// complain about, and how long it has left.
//
// The control plane calls this on the certificate a node connected with, so the
// warning lands while that node is still WORKING. A check that only fires on
// failure would tell an operator their host is down, which they already know.
func ExpiresSoon(cert *x509.Certificate) (time.Duration, bool) {
	left := time.Until(cert.NotAfter)

	return left, left < ExpiryWarning
}

func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)

	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("wirecert: pick a serial number: %w", err)
	}

	return serial, nil
}

func writeSecret(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return err
		}

		return fmt.Errorf("wirecert: write %s: %w", path, err)
	}

	return finish(f, path, body)
}

func writePublic(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("wirecert: %s already exists; billet will not overwrite it", path)
		}

		return fmt.Errorf("wirecert: write %s: %w", path, err)
	}

	return finish(f, path, body)
}

func finish(f *os.File, path string, body []byte) error {
	defer func() { _ = f.Close() }()

	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("wirecert: write %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("wirecert: sync %s: %w", path, err)
	}

	return nil
}

// syncDir persists the directory entries, not just the file contents.
//
// The same reason the deployment id does it: a synced file whose directory entry
// was lost is a file nothing can find, and here that means a CA key that exists
// on the platter and not in the filesystem — the half-initialised state, arrived
// at by power loss rather than by mistake.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("wirecert: open %s to sync it: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("wirecert: sync %s: %w", dir, err)
	}

	return nil
}
