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

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/wirecert"
)

// joinFakes replaces every step `billet join` composes with one that records
// what it was asked, in order. Not parallel-safe, like the seams it sets.
type joinFakes struct {
	trace []string
	// fetchErr fails the authority; generated is what the fake init writes at
	// the service path ("" writes nothing, as init does beside a foreign file).
	fetchErr  error
	generated string
	initArgs  []string
	sudoCalls []string
	// sshdErr fails the fake `sudo sshd -t`.
	sshdErr  error
	upConfig string
}

func (f *joinFakes) record(step string) { f.trace = append(f.trace, step) }

// onPlatform pins cli.HostOS, the effective uid and the architecture.
func onPlatform(t *testing.T, goos string, euid int) {
	t.Helper()

	host, uid, arch := cli.HostOS, joinGeteuid, joinGOARCH
	cli.HostOS, joinGeteuid, joinGOARCH = goos, func() int { return euid }, "arm64"
	t.Cleanup(func() { cli.HostOS, joinGeteuid, joinGOARCH = host, uid, arch })
}

// stageJoin installs the fakes, a service path under a temp directory, and a
// Mac whose prerequisites are all already in place except softnet's grant.
func stageJoin(t *testing.T, f *joinFakes) string {
	t.Helper()

	// RESOLVED, because the softnet file is: macOS's temp directory is behind
	// the /var symlink, and the Cellar it must sit in is compared as a prefix.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	mustOK(t, err)
	cfgPath := filepath.Join(dir, "etc", "billet.yaml")
	tlsDir := filepath.Join(dir, "etc", "tls")

	// A HOMEBREW LAYOUT: tart and softnet side by side in bin, softnet a
	// symlink into a Cellar whose file is not yet setuid-root.
	cellar := filepath.Join(dir, "Cellar")
	real := filepath.Join(cellar, "softnet", "0.23.0", "bin", "softnet")
	mustOK(t, os.MkdirAll(filepath.Dir(real), 0o755))
	mustOK(t, os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755))
	bin := filepath.Join(dir, "bin")
	mustOK(t, os.MkdirAll(bin, 0o755))
	mustOK(t, os.Symlink(real, filepath.Join(bin, "softnet")))
	mustOK(t, os.WriteFile(filepath.Join(bin, "tart"), nil, 0o755))

	owned := filepath.Join(dir, "usr-local-bin")
	mustOK(t, os.Mkdir(owned, 0o755))

	saved := []any{joinFetchAuthority, joinGenerate, joinEnroll, joinPullImages, joinUp, serviceConfigPath,
		prereqRun, prereqSudo, prereqLookPath, macServiceDirs, softnetCellars, prereqRootUID, prereqTrustRoot,
		sshdDropIn, prereqLookupUser}
	t.Cleanup(func() {
		joinFetchAuthority = saved[0].(func(context.Context, string, string) (string, error))
		joinGenerate = saved[1].(func(context.Context, cli.Env, []string) error)
		joinEnroll = saved[2].(func(context.Context, cli.Env, *config.Config, string, string, string) error)
		joinPullImages = saved[3].(func(context.Context, cli.Env, *config.Config) error)
		joinUp = saved[4].(func(context.Context, cli.Env, upOptions) error)
		serviceConfigPath = saved[5].(func() string)
		prereqRun = saved[6].(func(context.Context, string, ...string) (string, error))
		prereqSudo = saved[7].(func(context.Context, cli.Env, []string) error)
		prereqLookPath = saved[8].(func(string) (string, error))
		macServiceDirs = saved[9].([]string)
		softnetCellars = saved[10].([]string)
		prereqRootUID = saved[11].(int)
		prereqTrustRoot = saved[12].(string)
		sshdDropIn = saved[13].(string)
		prereqLookupUser = saved[14].(func(string) (*user.User, error))
	})

	joinFetchAuthority = func(_ context.Context, base, fp string) (string, error) {
		f.record("fetch " + base + " " + fp)

		return testJoinDeployment, f.fetchErr
	}
	joinGenerate = func(_ context.Context, _ cli.Env, args []string) error {
		f.record("generate")
		f.initArgs = args
		if f.generated != "" {
			mustOK(t, os.MkdirAll(filepath.Dir(cfgPath), 0o755))
			mustOK(t, os.WriteFile(cfgPath, []byte(strings.ReplaceAll(f.generated, "TLS", tlsDir)), 0o600))
		}

		return nil
	}
	joinEnroll = func(_ context.Context, _ cli.Env, cfg *config.Config, base, fp, token string) error {
		f.record("enroll " + cfg.Node.Name + " " + base + " " + fp + " " + token)

		return nil
	}
	joinPullImages = func(context.Context, cli.Env, *config.Config) error {
		f.record("images")

		return nil
	}
	joinUp = func(_ context.Context, _ cli.Env, o upOptions) error {
		f.record("up")
		f.upConfig = o.configPath

		return nil
	}
	serviceConfigPath = func() string { return cfgPath }

	// pmset and scutil already say what the join wants.
	prereqRun = func(_ context.Context, name string, args ...string) (string, error) {
		switch name + " " + strings.Join(args, " ") {
		case "pmset -g":
			return " sleep                0 (sleep prevented by powerd)\n disksleep            0\n", nil
		case "scutil --get HostName", "scutil --get LocalHostName", "scutil --get ComputerName":
			return "mac-1\n", nil
		}
		f.record("run " + name + " " + strings.Join(args, " "))

		return "", nil
	}
	prereqSudo = fakeSudo(t, func(call string) {
		f.record("sudo " + call)
		f.sudoCalls = append(f.sudoCalls, call)
	}, func(args []string) error {
		if args[0] == sudoSshd {
			return f.sshdErr
		}

		return nil
	})
	prereqLookPath = func(name string) (string, error) {
		switch name {
		case "tart", "softnet":
			return filepath.Join(bin, name), nil
		}

		return "", errors.New("not found")
	}
	macServiceDirs = []string{owned}
	softnetCellars = []string{cellar + "/"}

	// THIS TEST'S ACCOUNT STANDS FOR ROOT, under its own directory: the
	// pre-checks then read files it made exactly as they read root's.
	prereqRootUID = os.Getuid()
	prereqTrustRoot = dir
	sshdDropIn = filepath.Join(dir, "sshd_config.d", "000-headless.conf")
	mustOK(t, os.MkdirAll(filepath.Dir(sshdDropIn), 0o755))
	home := filepath.Join(dir, "home")
	mustOK(t, os.MkdirAll(filepath.Join(home, ".ssh"), 0o700))
	mustOK(t, os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte("ssh-ed25519 AAAA op\n"), 0o600))
	me, err := user.Current()
	mustOK(t, err)
	prereqLookupUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: me.Uid, Gid: me.Gid, HomeDir: home}, nil
	}

	return cfgPath
}

