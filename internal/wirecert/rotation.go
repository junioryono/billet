package wirecert

import (
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
	dir := CADir(stateDir)

	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	prevPath := filepath.Join(dir, previousCAFile)

	if _, err := os.Stat(prevPath); err == nil {
		return nil, fmt.Errorf(
			"wirecert: a rotation is already under way — %s exists. Finish it with `billet ca "+
				"retire` once every node has renewed, then rotate again", prevPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("wirecert: check %s: %w", prevPath, err)
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

	fresh, err := createCA(stateDir, dir, newCert, newKey, deployment)
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
	if err := writePublic(prevPath, current); err != nil {
		return nil, err
	}

	if err := writeSecret(filepath.Join(dir, "ca-previous.key"), currentKey); err != nil {
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

// Retire drops the authority a rotation replaced.
//
// AFTER THE FLEET HAS MOVED, which only an operator can judge: a node that has
// not renewed still trusts only the old authority, and retiring it makes that
// node unable to verify the control plane. `billet ca show` reports how many
// nodes are still on the old one.
func Retire(stateDir string) error {
	dir := CADir(stateDir)

	for _, name := range []string{previousCAFile, "ca-previous.key"} {
		path := filepath.Join(dir, name)

		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("wirecert: remove %s: %w", path, err)
		}
	}

	return nil
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

// TrustBundle is every authority a node should accept, newest first.
//
// A CONCATENATED PEM, because that is what x509.CertPool reads and what a node
// writes to its ca.crt. During an overlap it holds two, which is what lets a
// node verify the control plane whether or not it has renewed yet.
func TrustBundle(stateDir string, current *CA) ([]byte, error) {
	bundle := append([]byte(nil), current.CertPEM()...)

	_, prevPEM, err := PreviousCA(stateDir)
	if err != nil {
		return nil, err
	}

	if prevPEM != nil {
		bundle = append(bundle, prevPEM...)
	}

	return bundle, nil
}

// ServingCA is the authority whose key signs what the control plane PRESENTS.
//
// The PREVIOUS one while a rotation is running, because a node that has not
// renewed trusts only that. Nodes reach the new authority through renewal, which
// carries the trust bundle; the server follows them rather than leading.
func ServingCA(stateDir, deployment string, current *CA) (*CA, error) {
	dir := CADir(stateDir)

	certPEM, err := readPublic(filepath.Join(dir, previousCAFile))
	if errors.Is(err, os.ErrNotExist) {
		return current, nil
	} else if err != nil {
		return nil, fmt.Errorf("wirecert: read the previous authority: %w", err)
	}

	keyPEM, err := readSecret(filepath.Join(dir, "ca-previous.key"))
	if err != nil {
		return nil, fmt.Errorf("wirecert: read the previous authority's key: %w", err)
	}

	return parseCA(certPEM, keyPEM, deployment)
}

// RotationAge is how long the current overlap has been running.
func RotationAge(stateDir string) (time.Duration, bool) {
	info, err := os.Stat(filepath.Join(CADir(stateDir), previousCAFile))
	if err != nil {
		return 0, false
	}

	return time.Since(info.ModTime()), true
}
