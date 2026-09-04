// Package wirecert issues the certificates the node wire authenticates with.
//
// THE PROBLEM IT SOLVES: a node names itself in the request path and, without
// this, nothing verifies the claim. Anything that could reach the listener could
// call itself any node, bind leases, take commands, and ask for a JIT registration
// — a credential that registers a runner against the organisation. Without it the
// wire is safe only on loopback.
//
// The design is deliberately small. One CA per deployment, held by the control
// plane, and one certificate per node. There is no OCSP and no intermediate: a
// deployment with a compromised node revokes its certificate or re-issues the CA,
// which is a real cost and an honest one at this size. What matters is that the
// authenticated name in the certificate is the ONLY thing that decides which node
// a request is from.
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
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClockSkew is how far before its issuance a certificate becomes valid.
//
// Two machines' clocks disagree, and a node whose clock is a few minutes behind
// the control plane's would otherwise reject a certificate it was just handed.
// An hour is far more than any sane deployment drifts and costs nothing.
//
// NAMED, because a reader of the certificate has to undo it: NotBefore is an
// hour BEFORE the moment a certificate was issued, so anything comparing "when
// was this minted" against a wall clock has to add this back. Revocation by
// cutoff does exactly that, and reading NotBefore as the issuance time refused
// every replacement issued within an hour of a revocation.
const ClockSkew = time.Hour

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

// MinIssuedLifetime is the shortest leaf IssueNodeFor will mint.
//
// A NODE RENEWS ON A FIVE-MINUTE SWEEP ONCE LESS THAN A THIRD OF THE LIFE
// REMAINS, so the final third has to be longer than a sweep or the renewal can
// fall between two sweeps and the node expires in place, which is a host that
// has to be re-enrolled by hand. Twenty minutes puts the window at six minutes
// forty seconds against a five-minute cadence; ten, the first value chosen,
// put it at three minutes twenty and would have been missed.
const MinIssuedLifetime = 20 * time.Minute

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

// ErrAuthorityLost means this deployment had an authority and its files are gone.
//
// LOSING BOTH FILES IS INDISTINGUISHABLE FROM A FIRST RUN unless something
// remembers, and "mint a new one" is the wrong answer to exactly one of those.
// A restored backup that omitted the CA directory, a state directory recreated
// by a provisioning script, an operator clearing what they thought was cache —
// each looks like day one, and each would silently produce a NEW authority that
// every issued node bundle fails to verify against. The whole fleet drops off at
// once, and the control plane looks perfectly healthy.
var ErrAuthorityLost = errors.New("wirecert: this deployment had a certificate authority and it is gone")

