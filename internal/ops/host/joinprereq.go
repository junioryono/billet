package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
// privileged half, and the paths a test points elsewhere. Nothing privileged
// runs in the suite; what is recorded there is the argv.
var (
	prereqRun = func(ctx context.Context, name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, prereqCommandWait)
		defer cancel()

		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()

		return string(out), err
	}
	prereqSudo = func(ctx context.Context, env cli.Env, args []string) error {
		cmd := exec.CommandContext(ctx, "sudo", args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr

		return cmd.Run()
	}
	prereqLookPath   = exec.LookPath
	prereqExecutable = os.Executable
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

// macRootStep is one thing the privileged half does: what it is, whether it
// is already so, and how to make it so.
type macRootStep struct {
	what  string
	done  func(ctx context.Context) (bool, error)
	apply func(ctx context.Context) error
}

// macRootRequest is what the privileged half is asked to arrange.
type macRootRequest struct {
	account, nodeName, softnet string
	hardenSSH                  bool
}

// joinPrerequisites performs (macOS) or verifies (Linux) what this machine
// needs before its node can run.
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
		if err := installTart(ctx, env, req.dryRun); err != nil {
			return err
		}

		// THE GRANT ONLY FOR UNTRUSTED WORK: a trusted generation (a runner group)
		// runs no softnet, and setuid-root is not given to a binary nothing uses.
		if req.runnerGroup == "" {
			if root.softnet, err = resolveSoftnet(); err != nil {
				return err
			}
		}
	}

	steps := macRootSteps(root)

	var owed []string
	for _, s := range steps {
		done, err := s.done(ctx)
		if err != nil {
			return fmt.Errorf("billet join: %s: %w", s.what, err)
		}
		if !done {
			owed = append(owed, s.what)
		}
	}

	if len(owed) == 0 {
		fmt.Fprintln(env.Stdout, "join     this Mac already has every prerequisite that needs root")

		return nil
	}

	for _, what := range owed {
		fmt.Fprintf(env.Stdout, "join     needs root: %s\n", what)
	}

	if req.dryRun {
		return nil
	}

	self, err := prereqExecutable()
	if err != nil {
		return fmt.Errorf("billet join: find this billet to run its privileged half: %w", err)
	}

	args := []string{self, "join", "prerequisites", "--account", root.account, "--node-name", root.nodeName}
	if root.softnet != "" {
		args = append(args, "--softnet", root.softnet)
	}
	if root.hardenSSH {
		args = append(args, "--harden-ssh")
	}

	fmt.Fprintf(env.Stdout, "join     sudo %s\n", cli.ShellArgs(args))

	if err := prereqSudo(ctx, env, args); err != nil {
		return fmt.Errorf("billet join: the privileged prerequisites did not finish (%w); what they did "+
			"is kept, and a re-run does the rest", err)
	}

	return nil
}

