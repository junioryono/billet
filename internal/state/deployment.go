package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// deploymentIDFile holds the identity of one billet installation.
//
// A file rather than a row in the database, because the identity has to be
// readable by things that have not opened the database and must survive the
// database being rebuilt. Its lifetime is the state directory's.
const deploymentIDFile = "deployment-id"

// DeploymentID returns the stable identity of the billet installation rooted at
// this state directory, creating it on first use.
//
// THIS IS WHAT MAKES DESTRUCTIVE RECONCILIATION SAFE, and it is the reason the
// node name could not be used for it. The node name defaults to the hostname, so
// two billet installations on one machine — a production one and someone trying
// the quickstart, or two worktrees — carry the same name while keeping separate
// state directories. The process lock does not catch that: it guards a
// directory, and their directories differ. Labelling compute by node name would
// then let one installation enumerate the other's containers, find their lease
// ids absent from its own database, and destroy live jobs it has no relationship
// with.
//
// Renaming a node is the reason it cannot be derived from configuration either:
// a derived label would change under the operator's feet and orphan every
// running container by making it invisible to the thing that owns it.
//
// Random rather than derived from the path, so that copying a state directory
// does not silently produce two installations claiming one identity — the copy
// carries the original's id, which is the honest answer to "these are the same
// installation". That is what makes the copy DETECTABLE: LockDeployment keys a
// host-wide lock on the id, so running the copy alongside the original fails as a
// lock conflict rather than as a cross-destruction. A derived id would give the
// copy a different identity and no conflict to detect.
func DeploymentID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, deploymentIDFile)

	existing, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(existing))
		if id == "" {
			return "", fmt.Errorf(
				"state: %s is empty. RESTORE THE ORIGINAL IDENTITY if you can — from a backup, "+
					"or from the sh.billet.owner label on any container this installation "+
					"started. Deleting the file mints a NEW identity, and every container "+
					"labelled with the old one becomes invisible to billet: its leases expire, "+
					"its capacity is resold, and it runs forever. Only reset it once you have "+
					"confirmed no compute is left under the old identity", path)
		}

		if err := validDeploymentID(id); err != nil {
			return "", fmt.Errorf(
				"state: %s: %w. RESTORE THE ORIGINAL IDENTITY if you can — from a backup, or "+
					"from the sh.billet.owner label on any container this installation "+
					"started. Deleting the file mints a NEW identity, and every container "+
					"labelled with the old one becomes invisible to billet: its leases expire, "+
					"its capacity is resold, and it runs forever. Only reset it once you have "+
					"confirmed no compute is left under the old identity", path, err)
		}

		return id, nil
	}

	if !os.IsNotExist(err) {
		return "", fmt.Errorf("state: read deployment id: %w", err)
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("state: create state dir %s: %w", stateDir, err)
	}

	var raw [16]byte

	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("state: mint a deployment id: %w", err)
	}

	id := hex.EncodeToString(raw[:])

	// O_EXCL, so two processes racing to initialise the same directory cannot
	// each mint an id and have the loser's compute labelled with a value nothing
	// will look for afterwards. The loser re-reads the winner's.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return DeploymentID(stateDir)
		}

		return "", fmt.Errorf("state: write deployment id: %w", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(id + "\n"); err != nil {
		return "", fmt.Errorf("state: write deployment id: %w", err)
	}

	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("state: sync deployment id: %w", err)
	}

	// THE DIRECTORY IS SYNCED TOO, and that is not belt-and-braces. Syncing a new
	// file persists its CONTENTS; the directory entry that makes it findable is a
	// separate write, and losing power between the two leaves a state directory
	// with no identity in it. Billet would mint a fresh one on restart, and every
	// container labelled with the old id becomes invisible — leases reaped,
	// capacity resold, containers running forever.
	dir, err := os.Open(stateDir)
	if err != nil {
		return "", fmt.Errorf("state: open state dir to sync it: %w", err)
	}

	defer func() { _ = dir.Close() }()

	if err := dir.Sync(); err != nil {
		// THE FILE GOES WITH THE FAILURE. Leaving it behind means the next call
		// takes the read path, finds an id whose directory entry was never made
		// durable, and returns it as though the guarantee held — turning a
		// one-time startup error into a silent loss of the property this sync
		// exists to provide.
		if rmErr := os.Remove(path); rmErr != nil {
			return "", fmt.Errorf("state: sync state dir (%w), and the half-written "+
				"identity could not be removed (%v); delete %s by hand", err, rmErr, path)
		}

		return "", fmt.Errorf("state: sync state dir: %w", err)
	}

	return id, nil
}

// deploymentIDLen is the length of the hex encoding of the 16 random bytes an
// identity is minted from.
const deploymentIDLen = 32

// validDeploymentID refuses anything billet would not have minted.
//
// THE IDENTITY IS INTERPOLATED INTO PLACES THAT PARSE, which is what makes this
// worth checking rather than trusting. It becomes a filename in the host-wide
// lock directory, where a `/` or a `..` leaves that directory entirely — and
// silently, since the resulting lock failure is indistinguishable from a host
// that has nowhere to put one, so the protection degrades off while reporting a
// cache-directory problem. It is also written as a docker label and sent back as
// `--filter label=…`, where a comma or an `=` changes what is being asked.
//
// Billet mints this value itself, so anything failing this check is a hand-edit
// or a corrupted file. Refusing beats sanitising: a sanitised id is a DIFFERENT
// identity from the one already written onto running containers, so billet would
// come up unable to see its own compute while believing it could.
func validDeploymentID(id string) error {
	if len(id) != deploymentIDLen {
		return fmt.Errorf("deployment identity %q is %d characters, not %d", id, len(id), deploymentIDLen)
	}

	// Not hex.DecodeString: it accepts uppercase, and an identity that differs
	// only in case is two identities on a case-sensitive filesystem and one on a
	// case-insensitive one. Pinning to the encoding billet emits keeps the lock
	// meaning the same thing on both.
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("deployment identity %q contains %q, but identities are lowercase hex", id, r)
		}
	}

	return nil
}