// ErrForeignAuthority means the CA on disk belongs to a different deployment.
var ErrForeignAuthority = errors.New("wirecert: this authority was issued for another deployment")

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
func LoadOrCreateCA(stateDir, deployment string) (*CA, error) {
	// THE CALLER NAMES THE STATE DIRECTORY, NOT THE CA DIRECTORY, and that is what
	// lets the loss marker outlive the thing it witnesses. The marker cannot live
	// inside the ca directory — the failure it exists for is that directory being
	// omitted from a backup, which would take the witness with it — and deriving
	// its location from a parent path is worse than it sounds: two deployments
	// whose ca directories happen to share a parent would share one marker.
	//
	// So the layout is billet's to decide, in one place: the authority lives in
	// <state_dir>/ca, and the fact that it exists is recorded beside it.
	dir := CADir(stateDir)

	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")

	// THE CERTIFICATE IS READ FIRST, AND THAT IS LOAD BEARING. A rotation renames
	// ca.key and then ca.crt, so reading in this order makes a new certificate
	// proof that the new key is already in place — which leaves exactly one torn
	// read possible, the new key beside the old certificate, and repairPair below
	// resolves that one. The other order would produce the opposite tear, whose
	// missing half is a key that has already been renamed away.
	certPEM, certErr := readPublic(certPath)
	keyPEM, keyErr := readSecret(keyPath)

	switch {
	case certErr == nil && keyErr == nil:
		ca, err := parseCA(certPEM, keyPEM, deployment)
		if err != nil {
			if ca, err = repairPair(dir, certPEM, deployment, err); err != nil {
				return nil, err
			}
		}

		// BACKFILLED ON A SUCCESSFUL LOAD, so an installation created before the
		// marker existed is protected from the second boot onwards. Without this,
		// every deployment that predates this code keeps the old behaviour forever
		// — the upgrade silently does nothing for exactly the installations that
		// have the most to lose.
		if err := noteAuthority(stateDir, deployment); err != nil {
			return nil, err
		}

		return ca, nil

	case os.IsNotExist(certErr) && os.IsNotExist(keyErr):
		// BOTH GONE IS NOT AUTOMATICALLY DAY ONE. The marker below is the only
		// thing that can tell a first run from a restore that dropped the CA
		// directory, and getting it wrong mints a new authority that every issued
		// bundle fails against.
		if had, err := hadAuthority(stateDir); err != nil {
			return nil, err
		} else if had {
			return nil, fmt.Errorf(
				"%w: %s records that one was created here, but %s and %s are missing. Billet "+
					"will not mint a replacement — a new authority invalidates every node "+
					"certificate this deployment ever issued, and the whole fleet stops "+
					"connecting at once. Restore the directory from backup, or delete %s to "+
					"start a new authority deliberately and re-issue every node",
				ErrAuthorityLost, markerPath(stateDir), certPath, keyPath, markerPath(stateDir))
		}

		// NOTHING REPLACED: this is the first authority this deployment has ever
		// had, so it makes no claim about a predecessor.
		return createCA(stateDir, dir, certPath, keyPath, deployment, "")

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

// repairPair resolves the ONE torn read a rotation can produce, and refuses
// everything else.
//
// A rotation installs the new generation with two renames, ca.key and then
// ca.crt, and there is no way to make two renames one operation. So a reader
// that takes ca.crt before ca.key can come away with the NEW key beside the OLD
// certificate — a pair that does not hold together, on a deployment where
// nothing is wrong. Before this, a control plane starting in that instant
// refused with "ca.key is not the key for ca.crt", which describes a damaged
// authority and sends an operator looking for one.
//
// THE REPAIR IS EXACT RATHER THAN TOLERANT. Rotate publishes ca-previous.key,
// and syncs it, BEFORE it starts renaming — so in that torn state the key for
// the certificate on disk is sitting right there, and asking whether it pairs
// answers the question rather than guessing at it. In every committed state the
// question cannot fire: with no rotation running there is no previous key, and
// during an overlap ca.crt is the NEW certificate, which the previous key does
// not belong to. So a genuinely mismatched pair still returns the error it
// always did, with the original error preserved when the repair does not apply.
func repairPair(dir string, certPEM []byte, deployment string, mismatch error) (*CA, error) {
	prevKeyPEM, err := readSecret(filepath.Join(dir, previousCAKeyFile))
	if err != nil {
		// COULD NOT TELL IS NOT A REPAIR. The mismatch is what the caller asked
		// about and it is still true, so it is what comes back.
		return nil, mismatch
	}

	ca, err := parseCA(certPEM, prevKeyPEM, deployment)
	if err != nil {
		return nil, mismatch
	}

	return ca, nil
}

// createCA mints an authority, recording the one it replaces when there is one.
//
// THE REPLACED AUTHORITY IS NAMED IN THE CERTIFICATE ITSELF, which is what lets
// `billet ca retire` tell this deployment's previous generation from any other
// authority that happens to carry the same deployment id — see
// replacedAuthority.
func createCA(stateDir, dir, certPath, keyPath, deployment, replaces string) (*CA, error) {
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
			// EMPTY UNLESS THIS ONE REPLACED SOMETHING. An OU rather than a
			// custom extension because billet has no registered OID arc to hang
			// one from, and a made-up private OID is somebody else's to allocate;
			// an organizational unit is an ordinary directory attribute, is
			// ignored by every verifier, and shows up in `openssl x509 -text`
			// where an operator can read it.
			OrganizationalUnit: replacesOU(replaces),
		},
		NotBefore:             now.Add(-ClockSkew),
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
			return LoadOrCreateCA(stateDir, deployment)
		}

		return nil, err
	}

	if err := writePublic(certPath, certPEM); err != nil {
		return nil, err
	}

	// WRITTEN LAST, so a crash mid-creation leaves no claim that an authority
	// exists here. The marker's only job is to make a later ABSENCE meaningful:
	// an empty directory is day one, and an empty directory that once held an
	// authority is a loss.
	if err := noteAuthority(stateDir, deployment); err != nil {
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

// replacesPrefix marks the organizational unit that names a replaced authority.
//
// PREFIXED, so an OU billet did not write is not mistaken for a claim: this
// value decides whether a private key may be unlinked, and "some string in the
// OU" is not the same fact as "billet recorded a predecessor here".
const replacesPrefix = "billet-replaces:"

// replacesOU renders the claim, or nothing when there is no predecessor.
func replacesOU(replaces string) []string {
	if replaces == "" {
		return nil
	}

	return []string{replacesPrefix + replaces}
}

// replacedAuthority reads the authority a certificate says it replaced.
//
// THREE ANSWERS, NOT TWO, and the third is the whole reason this exists. A
// certificate that NAMES a predecessor lets `billet ca retire` prove the pair
// under ca-previous.* is that generation rather than merely an authority
// carrying the same deployment id — the gap the generation claim closes. A
// certificate that names NONE is either the first authority a deployment ever
// had or one minted by a billet older than this, and neither can be refused: the
// second is every deployment that rotated before this shipped.
//
// A SECOND CLAIM IS A REFUSAL. One certificate cannot have replaced two
// authorities, so an OU list carrying two is a certificate somebody has been
// editing, and guessing which one to believe is exactly the guess this check
// exists to remove.
func replacedAuthority(cert *x509.Certificate) (string, bool, error) {
	var (
		fingerprint string
		claimed     bool
	)

	for _, ou := range cert.Subject.OrganizationalUnit {
		rest, ok := strings.CutPrefix(ou, replacesPrefix)
		if !ok {
			continue
		}

		if claimed {
			return "", false, fmt.Errorf(
				"%w: this authority names more than one predecessor, so billet cannot tell "+
					"which generation it replaced", ErrForeignAuthority)
		}

		fingerprint, claimed = rest, true
	}

	return fingerprint, claimed, nil
}

// markerPath names the file that records an authority once existed here.
//
// BESIDE THE CA DIRECTORY, NOT INSIDE IT, because a witness that disappears with
// the thing it witnesses is not a witness. The failure this exists for — a backup
// or a provisioning script that omits the ca directory — would otherwise take the
// marker with it, hadAuthority would answer false, and a replacement authority
// would be minted exactly as before.
func markerPath(stateDir string) string {
	return filepath.Join(stateDir, "authority-created")
}

// CADir is where a deployment's authority lives inside its state directory.
func CADir(stateDir string) string { return filepath.Join(stateDir, "ca") }

// noteAuthority records that an authority exists, idempotently.
func noteAuthority(stateDir, deployment string) error {
	path := markerPath(stateDir)

	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("wirecert: read %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("wirecert: create %s: %w", filepath.Dir(path), err)
	}

	if err := writePublic(path, []byte(deployment+"\n")); err != nil {
		if os.IsExist(err) {
			return nil // another process won; the fact is what matters, not the writer
		}

		return err
	}

	return syncDir(filepath.Dir(path))
}

// hadAuthority reports whether one was ever created for this directory.
func hadAuthority(stateDir string) (bool, error) {
	_, err := os.Stat(markerPath(stateDir))
	if err == nil {
		return true, nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, fmt.Errorf("wirecert: read %s: %w", markerPath(stateDir), err)
}

func parseCA(certPEM, keyPEM []byte, deployment string) (*CA, error) {
	// THE PAIR RULES LIVE IN ONE PLACE. parsePair is also what validates an
	// ARCHIVE's authority bytes before a restore publishes them, and a second
	// copy of these checks is how a backup comes to accept what the control
	// plane it restores onto refuses.
	cert, key, err := parsePair(keyPEM, certPEM)
	if err != nil {
		return nil, err
	}

	// AND IT MUST BE THIS DEPLOYMENT'S AUTHORITY. Verifying against the CA is
	// what decides which deployment may connect at all, so a CA restored from
	// somewhere else silently re-points that decision: a holder of the OTHER
	// installation's node certificate connects, names this deployment in its
	// registration body, and is accepted.
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != deployment {
		return nil, fmt.Errorf(
			"%w: this control plane is deployment %s and %v was issued for %v. A certificate "+
				"authority decides which nodes may connect, so using another installation's "+
				"would let its nodes drive this one",
			ErrForeignAuthority, deployment, "ca.crt", cert.Subject.Organization)
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

	tmpl, err := c.leafTemplate("billet control plane", LeafLifetime)
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
	return c.IssueNodeFor(name, LeafLifetime)
}

// IssueNodeFor mints a node's certificate good for the given lifetime rather
// than LeafLifetime.
//
// A SHORT LEAF IS HOW A ROTATION IS REHEARSED. A node renews once less than a
// third of its certificate's life remains, so with year-long leaves nothing
// renews inside any rehearsal; a twenty-minute leaf renews inside the run. The
// bounds are enforced HERE, not only by the command that asks, because an
// exported entry point that trusts its caller is a second place the rule can be
// missing (the alloc.New argument). The same cap as every leaf applies: nothing
// outlives the authority.
func (c *CA) IssueNodeFor(name string, lifetime time.Duration) (Bundle, error) {
	if name == "" {
		return Bundle{}, errors.New("wirecert: a node certificate needs a node name")
	}

	if lifetime < MinIssuedLifetime || lifetime > LeafLifetime {
		return Bundle{}, fmt.Errorf("wirecert: a node certificate's lifetime of %s is outside "+
			"[%s, %s]: a node renews on a five-minute sweep once a third of the life remains, "+
			"so a shorter leaf can expire between two sweeps, and a longer one is not something "+
			"this authority issues", lifetime, MinIssuedLifetime, LeafLifetime)
	}

	tmpl, err := c.leafTemplate(name, lifetime)
	if err != nil {
		return Bundle{}, err
	}

	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}

	return c.sign(tmpl)
}

func (c *CA) leafTemplate(cn string, lifetime time.Duration) (*x509.Certificate, error) {
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()

	notAfter := now.Add(lifetime)
	if notAfter.After(c.cert.NotAfter) {
		// A LEAF MAY NOT OUTLIVE ITS AUTHORITY. Verification fails on the CA's
		// expiry regardless, so issuing past it would hand an operator a
		// certificate whose printed dates lie about when their node stops working.
		//
		// SILENT UNTIL IT MATTERS, WHICH IS THE TRAP. Once the CA has less than a
		// leaf's life left, every certificate it issues is quietly shorter than
		// the last — renewals keep working and keep getting cheaper, until one day
		// they are hours long and then the whole fleet stops together. Capped is
		// therefore something to SAY, not just to do; see CA.Capping.
		notAfter = c.cert.NotAfter
	}

	return &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{c.deployment},
		},
		NotBefore: now.Add(-ClockSkew),
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

// ServerTLS is the control plane's side of the OPERATIONAL wire.
//
// RequireAndVerifyClientCert, and the whole of the two-listener split turns on
// it. This was VerifyClientCertIfGiven so that a machine with no certificate
// could reach /v1/ca and /v1/enroll, with every other route refused in the
// handler -- which authenticated correctly and cost the fleet its availability.
// A certless caller completed a handshake, asked for the authority, and held a
// connection in keep-alive until the listener's idle timeout, out of a
// connection budget it SHARED with real nodes; once that budget was full, Accept
// blocked before the kernel accept and a healthy node was never admitted at all.
//
// A cap cannot separate the two, because the permit is taken BEFORE the
// handshake -- the handshake is itself work an anonymous caller can ask for --
// and which caller holds a certificate is not known until after it. So the
// separation has to be the handshake itself: those two routes moved to their own
// listener (BootstrapTLS, nodeplane.BootstrapHandler) and this one refuses a
// connection that presents nothing, before Go's HTTP server ever sees it.
//
// nodeplane.authorise still demands a verified chain on every route. It is no
// longer the only thing standing there, and it is kept because Handler can be
// built without TLS at all -- a loopback wire, and every handler-level test.
func ServerTLS(b Bundle) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("wirecert: load the server certificate: %w", err)
	}

	// A BUNDLE, not one certificate. During a rotation this carries both the new
	// authority and the one being retired, so a node that has renewed and a node
	// that has not are both still recognised. AppendCertsFromPEM reads every
	// certificate in the input.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CAPEM) {
		return nil, errors.New("wirecert: the CA certificate could not be parsed for verification")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// REQUIRE, so a caller with nothing to present is refused in the handshake
		// and never occupies a slot an enrolled node needs. The two routes that
		// exist for a machine with no certificate are not on this listener.
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS13,
	}, nil
}

