package wirecert

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// previousCAFile is the authority that is being retired, kept beside the one
// now issuing.
const previousCAFile = "ca-previous.crt"

// previousCAKeyFile is that authority's key, which signs what the control plane
// PRESENTS until every node has renewed.
const previousCAKeyFile = "ca-previous.key"

// Rotate replaces the issuing authority, keeping the old one trusted.
//
// AN OVERLAP, NOT A SWITCH, and the ordering is the whole design. A node trusts
// the authority it was given, so the moment the control plane starts PRESENTING
// a certificate from a new one, every node that has not yet heard about it fails
// to verify the server and drops out of the fleet. There is no way back from
// that over the wire, because the wire is what broke.
//
// So rotation runs in two phases:
//
//	billet ca rotate   new authority issues NODE certificates; the OLD one still
//	                   signs what the server presents, and both are trusted. Nodes
//	                   pick up the new one through ordinary renewal, which already
//	                   carries the authority alongside the certificate.
//	billet ca retire   once every node has renewed, the old authority is dropped
//	                   and the server presents a certificate from the new one.
//
// A node that misses the whole overlap has to be re-enrolled, which is why
// retiring is a separate command an operator runs when they can see the fleet
// has moved rather than something that happens on a timer.
func Rotate(stateDir, deployment string) (*CA, error) {
	// THE LOCK IS TAKEN HERE RATHER THAN BY THE COMMAND, because this is an
	// exported entry point and a rule enforced only at the CLI has a second way
	// in that does not enforce it — the same argument alloc.New makes about
	// re-applying its own safety rules.
	lock, err := LockAuthority(stateDir)
	if err != nil {
		return nil, err
	}

	ca, rotateErr := rotateLocked(stateDir, deployment)

	return ca, errors.Join(rotateErr, lock.Release())
}

