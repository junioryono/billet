package host

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// macState is a fake Mac's pmset and scutil, changed by what is run on it.
type macState struct {
	sleep string
	names map[string]string
	ran   []string
}

func (m *macState) run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	switch {
	case line == "pmset -g":
		return " sleep                " + m.sleep + "\n disksleep            0\n", nil
	case name == "scutil" && args[0] == "--get":
		return m.names[args[1]] + "\n", nil
	}

	m.ran = append(m.ran, line)

	switch {
	case line == "pmset -a sleep 0 disksleep 0":
		m.sleep = "0"
	case name == "scutil" && args[0] == "--set":
		m.names[args[1]] = args[2]
	}

	return "", nil
}

// THE PRIVILEGED HALF DOES ONLY WHAT DIFFERS, AND PROVES EACH STEP AFTERWARDS:
// a Mac already awake with two of its three names right gets one scutil and
// its missing directory, and nothing else.
//
// Mutations: a step that applies without reading first runs pmset; a step
// whose read-back is skipped passes a command that changed nothing.
func TestJoinPrerequisitesRootDoesOnlyWhatDiffers(t *testing.T) {
	onPlatform(t, "darwin", 0)

	me, err := user.Current()
	mustOK(t, err)

	mac := &macState{sleep: "0", names: map[string]string{
		"HostName": "mac-1", "LocalHostName": "mac-1", "ComputerName": "Somebody's Mac mini",
	}}
	missing := filepath.Join(t.TempDir(), "usr", "local", "var", "log", "billet")

	run, dirs := prereqRun, macServiceDirs
	t.Cleanup(func() { prereqRun, macServiceDirs = run, dirs })
	prereqRun, macServiceDirs = mac.run, []string{missing}

	if err := Join(t.Context(), processEnv(), []string{
		"prerequisites", "--account", me.Username, "--node-name", "mac-1",
	}); err != nil {
		t.Fatalf("join prerequisites: %v", err)
	}

	if want := []string{"scutil --set ComputerName mac-1"}; !slices.Equal(mac.ran, want) {
		t.Errorf("ran %v, want only %v", mac.ran, want)
	}

	if owned, err := ownedBy(missing, me.Username); err != nil || !owned {
		t.Errorf("%s was not created for %s: %v", missing, me.Username, err)
	}

	// A STEP THAT DOES NOT READ BACK AS DONE FAILS, rather than reporting done.
	mac.names["ComputerName"] = "stuck"
	prereqRun = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "scutil" && args[0] == "--set" {
			return "", nil
		}

		return mac.run(ctx, name, args...)
	}

	err = Join(t.Context(), processEnv(), []string{"prerequisites", "--account", me.Username, "--node-name", "mac-1"})
	if err == nil || !strings.Contains(err.Error(), "still does not read back as done") {
		t.Errorf("a step that changed nothing answered %v", err)
	}
}

// THE PRIVILEGED HALF REFUSES TO RUN WHERE IT DOES NOT BELONG.
func TestJoinPrerequisitesRootRefuses(t *testing.T) {
	for _, c := range []struct {
		name, goos string
		euid       int
		args       []string
		want       string
	}{
		{"not root", "darwin", 501, []string{"--account", "op", "--node-name", "mac-1"}, "needs root"},
		{"on Linux", "linux", 0, []string{"--account", "op", "--node-name", "mac-1"}, "prepares a Mac"},
		{"for root", "darwin", 0, []string{"--account", "root", "--node-name", "mac-1"}, "--account must name"},
		{"a bad name", "darwin", 0, []string{"--account", "op", "--node-name", "mac mini"}, "node-name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			onPlatform(t, c.goos, c.euid)

			err := Join(t.Context(), processEnv(), append([]string{"prerequisites"}, c.args...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("answered %v, want %q", err, c.want)
			}
		})
	}
}

// ROOT MAKES ONLY A HOMEBREW softnet SETUID-ROOT: a file elsewhere, by
// another name, or a symlink is refused before anything is changed.
func TestJoinGrantsOnlyACellarSoftnet(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	mustOK(t, err)

	cellars := softnetCellars
	t.Cleanup(func() { softnetCellars = cellars })
	softnetCellars = []string{filepath.Join(dir, "Cellar") + "/"}

	good := filepath.Join(dir, "Cellar", "softnet", "1", "bin", "softnet")
	mustOK(t, os.MkdirAll(filepath.Dir(good), 0o755))
	mustOK(t, os.WriteFile(good, nil, 0o755))

	elsewhere := filepath.Join(dir, "bin", "softnet")
	mustOK(t, os.MkdirAll(filepath.Dir(elsewhere), 0o755))
	mustOK(t, os.WriteFile(elsewhere, nil, 0o755))

	named := filepath.Join(dir, "Cellar", "softnet", "1", "bin", "sh")
	mustOK(t, os.WriteFile(named, nil, 0o755))

	link := filepath.Join(dir, "Cellar", "softnet", "2", "bin", "softnet")
	mustOK(t, os.MkdirAll(filepath.Dir(link), 0o755))
	mustOK(t, os.Symlink("/bin/sh", link))

	if _, err := checkedSoftnet(good); err != nil {
		t.Errorf("a Cellar softnet was refused: %v", err)
	}
	for _, p := range []string{elsewhere, named, link} {
		if _, err := checkedSoftnet(p); err == nil {
			t.Errorf("%s would be made setuid-root", p)
		}
	}
}

// THE sshd DROP-IN IS WRITTEN ONLY OVER A KEY, says exactly the reference
// hardware page's four lines, and is never the default.
func TestJoinHardensSSHOnlyWhenAskedAndOverAKey(t *testing.T) {
	dir := t.TempDir()

	keys := filepath.Join(dir, "authorized_keys")
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

	for _, s := range macRootSteps(macRootRequest{account: "op", nodeName: "mac-1"}) {
		if strings.Contains(s.what, "SSH") {
			t.Errorf("sshd is hardened without --harden-ssh: %s", s.what)
		}
	}
}

// LINUX IS VERIFIED, NEVER CHANGED: a missing account and an unreachable
// Docker daemon are both named, with the fix.
func TestJoinVerifiesALinuxNodeAndChangesNothing(t *testing.T) {
	onPlatform(t, "linux", 0)

	var ran []string

	run := prereqRun
	t.Cleanup(func() { prereqRun = run })
	prereqRun = func(_ context.Context, name string, args ...string) (string, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))

		return "Cannot connect to the Docker daemon", os.ErrNotExist
	}

	err := joinPrerequisites(t.Context(), cli.Env{Stdout: os.Stdout}, joinRequest{provider: "docker"})
	if err == nil || !strings.Contains(err.Error(), "Docker daemon") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("an unreachable daemon answered %v", err)
	}
	if !slices.Equal(ran, []string{"docker info --format {{.ServerVersion}}"}) {
		t.Errorf("ran %v, want only the docker read", ran)
	}
}
