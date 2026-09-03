// Package wireshare carries a deployment's node-wire authority between the
// controllers that share it.
//
// WHAT PROBLEM IT SOLVES. An active/passive pair serves one fleet, and a node
// verifies the control plane against the authority it was given. Promotion is
// therefore only half a failover: the promoted host has to present a certificate
// the fleet already trusts, and a host whose ca directory is empty mints a NEW
// authority instead — after which every node in the fleet drops off at once while
// the control plane looks perfectly healthy.
//
// IT IS REPLICATION, NOT A SHARED AUTHORITY, AND THE DIFFERENCE IS DELIBERATE.
// The file layout in internal/wirecert stays the source of truth on each host,
// with every guard it already has: the publication ORDER that makes each instant
// of a rotation a state a reader answers correctly, the torn-read repair, the
// retire guard that took three rounds. Porting that state machine onto a remote
// key/value store would carry the code and discard the reasoning — cross-key
// write ordering, per-file durable visibility, O_EXCL and a crash-releasing flock
// are all properties of a filesystem, and not one of them survives the move.
//
// SO THE STORE IS A CHANNEL. A controller PUBLISHES the authority it holds, and a
// host that holds NONE adopts it rather than minting one. Nothing here ever
// replaces a local authority: a host that already has one and disagrees is
// refused, naming both, because the file it would be writing over is the key
// every node in the fleet verifies against.
//
// WHAT THAT COSTS, said rather than implied: a rotation on one host reaches the
// other when somebody asks — `billet ca sync` — or when that host has nothing.
// Two controllers do not converge on their own, and the operator documentation
// says so in those words.
package wireshare

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/junioryono/billet/internal/wirecert"
)

// ErrNoAuthority means the store holds nothing for this deployment.
//
// AN ORDINARY STATE RATHER THAN A FAULT. A deployment whose first controller has
// not started yet has published nothing, and a host that finds nothing mints its
// own and publishes THAT — which is how the first one gets there.
var ErrNoAuthority = errors.New("wireshare: this deployment has published no authority")

// Store is the identity store, narrowed to the one value this package keeps in
// it.
//
// AN INTERFACE SO THE RULES ARE TESTABLE WITHOUT AWS, and so nothing here depends
// on which store a deployment chose. The AWS implementation is one adapter in
// cmd/billet; a fake is what the tests below use.
type Store interface {
	// GetAuthority returns the published document, or ErrNoAuthority.
	GetAuthority(ctx context.Context) ([]byte, error)
	// PutAuthority replaces the published document.
	PutAuthority(ctx context.Context, body []byte) error
}

// document is the wire form of an authority.
//
// THE ALLOWLIST IS THE SCHEMA. Files are keyed by wirecert.AuthorityFiles' own
// names, so a file added to that list travels without this package learning
// about it — and one that is not on the list cannot travel at all, which is the
// same closed-set rule the archive follows.
//
// BASE64 BECAUSE THE VALUES ARE PEM AND THE CONTAINER IS JSON. A PEM block is
// newline-separated text and JSON string escaping would survive it, but base64
// removes the question entirely: what comes back is byte-for-byte what went in,
// and byte-for-byte is what a checksum-free comparison needs.
type document struct {
	// Deployment is the identity this authority belongs to. It is checked on
	// adoption against the host's own, so a store shared by two deployments — or a
	// prefix pointed at the wrong one — cannot hand a host somebody else's CA.
	Deployment string `json:"deployment"`

	// Fingerprint is ca.crt's, so two hosts can be compared without either
	// decoding the other's key.
	Fingerprint string `json:"fingerprint"`

	// PublishedAt is a diagnostic. NOTHING DECIDES FROM IT: a clock is not a
	// generation, and an authority that looks newer is not one — the fingerprint
	// is what identifies a generation and the local files are what hold it.
	PublishedAt string `json:"published_at"`

	// Files maps an allowlisted name to base64 of its bytes.
	Files map[string]string `json:"files"`
}