func rotateLocked(stateDir, deployment string) (*CA, error) {
	dir := CADir(stateDir)

	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	prevPath := filepath.Join(dir, previousCAFile)

	// BOTH HALVES, AND "COULD NOT TELL" IS NOT "ABSENT". Publication below goes
	// through WriteFileAtomic, which REPLACES its destination — so unlike the
	// O_EXCL writes it took over from, nothing downstream refuses an existing
	// file, and a durable ca-previous.key left by an abandoned restore or an
	// operator's own recovery would be silently overwritten. That may be the only
	// copy of an authority key. A check is enough where an O_EXCL open would be
	// needed for the App key, and for one reason: this runs under LockAuthority,
	// so no other billet can create either file between the look and the write.
	//
	// TWO REFUSALS, NOT ONE, BECAUSE ONLY ONE OF THEM HAS A COMMAND BEHIND IT.
	// `billet ca retire` is gated on the CERTIFICATE — it prints "no rotation is
	// running" and exits 0 when that is absent — so answering a leftover KEY with
	// "finish it with billet ca retire" sent an operator to a command that told
	// them there was nothing to do, and left them with no way forward at all.
	if err := refuseIfPresent(prevPath, fmt.Sprintf(
		"a rotation is already under way — %s exists. Finish it with `billet ca retire` once "+
			"every node has renewed, then rotate again", prevPath)); err != nil {
		return nil, err
	}

	prevKeyPath := filepath.Join(dir, previousCAKeyFile)

	if err := refuseIfPresent(prevKeyPath, leftoverKeyAdvice(prevKeyPath, keyPath)); err != nil {
		return nil, err
	}

	current, err := readPublic(certPath)
	if err != nil {
		return nil, fmt.Errorf("wirecert: read the authority being replaced: %w", err)
	}

	currentKey, err := readSecret(keyPath)
	if err != nil {
		return nil, fmt.Errorf("wirecert: read the key of the authority being replaced: %w", err)
	}

	// MINTED ASIDE, THEN MOVED INTO PLACE. createCA writes with O_EXCL and, on
	// finding a key already there, falls back to LOADING the existing authority —
	// which is right for two processes racing to initialise one directory and
	// would silently make a rotation a no-op. Writing to temporary names first
	// keeps that guard intact and still produces a new authority.
	newCert := filepath.Join(dir, "ca.crt.new")
	newKey := filepath.Join(dir, "ca.key.new")

	for _, leftover := range []string{newCert, newKey} {
		if err := os.Remove(leftover); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("wirecert: clear %s: %w", leftover, err)
		}
	}

	// THE NEW AUTHORITY RECORDS THE ONE IT REPLACES, which is what makes the
	// pair under ca-previous.* provable later: `billet ca retire` unlinks a
	// private key, and "an authority for this deployment" is not the same fact as
	// "the generation this rotation replaced".
	replaced, err := FingerprintOfCAPEM(current)
	if err != nil {
		return nil, fmt.Errorf("wirecert: fingerprint the authority being replaced: %w", err)
	}

	fresh, err := createCA(stateDir, dir, newCert, newKey, deployment, replaced)
	if err != nil {
		return nil, err
	}

	// THE OLD PAIR IS COPIED ASIDE FIRST, and the new one is minted DIRECTLY
	// rather than by clearing the directory and asking LoadOrCreateCA for
	// another. That route would leave the authority momentarily missing while its
	// marker still said one had been created here — which is precisely the state
	// ErrAuthorityLost exists to refuse, so a rotation would either be blocked by
	// the guard or, worse, teach somebody to work around it.
	//
	// The key is kept as well as the certificate: it signs what the server
	// PRESENTS during the overlap, so nodes that have not renewed can still
	// verify the control plane. Keeping only the certificate would trust the old
	// fleet while making the server unverifiable to it.
	//
	// THE CERTIFICATE LEADS ITS KEY, and that ordering is the whole reason a
	// control plane can start at any instant during a rotation. ca-previous.crt
	// says a rotation was STARTED and ca-previous.key says it is COMMITTED, so a
	// reader landing between these two writes finds a certificate to widen its
	// trust with and no key to present with — and presenting with the CURRENT
	// authority is exactly right there, because the renames below have not run.
	// Written the other way round, the key would arrive first and there would be
	// no state in which the pair is half-published and inert.
	// AND EACH IS PUBLISHED WHOLE. writeSecret and writePublic create the FINAL
	// pathname with O_EXCL and write into it afterwards, so the name exists while
	// the file is still empty — which is a state that says "started" or
	// "committed" about bytes that are not there, and a crash or ENOSPC in that
	// instant leaves it permanently while ca-previous.crt blocks any retry. The
	// O_EXCL those two give up is not what protects this directory anyway:
	// LockAuthority is, and rotateLocked has already refused a rotation with a
	// previous certificate present. WriteFileAtomic also syncs the directory, so
	// the ordering these two comments rest on is durable and not merely
	// scheduled — which matters because the renames below leave a pair that only
	// ca-previous.key can repair.
	if err := WriteFileAtomic(prevPath, current, 0o644); err != nil {
		return nil, err
	}

	if err := WriteFileAtomic(filepath.Join(dir, previousCAKeyFile), currentKey, 0o600); err != nil {
		return nil, err
	}

	// THE KEY FIRST, matching how one is created: a crash between the two renames
	// must leave the half-initialised state that refuses loudly rather than a
	// certificate whose key belongs to something else.
	if err := os.Rename(newKey, keyPath); err != nil {
		return nil, fmt.Errorf("wirecert: install the new authority's key: %w", err)
	}

	if err := os.Rename(newCert, certPath); err != nil {
		return nil, fmt.Errorf("wirecert: install the new authority: %w", err)
	}

	if err := syncDir(dir); err != nil {
		return nil, err
	}

	return fresh, nil
}

// refuseIfPresent refuses when a path exists, and when billet cannot tell.
func refuseIfPresent(path, advice string) error {
	present, err := exists(path)
	if err != nil {
		return err
	}

	if present {
		return fmt.Errorf("wirecert: %s", advice)
	}

	return nil
}

