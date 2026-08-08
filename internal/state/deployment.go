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
// installation" and keeps the failure to a lock conflict rather than a
// cross-destruction.
func DeploymentID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, deploymentIDFile)

	existing, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(existing))
		if id == "" {
			return "", fmt.Errorf("state: %s is empty; delete it to have billet mint a new identity", path)
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

	return id, nil
}
