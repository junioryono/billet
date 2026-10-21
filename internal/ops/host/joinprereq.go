package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// prereqCommandWait bounds each command a prerequisite runs.
const prereqCommandWait = 2 * time.Minute

// The prerequisites' seams: every command they run, the sudo that runs the
// privileged steps, and the paths and the owner a test points elsewhere.
// Nothing privileged runs in the suite; what is recorded there is the argv.
var (
	prereqRun = func(ctx context.Context, name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, prereqCommandWait)
		defer cancel()

		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()

		return string(out), err
	}
	// SUDO BY ITS ABSOLUTE PATH: a PATH lookup would take a wrapper from any
	// directory this account can write, /usr/local/bin among them. A command
	// given input reads it from that, with its output discarded.
	prereqSudo = func(ctx context.Context, env cli.Env, input io.Reader, args []string) error {
		cmd := exec.CommandContext(ctx, sudoBinary, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
		if input != nil {
			cmd.Stdin, cmd.Stdout = input, io.Discard
		}

		return cmd.Run()
	}
	prereqLookPath   = exec.LookPath
	prereqLookupUser = user.Lookup
	// prereqRootUID is the owner a directory above a root write must have, and
	// prereqTrustRoot where the walk that checks it starts: no account but root
	// can then replace a component under the command. Seams, because a test's
	// directories are its own, under a temporary directory others may write.
	prereqRootUID   = 0
	prereqTrustRoot = "/"
	// macServiceDirs are the directories a Mac's services need under the
	// root-owned /usr/local, given to the node account (docs/deploying/mac-tart.md).
	macServiceDirs = []string{
		"/usr/local/bin", "/usr/local/etc/billet", "/usr/local/var/log/billet",
		"/usr/local/var/lib/billet", "/usr/local/var/run/billet/locks",
	}
	// softnetCellars are where a Homebrew softnet's real file lives.
	softnetCellars = []string{"/opt/homebrew/Cellar/", "/usr/local/Cellar/"}
	sshdDropIn     = "/etc/ssh/sshd_config.d/000-headless.conf"
	linuxKVM       = "/dev/kvm"
)

// The system binaries the privileged steps run, by absolute path: macOS keeps
// them on the sealed system volume, so the command sudo runs is the one named.
const (
	sudoBinary = "/usr/bin/sudo"
	sudoPmset  = "/usr/bin/pmset"
	sudoScutil = "/usr/sbin/scutil"
	sudoMkdir  = "/bin/mkdir"
	sudoChown  = "/usr/sbin/chown"
	sudoChmod  = "/bin/chmod"
	sudoTee    = "/usr/bin/tee"
	sudoSshd   = "/usr/sbin/sshd"
	sudoRm     = "/bin/rm"
)

// macRootStep is one prerequisite that needs root: what it is, whether it is
// already so (read as this account), what must hold before root acts on it,
// and the commands that make it so.
type macRootStep struct {
	what  string
	done  func(ctx context.Context) (bool, error)
	check func() error
	apply func(ctx context.Context, env cli.Env) error
}

// macRootRequest is what the privileged steps arrange.
type macRootRequest struct {
	account, nodeName, softnet string
	hardenSSH                  bool
}

