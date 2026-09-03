package wirecert

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AuthorityFile names one file that is part of a deployment's authority.
type AuthorityFile struct {
	// Name is the archive-stable name, which is also the basename on disk for
	// everything except the marker.
	Name string
	// Secret says whether it is a private key. A backup writes everything 0600
	// regardless; this decides what an ERROR may say about it and how it is read.
	Secret bool
	// Required says whether an authority is incomplete without it. The previous
	// generation exists only while a rotation is running.
	Required bool
}

// AuthorityFiles is the ALLOWLIST: everything that belongs to a deployment's
// authority, and nothing that does not.
//
// THE UNIT IS LARGER THAN "THE KEY AND THE CERTIFICATE", and each of the other
// three is here for a reason that cost something to learn:
//
//   - authority-created lives OUTSIDE the ca directory deliberately, so that
//     directory going missing is detectable. A backup that captured the ca
//     directory alone would restore an authority with no witness, and the next
//     loss would read as day one and mint a replacement — which is the whole
//     failure ErrAuthorityLost exists to refuse.
//   - ca-previous.crt and ca-previous.key are operationally REQUIRED while a
//     rotation is running: the previous key is what signs the certificate the
//     control plane presents, so an archive without it restores a deployment
//     that every un-renewed node fails to verify.
//
// An ALLOWLIST rather than "copy the ca directory", because Rotate leaves
// ca.crt.new / ca.key.new behind if it dies partway and ca.lock lives beside
// them; copying whatever is there would put a half-minted authority in an
// archive that says it is complete.
var AuthorityFiles = []AuthorityFile{
	{Name: "ca.key", Secret: true, Required: true},
	{Name: "ca.crt", Required: true},
	{Name: "ca-previous.key", Secret: true},
	{Name: "ca-previous.crt"},
	{Name: markerFile, Required: true},
}

// markerFile is the authority marker's basename. It sits in the STATE
// directory, not in the ca directory — see markerPath.
const markerFile = "authority-created"

// AuthorityPath is where one allowlisted file lives under a state directory.
func AuthorityPath(stateDir, name string) string {
	if name == markerFile {
		return markerPath(stateDir)
	}

	return filepath.Join(CADir(stateDir), name)
}

// Authority is a deployment's authority as it stands on disk.
type Authority struct {
	// Present maps an allowlisted name to its bytes.
	Present map[string][]byte
	// Unexpected names anything in the ca directory that is not allowlisted.
	// REPORTED RATHER THAN COPIED: a leftover ca.crt.new from an interrupted
	// rotation is worth an operator's attention and must not travel in an
	// archive as though it were authority state.
	Unexpected []string
}

// Rotating reports whether a rotation is running.
func (a Authority) Rotating() bool { return len(a.Present["ca-previous.crt"]) > 0 }

// ReadAuthority collects the allowlisted authority state and refuses an
// incomplete one.
//
// THE CALLER MUST ALREADY HOLD LockAuthority. This reads five files that
// `billet ca rotate` mutates in sequence, so without the lock it can return a
// key from one generation beside a certificate from another — an archive that
// loads cleanly and verifies nothing.
//
// INCOMPLETE IS A REFUSAL, NOT A SHORTER ANSWER. A backup of a half-initialised
// authority is the trap ErrHalfInitialised exists to stop one layer down: it
// restores as a directory holding one of its two files, which billet then
// refuses to repair, on a host where nobody is expecting it.
func ReadAuthority(stateDir string) (Authority, error) {
	out := Authority{Present: map[string][]byte{}}

	for _, f := range AuthorityFiles {
		path := AuthorityPath(stateDir, f.Name)

		var (
			body []byte
			err  error
		)

		if f.Secret {
			body, err = readSecret(path)
		} else {
			body, err = readPublic(path)
		}

		switch {
		case err == nil:
			out.Present[f.Name] = body
		case os.IsNotExist(err):
		default:
			return Authority{}, fmt.Errorf("wirecert: read %s: %w", path, err)
		}
	}

	unexpected, err := unexpectedInCADir(stateDir)
	if err != nil {
		return Authority{}, err
	}

	out.Unexpected = unexpected

	if err := out.validate(stateDir); err != nil {
		return Authority{}, err
	}

	return out, nil
}