// leftoverKeyAdvice says what to do about a previous key with no certificate.
//
// BILLET DOES NOT REMOVE IT. A private key whose certificate is gone is bytes
// nothing on the host explains, and the rule one credential over is that only
// ABSENT permits a deletion — so this names the file and hands the decision to a
// person. What it can do is answer the one question that decides it, and answer
// it from having READ both files rather than from reasoning about how the state
// arose: a key byte-identical to ca.key is a copy of an authority that is still
// here, and removing it loses nothing.
func leftoverKeyAdvice(prevKeyPath, keyPath string) string {
	base := fmt.Sprintf(
		"%s is there with no %s beside it, which is what an interrupted rotation or an "+
			"abandoned restore leaves. Billet will not write over a private key it cannot "+
			"account for, and `billet ca retire` will not remove it either — that command is "+
			"gated on the certificate. Look at the file and move it aside deliberately, then "+
			"rotate again", prevKeyPath, previousCAFile)

	prev, prevErr := readSecret(prevKeyPath)
	if prevErr != nil {
		return base
	}

	current, currentErr := readSecret(keyPath)
	if currentErr != nil {
		return base
	}

	if !bytes.Equal(prev, current) {
		return base + fmt.Sprintf(
			". It is NOT a copy of %s, so it belongs to some other authority and may be the "+
				"only one left of it", keyPath)
	}

	return base + fmt.Sprintf(
		". Billet has compared it with %s and they are byte-identical, so it is a copy of the "+
			"authority that is still installed and nothing is lost by removing it", keyPath)
}

// Retire drops the authority a rotation replaced.
//
// AFTER THE FLEET HAS MOVED, which only an operator can judge: a node that has
// not renewed still trusts only the old authority, and retiring it makes that
// node unable to verify the control plane. `billet ca show` reports how many
// nodes are still on the old one.
func Retire(stateDir, deployment string) error {
	lock, err := LockAuthority(stateDir)
	if err != nil {
		return err
	}

	return errors.Join(retireLocked(stateDir, deployment), lock.Release())
}

func retireLocked(stateDir, deployment string) error {
	dir := CADir(stateDir)

	if err := refuseRetireWhilePreviousKeyIsLoadBearing(dir, deployment); err != nil {
		return err
	}

	// THE KEY FIRST, WHICH IS THE MIRROR OF HOW A ROTATION PUBLISHES IT. Both
	// orders are safe for a reader — a reader is gated on the CERTIFICATE, so
	// removing that one first would simply end the overlap a moment early — and
	// this one is the WIDER of the two: a control plane starting between these
	// two removals presents with the current authority and still trusts the old
	// certificate, so a node that has not renewed keeps working for that moment
	// rather than stopping. It is also one rule rather than two, which is worth
	// more than the moment: the key is what commits a rotation and what uncommits
	// it, everywhere it is written or removed.
	for _, name := range []string{previousCAKeyFile, previousCAFile} {
		path := filepath.Join(dir, name)

		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("wirecert: remove %s: %w", path, err)
		}

		if err := syncDir(dir); err != nil {
			return err
		}
	}

	return nil
}

