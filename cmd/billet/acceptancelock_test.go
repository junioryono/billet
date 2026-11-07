package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func closeOnExec(t *testing.T, fd uintptr) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("F_GETFD: %v", errno)
	}

	return flags&syscall.FD_CLOEXEC != 0
}

// A LOCK HANDED DOWN STAYS WITH THE RUN: its descriptor is marked close-on-exec, so the
// services the run starts never inherit it; nothing named is nothing to keep, and a
// name that is no open descriptor is refused rather than ignored.
func TestARunKeepsTheLockItWasHandedToItself(t *testing.T) {
	t.Parallel()
	f, err := os.Create(filepath.Join(t.TempDir(), "run.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	fd := f.Fd()
	// As a supervisor's pass_fds leaves it: inheritable.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, 0); errno != 0 {
		t.Fatalf("F_SETFD: %v", errno)
	}
	if closeOnExec(t, fd) {
		t.Fatal("the fixture's descriptor is already close-on-exec; this test proves nothing")
	}
	env := map[string]string{acceptanceLockFD: strconv.Itoa(int(fd))}
	if err := keepAcceptanceLock(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("keep the lock: %v", err)
	}
	if !closeOnExec(t, fd) {
		t.Fatal("the handed-down lock would go on to every service the run starts")
	}

	if err := keepAcceptanceLock(func(string) string { return "" }); err != nil {
		t.Errorf("no lock named answered %v", err)
	}
	for _, bad := range []string{"two", "1", "987654"} {
		err := keepAcceptanceLock(func(string) string { return bad })
		if err == nil || !strings.Contains(err.Error(), acceptanceLockFD) {
			t.Errorf("%s=%s answered %v", acceptanceLockFD, bad, err)
		}
	}
}