// joinedNodeConfig is the node-only config the fake init writes.
const joinedNodeConfig = `node:
  name: mac-1
  server_addr: cp.example:7717
  provider: tart
  tls:
    cert: TLS/node.crt
    key: TLS/node.key
    ca: TLS/ca.crt
`

var joinArgs = []string{
	"--server", "cp.example:7717", "--ca-fingerprint", "SHA256:abc", "--token", "tok-1",
	"--node-name", "mac-1", "--org", "acme",
}

// THE WHOLE JOIN ON A MAC, IN ITS ORDER (#181): the authority is proved before
// anything else, the one privileged step still owed (softnet's grant) runs
// under one sudo, the generation is bound to the deployment the authority
// named, and enrollment, the images and `local up` follow.
//
// Mutations: moving the fetch after the prerequisites, dropping
// --join-deployment, or skipping the images each change the trace or the args.
func TestJoinFollowsTheOrderItsSafetyDependsOn(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig}
	cfgPath := stageJoin(t, f)

	if err := Join(t.Context(), processEnv(), joinArgs); err != nil {
		t.Fatalf("join: %v", err)
	}

	real := strings.TrimPrefix(f.sudoCalls[1], "/usr/sbin/chown -h root ")
	want := []string{
		"fetch https://cp.example:7717 SHA256:abc",
		// ONE PASSWORD, THEN THE SYSTEM BINARIES: billet itself never runs as root.
		"sudo -v",
		"sudo /usr/sbin/chown -h root " + real,
		"sudo /bin/chmod u+s " + real,
		"generate",
		"enroll mac-1 https://cp.example:7717 SHA256:abc tok-1",
		"images",
		"up",
	}
	if strings.Join(f.trace, " → ") != strings.Join(want, " → ") {
		t.Errorf("join did:\n  %s\nwant:\n  %s", strings.Join(f.trace, " → "), strings.Join(want, " → "))
	}

	if !strings.HasSuffix(real, "/Cellar/softnet/0.23.0/bin/softnet") {
		t.Errorf("the grant landed on %q, want the Cellar file behind the symlink", real)
	}

	// AND --harden-ssh ADDS THE DROP-IN, installed by the system's install and
	// checked by sshd, on a Mac whose softnet is already granted.
	f2 := &joinFakes{generated: joinedNodeConfig}
	stageJoin(t, f2)
	if err := Join(t.Context(), processEnv(), append(slices.Clone(joinArgs), "--harden-ssh")); err != nil {
		t.Fatalf("join --harden-ssh: %v", err)
	}
	if !slices.ContainsFunc(f2.sudoCalls, func(c string) bool { return strings.HasPrefix(c, "/usr/bin/install ") }) ||
		!slices.Contains(f2.sudoCalls, "/usr/sbin/sshd -t") {
		t.Errorf("--harden-ssh did not install and check the drop-in: %v", f2.sudoCalls)
	}

	for _, pair := range [][2]string{
		{"--join", "cp.example:7717"}, {"--join-deployment", testJoinDeployment}, {"--node-name", "mac-1"},
		{"--profile", "local-service"}, {"--config", cfgPath}, {"--org", "acme"}, {"--provider", "tart"},
	} {
		i := slices.Index(f.initArgs, pair[0])
		if i < 0 || i+1 >= len(f.initArgs) || f.initArgs[i+1] != pair[1] {
			t.Errorf("init was given %v, want %s %s", f.initArgs, pair[0], pair[1])
		}
	}

	if f.upConfig != cfgPath {
		t.Errorf("local up ran on %q, want %q", f.upConfig, cfgPath)
	}
}