// refuseRetireWhilePreviousKeyIsLoadBearing stops a retire that would leave a
// deployment with no authority it can use.
//
// WHAT RETIRING ASSUMES IS THAT THE CURRENT PAIR TOOK OVER, and that assumption
// is what has to be checked rather than the shape of the wreckage beside it. So
// there is one way through: ca.crt and ca.key read, parse, match, AND name this
// deployment. Everything else refuses, and the only thing keyVerdict decides is
// which sentence the operator gets.
//
// TWO ROUNDS OF NARROWER RULES ARE WHY IT IS PUT THAT WAY. Asking "is something
// missing" let an absent ca.key through, beside a ca.crt that ca-previous.key
// matched — a deployment one `cp` from recovery, and the key deleted. Asking
// "does the previous key match ca.crt" then let a coherent authority from
// ANOTHER deployment through: it parses, it is a CA, its key is its key, and
// LoadOrCreateCA will not start on it — so the previous pair beside it was the
// only authority on the host that meant anything, and it was the one removed.
// Both were the same mistake, which is deriving permission from what is broken
// instead of from what is proved.
func refuseRetireWhilePreviousKeyIsLoadBearing(dir, deployment string) error {
	prevKeyPath := filepath.Join(dir, previousCAKeyFile)

	// NOTHING TO PROTECT IS THE ONLY CHEAP ANSWER. Everything below is about
	// whether removing this file is reversible, so an absent one — the ordinary
	// case, a rotation that committed and a fleet that renewed — short-circuits
	// before any judgement about the current pair is needed.
	present, err := exists(prevKeyPath)
	if err != nil {
		return err
	}

	if !present {
		return nil
	}

	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")

	// BEFORE ANY WAY THROUGH, because a healthy current pair says the rotation
	// COMPLETED and says nothing at all about what is under the previous names.
	if err := refusePreviousPairThatIsNotOurs(dir, prevKeyPath, deployment); err != nil {
		return err
	}

	certPEM, certErr := readPublic(certPath)
	if certErr == nil {
		if keyPEM, keyErr := readSecret(keyPath); keyErr == nil {
			// parseCA RATHER THAN parsePair: "this is an authority" is not "this
			// is OUR authority", and only the second means the current pair has
			// actually taken over.
			if _, caErr := parseCA(certPEM, keyPEM, deployment); caErr == nil {
				return nil
			}
		}
	}

	if certErr != nil {
		return fmt.Errorf(
			"wirecert: %s is present and billet cannot read %s to tell what it belongs to, so "+
				"removing it could be irreversible: %w", prevKeyPath, certPath, certErr)
	}

	switch keyBelongsTo(prevKeyPath, certPEM) {
	case keyBelongsHere:
		return fmt.Errorf(
			"wirecert: %s is the only key that matches %s, so removing it would leave a control "+
				"plane that cannot start. A rotation was interrupted between installing the new "+
				"key and the new certificate; billet is running on the previous authority in the "+
				"meantime. If %s is there, it is the certificate that rotation never installed — "+
				"finish or undo the rotation by hand before retiring",
			prevKeyPath, certPath, filepath.Join(dir, "ca.crt.new"))

	case keyUnreadable:
		// COULD NOT TELL IS NOT "NO KEY HERE", and this is the one place in this
		// function where that distinction decides whether a private key is
		// deleted. A ca-previous.key that billet cannot read — restored 0644, or
		// on a filesystem it cannot open — may still be the only key matching
		// ca.crt, and the control plane cannot start until somebody fixes the
		// file. Removing it turns a `chmod 600` away from recoverable into gone.
		return fmt.Errorf(
			"wirecert: %s does not hold together with %s, and billet cannot read %s to tell "+
				"whether that is the key holding this authority up. Removing it would be "+
				"irreversible if it is — look at the file (a private key must be mode 0600 and "+
				"a regular file) before retiring",
			keyPath, certPath, prevKeyPath)
	}

	return fmt.Errorf(
		"wirecert: %s is not an authority this deployment can use, so the pair beside it may be "+
			"the only one that is, and retiring removes exactly that. Billet will not start on "+
			"the current authority either — fix or restore it first, and retire once it is the "+
			"one in force", certPath)
}