// BootstrapTLS is the control plane's side of the ENROLLMENT wire: the small,
// separately bound listener serving /v1/ca and /v1/enroll.
//
// NoClientCert, and that is the honest description rather than a relaxation. A
// machine reaching here has no certificate by definition, so asking for one buys
// nothing and costs an X.509 parse and a chain verification that a stranger
// chooses the inputs to. What secures these two routes is elsewhere and always
// was: the node compares this authority's fingerprint against a value an operator
// read off the control plane, asking requires a join token, and admission waits
// for a human to compare the node's own fingerprint back.
//
// It presents the SAME server certificate as the operational wire, so whatever
// name a node dials this listener by must be among the certificate's subject
// names -- server.node_tls_hosts, or the concrete listen hosts it is derived from.
func BootstrapTLS(b Bundle) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("wirecert: load the server certificate: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLS is a node's side of the wire.
//
// VERIFIES THE LEAF AGAINST ITS OWN CA, not merely that the certificate and key
// are a pair. A bundle whose node.crt came from one deployment and whose ca.crt
// came from another would otherwise load cleanly — and since the node adopts its
// DEPLOYMENT from the leaf, it would write the wrong identity permanently, trust a
// server that would reject it, and then refuse the correct bundle as a conflict.
func ClientTLS(b Bundle) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("wirecert: load the node certificate: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CAPEM) {
		return nil, errors.New("wirecert: the CA certificate could not be parsed for verification")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("wirecert: parse the node certificate: %w", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf(
			"wirecert: this node's certificate was not issued by the authority beside it, so "+
				"the control plane would reject it: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// LoadBundle reads a bundle a node was given.
//
// The node's key is held to the same standard as the control plane's: it is the
// credential that lets this host act as this node, and a host whose key any
// local user can read is a host any local user can impersonate.
func LoadBundle(certPath, keyPath, caPath string) (Bundle, error) {
	certPEM, err := readPublic(certPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: read %s: %w", certPath, err)
	}

	keyPEM, err := readSecret(keyPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: read %s: %w", keyPath, err)
	}

	caPEM, err := readPublic(caPath)
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

// maxPEM bounds what will be read as a certificate or a key.
//
// A key is a few hundred bytes. Without a limit, a path pointing at something
// enormous — a device, a log, a mistake — is read entirely into the control
// plane's memory before anything notices it is not a key.
const maxPEM = 1 << 20

// readSecret reads a private key, refusing anything a private key must not be.
//
// FAIL CLOSED ON THE FILE ITSELF, because creation's 0600 says nothing about
// what is there NOW. A backup that restored ca.key as 0644 into a traversable
// directory starts billet perfectly happily while any local user copies the
// authority and mints node identities at will. A symlink is refused for the
// same reason: the path billet was told to read is the only one it should read.
// ReadSecret is readSecret for callers outside this package that hold a private
// key of their own — the staged enrollment key, which is a node identity waiting
// to be signed and has to meet the same bar as one that already is.
func ReadSecret(path string) ([]byte, error) { return readSecret(path) }

func readSecret(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err // may be os.IsNotExist, which callers branch on
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf(
			"wirecert: %s is a symlink; billet reads a private key only from the path it was "+
				"given, so that what it loads is what an operator secured", path)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("wirecert: %s is not a regular file", path)
	}

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf(
			"wirecert: %s is mode %04o and must not be readable by anyone else; it signs every "+
				"node identity in this deployment. Run: chmod 600 %s", path, perm, path)
	}

	return readCapped(path)
}

// readPublic reads a certificate, which is not a secret but is still a file
// billet is about to trust.
func readPublic(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("wirecert: %s is a symlink", path)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("wirecert: %s is not a regular file", path)
	}

	return readCapped(path)
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(io.LimitReader(f, maxPEM+1))
	if err != nil {
		return nil, fmt.Errorf("wirecert: read %s: %w", path, err)
	}

	if len(body) > maxPEM {
		return nil, fmt.Errorf("wirecert: %s is larger than %d bytes, which no key or "+
			"certificate is", path, maxPEM)
	}

	return body, nil
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

// RenewalDue reports whether a certificate is far enough through its life to
// replace, and how long it has left.
//
// AT A THIRD OF THE WAY FROM THE END, deliberately earlier than ExpiryWarning.
// A warning is for a human who may be on holiday; this is for the node itself,
// and the window has to be wide enough that a control plane which is down for a
// week, or a node powered off for a month, still has time to renew when it comes
// back. A node that lets its certificate expire cannot renew — renewal is
// authenticated by the certificate being renewed — so it has to be re-enrolled
// by hand, which is the outcome this width exists to avoid.
//
// Computed from the certificate's OWN lifetime rather than from LeafLifetime, so
// a leaf shortened by the CA's own expiry still renews proportionally rather
// than being judged against a year it never had.
//
// THE BACKDATED HOUR IS NOT LIFE. leafTemplate sets NotBefore an hour before
// issue (ClockSkew) so a host whose clock runs behind can use the certificate
// at once; that hour was never time the certificate had left. Counting it made
// a twenty-minute leaf look eighty minutes long and due the moment it was
// issued, which a rehearsal with short leaves found. A certificate that was not
// backdated (nothing billet issues) is measured from its NotBefore as before.
func RenewalDue(cert *x509.Certificate) (time.Duration, bool) {
	left := time.Until(cert.NotAfter)

	issued := cert.NotBefore.Add(ClockSkew)
	if !issued.Before(cert.NotAfter) {
		issued = cert.NotBefore
	}

	lifetime := cert.NotAfter.Sub(issued)

	return left, left < lifetime/3
}

// Serial is a certificate's serial number as the ledger stores it.
//
// Hex, because a serial is a 128-bit integer and every other rendering of one —
// decimal, base64 — makes it harder to match against what `openssl x509` prints
// when somebody is trying to work out which credential they are looking at.
func Serial(cert *x509.Certificate) string {
	return fmt.Sprintf("%x", cert.SerialNumber)
}

// SignNodeCSR issues a node certificate for a key the node generated itself.
//
// THE PRIVATE KEY NEVER CROSSES THE WIRE, which is the whole reason renewal
// takes a CSR rather than returning a fresh bundle. A renewal endpoint that
// minted the key server-side would put a node's identity on the network once a
// year, and into the control plane's memory and logs on the way.
//
// THE NAME COMES FROM THE CALLER, NOT FROM THE CSR. The subject in a CSR is
// whatever the requester typed; the authenticated identity is what the wire
// proved. Signing the former would let any node with a valid certificate mint
// one for any name it liked — which is every node able to impersonate every
// other, through the endpoint meant to keep them working.
func (c *CA) SignNodeCSR(name string, csrPEM []byte) (Bundle, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return Bundle{}, errors.New("wirecert: not a PEM certificate request")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: parse the certificate request: %w", err)
	}

	// CHECKED, because a CSR carries its own signature over its own public key
	// and an unverified one proves nothing about who holds the private half.
	if err := csr.CheckSignature(); err != nil {
		return Bundle{}, fmt.Errorf("wirecert: the certificate request is not correctly signed: %w", err)
	}

	tmpl, err := c.leafTemplate(name, LeafLifetime)
	if err != nil {
		return Bundle{}, err
	}

	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return Bundle{}, fmt.Errorf("wirecert: sign the certificate request: %w", err)
	}

	return Bundle{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		CAPEM:   c.CertPEM(),
	}, nil
}

// NewNodeCSR generates a key and a certificate request for a node name.
//
// Returns the request to send and the key to keep. The key is PEM and is written
// 0600 by the caller; it is the node's identity and never leaves the machine.
func NewNodeCSR(name string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("wirecert: generate a key: %w", err)
	}

	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("wirecert: create a certificate request: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("wirecert: encode a key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// Capping reports whether this authority is close enough to its own expiry that
// the certificates it issues are being shortened, and how long it has left.
//
// THE FAILURE IT NAMES IS A SLOW ONE. A leaf may not outlive its authority, so
// from one leaf-lifetime out every certificate issued is shorter than a full
// life: renewals come round faster and faster, nothing errors, and then the whole
// fleet expires on the same day the authority does.
//
// Rotating is an overlap, not a switch: issue a new authority, keep trusting the
// old one while nodes pick the new one up through renewal, then retire it. That
// is why renewal returns the CA alongside the certificate.
func (c *CA) Capping() (time.Duration, bool) {
	left := time.Until(c.cert.NotAfter)

	return left, left < LeafLifetime
}
