package host

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stageSSH points the drop-in and the account's home at the test's own
// directories, with sudo stood in for.
func stageSSH(t *testing.T, sshd func() error) (*[]string, string, string) {
	t.Helper()

	home := t.TempDir()
	dropIn := filepath.Join(t.TempDir(), "000-headless.conf")

	var calls []string

	lookup, sudo, path := prereqLookupUser, prereqSudo, sshdDropIn
	t.Cleanup(func() { prereqLookupUser, prereqSudo, sshdDropIn = lookup, sudo, path })
	prereqLookupUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: "501", Gid: "20", HomeDir: home}, nil
	}
	prereqSudo = fakeSudo(t, func(c string) { calls = append(calls, c) }, func(args []string) error {
		if args[0] == sudoSshd {
			return sshd()
		}

		return nil
	})
	sshdDropIn = dropIn

	return &calls, home, dropIn
}

// THE sshd STEP EXISTS ONLY WITH --harden-ssh; IT REFUSES OVER A DROP-IN
// ALREADY THERE (whatever it says, it is someone's policy) AND FOR AN ACCOUNT
// WITH NO KEY; it writes the policy as root from standard input, never through
// a file this account could swap, and when sshd refuses the result, or it does
// not read back as what was sent, it is removed again, which is exactly the
// state it found.
func TestJoinHardensSSHDOnlyOverAKeyAndUndoesARefusedDropIn(t *testing.T) {
	sshdErr := errors.New("exit status 255")
	calls, home, dropIn := stageSSH(t, func() error { return sshdErr })

	for _, hard := range []bool{false, true} {
		asked := slices.ContainsFunc(macRootSteps(macRootRequest{account: "op", nodeName: "mac-1", hardenSSH: hard}),
			func(s macRootStep) bool { return strings.Contains(s.what, "SSH") })
		if asked != hard {
			t.Errorf("with --harden-ssh %v the sshd step is planned: %v", hard, asked)
		}
	}

	if err := sshdHardenable("op"); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("hardening with no authorized key answered %v", err)
	}

	mustOK(t, os.MkdirAll(filepath.Join(home, ".ssh"), 0o700))
	mustOK(t, os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte("# a note\nssh-ed25519 AAAA op\n"), 0o600))
	mustOK(t, sshdHardenable("op"))

	err := hardenSSHD(t.Context(), processEnv(), "op")
	if err == nil || !strings.Contains(err.Error(), "removed again") {
		t.Errorf("a drop-in sshd refused answered %v", err)
	}
	if _, err := os.Stat(dropIn); !os.IsNotExist(err) {
		t.Errorf("the drop-in sshd refused is still there (%v)", err)
	}
	if want := []string{"/usr/bin/tee " + dropIn, "/bin/chmod 0644 " + dropIn, "/usr/sbin/sshd -t", "/bin/rm -f " + dropIn}; !slices.Equal(*calls, want) {
		t.Errorf("sudo ran %v, want %v", *calls, want)
	}

	// A WRITE THAT DID NOT LAND AS SENT is removed before sshd is asked.
	*calls = nil
	sshdErr = nil
	teeWrites = func([]byte) []byte { return []byte("PermitRootLogin yes\n") }
	t.Cleanup(func() { teeWrites = nil })
	err = hardenSSHD(t.Context(), processEnv(), "op")
	if err == nil || !strings.Contains(err.Error(), "does not read back") || !strings.Contains(err.Error(), "removed again") {
		t.Errorf("a drop-in that did not read back answered %v", err)
	}
	if want := []string{"/usr/bin/tee " + dropIn, "/bin/chmod 0644 " + dropIn, "/bin/rm -f " + dropIn}; !slices.Equal(*calls, want) {
		t.Errorf("sudo ran %v, want %v", *calls, want)
	}
	teeWrites = nil

	sshdErr = nil
	mustOK(t, hardenSSHD(t.Context(), processEnv(), "op"))
	if done, err := sshdHardened("op"); err != nil || !done {
		t.Errorf("the drop-in sshd accepted does not read back as done (%v)", err)
	}
	if info, err := os.Stat(dropIn); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("the drop-in is %v (%v), want 0644 whatever the umask left", info.Mode().Perm(), err)
	}

	// AND NOW IT IS THERE, so another run refuses rather than replacing it.
	if err := sshdHardenable("op"); err == nil || !strings.Contains(err.Error(), "already there") {
		t.Errorf("hardening over a drop-in already there answered %v", err)
	}
}

// THE KEY CHECK READS ONLY KEYS: a file of comments and blank lines is none.
func TestJoinCountsOnlyAKeyAsAWayIn(t *testing.T) {
	keys := filepath.Join(t.TempDir(), "authorized_keys")

	mustOK(t, os.WriteFile(keys, []byte("# no keys yet\n\n"), 0o600))
	if holdsAuthorizedKey(keys) {
		t.Error("a file of comments counted as a key")
	}
	mustOK(t, os.WriteFile(keys, []byte("ssh-ed25519 AAAA op@laptop\n"), 0o600))
	if !holdsAuthorizedKey(keys) {
		t.Error("a key was not counted")
	}

	want := "PermitRootLogin no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nAllowUsers op\n"
	if got := sshdDropInBody("op"); got != want {
		t.Errorf("the drop-in says %q, want %q", got, want)
	}
}

// LINUX NAMES THE MISSING ACCOUNT, whatever host the test runs on.
func TestJoinNamesAMissingBilletAccount(t *testing.T) {
	onPlatform(t, "linux", 0)

	lookup, run := prereqLookupUser, prereqRun
	t.Cleanup(func() { prereqLookupUser, prereqRun = lookup, run })
	prereqLookupUser = func(string) (*user.User, error) { return nil, user.UnknownUserError("billet") }
	prereqRun = func(context.Context, string, ...string) (string, error) { return "29.0.0", nil }

	err := joinPrerequisites(t.Context(), processEnv(), joinRequest{provider: "docker"})
	if err == nil || !strings.Contains(err.Error(), "the billet account") {
		t.Errorf("a host with no billet account answered %v", err)
	}

	prereqLookupUser = func(name string) (*user.User, error) { return &user.User{Username: name}, nil }
	if err := joinPrerequisites(t.Context(), processEnv(), joinRequest{provider: "docker"}); err != nil {
		t.Errorf("a prepared docker host was refused: %v", err)
	}
}
