package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE MARKER DECIDES ONLY ONE WAY ON ITS OWN: absent is a handoff, and present or
// unreadable is a drain, because draining is the stop billet always had and a
// handoff is the one that needs permission.
func TestTheStopIsFinalOnlyWithoutProofOfTheMarkersAbsence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	final := stopIsFinal(dir)
	if final() {
		t.Error("a host with no marker was told its stop is final")
	}

	if err := os.WriteFile(filepath.Join(dir, drainOnStopName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !final() {
		t.Error("a host whose removal marked its stop final was told to hand over")
	}

	// A state directory that is a file makes the check fail with something other
	// than "absent".
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !stopIsFinal(file)() {
		t.Error("a marker that could not be checked was read as absent")
	}
}

// A MARKER AN UNFINISHED REMOVAL LEFT IS CLEARED WHEN THE SERVER STARTS, so a
// reinstalled control plane hands over on its next restart.
func TestAStaleStopMarkerIsClearedAtStartup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, drainOnStopName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	clearStopMarker(dir)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the stale marker survived startup: %v", err)
	}

	// And an absent one is no error.
	clearStopMarker(dir)
}

// THE PACKAGE SCRIPT AND THE SERVER NAME THE SAME FILE IN THE SAME DIRECTORY, or a
// removal marks nothing the server reads and hands over to nobody.
func TestThePackageRemovalMarksTheFileTheServerReads(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "preremove.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)

	if !strings.Contains(script, `"${server_state}/`+drainOnStopName+`"`) {
		t.Errorf("preremove.sh does not write %s", drainOnStopName)
	}
	if !strings.Contains(script, "server_state=${BILLET_SERVER_STATE_DIR:-/var/lib/billet/server}") {
		t.Error("preremove.sh no longer marks the packaged server state directory")
	}
}
