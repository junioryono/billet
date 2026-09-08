package wirecert

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// AuthoritySnapshot is what a read-only observer can say about a deployment's
// authority from its files alone: the current CA certificate, the predecessor
// while a rotation is in progress, and whether the creation marker is present.
// It carries no key material.
type AuthoritySnapshot struct {
	Current     *x509.Certificate
	CurrentPEM  []byte
	Previous    *x509.Certificate
	PreviousPEM []byte
	Created     bool
}

// Rotating reports whether a predecessor authority is still present.
func (s AuthoritySnapshot) Rotating() bool { return s.Previous != nil }

// ErrAuthorityChanging means two reads of the authority files disagreed every
// time the snapshot tried, so nothing coherent can be said about them.
var ErrAuthorityChanging = errors.New("wirecert: the authority changed during the observation")

// snapshotAttempts bounds the confirming reads before the snapshot gives up.
const snapshotAttempts = 3

// snapshotBetweenReads runs between the two reads of one attempt. Tests use it
// to change the files under the observer; production leaves it nil.
var snapshotBetweenReads func()

// SnapshotAuthority reads the PUBLIC half of a deployment's authority without
// taking a lock and without creating anything, and answers only when two
// consecutive reads agree.
//
// LoadServing takes the same confirming-read approach because a rotation or a
// retirement can cross a single read; an observer that took LockAuthority
// instead would create the lock file and its directory on a controller that
// has neither, which is exactly what a read-only inspection must not do. The
// files are read as readPublic reads them (no symlink, a regular file, capped)
// and the key files are never opened.
func SnapshotAuthority(stateDir string) (AuthoritySnapshot, error) {
	for range snapshotAttempts {
		first, err := readPublicAuthority(stateDir)
		if err != nil {
			return AuthoritySnapshot{}, err
		}
		if snapshotBetweenReads != nil {
			snapshotBetweenReads()
		}
		second, err := readPublicAuthority(stateDir)
		if err != nil {
			return AuthoritySnapshot{}, err
		}
		if !first.equal(second) {
			continue
		}
		return first.parse(stateDir)
	}
	return AuthoritySnapshot{}, ErrAuthorityChanging
}

// publicAuthority is one read of the three public authority files; an absent
// file is a nil entry, which is a fact and not an error.
type publicAuthority struct {
	current, previous []byte
	marker            bool
}

func (p publicAuthority) equal(q publicAuthority) bool {
	// An absent file (nil) and an empty one are different observations, and
	// bytes.Equal alone would call them the same, confirming a read that saw
	// no predecessor with one that saw a malformed one.
	return (p.current == nil) == (q.current == nil) && bytes.Equal(p.current, q.current) &&
		(p.previous == nil) == (q.previous == nil) && bytes.Equal(p.previous, q.previous) &&
		p.marker == q.marker
}

func readPublicAuthority(stateDir string) (publicAuthority, error) {
	var out publicAuthority
	var err error
	if out.current, err = readPublicIfPresent(AuthorityPath(stateDir, "ca.crt")); err != nil {
		return publicAuthority{}, err
	}
	if out.previous, err = readPublicIfPresent(AuthorityPath(stateDir, "ca-previous.crt")); err != nil {
		return publicAuthority{}, err
	}
	marker, err := readPublicIfPresent(markerPath(stateDir))
	if err != nil {
		return publicAuthority{}, err
	}
	out.marker = marker != nil
	return out, nil
}

// readPublicIfPresent distinguishes an absent file (nil, nil) from one that
// could not be read, because the first is an answer and the second is not.
func readPublicIfPresent(path string) ([]byte, error) {
	body, err := readPublic(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("wirecert: read %s: %w", path, err)
	}
	if body == nil {
		body = []byte{}
	}
	return body, nil
}

func (p publicAuthority) parse(stateDir string) (AuthoritySnapshot, error) {
	if p.current == nil {
		return AuthoritySnapshot{}, fmt.Errorf("%w: %s holds no certificate authority",
			ErrAuthorityLost, CADir(stateDir))
	}
	current, err := parseCertPEM(p.current)
	if err != nil {
		return AuthoritySnapshot{}, fmt.Errorf("wirecert: %s: %w", AuthorityPath(stateDir, "ca.crt"), err)
	}
	out := AuthoritySnapshot{Current: current, CurrentPEM: p.current, Created: p.marker}
	if p.previous != nil {
		previous, err := parseCertPEM(p.previous)
		if err != nil {
			return AuthoritySnapshot{}, fmt.Errorf("wirecert: %s: %w",
				AuthorityPath(stateDir, "ca-previous.crt"), err)
		}
		out.Previous, out.PreviousPEM = previous, p.previous
	}
	return out, nil
}

