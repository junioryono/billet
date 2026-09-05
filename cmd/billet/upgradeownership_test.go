package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// chownCall is one owner handed out by the transaction's copies.
type chownCall struct {
	path     string
	uid, gid int
}

// recordChowns stands a recorder in for the descriptor's Chown and makes the
// process look like the root updater, which is the only one that gives files
// away. NOT PARALLEL: the seams are package variables, and the umask test in this
// package has the same shape.
func recordChowns(t *testing.T) *[]chownCall {
	t.Helper()

	var calls []chownCall

	previous, previousUID := chownFd, geteuid
	chownFd = func(f *os.File, uid, gid int) error {
		calls = append(calls, chownCall{path: f.Name(), uid: uid, gid: gid})

		return nil
	}
	geteuid = func() int { return 0 }

	t.Cleanup(func() { chownFd, geteuid = previous, previousUID })

	return &calls
}

// AN UNPRIVILEGED UPDATER GIVES NOTHING AWAY. The launchd transaction runs as the
// operator; a binary a `sudo` once installed is root's and readable, and a copy of
// it asked to become root's again would be refused with EPERM and end every upgrade
// on that Mac. The operator's copy stays the operator's, which is what the launch
// agents expect.
func TestAnUnprivilegedUpdaterLeavesItsCopyAsItsOwn(t *testing.T) {
	calls := recordChowns(t)
	geteuid = func() int { return 501 }

	dir := t.TempDir()
	from := filepath.Join(dir, "billet")
	to := filepath.Join(dir, "installed", "billet")

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(from, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// THE SOURCE READS AS ROOT'S, the shape a sudo-installed binary has.
	if err := copyFileOwnedBy(from, to, syntheticOwner{uid: 0, gid: 0}); err != nil {
		t.Fatalf("copyFileOwnedBy as the operator: %v", err)
	}

	if len(*calls) != 0 {
		t.Fatalf("an unprivileged updater tried to give its copy away: %+v", *calls)
	}

	info, err := os.Stat(to)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the copy was not published 0755 (err %v, info %v)", err, info)
	}
}

// syntheticOwner is a FileInfo whose only truth is who owns it: an owner the test
// process is not, so a copy that took the owner from the wrong place (the
// snapshot, which the process owns) is told apart from one that took it from
// the identity directory.
type syntheticOwner struct {
	os.FileInfo
	uid, gid uint32
}

func (o syntheticOwner) Sys() any { return &syscall.Stat_t{Uid: o.uid, Gid: o.gid} }

const syntheticUID, syntheticGID = 4242, 4343

// identityOwnedBy makes the identity directory read as owned by the synthetic
// account for the rest of the test.
func identityOwnedBy(t *testing.T, uid, gid uint32) {
	t.Helper()

	previous := identityOwner
	identityOwner = func(dir string) (os.FileInfo, error) {
		info, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}

		return syntheticOwner{FileInfo: info, uid: uid, gid: gid}, nil
	}

	t.Cleanup(func() { identityOwner = previous })
}

func ownerOf(t *testing.T, path string) (int, int) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no Unix ownership on this platform")
	}

	return int(st.Uid), int(st.Gid)
}

// A COPY KEEPS ITS SOURCE'S OWNER, AS WELL AS ITS MODE. The updater is root, and
// the preserved /etc/billet/billet.yaml is root:<service group> 0640 so the
// unprivileged server can read it; a rollback that put it back owned by root and
// root alone left a control plane that could not read its own configuration.
// The recorder sees the owner the copy asked for, which is the source's.
func TestACopyGivesTheStagedFileItsSourcesOwner(t *testing.T) {
	calls := recordChowns(t)

	dir := t.TempDir()
	from := filepath.Join(dir, "billet.yaml")
	to := filepath.Join(dir, "restored", "billet.yaml")

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(from, []byte("server: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(from, to); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	uid, gid := ownerOf(t, from)

	if len(*calls) != 1 || (*calls)[0].uid != uid || (*calls)[0].gid != gid ||
		(*calls)[0].path != to+".billet-upgrade" {
		t.Fatalf("the copy handed out %+v; want the staging file given %d:%d", *calls, uid, gid)
	}
}

// THE RESTORED LEDGER BELONGS TO WHOEVER OWNS THE IDENTITY DIRECTORY, not to the
// root that took the snapshot. The control plane runs as the account that owns
// that directory and cannot open a ledger owned by root; a rollback that restored
// the snapshot as root's left the restored release unable to start.
func TestARestoredLedgerIsGivenToTheIdentityDirectorysOwner(t *testing.T) {
	calls := recordChowns(t)
	identityOwnedBy(t, syntheticUID, syntheticGID)

	identity := t.TempDir()
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")

	if err := os.WriteFile(snapshot, []byte("not really sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	host := &ledgerHost{cfg: &config.Config{Server: &config.ServerConfig{IdentityDir: identity}}}

	if err := host.RestoreLedger(t.Context(), snapshot); err != nil {
		t.Fatalf("RestoreLedger: %v", err)
	}

	// THE IDENTITY DIRECTORY'S OWNER, WHICH THE PROCESS IS NOT: a copy that kept the
	// snapshot's owner would hand out this process's own ids and be told apart.
	uid, gid := syntheticUID, syntheticGID
	ledger := filepath.Join(identity, "billet.db")

	// ON THE STAGING FILE, before the sync and the rename: an owner set on the
	// published name after the copy's sync is not durable, and a power cut after
	// the journal records the rollback would resurrect a ledger that is root's.
	var gaveStaging bool

	for _, c := range *calls {
		if c.path == ledger+".billet-upgrade" && c.uid == uid && c.gid == gid {
			gaveStaging = true
		}

		if c.path == ledger {
			t.Fatalf("the ledger's owner was set on the published name, after the sync: %+v", c)
		}
	}

	if !gaveStaging {
		t.Fatalf("the restored ledger was not given to the identity directory's owner %d:%d on "+
			"its staging file; the copies handed out %+v", uid, gid, *calls)
	}

	if _, err := os.Stat(ledger); err != nil {
		t.Fatalf("the ledger was not restored: %v", err)
	}
}