// joinPrerequisites performs (macOS) or verifies (Linux) what this machine
// needs before its node can run.
//
// BILLET NEVER RUNS AS ROOT ON A MAC. The steps that need root run as `sudo`
// of the system binaries themselves, after one `sudo -v` so the password is
// asked once, and every read before and after them runs as this account. The
// first design ran `sudo billet join prerequisites` from os.Executable, and
// the node account owns /usr/local/bin (the updater renames into it), so any
// process running as that account could replace the binary sudo was about to
// run as root (Codex, 2026-10-21).
func joinPrerequisites(ctx context.Context, env cli.Env, req joinRequest) error {
	if cli.HostOS == "linux" {
		return verifyLinuxPrerequisites(ctx, env, req)
	}

	account, err := user.Current()
	if err != nil {
		return fmt.Errorf("billet join: who is running this: %w", err)
	}

	root := macRootRequest{account: account.Username, nodeName: req.nodeName, hardenSSH: req.hardenSSH}

	if req.provider == string(config.ProviderTart) {
		installed, err := installTart(ctx, env, req.dryRun)
		if err != nil {
			return err
		}

		// THE GRANT ONLY FOR UNTRUSTED WORK: a trusted generation (a runner group)
		// runs no softnet, and setuid-root is not given to a binary nothing uses.
		switch {
		case req.runnerGroup != "":
		case !installed:
			fmt.Fprintln(env.Stdout, "join     would let softnet confine untrusted guests once it is installed")
		default:
			if root.softnet, err = resolveSoftnet(); err != nil {
				return err
			}
		}
	}

	var owed []macRootStep
	for _, s := range macRootSteps(root) {
		done, err := s.done(ctx)
		if err != nil {
			return fmt.Errorf("billet join: %s: could not tell whether it is done: %w", s.what, err)
		}
		if !done {
			owed = append(owed, s)
		}
	}

	if len(owed) == 0 {
		fmt.Fprintln(env.Stdout, "join     this Mac already has every prerequisite that needs root")

		return nil
	}

	for _, s := range owed {
		fmt.Fprintf(env.Stdout, "join     needs root: %s\n", s.what)
	}

	if req.dryRun {
		return nil
	}

	// EVERY PRE-CHECK BEFORE THE PASSWORD, so a refused step asks for nothing.
	for _, s := range owed {
		if s.check == nil {
			continue
		}
		if err := s.check(); err != nil {
			return fmt.Errorf("billet join: %s: %w; nothing was run as root", s.what, err)
		}
	}

	fmt.Fprintln(env.Stdout, "join     asking for sudo once for these (a host whose sudo keeps no timestamp asks for each)")
	if err := prereqSudo(ctx, env, nil, []string{"-v"}); err != nil {
		return fmt.Errorf("billet join: sudo: %w; nothing was run as root", err)
	}

	for _, s := range owed {
		if err := s.apply(ctx, env); err != nil {
			return fmt.Errorf("billet join: %s: %w (what was done before it is kept, and a re-run does the rest)",
				s.what, err)
		}

		// PROVED, NOT ASSUMED: a command that exits 0 and changes nothing is a
		// step not done.
		switch done, err := s.done(ctx); {
		case err != nil:
			return fmt.Errorf("billet join: %s: done, and whether it reads back as done could not be told: %w",
				s.what, err)
		case !done:
			return fmt.Errorf("billet join: %s: done, and it still does not read back as done", s.what)
		}

		fmt.Fprintf(env.Stdout, "done     %s\n", s.what)
	}

	return nil
}

// sudoRun runs one system binary as root, printing it first.
func sudoRun(ctx context.Context, env cli.Env, argv ...string) error {
	fmt.Fprintf(env.Stdout, "         sudo %s\n", cli.ShellArgs(argv))

	if err := prereqSudo(ctx, env, nil, argv); err != nil {
		return fmt.Errorf("sudo %s: %w", cli.ShellArgs(argv), err)
	}

	return nil
}

// sudoRunInput is sudoRun with input on the command's standard input.
func sudoRunInput(ctx context.Context, env cli.Env, input string, argv ...string) error {
	fmt.Fprintf(env.Stdout, "         sudo %s\n", cli.ShellArgs(argv))

	if err := prereqSudo(ctx, env, strings.NewReader(input), argv); err != nil {
		return fmt.Errorf("sudo %s: %w", cli.ShellArgs(argv), err)
	}

	return nil
}

// installTart installs tart and softnet with Homebrew when either is missing,
// as this account: Homebrew refuses to run as root. It reports whether both
// are installed now, which a dry run leaves false when they were not.
func installTart(ctx context.Context, env cli.Env, dryRun bool) (bool, error) {
	_, tartErr := prereqLookPath("tart")
	_, softnetErr := prereqLookPath("softnet")
	if tartErr == nil && softnetErr == nil {
		return true, nil
	}

	brew, err := prereqLookPath("brew")
	if err != nil {
		return false, errors.New("billet join: tart is not installed, and neither is Homebrew, which installs it: " +
			"install Homebrew (https://brew.sh), then run this again")
	}

	args := []string{"install", "openai/tools/tart", "openai/tools/softnet"}
	fmt.Fprintf(env.Stdout, "join     %s %s\n", brew, strings.Join(args, " "))

	if dryRun {
		return false, nil
	}

	if out, err := prereqRun(ctx, brew, args...); err != nil {
		return false, fmt.Errorf("billet join: brew install: %w\n%s", err, out)
	}

	return true, nil
}