// refusePreviousPairThatIsNotOurs stops a retire from unlinking a private key
// billet cannot account for.
//
// WHAT RETIRE IS FOR is dropping the authority a rotation replaced, and it
// removes a private key to do that — so the pair it removes has to be one billet
// can SHOW is that authority, not merely two files sitting under those names.
//
// parseCA ALONE CANNOT SHOW GENERATION, which was the gap: it proves the pair is
// an authority for THIS deployment, not that it is the one this rotation
// replaced, so a second self-minted CA under these names was retired like the
// real predecessor. refuseAnotherGeneration below is what closes that — the
// record binding the two generations is written into the certificate a rotation
// installs, so it travels with ca.crt rather than being a fifth file the backup
// allowlist and restore's publication order would have to learn. A key that
// belongs to no certificate here is bytes nothing on this host explains, and
// unlinking those is the one act that cannot be undone. A healthy current pair
// proves the rotation completed and proves nothing whatever about what is under
// the previous names, which is why this runs before that.
//
// THE CALLER HAS ALREADY ESTABLISHED THE KEY IS THERE. A previous CERTIFICATE
// with no key beside it is the ordinary crashed-rotation leftover — public, and
// clearing it is exactly what an operator runs this for — so it never reaches
// here.
func refusePreviousPairThatIsNotOurs(dir, prevKeyPath, deployment string) error {
	prevCertPath := filepath.Join(dir, previousCAFile)

	prevCertPEM, err := readPublic(prevCertPath)

	switch {
	case errors.Is(err, os.ErrNotExist):
		// A KEY WITH NO CERTIFICATE. There is no previous authority here to drop
		// and the file that is left is the half worth keeping. Exported Retire
		// refuses this itself rather than leaving it to `billet ca retire`'s own
		// certificate pre-check, which is the alloc.New rule: a guard enforced
		// only at the CLI has a second way in that does not enforce it.
		return fmt.Errorf("wirecert: %s", leftoverKeyAdvice(prevKeyPath,
			filepath.Join(dir, "ca.key")))

	case err != nil:
		// AND "COULD NOT READ IT" IS ITS OWN ANSWER. Collapsing it into the
		// branch above tells an operator there is no certificate beside their key
		// when there is one billet could not open — after which moving only the
		// key aside leaves rotate still blocked by a file nobody mentioned.
		return fmt.Errorf(
			"wirecert: %s is present and billet cannot read %s beside it, so it cannot tell "+
				"whether that key is this deployment's previous authority. Retiring would "+
				"unlink it either way — look at both files before retiring: %w",
			prevKeyPath, prevCertPath, err)
	}

	prevKeyPEM, err := readSecret(prevKeyPath)
	if err != nil {
		return fmt.Errorf(
			"wirecert: billet cannot read %s, so it cannot tell whether that is the previous "+
				"authority's key or something else entirely, and retiring would unlink it "+
				"either way. Look at the file (a private key must be mode 0600 and a regular "+
				"file) before retiring: %w", prevKeyPath, err)
	}

	prevCert, err := parseCA(prevCertPEM, prevKeyPEM, deployment)
	if err != nil {
		return fmt.Errorf(
			"wirecert: %s and %s are not this deployment's previous authority, so retiring "+
				"would unlink a private key billet cannot account for. Move them aside "+
				"deliberately once you know what they are, and retire when the pair under those "+
				"names is the one this rotation replaced: %w",
			prevCertPath, prevKeyPath, err)
	}

	if err := refuseAnotherGeneration(dir, prevCert, prevCertPath); err != nil {
		return err
	}

	// AND THE WHOLE FILE, NOT ITS FIRST BLOCK. pem.Decode returns the remainder
	// and parsePair discards it, which is right for LOADING — a bundle may carry
	// more than one certificate — and wrong for a proof that ends in an unlink.
	// Append a second private key after a legitimate one and the pair still
	// parses, so the guard passes and the whole file goes, second key included.
	// A deletion may only be authorised by bytes that were all accounted for.
	for _, f := range []struct {
		path string
		body []byte
		kind string
	}{{prevCertPath, prevCertPEM, "CERTIFICATE"}, {prevKeyPath, prevKeyPEM, "EC PRIVATE KEY"}} {
		if !isOnePEMBlock(f.body, f.kind) {
			return fmt.Errorf(
				"wirecert: %s holds more than the one %s block this deployment's previous "+
					"authority accounts for, so retiring would unlink material billet has not "+
					"checked. Look at the file and move it aside deliberately", f.path, f.kind)
		}
	}

	return nil
}

// isOnePEMBlock reports whether body is exactly one PEM block of kind and
// nothing else at all.
//
// FOUR QUESTIONS, BECAUSE THE FIRST VERSION ASKED ONE. It compared only the
// REMAINDER pem.Decode hands back, and pem.Decode SKIPS FORWARD to the first
// BEGIN line — so bytes before the block, where a second private key fits
// perfectly well, were never looked at, and the unlink took them with the file.
// A PEM block also carries HEADERS of its own, which is another place to put
// material that this never examined. And the type matters: a file under a key's
// name holding a certificate is not something to unlink on the strength of a
// check that only counted blocks.
//
// NOT A CANONICAL RE-ENCODING COMPARISON, which was the other candidate and is
// stricter than the question: it would also refuse a file that differs only in
// line wrapping, and refusing a legitimate retire over formatting is the failure
// direction ADR-005 names — the next thing anybody does is delete the check.
func isOnePEMBlock(body []byte, kind string) bool {
	trimmed := bytes.TrimSpace(body)

	block, rest := pem.Decode(trimmed)
	if block == nil || block.Type != kind || len(block.Headers) != 0 {
		return false
	}

	if len(bytes.TrimSpace(rest)) != 0 {
		return false
	}

	// AND IT STARTS WHERE THE FILE DOES. This is the half pem.Decode will not
	// answer, because skipping to the first BEGIN line is exactly what it is
	// specified to do.
	return bytes.HasPrefix(trimmed, []byte("-----BEGIN "+kind+"-----"))
}

