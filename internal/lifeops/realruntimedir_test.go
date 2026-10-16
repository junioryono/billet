package lifeops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
)

// realHostLockDir makes the host lock directory as billet-tmpfiles.conf does,
// root:root 0750 under a 0755 /run/billet, when the disposable host has none,
// and removes only what it made once the test is done. The shipped node and
// image-refresh units name it in ReadWritePaths without a '-', so a unit that
// carries their directory settings does not start without it.
func realHostLockDir(t *testing.T) {
	t.Helper()

	parent := filepath.Dir(deploy.NodeLockDir)
	var made []string

	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{{parent, 0o755}, {deploy.NodeLockDir, 0o750}} {
		switch _, err := os.Lstat(dir.path); {
		case err == nil:
			continue
		case !errors.Is(err, os.ErrNotExist):
			t.Fatal(err)
		}

		if err := os.Mkdir(dir.path, dir.mode); err != nil {
			t.Fatal(err)
		}

		made = append(made, dir.path)
	}

	t.Cleanup(func() {
		for i := len(made) - 1; i >= 0; i-- {
			if err := os.Remove(made[i]); err != nil {
				t.Errorf("remove the host lock directory this test made: %v", err)
			}
		}
	})
}

// TestRealSystemdHostLockDirectoryOutlivesTheNodeStop measures #175 against the
// units billet ships. A process holding a lock in /run/billet/locks keeps it
// across a stop and a start of the node, because the directory is the
// tmpfiles entry's and the unit only names it: the name still leads to the
// locked inode, and a second lock on it is refused. The counterfactual is the
// unit from before, which declared its lock directory a RuntimeDirectory: its
// stop removes the name under the holder, and a second lock on the recreated
// file is granted, which is how two verifications became one microVM. The
// image refresh writes there under the same naming, and a missing directory
// refuses the node's start with 226/NAMESPACE rather than starting a node that
// cannot lock.
func TestRealSystemdHostLockDirectoryOutlivesTheNodeStop(t *testing.T) {
	if os.Getenv("BILLET_TEST_SYSTEMD_OPERATION_EFFECTS") != "1" {
		t.Skip("requires BILLET_TEST_SYSTEMD_OPERATION_EFFECTS=1 in a disposable systemd host")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("the disposable systemd host must run Linux as root")
	}

	h := newRealOperationHost(t)
	realHostLockDir(t)

	for _, dir := range []string{"/etc/" + h.prefix, "/var/lib/" + h.prefix} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() {
		for _, dir := range []string{"/etc/" + h.prefix, "/var/lib/" + h.prefix, "/run/" + h.prefix} {
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
		}
	})

	// THE SHIPPED DIRECTORY SETTINGS, the workload replaced: the lock directory
	// keeps its name in both spellings, absolute and as a RuntimeDirectory
	// entry would write it, so a unit that declared it again would remove the
	// real one here; every other billet path moves under this test's prefix.
	lockEntry := strings.TrimPrefix(deploy.NodeLockDir, "/run/")
	adapt := func(unit, exec string, extra ...string) string {
		text := strings.NewReplacer(append(extra, deploy.NodeLockDir, deploy.NodeLockDir, lockEntry, lockEntry,
			"/var/lib/billet", "/var/lib/"+h.prefix, "/etc/billet", "/etc/"+h.prefix, "billet/", h.prefix+"/")...).Replace(unit)

		var lines []string
		for _, line := range strings.Split(text, "\n") {
			switch {
			case strings.HasPrefix(line, "ExecStart="):
				line = "ExecStart=" + exec
			case line == "Type=notify":
				line = "Type=exec"
			}
			lines = append(lines, line)
		}

		return strings.Join(lines, "\n")
	}

	node, refresh, legacy, missing := h.prefix+"-node.service", h.prefix+"-refresh.service",
		h.prefix+"-legacy.service", h.prefix+"-missing.service"
	legacyDir := "/run/" + h.prefix + "/locks"
	missingDir := "/run/" + h.prefix + "/missing"

	refreshMarker := filepath.Join(deploy.NodeLockDir, h.prefix+"-refresh")
	t.Cleanup(func() {
		if err := os.Remove(refreshMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})

	h.write(node, adapt(deploy.NodeUnit, "/bin/sleep infinity"))
	h.write(refresh, adapt(deploy.ImagesRefreshUnit, "/bin/sh -c 'touch "+refreshMarker+"'"))
	h.write(legacy, adapt(deploy.NodeUnit, "/bin/sleep infinity",
		"RuntimeDirectory=billet/registration", "RuntimeDirectory="+h.prefix+"/locks "+h.prefix+"/registration"))
	missingUnit := adapt(deploy.NodeUnit, "/bin/sleep infinity", "-/srv/jailer "+deploy.NodeLockDir, "-/srv/jailer "+missingDir)
	if !strings.Contains(missingUnit, "ReadWritePaths=-/etc/"+h.prefix+"/tls -/srv/jailer "+missingDir+"\n") {
		t.Fatalf("the missing-directory control did not move the shipped ReadWritePaths entry:\n%s", missingUnit)
	}
	h.write(missing, missingUnit)
	h.run("daemon-reload")

	held := lockIn(t, filepath.Join(deploy.NodeLockDir, h.prefix+"-held.lock"))

	h.run("start", "--", node)
	h.run("stop", "--", node)
	h.run("start", "--", node)
	h.run("stop", "--", node)

	if !held.stillNamed(t) {
		t.Fatalf("%s no longer leads to the locked inode after the node's stop and start", held.path)
	}

	if granted(t, held.path) {
		t.Fatalf("a second lock on %s was granted while the first is held", held.path)
	}

	h.run("start", "--", refresh)

	if got := h.property(refresh, "Result"); got != "success" {
		t.Fatalf("the image refresh's start under the shipped ReadWritePaths ended %q", got)
	}

	if _, err := os.Stat(refreshMarker); err != nil {
		t.Fatalf("the image refresh did not write the host lock directory: %v", err)
	}

	// THE COUNTERFACTUAL: the unit from before, its lock directory declared.
	h.run("start", "--", legacy)

	before := lockIn(t, filepath.Join(legacyDir, "held.lock"))

	h.run("stop", "--", legacy)
	h.run("start", "--", legacy)

	if before.stillNamed(t) {
		t.Fatal("the counterfactual's stop left its declared lock directory's name in place; the measurement is void")
	}

	if err := os.WriteFile(before.path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if !granted(t, before.path) {
		t.Fatal("the counterfactual's recreated lock was refused; the race #175 describes is not what this host does")
	}

	h.run("stop", "--", legacy)

	// AND A MISSING DIRECTORY REFUSES THE START, naming the path.
	if err := h.ctl(t.Context(), "start", "--", missing); err == nil {
		t.Fatal("a node unit whose lock directory is missing started")
	}

	if got := h.property(missing, "ExecMainStatus"); got != "226" {
		t.Fatalf("a node unit whose lock directory is missing started (ExecMainStatus=%q), want 226/NAMESPACE", got)
	}

	t.Logf("measured %s: the shipped units keep a held lock across the node's stop; the declared one does not",
		time.Now().UTC().Format(time.RFC3339))
}

// heldLock is a lock this process holds on a file, and the file it locked.
type heldLock struct {
	path string
	file *os.File
	info os.FileInfo
}

func lockIn(t *testing.T, path string) heldLock {
	t.Helper()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = file.Close(); _ = os.Remove(path) })

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock %s: %v", path, err)
	}

	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}

	return heldLock{path: path, file: file, info: info}
}

// stillNamed reports whether the path still names the inode this lock holds.
func (l heldLock) stillNamed(t *testing.T) bool {
	t.Helper()

	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}

	if err != nil {
		t.Fatal(err)
	}

	return os.SameFile(info, l.info)
}

// granted reports whether a second, independent lock on path is granted now.
func granted(t *testing.T, path string) bool {
	t.Helper()

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = file.Close() }()

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false
	default:
		t.Fatal(fmt.Errorf("lock %s a second time: %w", path, err))

		return false
	}
}
