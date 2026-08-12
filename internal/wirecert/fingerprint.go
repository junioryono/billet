package wirecert

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// Fingerprint is a hash of a public key, in the shape a human compares.
//
// OF THE PUBLIC KEY, NOT OF THE CERTIFICATE. A certificate changes every time it
// is renewed or re-issued; the key underneath it does not have to. Fingerprinting
// the certificate would mean the value an operator wrote down stops matching the
// moment anything is re-issued, and a check that goes stale is a check people
// learn to skip.
//
// SHA256:base64 is OpenSSH's format, chosen because it is the one operators have
// already compared by eye a hundred times — and because a format nobody
// recognises invites pasting rather than reading.
func Fingerprint(spki []byte) string {
	sum := sha256.Sum256(spki)

	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// FingerprintOfCert is the fingerprint of a certificate's public key.
func FingerprintOfCert(cert *x509.Certificate) string {
	return Fingerprint(cert.RawSubjectPublicKeyInfo)
}

// FingerprintOfCSR is the fingerprint of the key a certificate request is for.
//
// THE SAME VALUE THE ISSUED CERTIFICATE WILL HAVE, which is the whole point: an
// operator approves a fingerprint they read off the node's console, and that
// fingerprint has to survive being signed or the approval means nothing.
func FingerprintOfCSR(csrPEM []byte) (string, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", errors.New("wirecert: not a PEM certificate request")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("wirecert: parse the certificate request: %w", err)
	}

	return Fingerprint(csr.RawSubjectPublicKeyInfo), nil
}

// FingerprintOfCAPEM is the fingerprint of an authority, from its PEM.
//
// What a node checks the control plane against before it will send anything: the
// operator reads this off the server with `billet ca show` and gives it to the
// node, so the first connection is verified against a value that travelled by a
// channel an attacker on the network does not control.
func FingerprintOfCAPEM(caPEM []byte) (string, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("wirecert: not a PEM certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("wirecert: parse the certificate: %w", err)
	}

	return FingerprintOfCert(cert), nil
}

// Fingerprint is this authority's own fingerprint.
func (c *CA) Fingerprint() string { return FingerprintOfCert(c.cert) }

// SameFingerprint compares two fingerprints, tolerating the ways a human
// transcribes one.
//
// CASE AND SURROUNDING SPACE ONLY. It deliberately does NOT tolerate a missing
// "SHA256:" prefix or a different separator: those are the shapes a DIFFERENT
// hash function's output takes, and quietly accepting them would mean comparing
// values that were never the same kind of thing.
func SameFingerprint(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