// validate refuses an authority that is not whole.
func (a Authority) validate(stateDir string) error {
	haveKey := len(a.Present["ca.key"]) > 0
	haveCert := len(a.Present["ca.crt"]) > 0

	switch {
	case !haveKey && !haveCert:
		return fmt.Errorf(
			"%w: %s holds no certificate authority at all. A control plane that has never "+
				"started has none yet; run it once, or point at the state directory that has one",
			ErrAuthorityLost, CADir(stateDir))
	case haveKey != haveCert:
		present, missing := AuthorityPath(stateDir, "ca.key"), AuthorityPath(stateDir, "ca.crt")
		if !haveKey {
			present, missing = missing, present
		}

		return fmt.Errorf(
			"%w: %s exists but %s does not, so there is no complete authority here to capture. "+
				"An archive holding half of one restores a deployment billet refuses to start and "+
				"cannot repair", ErrHalfInitialised, present, missing)
	}

	// THE KEY MUST BE THIS CERTIFICATE'S KEY, checked here rather than trusted to
	// the restore: an archive is written once and read on the worst day of a
	// deployment's life, and two unrelated halves load happily and then sign
	// leaves nothing can verify.
	if _, _, err := parsePair(a.Present["ca.key"], a.Present["ca.crt"]); err != nil {
		return err
	}

	prevKey := len(a.Present["ca-previous.key"]) > 0
	prevCert := len(a.Present["ca-previous.crt"]) > 0

	if prevKey != prevCert {
		return fmt.Errorf(
			"%w: this deployment is mid-rotation and only half of the previous authority is "+
				"present (%s). The previous KEY signs what the control plane presents while the "+
				"fleet renews, so an archive without both leaves every un-renewed node unable to "+
				"verify it — finish or undo the rotation before backing up",
			ErrHalfInitialised, presentHalf(stateDir, prevKey))
	}

	if prevKey && prevCert {
		if _, _, err := parsePair(a.Present["ca-previous.key"], a.Present["ca-previous.crt"]); err != nil {
			return fmt.Errorf("the previous authority does not hold together: %w", err)
		}
	}

	if len(a.Present[markerFile]) == 0 {
		return fmt.Errorf(
			"%s is missing while %s holds a complete authority. That marker is what makes a "+
				"LATER absence mean loss rather than day one, so an archive without it restores a "+
				"deployment that would silently mint a replacement authority and drop every node "+
				"in the fleet. Start the control plane once to write it",
			markerPath(stateDir), CADir(stateDir))
	}

	return nil
}

func presentHalf(stateDir string, keyPresent bool) string {
	if keyPresent {
		return AuthorityPath(stateDir, "ca-previous.key")
	}

	return AuthorityPath(stateDir, "ca-previous.crt")
}

// parsePair proves a key and certificate belong together and that the
// certificate is an authority.
//
// ONE IMPLEMENTATION, called by parseCA as well: written twice these drift, and
// the divergence would be an archive accepted by the backup and refused by the
// control plane that restores it, or the reverse.
func parsePair(keyPEM, certPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, errors.New("wirecert: the CA certificate is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("wirecert: parse the CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("wirecert: the CA key is not PEM")
	}

	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("wirecert: parse the CA key: %w", err)
	}

	if !cert.IsCA {
		return nil, nil, errors.New("wirecert: the CA certificate is not a certificate authority")
	}

	// AND IT SIGNED ITSELF. billet's authorities are self-signed by construction,
	// so this is free to require — and without it the pair check proves only that
	// somebody put a matching key beside a certificate, not that the certificate
	// is the one that key produced. Every leaf verification on the wire already
	// depends on this holding, so a certificate that fails here is one no node
	// could have used anyway; what it changes is that a decision made from the
	// certificate's CONTENTS — which generation it says it replaced, and so
	// whether a private key may be unlinked — cannot be made from bytes anyone
	// assembled.
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf(
			"wirecert: this authority did not sign itself, so it is not an authority billet "+
				"minted and nothing it says about itself can be relied on: %w", err)
	}

	// THE KEY MUST BE THIS CERTIFICATE'S KEY. Two unrelated halves load happily
	// and then sign leaves nothing can verify — a failure that surfaces on a
	// node, days later, as a handshake error naming neither file.
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New(
			"wirecert: ca.key is not the key for ca.crt; every certificate signed with this " +
				"pair would fail verification on the node that presented it")
	}

	return cert, key, nil
}