// refuseAnotherGeneration proves the previous pair is the generation the CURRENT
// authority replaced, and not merely an authority for the same deployment.
//
// THE SUBJECT PROVES THE DEPLOYMENT, NOT THE GENERATION, and that is the whole
// of the gap: a second, independently minted CA carrying this deployment id
// parses, is a CA, and its key is its key — so `billet ca retire` unlinked it
// exactly as it would the real predecessor. What closes it is that a rotation
// writes the replaced authority's fingerprint INTO the certificate it installs,
// so the current authority says which generation it took over from and the pair
// under ca-previous.* has to be that one.
//
// AN AUTHORITY THAT CLAIMS NOTHING IS NOT REFUSED, and that is deliberate rather
// than a gap left open: a deployment that rotated before this shipped has a
// current certificate with no claim in it, and refusing there would make an
// operator unable to finish a rotation that is already running, with no command
// that could fix it. Those fall back to the checks above — which is where every
// deployment was before the generation claim — and the next rotation records the
// claim.
func refuseAnotherGeneration(dir string, prevCert *CA, prevCertPath string) error {
	// A CURRENT AUTHORITY THIS CANNOT READ IS NOT THIS CHECK'S TO REPORT. The
	// caller asks whether the current pair is conclusively whole immediately
	// after and refuses with a message about exactly that; answering first would
	// replace it with one about a claim nobody was asking after.
	current, err := currentAuthorityCert(dir)
	if err != nil {
		return nil //nolint:nilerr // the caller's own refusal is the one that speaks here
	}

	replaced, claimed, err := replacedAuthority(current)
	if err != nil {
		return err
	}

	if !claimed {
		return nil
	}

	// EXACT, NOT SameFingerprint. That comparator case-folds and trims because it
	// exists for a value a human read off one console and typed into another —
	// and base64 IS case-sensitive, so it calls two different fingerprints equal.
	// Both sides here are machine-generated and neither has been transcribed, so
	// there is nothing to tolerate; a predicate that authorises unlinking a
	// private key should prove equality rather than accept a resemblance.
	if got := prevCert.Fingerprint(); got != replaced {
		return fmt.Errorf(
			"wirecert: the authority in force replaced %s, and the pair under %s is %s — a "+
				"different authority that happens to carry this deployment's name. Retiring "+
				"would unlink a private key that is not the one this rotation replaced, so move "+
				"those two files aside deliberately once you know what they are",
			replaced, prevCertPath, got)
	}

	return nil
}

// currentAuthorityCert parses ca.crt.
func currentAuthorityCert(dir string) (*x509.Certificate, error) {
	certPEM, err := readPublic(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("wirecert: %s is not PEM", filepath.Join(dir, "ca.crt"))
	}

	return x509.ParseCertificate(block.Bytes)
}

// keyVerdict is what can be said about a key file beside a certificate.
//
// THREE-VALUED, BECAUSE THE THIRD IS THE ONE THAT MATTERS. Collapsing
// "unreadable" into "absent" is how billet ends up deleting the only key that
// matches its certificate; it is the same rule inspectKey follows one credential
// over, and the same rule the ec2 credential paths follow.
type keyVerdict int

const (
	// keyIsElsewhere: the file is absent, or holds a key for something else.
	keyIsElsewhere keyVerdict = iota
	// keyBelongsHere: it is this certificate's key.
	keyBelongsHere
	// keyUnreadable: it is there and billet could not tell.
	keyUnreadable
)

// keyBelongsTo says whether the key at path is the key for certPEM.
func keyBelongsTo(path string, certPEM []byte) keyVerdict {
	keyPEM, err := readSecret(path)

	switch {
	case os.IsNotExist(err):
		return keyIsElsewhere
	case err != nil:
		return keyUnreadable
	}

	if _, _, err := parsePair(keyPEM, certPEM); err != nil {
		// READ AND STILL NOT THIS CERTIFICATE'S. Whether the bytes are another
		// authority's key or not a key at all, they cannot be what is holding
		// this certificate up, so removing the file loses nothing that a start
		// could have used. The case worth protecting is the one above: bytes
		// billet could not get at, which may be exactly that key.
		return keyIsElsewhere
	}

	return keyBelongsHere
}

