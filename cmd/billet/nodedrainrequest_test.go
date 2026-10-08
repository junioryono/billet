package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A DRAIN REQUEST HOLDS WHILE IT EXISTS, AND ONE THAT CANNOT BE READ DRAINS
// (#374). The request is held for the whole operation that must not leave guests
// behind, so a process systemd started in the middle of it drains as well; an
// answer that cannot be read is the slow one rather than one that strands guests.
func TestADrainRequestHoldsWhileItExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet-node-drain")

	if drainRequestedAt(path) {
		t.Error("no request at all asked for a drain")
	}
	if err := requestNodeDrain(path); err != nil {
		t.Fatalf("request: %v", err)
	}
	if !drainRequestedAt(path) {
		t.Error("a request that exists did not ask for a drain")
	}
	if err := releaseNodeDrain(path); err != nil {
		t.Fatalf("release: %v", err)
	}
	if drainRequestedAt(path) {
		t.Error("a withdrawn request still asked for a drain")
	}
	if err := releaseNodeDrain(path); err != nil {
		t.Errorf("withdrawing a request that is not there failed: %v", err)
	}

	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !drainRequestedAt(filepath.Join(file, "beneath")) {
		t.Error("a request that could not be read was taken as no request")
	}
}

// AND EVERY WRITER WRITES THE PATH THE NODE READS, BEFORE THE STOP IT IS FOR.
// The writes themselves are checked, not the path's appearance anywhere, because
// every one of these files also names it in a comment.
func TestEveryDrainRequestIsWrittenWhereTheNodeReadsItBeforeTheStop(t *testing.T) {
	t.Parallel()

	read := func(file string) string {
		t.Helper()

		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		return string(body)
	}
	before := func(file, body, first, then string) {
		t.Helper()

		at, later := strings.Index(body, first), strings.Index(body, then)
		if at < 0 || later < 0 || at > later {
			t.Errorf("%s does not do %q before %q", file, first, then)
		}
	}

	roles := "../../ansible_collections/junioryono/billet/roles/host/tasks/"
	if body := read(roles + "request-node-drain.yml"); !strings.Contains(body, "dest: "+nodeDrainRequestPath+"\n") {
		t.Errorf("request-node-drain.yml does not write %s", nodeDrainRequestPath)
	}
	if body := read(roles + "release-node-drain.yml"); !strings.Contains(body, "path: "+nodeDrainRequestPath+"\n") ||
		!strings.Contains(body, "state: absent") {
		t.Errorf("release-node-drain.yml does not remove %s", nodeDrainRequestPath)
	}

	preremove := read("../../deploy/scripts/preremove.sh")
	before("preremove.sh", preremove, ">"+nodeDrainRequestPath+"; then", `systemctl stop "${unit}"`)

	localdown := read("localdown.go")
	before("localdown.go", localdown, "requestNodeDrain(nodeDrainRequestFile)", "c.StopAndProve(")
	// ASKED OF EVERY LINUX NODE, never decided by the stop policy on disk: the
	// running process's policy is what decides, and it may have loaded another.
	if !strings.Contains(localdown, "if req.WantNode && cli.HostOS == \"linux\" {\n\t\tif err := "+
		"requestNodeDrain(nodeDrainRequestFile)") {
		t.Error("local down's drain request is guarded by something other than a Linux node being stopped")
	}
	if !strings.Contains(read("localup.go"), "releaseNodeDrain(nodeDrainRequestFile)") {
		t.Error("local up does not withdraw the drain request a down left")
	}
}