// A WRONG FINGERPRINT OR ADDRESS CHANGES NOTHING: the authority is the first
// thing asked, and nothing after it runs.
func TestJoinChangesNothingWhenTheAuthorityDoesNotMatch(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig, fetchErr: errors.New("fingerprint mismatch")}
	stageJoin(t, f)

	err := Join(t.Context(), processEnv(), joinArgs)
	if err == nil || !strings.Contains(err.Error(), "nothing on this machine was changed") {
		t.Fatalf("a mismatched authority answered %v", err)
	}
	if len(f.trace) != 1 {
		t.Errorf("after a mismatched authority the join went on: %v", f.trace)
	}
}

// A CONFIG INIT LEFT ALONE IS NOT STARTED: init writes `<path>.new` beside a
// file it cannot prove is its own, and enrolling or starting then would act on
// the old file.
func TestJoinStopsWhenInitLeftTheConfigAlone(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{}
	cfgPath := stageJoin(t, f)
	mustOK(t, os.MkdirAll(filepath.Dir(cfgPath), 0o755))
	other := strings.ReplaceAll(strings.Replace(joinedNodeConfig, "mac-1", "other", 1), "TLS", filepath.Dir(cfgPath))
	mustOK(t, os.WriteFile(cfgPath, []byte(other), 0o600))

	err := Join(t.Context(), processEnv(), joinArgs)
	if err == nil || !strings.Contains(err.Error(), "is not the node config this join generated") {
		t.Fatalf("a config init did not write answered %v", err)
	}
	if slices.ContainsFunc(f.trace, func(s string) bool { return strings.HasPrefix(s, "enroll") || s == "up" }) {
		t.Errorf("the join enrolled or started over a config it did not generate: %v", f.trace)
	}
}

