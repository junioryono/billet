package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A DRAIN REQUEST APPLIES TO THE PROCESS IT NAMES, AND AN UNREADABLE ONE DRAINS
// (#374). A request left by an earlier stop names an earlier process and never
// turns a later handoff into a drain; one that cannot be read is answered the
// slow way rather than leaving guests on a host being taken apart.
func TestADrainRequestAppliesOnlyToTheProcessItNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet-node-drain")
	const pid = 4242

	if drainRequestedFor(path, pid) {
		t.Error("no request at all asked for a drain")
	}

	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !drainRequestedFor(path, pid) {
		t.Error("a request naming this process did not ask for a drain")
	}
	if drainRequestedFor(path, pid+1) {
		t.Error("a request naming another process asked this one to drain")
	}

	unreadable := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if !drainRequestedFor(unreadable, pid) {
		t.Error("a request that could not be read was taken as no request")
	}
}

// AND EVERY WRITER SPELLS THE PATH THE NODE READS. The host role's task file and
// the package's preremove write it; a different spelling would be a request no
// node ever sees.
func TestEveryDrainRequestWriterNamesTheNodesPath(t *testing.T) {
	t.Parallel()

	for _, file := range []string{
		"../../deploy/scripts/preremove.sh",
		"../../ansible_collections/junioryono/billet/roles/host/tasks/request-node-drain.yml",
	} {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(body), nodeDrainRequestPath) {
			t.Errorf("%s does not write %s", file, nodeDrainRequestPath)
		}
	}
}