// resolveSoftnet is the softnet file a grant would land on: the Cellar target
// behind Homebrew's symlink, and only one beside the tart binary, for the
// reason `billet check` gives (a writable directory early on PATH must not
// turn this into making a stranger's binary setuid-root).
func resolveSoftnet() (string, error) {
	tart, err := prereqLookPath("tart")
	if err != nil {
		return "", fmt.Errorf("billet join: tart is not on PATH after installing it: %w", err)
	}

	softnet, err := prereqLookPath("softnet")
	if err != nil {
		return "", fmt.Errorf("billet join: softnet is not on PATH after installing it: %w", err)
	}

	if filepath.Dir(softnet) != filepath.Dir(tart) {
		return "", fmt.Errorf("billet join: the softnet on PATH (%s) is not beside tart (%s), so billet will "+
			"not make it setuid-root; put tart's own first on PATH, or grant it yourself as `billet check` "+
			"prints", softnet, tart)
	}

	return filepath.EvalSymlinks(softnet)
}

// macRootSteps are the privileged prerequisites, in order.
func macRootSteps(r macRootRequest) []macRootStep {
	steps := []macRootStep{{
		what: "keep the Mac awake (pmset -a sleep 0 disksleep 0)",
		done: pmsetAwake,
		apply: func(ctx context.Context, env cli.Env) error {
			return sudoRun(ctx, env, sudoPmset, "-a", "sleep", "0", "disksleep", "0")
		},
	}}

	for _, key := range []string{"HostName", "LocalHostName", "ComputerName"} {
		steps = append(steps, macRootStep{
			what: fmt.Sprintf("name the Mac %s (scutil --set %s)", r.nodeName, key),
			done: func(ctx context.Context) (bool, error) { return scutilNamed(ctx, key, r.nodeName) },
			apply: func(ctx context.Context, env cli.Env) error {
				return sudoRun(ctx, env, sudoScutil, "--set", key, r.nodeName)
			},
		})
	}

	for _, dir := range macServiceDirs {
		steps = append(steps, macRootStep{
			what:  fmt.Sprintf("give %s to %s", dir, r.account),
			done:  func(context.Context) (bool, error) { return ownedBy(dir, r.account) },
			check: func() error { return rootOwnedAbove(dir) },
			apply: func(ctx context.Context, env cli.Env) error {
				if err := sudoRun(ctx, env, sudoMkdir, "-p", dir); err != nil {
					return err
				}

				return sudoRun(ctx, env, sudoChown, "-h", r.account, dir)
			},
		})
	}

	if r.softnet != "" {
		steps = append(steps, macRootStep{
			what:  fmt.Sprintf("let softnet confine untrusted guests (chown root, then chmod u+s %s)", r.softnet),
			done:  func(context.Context) (bool, error) { return softnetGranted(r.softnet) },
			check: func() error { return grantableSoftnet(r.softnet, r.account) },
			// BOTH, IN THIS ORDER: chown clears the setuid bit (measured on macOS).
			apply: func(ctx context.Context, env cli.Env) error {
				if err := sudoRun(ctx, env, sudoChown, "-h", "root", r.softnet); err != nil {
					return err
				}

				return sudoRun(ctx, env, sudoChmod, "u+s", r.softnet)
			},
		})
	}

	if r.hardenSSH {
		steps = append(steps, macRootStep{
			what:  fmt.Sprintf("allow only key logins, as %s, over SSH (%s)", r.account, sshdDropIn),
			done:  func(context.Context) (bool, error) { return sshdHardened(r.account) },
			check: func() error { return sshdHardenable(r.account) },
			apply: func(ctx context.Context, env cli.Env) error { return hardenSSHD(ctx, env, r.account) },
		})
	}

	return steps
}

// pmsetAwake reads whether system and disk sleep are both off.
func pmsetAwake(ctx context.Context) (bool, error) {
	out, err := prereqRun(ctx, "pmset", "-g")
	if err != nil {
		return false, fmt.Errorf("pmset -g: %w", err)
	}

	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if f := strings.Fields(scanner.Text()); len(f) >= 2 {
			values[f[0]] = f[1]
		}
	}

	return values["sleep"] == "0" && values["disksleep"] == "0", nil
}

// scutilNamed reads whether a system name is already want. An unset name is
// no; any other failure is could-not-tell, never no.
func scutilNamed(ctx context.Context, key, want string) (bool, error) {
	out, err := prereqRun(ctx, "scutil", "--get", key)
	switch {
	case err == nil:
		return strings.TrimSpace(out) == want, nil
	case strings.Contains(out, "not set"):
		return false, nil
	default:
		return false, fmt.Errorf("scutil --get %s: %w %s", key, err, strings.TrimSpace(out))
	}
}