// A NODE ALREADY HOLDING ITS BUNDLE IS NOT ENROLLED AGAIN, so a re-run after
// the approval goes on to the images and the start.
//
// AND ONLY A BUNDLE THAT PROVES IT: one issued for another node, by another
// deployment, or that is not a bundle at all is refused, never enrolled over or
// started with.
func TestJoinDoesNotEnrollANodeThatHoldsItsBundle(t *testing.T) {
	issue := func(t *testing.T, deployment, name string) wirecert.Bundle {
		t.Helper()

		ca, err := wirecert.LoadOrCreateCA(t.TempDir(), deployment)
		mustOK(t, err)
		b, err := ca.IssueNode(name)
		mustOK(t, err)

		return b
	}

	for name, c := range map[string]struct {
		write func(t *testing.T, dir string)
		want  string
	}{
		"its own": {func(t *testing.T, dir string) { mustOK(t, issue(t, testJoinDeployment, "mac-1").Write(dir)) }, ""},
		"another node's": {func(t *testing.T, dir string) {
			mustOK(t, issue(t, testJoinDeployment, "mac-2").Write(dir))
		}, `node "mac-2"'s`},
		"another deployment's": {func(t *testing.T, dir string) {
			mustOK(t, issue(t, "fedcba9876543210fedcba9876543210", "mac-1").Write(dir))
		}, "deployment fedcba9876543210fedcba9876543210's"},
		"not a bundle": {func(t *testing.T, dir string) {
			mustOK(t, os.MkdirAll(dir, 0o700))
			for _, n := range []string{"node.crt", "node.key", "ca.crt"} {
				mustOK(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600))
			}
		}, "cannot be used"},
	} {
		t.Run(name, func(t *testing.T) {
			onPlatform(t, "darwin", 501)

			f := &joinFakes{generated: joinedNodeConfig}
			cfgPath := stageJoin(t, f)
			c.write(t, filepath.Join(filepath.Dir(cfgPath), "tls"))

			err := Join(t.Context(), processEnv(), joinArgs)
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("a bundle that is %s answered %v, want a refusal saying %q", name, err, c.want)
				}
				if slices.ContainsFunc(f.trace, func(s string) bool { return strings.HasPrefix(s, "enroll") || s == "up" }) {
					t.Errorf("a bundle that is %s went on to %v", name, f.trace)
				}

				return
			}

			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if slices.ContainsFunc(f.trace, func(s string) bool { return strings.HasPrefix(s, "enroll") }) {
				t.Errorf("an enrolled node enrolled again: %v", f.trace)
			}
			if f.trace[len(f.trace)-1] != "up" {
				t.Errorf("the re-run did not end with local up: %v", f.trace)
			}
		})
	}
}