// parseCertPEM parses EXACTLY one PEM certificate: a file holding a second
// block, a malformed block, or anything but whitespace around the one block,
// is not a certificate authority's public half and is refused rather than read
// for a block pem.Decode would recover to, because a caller that publishes the
// file's bytes would publish the rest too.
func parseCertPEM(body []byte) (*x509.Certificate, error) {
	// isOnePEMBlock is the one answer to "is this file exactly one block":
	// nothing skipped before it, nothing after it, no headers, no malformed
	// block pem.Decode would skip.
	if !isOnePEMBlock(body, "CERTIFICATE") {
		return nil, errors.New("not exactly one PEM certificate")
	}
	block, _, _ := decodeFirstPEM(skipBlankLines(body), "CERTIFICATE")
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the certificate: %w", err)
	}
	return cert, nil
}

// ParseCertificates parses every CERTIFICATE block in a PEM bundle, in order,
// and refuses a bundle with none, with a block that is not a certificate or
// does not decode, or with any bytes between, before or after the blocks that
// are not whitespace. pem.Decode skips such bytes, and a malformed block, to
// reach the next BEGIN, which is the trap decodeFirstPEM exists to refuse.
func ParseCertificates(body []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := skipBlankLines(body)
	for len(rest) != 0 {
		block, after, ok := decodeFirstPEM(rest, "CERTIFICATE")
		if !ok {
			return nil, errors.New("wirecert: bytes that are not one well-formed PEM certificate block in the bundle")
		}
		rest = skipBlankLines(after)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("wirecert: parse a certificate in the bundle: %w", err)
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, errors.New("wirecert: no certificate in the bundle")
	}
	return out, nil
}

// skipBlankLines drops whole lines holding only whitespace, and nothing else.
// Go's decoder, and so the TLS loader, recognises a BEGIN marker only at the
// start of the text or after a newline, so indentation before a BEGIN hides
// the block from the loader and must hide it from this parser too, or the
// report would publish a certificate the trust store does not hold; a blank
// line between blocks hides nothing.
func skipBlankLines(text []byte) []byte {
	for {
		nl := bytes.IndexByte(text, '\n')
		if nl < 0 {
			if len(bytes.TrimSpace(text)) == 0 {
				return nil
			}
			return text
		}
		if len(bytes.TrimSpace(text[:nl])) != 0 {
			return text
		}
		text = text[nl+1:]
	}
}

// decodeFirstPEM decodes the block at the very start of text, refusing what
// pem.Decode would forgive: text that does not begin with the BEGIN line of
// the kind asked for, a block whose END line is missing or is not a whole line
// (an END marker with more than whitespace after it on its line is a malformed
// boundary that Go's decoder and the TLS loader refuse, so two blocks glued
// together without a newline are not two blocks), a block that does not decode (pem.Decode skips
// a malformed block and recovers at the next BEGIN, which would read a later
// certificate as the first), a second BEGIN inside the span, or headers. rest
// is what follows the END line's terminator.
func decodeFirstPEM(text []byte, kind string) (*pem.Block, []byte, bool) {
	begin := []byte("-----BEGIN " + kind + "-----")
	end := []byte("-----END " + kind + "-----")
	if !bytes.HasPrefix(text, begin) {
		return nil, nil, false
	}
	endAt := bytes.Index(text, end)
	if endAt < 0 {
		return nil, nil, false
	}
	// Go's decoder lets the END line end in spaces or tabs; it is still the
	// whole line, so they are consumed before the terminator is required.
	after := bytes.TrimLeft(text[endAt+len(end):], " \t")
	switch {
	case len(after) == 0:
	case bytes.HasPrefix(after, []byte("\n")):
		after = after[1:]
	case bytes.HasPrefix(after, []byte("\r\n")):
		after = after[2:]
	default:
		return nil, nil, false
	}
	span := text[:endAt+len(end)]
	if bytes.Count(span, []byte("-----BEGIN ")) != 1 {
		return nil, nil, false
	}
	block, leftover := pem.Decode(span)
	if block == nil || block.Type != kind || len(block.Headers) != 0 || len(bytes.TrimSpace(leftover)) != 0 {
		return nil, nil, false
	}
	return block, after, true
}