// installTart installs tart and softnet with Homebrew when either is missing,
// as this account: Homebrew refuses to run as root.
func installTart(ctx context.Context, env cli.Env, dryRun bool) error {
	_, tartErr := prereqLookPath("tart")
	_, softnetErr := prereqLookPath("softnet")
	if tartErr == nil && softnetErr == nil {
		return nil
	}

	brew, err := prereqLookPath("brew")
	if err != nil {
		return errors.New("billet join: tart is not installed, and neither is Homebrew, which installs it: " +
			"install Homebrew (https://brew.sh), then run this again")
	}

	args := []string{"install", "openai/tools/tart", "openai/tools/softnet"}
	fmt.Fprintf(env.Stdout, "join     %s %s\n", brew, strings.Join(args, " "))

	if dryRun {
		return nil
	}

	if out, err := prereqRun(ctx, brew, args...); err != nil {
		return fmt.Errorf("billet join: brew install: %w\n%s", err, out)
	}

	return nil
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
		what:  "keep the Mac awake (pmset -a sleep 0 disksleep 0)",
		done:  pmsetAwake,
		apply: func(ctx context.Context) error { return runStep(ctx, "pmset", "-a", "sleep", "0", "disksleep", "0") },
	}}

	for _, key := range []string{"HostName", "LocalHostName", "ComputerName"} {
		steps = append(steps, macRootStep{
			what: fmt.Sprintf("name the Mac %s (scutil --set %s)", r.nodeName, key),
			done: func(ctx context.Context) (bool, error) {
				out, err := prereqRun(ctx, "scutil", "--get", key)

				return err == nil && strings.TrimSpace(out) == r.nodeName, nil
			},
			apply: func(ctx context.Context) error { return runStep(ctx, "scutil", "--set", key, r.nodeName) },
		})
	}

	for _, dir := range macServiceDirs {
		steps = append(steps, macRootStep{
			what:  fmt.Sprintf("give %s to %s", dir, r.account),
			done:  func(context.Context) (bool, error) { return ownedBy(dir, r.account) },
			apply: func(context.Context) error { return giveDir(dir, r.account) },
		})
	}

	if r.softnet != "" {
		steps = append(steps, macRootStep{
			what:  fmt.Sprintf("let softnet confine untrusted guests (chown root, then chmod u+s %s)", r.softnet),
			done:  func(context.Context) (bool, error) { return softnetGranted(r.softnet) },
			apply: func(context.Context) error { return grantSoftnet(r.softnet) },
		})
	}

	if r.hardenSSH {
		steps = append(steps, macRootStep{
			what:  fmt.Sprintf("allow only key logins, as %s, over SSH (%s)", r.account, sshdDropIn),
			done:  func(context.Context) (bool, error) { return sshdHardened(r.account) },
			apply: func(ctx context.Context) error { return hardenSSHD(ctx, r.account) },
		})
	}

	return steps
}

// joinPrerequisitesRoot is `billet join prerequisites`, the privileged half
// `billet join` runs under sudo on a Mac. Each step is read first, applied
// only where it differs, and read again afterwards.
func joinPrerequisitesRoot(ctx context.Context, env cli.Env, args []string) error {
	var r macRootRequest

	fs := cli.NewFlagSet("billet join prerequisites", env.Stdout)
	fs.StringVar(&r.account, "account", "", "the account that will run the node (required)")
	fs.StringVar(&r.nodeName, "node-name", "", "this Mac's node name (required)")
	fs.StringVar(&r.softnet, "softnet", "", "the resolved softnet to make setuid-root")
	fs.BoolVar(&r.hardenSSH, "harden-ssh", false, "also write the key-only sshd drop-in")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if cli.HostOS != "darwin" {
		return errors.New("billet join prerequisites: it prepares a Mac; on Linux the package and the host " +
			"role prepare a node")
	}
	if joinGeteuid() != 0 {
		return errors.New("billet join prerequisites: run by `billet join` under sudo; it needs root")
	}
	if r.account == "" || r.account == "root" {
		return errors.New("billet join prerequisites: --account must name the account that will run the node")
	}
	if err := config.ValidateNodeName("--node-name", r.nodeName); err != nil {
		return err
	}

	for _, s := range macRootSteps(r) {
		done, err := s.done(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
		if done {
			fmt.Fprintf(env.Stdout, "ok       %s\n", s.what)

			continue
		}

		if err := s.apply(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}

		// PROVED, NOT ASSUMED: a command that exits 0 and changes nothing is a
		// step not done.
		if done, err := s.done(ctx); err != nil || !done {
			return fmt.Errorf("%s: done, and it still does not read back as done (%v)", s.what, err)
		}

		fmt.Fprintf(env.Stdout, "done     %s\n", s.what)
	}

	return nil
}

// runStep runs one command, failing with its output.
func runStep(ctx context.Context, name string, args ...string) error {
	if out, err := prereqRun(ctx, name, args...); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}

	return nil
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

// ownedBy reports whether dir is a directory account owns. Absent is not
// owned; anything else unreadable is an error, never a no.
func ownedBy(dir, account string) (bool, error) {
	uid, _, err := accountIDs(account)
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
		return false, fmt.Errorf("%s is not a directory", dir)
	}

	return fileOwner(info) == uid, nil
}

