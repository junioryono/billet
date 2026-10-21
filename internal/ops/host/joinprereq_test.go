package host

import (
	"context"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// fakeSudo stands in for sudo, recording each command and acting it out on the
// test's own files: mkdir makes the directory, chmod u+s sets the bit, tee
// writes its input, rm removes, and chown changes nothing because the test's
// account already owns what it made. extra answers the rest (pmset, scutil,
// sshd -t), and with no extra they succeed and change nothing.
func fakeSudo(t *testing.T, record func(string), extra func(args []string) error) func(context.Context, cli.Env, io.Reader, []string) error {
	t.Helper()

	return func(_ context.Context, _ cli.Env, input io.Reader, args []string) error {
		record(strings.Join(args, " "))

		switch args[0] {
		case "-v", sudoChown:
		case sudoMkdir:
			return os.MkdirAll(args[len(args)-1], 0o755)
		case sudoChmod:
			if args[1] != "u+s" {
				t.Fatalf("an unexpected chmod: %v", args)
			}
			info, err := os.Stat(args[2])
			if err != nil {
				return err
			}

			return os.Chmod(args[2], info.Mode().Perm()|os.ModeSetuid)
		case sudoTee:
			raw, err := io.ReadAll(input)
			if err != nil {
				return err
			}
			if teeWrites != nil {
				raw = teeWrites(raw)
			}

			return os.WriteFile(args[len(args)-1], raw, 0o644)
		case sudoRm:
			if err := os.Remove(args[len(args)-1]); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		default:
			if extra != nil {
				return extra(args)
			}
		}

		return nil
	}
}

// teeWrites, when set, changes what the fake tee writes, standing for a write
// that did not land as sent.
var teeWrites func([]byte) []byte

// macState is a fake Mac's pmset and scutil, changed by what sudo runs.
type macState struct {
	sleep   string
	names   map[string]string
	getErr  error
	setting bool
}

func (m *macState) run(_ context.Context, name string, args ...string) (string, error) {
	switch {
	case name == "pmset" && len(args) == 1 && args[0] == "-g":
		return " sleep                " + m.sleep + "\n disksleep            0\n", nil
	case name == "scutil" && args[0] == "--get":
		if m.getErr != nil {
			return "", m.getErr
		}
		if m.names[args[1]] == "" {
			return args[1] + ": not set\n", errors.New("exit status 1")
		}

		return m.names[args[1]] + "\n", nil
	}

	return "", errors.New("not run here: " + name)
}

func (m *macState) sudo(args []string) error {
	switch {
	case args[0] == sudoPmset:
		m.sleep = "0"
	case args[0] == sudoScutil && m.setting:
		m.names[args[2]] = args[3]
	}

	return nil
}

// stageMac pins a Mac whose prerequisites live under the test's own
// directory, the test's account standing for root there.
func stageMac(t *testing.T, mac *macState, dirs ...string) (*[]string, string) {
	t.Helper()

	onPlatform(t, "darwin", 501)

	top, err := filepath.EvalSymlinks(t.TempDir())
	mustOK(t, err)

	var calls []string

	run, sudo, svc, uid, trust := prereqRun, prereqSudo, macServiceDirs, prereqRootUID, prereqTrustRoot
	t.Cleanup(func() {
		prereqRun, prereqSudo, macServiceDirs, prereqRootUID, prereqTrustRoot = run, sudo, svc, uid, trust
	})
	prereqRun = mac.run
	prereqSudo = fakeSudo(t, func(c string) { calls = append(calls, c) }, mac.sudo)
	prereqRootUID, prereqTrustRoot = os.Getuid(), top

	macServiceDirs = nil
	for _, d := range dirs {
		macServiceDirs = append(macServiceDirs, filepath.Join(top, d))
	}

	return &calls, top
}

// THE PREREQUISITES DO ONLY WHAT DIFFERS, AS sudo OF THE SYSTEM BINARIES, AND
// PROVE EACH STEP AFTERWARDS (#181): a Mac already awake with two of its three
// names right gets one password, one scutil, and its missing directory made
// and handed over, and nothing else; billet itself never runs as root.
//
// Mutations: a step applied without reading first runs pmset; a step whose
// read-back is skipped passes a command that changed nothing; dropping the
// `sudo -v` asks for a password per step.
func TestJoinPrerequisitesDoOnlyWhatDiffers(t *testing.T) {
	me, err := user.Current()
	mustOK(t, err)

	mac := &macState{sleep: "0", setting: true, names: map[string]string{
		"HostName": "mac-1", "LocalHostName": "mac-1", "ComputerName": "Somebody's Mac mini",
	}}
	calls, top := stageMac(t, mac, filepath.Join("usr", "local", "var", "log", "billet"))
	dir := filepath.Join(top, "usr", "local", "var", "log", "billet")

	if err := joinPrerequisites(t.Context(), processEnv(), joinRequest{provider: "docker", nodeName: "mac-1"}); err != nil {
		t.Fatalf("join prerequisites: %v", err)
	}

	want := []string{"-v", "/usr/sbin/scutil --set ComputerName mac-1", "/bin/mkdir -p " + dir,
		"/usr/sbin/chown -h " + me.Username + " " + dir}
	if !slices.Equal(*calls, want) {
		t.Errorf("sudo ran\n  %v\nwant\n  %v", *calls, want)
	}

	// A STEP THAT DOES NOT READ BACK AS DONE FAILS, rather than reporting done.
	mac.names["ComputerName"], mac.setting = "stuck", false
	err = joinPrerequisites(t.Context(), processEnv(), joinRequest{provider: "docker", nodeName: "mac-1"})
	if err == nil || !strings.Contains(err.Error(), "still does not read back as done") {
		t.Errorf("a step that changed nothing answered %v", err)
	}
}

// NOTHING RUNS AS ROOT WHERE A CHECK REFUSES, not even the password: a
// directory under a symbolic link, under a directory others may write, or a
// name scutil could not read.
func TestJoinPrerequisitesRefuseBeforeThePassword(t *testing.T) {
	for name, c := range map[string]struct {
		stage func(top string)
		mac   *macState
		want  string
	}{
		"a symlinked ancestor": {func(top string) {
			mustOK(t, os.MkdirAll(filepath.Join(top, "elsewhere"), 0o755))
			mustOK(t, os.Symlink(filepath.Join(top, "elsewhere"), filepath.Join(top, "usr")))
		}, nil, "symbolic link"},
		"an ancestor others may write": {func(top string) {
			mustOK(t, os.MkdirAll(filepath.Join(top, "usr"), 0o755))
			mustOK(t, os.Chmod(filepath.Join(top, "usr"), 0o777))
		}, nil, "only root can change"},
		"a name scutil could not read": {func(string) {}, &macState{sleep: "0", names: map[string]string{},
			getErr: errors.New("signal: killed")}, "could not tell"},
	} {
		t.Run(name, func(t *testing.T) {
			mac := c.mac
			if mac == nil {
				mac = &macState{sleep: "0", names: map[string]string{
					"HostName": "mac-1", "LocalHostName": "mac-1", "ComputerName": "mac-1"}}
			}
			calls, top := stageMac(t, mac, filepath.Join("usr", "local", "var", "log", "billet"))
			c.stage(top)

			err := joinPrerequisites(t.Context(), processEnv(), joinRequest{provider: "docker", nodeName: "mac-1"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("answered %v, want a refusal saying %q", err, c.want)
			}
			if len(*calls) != 0 {
				t.Errorf("a refused run ran sudo %v", *calls)
			}
		})
	}
}

// A DRY RUN ON A FRESH MAC SAYS WHAT IT WOULD DO: tart is not installed yet, so
// there is no softnet to resolve, and that is a plan, not a failure.
func TestJoinDryRunOnAFreshMacPlansTheInstall(t *testing.T) {
	mac := &macState{sleep: "0", names: map[string]string{
		"HostName": "mac-1", "LocalHostName": "mac-1", "ComputerName": "mac-1"}}
	calls, _ := stageMac(t, mac)

	look := prereqLookPath
	t.Cleanup(func() { prereqLookPath = look })
	prereqLookPath = func(name string) (string, error) {
		if name == "brew" {
			return "/opt/homebrew/bin/brew", nil
		}

		return "", errors.New("not found")
	}

	var out strings.Builder
	env := processEnv()
	env.Stdout = &out
	if err := joinPrerequisites(t.Context(), env, joinRequest{provider: "tart", nodeName: "mac-1", dryRun: true}); err != nil {
		t.Fatalf("a dry run on a fresh Mac: %v", err)
	}
	for _, want := range []string{"brew install openai/tools/tart", "would let softnet confine"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry run does not say %q:\n%s", want, out.String())
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a dry run ran sudo %v", *calls)
	}
}

// ROOT MAKES ONLY A HOMEBREW softnet SETUID-ROOT: a file outside a Cellar, by
// another name, reached through a link, or with a second name is refused.
func TestJoinGrantsOnlyACellarSoftnet(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	mustOK(t, err)
	me, err := user.Current()
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
	mustOK(t, os.Symlink(good, link))

	mustOK(t, os.Symlink(filepath.Join(dir, "Cellar", "softnet", "1"), filepath.Join(dir, "Cellar", "softnet", "3")))
	throughLink := filepath.Join(dir, "Cellar", "softnet", "3", "bin", "softnet")

	twice := filepath.Join(dir, "Cellar", "softnet", "4", "bin", "softnet")
	mustOK(t, os.MkdirAll(filepath.Dir(twice), 0o755))
	mustOK(t, os.WriteFile(twice, nil, 0o755))
	mustOK(t, os.Link(twice, filepath.Join(dir, "second-name")))

	if err := grantableSoftnet(good, me.Username); err != nil {
		t.Errorf("a Cellar softnet was refused: %v", err)
	}
	for p, want := range map[string]string{
		elsewhere: "not a softnet inside a Homebrew Cellar", named: "not a softnet inside a Homebrew Cellar",
		link: "symbolic link", throughLink: "symbolic link", twice: "more than one name",
	} {
		if err := grantableSoftnet(p, me.Username); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s answered %v, want a refusal saying %q", p, err, want)
		}
	}
}

// LINUX IS VERIFIED, NEVER CHANGED: an unreachable Docker daemon is named,
// with the fix.
func TestJoinVerifiesALinuxNodeAndChangesNothing(t *testing.T) {
	onPlatform(t, "linux", 0)

	var ran []string

	run, lookup := prereqRun, prereqLookupUser
	t.Cleanup(func() { prereqRun, prereqLookupUser = run, lookup })
	prereqLookupUser = func(name string) (*user.User, error) { return &user.User{Username: name}, nil }
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