// Publish writes the authority this host holds into the store.
//
// THE CALLER MUST HOLD wirecert.LockAuthority, because this reads the five files
// a rotation mutates in sequence and a reader without it can come away with a key
// from one generation beside a certificate from another — an authority that loads
// cleanly and verifies nothing, discovered on the day it is adopted.
//
// IT REPLACES, and that is the one place this package overwrites anything. What
// it is overwriting is a COPY: the authority itself lives in the file layout on
// each host, and the store's job is to hold whatever the deployment currently
// has. Refusing to replace would make a rotation unpublishable.
func Publish(ctx context.Context, store Store, stateDir, deployment string) error {
	authority, err := wirecert.ReadAuthority(stateDir)
	if err != nil {
		return err
	}

	cert, err := wirecert.ParseAuthorityPair(
		authority.Present["ca.key"], authority.Present["ca.crt"])
	if err != nil {
		return err
	}

	named, err := wirecert.AuthorityDeployment(cert)
	if err != nil {
		return err
	}

	if named != deployment {
		return fmt.Errorf(
			"%w: the authority in %s names deployment %s and this host is %s",
			wirecert.ErrForeignAuthority, stateDir, named, deployment)
	}

	doc := document{
		Deployment:  deployment,
		Fingerprint: wirecert.FingerprintOfCert(cert),
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Files:       make(map[string]string, len(authority.Present)),
	}

	for name, body := range authority.Present {
		doc.Files[name] = base64.StdEncoding.EncodeToString(body)
	}

	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("wireshare: encode the authority: %w", err)
	}

	return store.PutAuthority(ctx, body)
}

// Adopted says what Adopt did, so a caller can report it without inferring.
type Adopted int

const (
	// AdoptedNothing means the store held no authority. The caller mints its own
	// and publishes it.
	AdoptedNothing Adopted = iota
	// AdoptedInstalled means this host had none and now holds the deployment's.
	AdoptedInstalled
	// AdoptedAlreadyHeld means this host already held the same authority.
	AdoptedAlreadyHeld
)

// Adopt gives a host with no authority the one this deployment already uses.
//
// THE CALLER MUST HOLD wirecert.LockAuthority.
//
// THREE ANSWERS AND NOT TWO. "Nothing published" and "already held" are both
// success and mean opposite things to the caller: the first says this host is the
// one that has to publish, the second says there is nothing to do. Collapsing
// them would make a first controller publish nothing and a fleet have no
// authority in the store at all.
//
// A LOCAL AUTHORITY IS NEVER REPLACED. If this host holds one and it is not the
// published one, that is refused naming both fingerprints — because the file
// being written over would be the key every node in the fleet verifies against,
// and billet cannot tell a host that was left behind by a rotation from a host
// pointed at the wrong deployment. An operator can, and `billet ca sync --force`
// is where they say so.
func Adopt(
	ctx context.Context, store Store, stateDir, deployment string, replace bool,
) (Adopted, error) {
	body, err := store.GetAuthority(ctx)
	if err != nil {
		if errors.Is(err, ErrNoAuthority) {
			return AdoptedNothing, nil
		}

		return AdoptedNothing, err
	}

	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		return AdoptedNothing, fmt.Errorf(
			"wireshare: the published authority is not a document billet wrote: %w", err)
	}

	if doc.Deployment != deployment {
		return AdoptedNothing, fmt.Errorf(
			"%w: the published authority belongs to deployment %s and this host is %s; check "+
				"that the identity store's prefix names this deployment",
			wirecert.ErrForeignAuthority, doc.Deployment, deployment)
	}

	files, err := decodeFiles(doc)
	if err != nil {
		return AdoptedNothing, err
	}

	local, localErr := wirecert.ReadAuthority(stateDir)

	switch {
	case localErr == nil:
		return adoptOverExisting(local, doc, files, stateDir, deployment, replace)

	// A DIRECTORY WITH NO AUTHORITY AT ALL IS THE CASE THIS EXISTS FOR: a second
	// controller, provisioned and never started. Every other refusal ReadAuthority
	// can produce — half an authority, a pair that does not match, a missing
	// marker beside a complete pair — is a state an operator has to look at, and
	// installing over it would be doing exactly what this package refuses to do.
	case errors.Is(localErr, wirecert.ErrAuthorityLost):
		if err := wirecert.InstallAuthority(stateDir, deployment, files); err != nil {
			return AdoptedNothing, err
		}

		return AdoptedInstalled, nil

	default:
		return AdoptedNothing, fmt.Errorf(
			"wireshare: this host's own authority cannot be read, so billet will not install "+
				"another over it: %w", localErr)
	}
}