// giveDir creates dir and hands it to account, never following a symlink.
func giveDir(dir, account string) error {
	uid, gid, err := accountIDs(account)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a %s, not a directory, so it was not handed over", dir, info.Mode().Type())
	}

	return os.Lchown(dir, uid, gid)
}

// softnetGranted reports whether the softnet file is setuid and root's.
func softnetGranted(path string) (bool, error) {
	info, err := checkedSoftnet(path)
	if err != nil {
		return false, err
	}

	return info.Mode()&fs.ModeSetuid != 0 && fileOwner(info) == 0, nil
}

// grantSoftnet makes softnet setuid-root: chown first, because chown clears
// the setuid bit (measured on macOS), then the bit.
func grantSoftnet(path string) error {
	info, err := checkedSoftnet(path)
	if err != nil {
		return err
	}

	if err := os.Lchown(path, 0, -1); err != nil {
		return err
	}

	return os.Chmod(path, info.Mode().Perm()|fs.ModeSetuid)
}

// checkedSoftnet refuses a softnet path that is not a regular file named
// softnet inside a Homebrew Cellar: root is about to make it setuid-root.
func checkedSoftnet(path string) (fs.FileInfo, error) {
	inCellar := false
	for _, cellar := range softnetCellars {
		inCellar = inCellar || strings.HasPrefix(filepath.Clean(path), cellar)
	}

	if !filepath.IsAbs(path) || filepath.Base(path) != "softnet" || !inCellar {
		return nil, fmt.Errorf("%s is not a softnet inside a Homebrew Cellar, so it is not made setuid-root", path)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file, so it is not made setuid-root", path)
	}

	return info, nil
}

// sshdDropInBody is the key-only drop-in, from
// docs/reference/reference-hardware.md#headless-operation.
func sshdDropInBody(account string) string {
	return "PermitRootLogin no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nAllowUsers " +
		account + "\n"
}

// sshdHardened reports whether the drop-in says exactly what it should.
func sshdHardened(account string) (bool, error) {
	raw, err := os.ReadFile(sshdDropIn)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return string(raw) == sshdDropInBody(account), nil
}

// hardenSSHD writes the drop-in, and removes it again when sshd refuses the
// result.
//
// ONLY OVER A KEY THIS ACCOUNT HOLDS: with no authorized key, key-only SSH is
// no SSH at all for it, and AllowUsers shuts every other account out.
func hardenSSHD(ctx context.Context, account string) error {
	u, err := user.Lookup(account)
	if err != nil {
		return err
	}

	if !holdsAuthorizedKey(filepath.Join(u.HomeDir, ".ssh", "authorized_keys")) {
		return fmt.Errorf("%s has no key in ~/.ssh/authorized_keys, so a key-only sshd would leave it no way "+
			"in; add one, prove a key login, and run this again", account)
	}

	if err := os.WriteFile(sshdDropIn, []byte(sshdDropInBody(account)), 0o644); err != nil {
		return err
	}

	if out, err := prereqRun(ctx, "sshd", "-t"); err != nil {
		if rmErr := os.Remove(sshdDropIn); rmErr != nil {
			return fmt.Errorf("sshd -t refused the drop-in (%w: %s), and removing it again failed: %v: "+
				"remove %s by hand before sshd restarts", err, strings.TrimSpace(out), rmErr, sshdDropIn)
		}

		return fmt.Errorf("sshd -t refused the drop-in, so it was removed again: %w: %s", err, strings.TrimSpace(out))
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

// accountIDs is an account's uid and primary gid.
func accountIDs(account string) (int, int, error) {
	u, err := user.Lookup(account)
	if err != nil {
		return 0, 0, err
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}

	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}

	return uid, gid, nil
}

// verifyLinuxPrerequisites refuses a Linux machine the node cannot run on,
// naming the fix. It changes nothing: the package makes the account and the
// units, and a Firecracker host is prepared by the host role.
func verifyLinuxPrerequisites(ctx context.Context, env cli.Env, req joinRequest) error {
	var missing []string

	if _, err := user.Lookup(initconfig.ServiceGroup); err != nil {
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