// PreviousCA is the authority being retired, or nil when no rotation is running.
func PreviousCA(stateDir string) (*x509.Certificate, []byte, error) {
	pemBytes, err := readPublic(filepath.Join(CADir(stateDir), previousCAFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("wirecert: read the previous authority: %w", err)
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("wirecert: the previous authority is not a PEM certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("wirecert: parse the previous authority: %w", err)
	}

	return cert, pemBytes, nil
}

// Serving is everything a control plane needs from its authority, read once.
//
// ONE READ, BECAUSE FOUR WERE A RACE OF THEIR OWN. This used to be four separate
// walks of the ca directory during startup — LoadOrCreateCA, ServingCA,
// TrustBundle and RotationAge — and a `billet ca retire` landing between the
// second and the third produced a control plane that PRESENTS a certificate
// signed by the retired authority while trusting only the new one. It starts, it
// looks healthy, and not one node can verify it. That failure is worse than
// anything a half-published rotation can cause, because the others all refuse.
type Serving struct {
	// Issuing signs node certificates and renewals: the NEW authority during an
	// overlap, which is how nodes adopt it.
	Issuing *CA
	// Presents signs the certificate the control plane serves: the PREVIOUS
	// authority during an overlap, because a node that has not renewed trusts
	// only that one. The server follows the fleet rather than leading it.
	Presents *CA
	// Trust is every authority a node should accept, newest first — a
	// concatenated PEM, which is what x509.CertPool reads and what a node writes
	// to its ca.crt. During an overlap it holds two.
	Trust []byte
	// Rotating reports that an overlap was started, and RotationAge how long ago.
	Rotating    bool
	RotationAge time.Duration
}

// LoadServing reads a deployment's authority as one consistent picture, minting
// it on first use.
//
// THE READ ORDER IS PART OF THE ANSWER, and it is chosen against the order a
// rotation writes in so that every instant of a rotation is a state this returns
// something correct for. ca.crt then ca.key (LoadOrCreateCA), then
// ca-previous.crt then ca-previous.key:
//
//   - Rotate writes ca-previous.crt then ca-previous.key. Landing between them
//     finds a certificate and no key, so there is nothing to present the
//     previous authority with — and the current pair IS the old one at that
//     point, because the renames have not run. Presenting with it is correct.
//   - Retire removes ca-previous.key then ca-previous.crt. Landing between them
//     finds the same shape and presents with the current authority, which is the
//     new one, while still trusting the old certificate. Wider than needed, and
//     safe.
//   - Rotate renames ca.key then ca.crt. Landing between them is the one torn
//     read the ordering can produce and LoadOrCreateCA repairs it; see there.
//
// Both fallbacks go the same way: an absent previous KEY means present with the
// current authority, and a present previous CERTIFICATE means trust one more
// thing. Neither can serve an authority the fleet does not have.
func LoadServing(stateDir, deployment string) (Serving, error) {
	// AND THE SNAPSHOT IS CONFIRMED, because the ordering above bounds what a
	// reader can see INSIDE one rotation and says nothing about a whole rotation
	// and retirement passing between two of its reads. That interleaving —
	// current pair read as generation A, then A rotated to B and A retired, then
	// ca-previous.crt found absent — returns A as issuing, presenting AND
	// trusted: a control plane serving the authority an operator has just
	// retired, which is not fail-closed. So ca.crt is re-read at the end and a
	// changed one starts over. Retrying rather than refusing because a rotation
	// is an operator command: the loop cannot spin unless somebody is running
	// them back to back, and the bound is there so that "cannot" is structural.
	for range servingReadAttempts {
		out, settled, err := readServing(stateDir, deployment)
		if err != nil {
			return Serving{}, err
		}

		if settled {
			return out, nil
		}
	}

	return Serving{}, fmt.Errorf(
		"wirecert: %s changed underneath this read %d times running, so billet could not "+
			"capture one consistent authority. A rotation or a retirement is running right now; "+
			"wait for it to finish and start again",
		filepath.Join(CADir(stateDir), "ca.crt"), servingReadAttempts)
}

// onServingRead fires inside readServing, after the snapshot is built and
// before it is confirmed. Nil in production; see readServing.
var onServingRead func()

// servingReadAttempts bounds the confirm-and-retry above.
const servingReadAttempts = 3

// readServing takes one snapshot and reports whether it settled.
func readServing(stateDir, deployment string) (Serving, bool, error) {
	ca, err := LoadOrCreateCA(stateDir, deployment)
	if err != nil {
		return Serving{}, false, err
	}

	dir := CADir(stateDir)

	prevPEM, modTime, err := readPreviousCert(dir)
	if err != nil {
		return Serving{}, false, err
	}

	out := Serving{Issuing: ca, Presents: ca, Trust: ca.CertPEM()}

	prevKeyPresent := false

	if prevPEM != nil {
		out.Rotating = true
		out.RotationAge = time.Since(modTime)
		out.Trust = append(out.Trust, prevPEM...)

		keyPEM, keyErr := readSecret(filepath.Join(dir, previousCAKeyFile))
		prevKeyPresent = keyErr == nil

		switch {
		case os.IsNotExist(keyErr):
			// HALF-PUBLISHED OR HALF-RETIRED, and both mean the same thing here:
			// there is no previous authority to present with, so the current one
			// is what this control plane serves. Not an error — see the read
			// order above.
		case keyErr != nil:
			return Serving{}, false, fmt.Errorf(
				"wirecert: read the previous authority's key: %w", keyErr)
		default:
			presents, parseErr := parseCA(prevPEM, keyPEM, deployment)
			if parseErr != nil {
				return Serving{}, false, parseErr
			}

			out.Presents = presents
		}
	}

	// A TEST HOOK, nil in production. The window this confirming read closes is
	// between two syscalls, so it cannot be staged from outside the package and a
	// test that raced a goroutine against it would pass on every run that did not
	// interleave — which is most of them.
	if onServingRead != nil {
		onServingRead()
	}

	// THE CONFIRMING READ, OVER EVERY INPUT AN ANSWER HERE DEPENDS ON. An earlier
	// version re-read ca.crt alone and said that meant nothing had moved. It does
	// not: Presents, Trust and Rotating come from the PREVIOUS pair, and a
	// `billet ca retire` between this snapshot's two reads removes that pair
	// without touching ca.crt at all — leaving a control plane reporting an
	// overlap, and presenting an authority, that an operator has just retired.
	settled, err := stillMatches(dir, ca.CertPEM(), prevPEM, prevKeyPresent)
	if err != nil {
		return Serving{}, false, err
	}

	return out, settled, nil
}

// stillMatches reports whether the directory still holds what a snapshot was
// built from.
//
// THE PREVIOUS KEY BY PRESENCE RATHER THAN BY BYTES, deliberately: its content
// only ever changes together with the previous certificate, which is compared in
// full, and re-reading a private key to compare it is a second copy of a secret
// in memory for an answer already available.
func stillMatches(dir string, cert, prevCert []byte, prevKey bool) (bool, error) {
	nowCert, err := readPublic(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return false, fmt.Errorf("wirecert: re-read the current authority: %w", err)
	}

	nowPrevCert, _, err := readPreviousCert(dir)
	if err != nil {
		return false, err
	}

	nowPrevKey, err := exists(filepath.Join(dir, previousCAKeyFile))
	if err != nil {
		return false, err
	}

	return bytes.Equal(nowCert, cert) &&
		bytes.Equal(nowPrevCert, prevCert) &&
		nowPrevKey == prevKey, nil
}

// exists answers presence, and refuses to guess when it cannot tell.
func exists(path string) (bool, error) {
	switch _, err := os.Lstat(path); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("wirecert: check %s: %w", path, err)
	}
}

// readPreviousCert reads the certificate that says a rotation was started, and
// when it was.
func readPreviousCert(dir string) ([]byte, time.Time, error) {
	path := filepath.Join(dir, previousCAFile)

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, nil
	} else if err != nil {
		return nil, time.Time{}, fmt.Errorf("wirecert: read the previous authority: %w", err)
	}

	pemBytes, err := readPublic(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, nil
	} else if err != nil {
		return nil, time.Time{}, fmt.Errorf("wirecert: read the previous authority: %w", err)
	}

	return pemBytes, info.ModTime(), nil
}

// RotationAge is how long ago a rotation was started.
//
// THE CERTIFICATE'S FACT, NOT THE KEY'S, and that is the right one for an
// operator: `billet ca retire` asks whether there is an overlap to finish, and a
// rotation interrupted before its key was written still left one to clean up.
func RotationAge(stateDir string) (time.Duration, bool) {
	info, err := os.Stat(filepath.Join(CADir(stateDir), previousCAFile))
	if err != nil {
		return 0, false
	}

	return time.Since(info.ModTime()), true
}