// ownedBy reports whether dir is a directory account owns. Absent is not
// owned; anything else unreadable is an error, never a no.
func ownedBy(dir, account string) (bool, error) {
	uid, err := accountUID(account)
	if err != nil {
		return false, err
	}

	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !info.IsDir():
		return false, fmt.Errorf("%s is a %s, not a directory", dir, info.Mode().Type())
	}

	return fileOwner(info) == uid, nil
}

// rootOwnedAbove holds what root is about to create and hand over to a tree no
// other account can change under it: every existing component above dir is a
// real directory root owns that no group or other may write, and dir itself,
// if it exists, is a directory and not a link. Then nothing can turn a
// component into a link between this check and the commands.
func rootOwnedAbove(dir string) error {
	dir = filepath.Clean(dir)
	top := filepath.Clean(prereqTrustRoot)

	rel, err := filepath.Rel(top, dir)
	if !filepath.IsAbs(dir) || err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not an absolute path under %s", dir, top)
	}

	parts := strings.Split(rel, string(filepath.Separator))
	at := top

	for i, part := range parts {
		at = filepath.Join(at, part)

		info, err := os.Lstat(at)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}

		last := i == len(parts)-1
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link, and root will not create or hand over through one", at)
		case !info.IsDir():
			return fmt.Errorf("%s is not a directory", at)
		case !last && (fileOwner(info) != prereqRootUID || info.Mode().Perm()&0o022 != 0):
			return fmt.Errorf("%s is not a directory only root can change (owner uid %d, mode %04o), so what "+
				"root would create under it could be redirected", at, fileOwner(info), info.Mode().Perm())
		}
	}

	return nil
}

// softnetGranted reports whether the softnet file is setuid and root's.
func softnetGranted(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}

	return info.Mode()&fs.ModeSetuid != 0 && fileOwner(info) == prereqRootUID, nil
}

// grantableSoftnet refuses a softnet that root should not make setuid-root: it
// must be a regular file named softnet inside a Homebrew Cellar, reached by no
// symbolic link, with one name only (a hard link could be another program),
// and owned by this account or root.
//
// WHAT IS LEFT IS THE MANUAL GRANT'S OWN RACE: the Cellar is this account's, so
// a process running as it could swap the file between this check and the two
// commands, exactly as it could between `billet check`'s printed commands and
// an operator typing them. Closing that needs root code holding the file open,
// which is what this command no longer runs.
func grantableSoftnet(path, account string) error {
	inCellar := false
	for _, cellar := range softnetCellars {
		inCellar = inCellar || strings.HasPrefix(filepath.Clean(path), cellar)
	}

	if !filepath.IsAbs(path) || filepath.Base(path) != "softnet" || !inCellar {
		return fmt.Errorf("%s is not a softnet inside a Homebrew Cellar, so it is not made setuid-root", path)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	if resolved != filepath.Clean(path) {
		return fmt.Errorf("%s is reached through a symbolic link (it is %s), so it is not made setuid-root",
			path, resolved)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	uid, err := accountUID(account)
	if err != nil {
		return err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file, so it is not made setuid-root", path)
	case !ok || st.Nlink != 1:
		return fmt.Errorf("%s has more than one name, so it may be another program, and it is not made setuid-root", path)
	case fileOwner(info) != uid && fileOwner(info) != prereqRootUID:
		return fmt.Errorf("%s belongs to uid %d, neither %s nor root, so it is not made setuid-root",
			path, fileOwner(info), account)
	}

	return nil
}

// sshdDropInBody is the key-only drop-in, from
// docs/reference/reference-hardware.md#headless-operation.
func sshdDropInBody(account string) string {
	return "PermitRootLogin no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nAllowUsers " +
		account + "\n"
}

// sshdHardened reports whether the drop-in says exactly what it should.
func sshdHardened(account string) (bool, error) {
	info, err := os.Lstat(sshdDropIn)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", sshdDropIn)
	}

	raw, err := os.ReadFile(sshdDropIn)
	if err != nil {
		return false, err
	}

	return string(raw) == sshdDropInBody(account), nil
}

// sshdHardenable refuses the drop-in where it would take something away:
// over a drop-in already there (whatever it says, it is someone's policy, and
// a refused one would be removed rather than restored), and for an account
// with no key in its authorized_keys, for which key-only SSH is no SSH at all
// while AllowUsers shuts every other account out.
func sshdHardenable(account string) error {
	switch _, err := os.Lstat(sshdDropIn); {
	case err == nil:
		return fmt.Errorf("%s is already there; merge the four lines from the reference hardware page "+
			"into it yourself", sshdDropIn)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("whether %s is already there could not be told: %w", sshdDropIn, err)
	}

	u, err := prereqLookupUser(account)
	if err != nil {
		return err
	}

	if !holdsAuthorizedKey(filepath.Join(u.HomeDir, ".ssh", "authorized_keys")) {
		return fmt.Errorf("%s has no key in ~/.ssh/authorized_keys, so a key-only sshd would leave it no way "+
			"in; add one, prove a key login, and run this again", account)
	}

	return nil
}

// hardenSSHD writes the drop-in as root, reads it back, has sshd check it, and
// removes it again when either is not what it should be: it was not there
// before (sshdHardenable), so removing it is the exact state it found.
//
// THE POLICY TRAVELS ON root's STANDARD INPUT, never through a file this
// account can write: a file staged in the account's temporary directory and
// copied by root could be swapped by a process running as the account between
// its write and root's read, installing policy of that process's choosing, or
// copying a file only root may read into a world-readable one (Codex,
// 2026-10-21). The destination's directory is root's, so nothing else can put
// a link there.
func hardenSSHD(ctx context.Context, env cli.Env, account string) error {
	undo := func(cause error) error {
		if rmErr := sudoRun(ctx, env, sudoRm, "-f", sshdDropIn); rmErr != nil {
			return fmt.Errorf("%w, and removing the drop-in again failed: %v: remove %s by hand before "+
				"sshd restarts", cause, rmErr, sshdDropIn)
		}

		return fmt.Errorf("%w, so the drop-in was removed again", cause)
	}

	if err := sudoRunInput(ctx, env, sshdDropInBody(account), sudoTee, sshdDropIn); err != nil {
		return undo(err)
	}

	switch done, err := sshdHardened(account); {
	case err != nil:
		return undo(fmt.Errorf("the drop-in could not be read back: %w", err))
	case !done:
		return undo(errors.New("the drop-in does not read back as what was written"))
	}

	if err := sudoRun(ctx, env, sudoSshd, "-t"); err != nil {
		return undo(fmt.Errorf("sshd refused the drop-in: %w", err))
	}

	return nil
}

// holdsAuthorizedKey reports whether an authorized_keys file names a key.
func holdsAuthorizedKey(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	for line := range strings.SplitSeq(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return true
		}
	}

	return false
}