// InstallAuthority writes a whole authority into a state directory that has
// none.
//
// EXPORTED SO A SECOND CONTROLLER CAN ADOPT ONE, which is the whole of what an
// active/passive pair needs from the identity store: not a shared mutable
// authority, but the ability for a host with nothing to end up holding exactly
// what the deployment already has. `billet ca issue` and a copy do the same job
// by hand; this is what does it without a person.
//
// THE CALLER MUST HOLD LockAuthority, for the reason ReadAuthority states: these
// are the files `billet ca rotate` mutates in sequence.
//
// IT CREATES AND NEVER REPLACES. Every write is O_EXCL, so a directory that
// already holds any part of an authority refuses rather than being merged into —
// the rule `billet local restore` states as "absent, byte-identical, or preserved
// and refused", and the reason it is absolute here is that the thing being
// written over would be the key every node in a fleet verifies against.
//
// AND IT VERIFIES WHAT IT WROTE. The bytes arrived over a network from a store,
// so nothing about them is billet's until they have been parsed, proved to hold
// together as a pair, and proved to name this deployment.
func InstallAuthority(stateDir, deployment string, files map[string][]byte) error {
	cert, err := ParseAuthorityPair(files["ca.key"], files["ca.crt"])
	if err != nil {
		return err
	}

	named, err := AuthorityDeployment(cert)
	if err != nil {
		return err
	}

	if named != deployment {
		return fmt.Errorf(
			"%w: the stored authority names deployment %s and this one is %s; an authority "+
				"decides which nodes may connect, so installing one that names somebody else "+
				"would silently re-point that decision",
			ErrForeignAuthority, named, deployment)
	}

	// THE PREVIOUS PAIR TRAVELS WHOLE OR NOT AT ALL, the rule ReadAuthority's own
	// validation states: the previous KEY is what signs the certificate a control
	// plane presents while the fleet renews, so half of it leaves every un-renewed
	// node unable to verify.
	prevKey, prevCert := files["ca-previous.key"], files["ca-previous.crt"]
	if (len(prevKey) > 0) != (len(prevCert) > 0) {
		return fmt.Errorf(
			"%w: the stored authority carries only half of a previous generation, so a "+
				"rotation cannot be completed from it", ErrHalfInitialised)
	}

	if len(prevKey) > 0 {
		if _, err := ParseAuthorityPair(prevKey, prevCert); err != nil {
			return fmt.Errorf("the stored previous authority does not hold together: %w", err)
		}
	}

	if err := os.MkdirAll(CADir(stateDir), 0o700); err != nil {
		return fmt.Errorf("wirecert: create %s: %w", CADir(stateDir), err)
	}

	// IN THE ORDER Rotate PUBLISHES IN, so that a crash part-way through leaves a
	// state the ordinary readers already answer correctly: a previous certificate
	// with no key beside it always means "not committed", and the current pair is
	// what a reader takes last.
	for _, f := range AuthorityFiles {
		body := files[f.Name]
		if len(body) == 0 {
			continue
		}

		path := AuthorityPath(stateDir, f.Name)

		var err error
		if f.Secret {
			err = writeSecret(path, body)
		} else {
			err = writePublic(path, body)
		}

		if err != nil {
			return fmt.Errorf("wirecert: install %s: %w", path, err)
		}
	}

	if err := syncDir(CADir(stateDir)); err != nil {
		return err
	}

	// THE MARKER LIVES OUTSIDE THE CA DIRECTORY, so its directory is synced too —
	// a marker whose entry was lost makes the next start read a complete authority
	// as day one.
	if err := syncDir(stateDir); err != nil {
		return err
	}

	// READ BACK THROUGH THE ORDINARY READER, which is the one that decides whether
	// a control plane will start. Anything this wrote that it would refuse is
	// better found here than at the next boot.
	if _, err := ReadAuthority(stateDir); err != nil {
		return fmt.Errorf(
			"wirecert: the authority installed from the identity store does not hold "+
				"together: %w", err)
	}

	return nil
}

// ParseAuthorityPair validates a CA key and certificate that are not on disk
// yet, returning the certificate.
//
// EXPORTED so a restore applies the same rules to an ARCHIVE's bytes before it
// publishes them, rather than discovering on the next control-plane start that
// what it installed does not hold together.
func ParseAuthorityPair(keyPEM, certPEM []byte) (*x509.Certificate, error) {
	cert, _, err := parsePair(keyPEM, certPEM)

	return cert, err
}

// AuthorityDeployment reads the deployment a CA certificate was issued for.
//
// THE SUBJECT ORGANIZATION IS THE ANSWER, and it is what parseCA compares
// against a live control plane's identity: verifying against the CA is what
// decides which nodes may connect, so an authority carrying another
// installation's name would silently re-point that decision.
func AuthorityDeployment(cert *x509.Certificate) (string, error) {
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] == "" {
		return "", fmt.Errorf("%w: this authority names no deployment", ErrForeignAuthority)
	}

	return cert.Subject.Organization[0], nil
}

// unexpectedInCADir lists what is in the ca directory and not allowlisted.
func unexpectedInCADir(stateDir string) ([]string, error) {
	dir := CADir(stateDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("wirecert: read %s: %w", dir, err)
	}

	allowed := map[string]bool{authorityLockFile: true}

	for _, f := range AuthorityFiles {
		if f.Name != markerFile {
			allowed[f.Name] = true
		}
	}

	var out []string

	for _, e := range entries {
		if allowed[e.Name()] {
			continue
		}

		out = append(out, filepath.Join(dir, e.Name()))
	}

	sort.Strings(out)

	return out, nil
}

// RotationLeftovers reports whether an unexpected entry looks like an
// interrupted rotation, which is the one an operator can act on directly.
func RotationLeftovers(unexpected []string) []string {
	var out []string

	for _, path := range unexpected {
		if strings.HasSuffix(path, ".new") {
			out = append(out, path)
		}
	}

	return out
}