// adoptOverExisting decides what to do when this host already holds one.
func adoptOverExisting(
	local wirecert.Authority, doc document, files map[string][]byte,
	stateDir, deployment string, replace bool,
) (Adopted, error) {
	cert, err := wirecert.ParseAuthorityPair(local.Present["ca.key"], local.Present["ca.crt"])
	if err != nil {
		return AdoptedNothing, err
	}

	held := wirecert.FingerprintOfCert(cert)
	if wirecert.SameFingerprint(held, doc.Fingerprint) {
		return AdoptedAlreadyHeld, nil
	}

	if !replace {
		return AdoptedNothing, fmt.Errorf(
			"this host holds authority %s and the identity store publishes %s. billet will "+
				"not write over an authority: the file it would replace is what every node in "+
				"the fleet verifies this control plane against, and a host left behind by a "+
				"rotation looks exactly like a host pointed at the wrong deployment. If this "+
				"host is the one that is behind, move %s aside and run `billet ca sync` again",
			held, doc.Fingerprint, wirecert.CADir(stateDir))
	}

	// --force IS AN OPERATOR SAYING WHICH ONE IS RIGHT, and it still does not
	// overwrite: the existing directory is moved aside so the mistake is
	// recoverable, because the thing being set aside is a private key.
	moved, err := setAsideAuthority(stateDir)
	if err != nil {
		return AdoptedNothing, err
	}

	if err := wirecert.InstallAuthority(stateDir, deployment, files); err != nil {
		return AdoptedNothing, fmt.Errorf(
			"%w (the authority this host held is at %s)", err, moved)
	}

	return AdoptedInstalled, nil
}

// setAsideAuthority moves this host's ca directory and marker out of the way,
// returning where they went.
//
// MOVED RATHER THAN DELETED, and never at all without --force. What is being set
// aside is a private key: an operator who forced the wrong direction has to be
// able to put it back, and a deletion is the one outcome nothing recovers from.
//
// THE MARKER GOES WITH IT. It lives outside the ca directory precisely so its
// absence is detectable, so leaving it behind would make the freshly installed
// authority sit beside a witness for a different one.
func setAsideAuthority(stateDir string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dest := wirecert.CADir(stateDir) + ".superseded-" + stamp

	if err := os.Rename(wirecert.CADir(stateDir), dest); err != nil {
		return "", fmt.Errorf("wireshare: move %s aside: %w", wirecert.CADir(stateDir), err)
	}

	marker := wirecert.AuthorityPath(stateDir, "authority-created")
	if err := os.Rename(marker, dest+".authority-created"); err != nil && !os.IsNotExist(err) {
		return dest, fmt.Errorf("wireshare: move %s aside: %w", marker, err)
	}

	return dest, nil
}

// decodeFiles turns the document's base64 back into bytes, refusing anything the
// allowlist does not name.
//
// A CLOSED SET, the rule the archive states: a name this build does not know how
// to install must refuse the document rather than travel in it unread. A store
// written by a NEWER billet is the case that matters, and silently dropping half
// an authority there is how a host adopts something that does not hold together.
func decodeFiles(doc document) (map[string][]byte, error) {
	allowed := make(map[string]bool, len(wirecert.AuthorityFiles))
	for _, f := range wirecert.AuthorityFiles {
		allowed[f.Name] = true
	}

	out := make(map[string][]byte, len(doc.Files))

	for name, encoded := range doc.Files {
		if !allowed[name] {
			return nil, fmt.Errorf(
				"wireshare: the published authority carries %q, which this billet does not "+
					"know how to install; it was written by a newer version", name)
		}

		body, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("wireshare: decode %s from the published authority: %w",
				name, err)
		}

		out[name] = body
	}

	for _, f := range wirecert.AuthorityFiles {
		if f.Required && len(out[f.Name]) == 0 {
			return nil, fmt.Errorf(
				"wireshare: the published authority is missing %s, so it is not a whole one",
				f.Name)
		}
	}

	return out, nil
}