// A CONFIG INIT LEFT ALONE BESIDE A `.new` STOPS THE JOIN, even when the old
// file names this join's node and server: init returned success and the
// generation is not what the services would start.
func TestJoinStopsWhenInitWroteBesideTheConfig(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig}
	cfgPath := stageJoin(t, f)

	generate := joinGenerate
	joinGenerate = func(ctx context.Context, env cli.Env, args []string) error {
		if err := generate(ctx, env, args); err != nil {
			return err
		}

		return os.WriteFile(cfgPath+".new", []byte("node: {}\n"), 0o600)
	}

	err := Join(t.Context(), processEnv(), joinArgs)
	if err == nil || !strings.Contains(err.Error(), "wrote "+cfgPath+".new") {
		t.Fatalf("a generation written beside the config answered %v", err)
	}
	if slices.ContainsFunc(f.trace, func(s string) bool { return strings.HasPrefix(s, "enroll") || s == "up" }) {
		t.Errorf("the join went on to %v", f.trace)
	}

	// A `.new` LEFT FROM BEFORE, unchanged by this run, is not this run's.
	f2 := &joinFakes{generated: joinedNodeConfig}
	cfgPath = stageJoin(t, f2)
	mustOK(t, os.MkdirAll(filepath.Dir(cfgPath), 0o755))
	mustOK(t, os.WriteFile(cfgPath+".new", []byte("node: {}\n"), 0o600))
	if err := Join(t.Context(), processEnv(), joinArgs); err != nil {
		t.Errorf("a .new this run did not write stopped the join: %v", err)
	}
}

// testJoinDeployment is the deployment the fake authority names.
const testJoinDeployment = "0123456789abcdef0123456789abcdef"

// --dry-run PROVES THE AUTHORITY AND READS, AND CHANGES NOTHING.
func TestJoinDryRunChangesNothing(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig}
	stageJoin(t, f)

	if err := Join(t.Context(), processEnv(), append(slices.Clone(joinArgs), "--dry-run")); err != nil {
		t.Fatalf("join --dry-run: %v", err)
	}
	if len(f.trace) != 1 || !strings.HasPrefix(f.trace[0], "fetch") {
		t.Errorf("--dry-run did more than fetch the authority: %v", f.trace)
	}
}

// THE REFUSALS THE FLAGS AND THE PLATFORM DECIDE, each before the authority is
// even asked.
func TestJoinRefusesWhatItCannotDo(t *testing.T) {
	for _, c := range []struct {
		name, goos string
		euid       int
		arch       string
		args       []string
		want       string
	}{
		{"root on a Mac", "darwin", 0, "arm64", joinArgs, "without sudo"},
		{"tart on an Intel Mac", "darwin", 501, "amd64", joinArgs, "Apple silicon"},
		{"a user on Linux", "linux", 1000, "amd64", joinArgs, "run it as root"},
		{"no node name", "darwin", 501, "arm64", joinArgs[:6], "--node-name is required"},
		{"another config", "darwin", 501, "arm64", append(slices.Clone(joinArgs), "--config", "/tmp/x.yaml"), "is not the config the services read"},
		{"a stray argument", "darwin", 501, "arm64", append(slices.Clone(joinArgs), "extra"), "unexpected argument"},
	} {
		t.Run(c.name, func(t *testing.T) {
			onPlatform(t, c.goos, c.euid)
			joinGOARCH = c.arch

			f := &joinFakes{generated: joinedNodeConfig}
			stageJoin(t, f)

			err := Join(t.Context(), processEnv(), c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("answered %v, want a refusal saying %q", err, c.want)
			}
			if len(f.trace) != 0 {
				t.Errorf("a refused join did %v", f.trace)
			}
		})
	}
}

// INIT'S OWN FLAGS GO AFTER `--`, AS THEY ARE.
func TestJoinPassesInitsFlagsThrough(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig}
	stageJoin(t, f)

	args := append(slices.Clone(joinArgs), "--dry-run", "--", "--macos-image", "ghcr.io/x/y:1")
	if err := Join(t.Context(), processEnv(), args); err != nil {
		t.Fatalf("join: %v", err)
	}

	req, err := parseJoin(processEnv(), args)
	if err != nil {
		t.Fatal(err)
	}
	got := joinInitArgs(req, testJoinDeployment)
	if !slices.Equal(got[len(got)-2:], []string{"--macos-image", "ghcr.io/x/y:1"}) {
		t.Errorf("init would be given %v, want the flags after -- at the end", got)
	}
}