// fileOwner is a file's owning uid, or -1 when the platform does not say.
func fileOwner(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}

	return -1
}

// accountUID is an account's uid.
func accountUID(account string) (int, error) {
	u, err := prereqLookupUser(account)
	if err != nil {
		return 0, err
	}

	return strconv.Atoi(u.Uid)
}

// verifyLinuxPrerequisites refuses a Linux machine the node cannot run on,
// naming the fix. It changes nothing: the package makes the account and the
// units, and a Firecracker host is prepared by the host role.
func verifyLinuxPrerequisites(ctx context.Context, env cli.Env, req joinRequest) error {
	var missing []string

	if _, err := prereqLookupUser(initconfig.ServiceGroup); err != nil {
		missing = append(missing, fmt.Sprintf("the %s account, which the package's postinstall creates: "+
			"install the billet .deb or .rpm", initconfig.ServiceGroup))
	}

	switch config.ProviderKind(req.provider) {
	case config.ProviderDocker:
		if out, err := prereqRun(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
			missing = append(missing, fmt.Sprintf("a Docker daemon this host can reach (`docker info`: %v %s): "+
				"install and start Docker", err, strings.TrimSpace(out)))
		}
	case config.ProviderFirecracker:
		if _, err := os.Stat(linuxKVM); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%v): a Firecracker host needs hardware virtualization", linuxKVM, err))
		}
		for _, bin := range []string{"firecracker", "jailer"} {
			if _, err := prereqLookPath(bin); err != nil {
				missing = append(missing, bin+" on PATH")
			}
		}
	}

	if len(missing) > 0 {
		var b strings.Builder
		for _, m := range missing {
			b.WriteString("\n  - " + m)
		}

		remedy := ""
		if req.provider == string(config.ProviderFirecracker) {
			remedy = "\nA Firecracker host is prepared by the collection's host role " +
				"(docs/deploying/linux-firecracker-host.md), not by billet join."
		}

		return fmt.Errorf("billet join: this machine is missing what its node needs, and nothing was changed:%s%s",
			b.String(), remedy)
	}

	fmt.Fprintln(env.Stdout, "join     this machine has what its node needs")

	return nil
}
