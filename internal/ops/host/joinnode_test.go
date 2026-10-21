package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
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
	sudoArgs  []string
	upConfig  string
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
		prereqRun, prereqSudo, prereqLookPath, prereqExecutable, macServiceDirs, softnetCellars}
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
		prereqExecutable = saved[9].(func() (string, error))
		macServiceDirs = saved[10].([]string)
		softnetCellars = saved[11].([]string)
	})

	joinFetchAuthority = func(_ context.Context, base, fp string) (string, error) {
		f.record("fetch " + base + " " + fp)

		return "dep-1", f.fetchErr
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
	prereqSudo = func(_ context.Context, _ cli.Env, args []string) error {
		f.record("sudo")
		f.sudoArgs = args

		return nil
	}
	prereqLookPath = func(name string) (string, error) {
		switch name {
		case "tart", "softnet":
			return filepath.Join(bin, name), nil
		}

		return "", errors.New("not found")
	}
	prereqExecutable = func() (string, error) { return "/usr/local/bin/billet", nil }
	macServiceDirs = []string{owned}
	softnetCellars = []string{cellar + "/"}

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

	want := []string{
		"fetch https://cp.example:7717 SHA256:abc",
		"sudo",
		"generate",
		"enroll mac-1 https://cp.example:7717 SHA256:abc tok-1",
		"images",
		"up",
	}
	if strings.Join(f.trace, " → ") != strings.Join(want, " → ") {
		t.Errorf("join did:\n  %s\nwant:\n  %s", strings.Join(f.trace, " → "), strings.Join(want, " → "))
	}

	if !slices.Contains(f.sudoArgs, "--softnet") || f.sudoArgs[1] != "join" || f.sudoArgs[2] != "prerequisites" {
		t.Errorf("the privileged half was asked %v, want `billet join prerequisites ... --softnet <file>`", f.sudoArgs)
	}

	for _, pair := range [][2]string{
		{"--join", "cp.example:7717"}, {"--join-deployment", "dep-1"}, {"--node-name", "mac-1"},
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
func TestJoinDoesNotEnrollANodeThatHoldsItsBundle(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig}
	cfgPath := stageJoin(t, f)
	tlsDir := filepath.Join(filepath.Dir(cfgPath), "tls")
	mustOK(t, os.MkdirAll(tlsDir, 0o700))
	for _, name := range []string{"node.crt", "node.key", "ca.crt"} {
		mustOK(t, os.WriteFile(filepath.Join(tlsDir, name), []byte("x"), 0o600))
	}

	if err := Join(t.Context(), processEnv(), joinArgs); err != nil {
		t.Fatalf("join: %v", err)
	}
	if slices.ContainsFunc(f.trace, func(s string) bool { return strings.HasPrefix(s, "enroll") }) {
		t.Errorf("an enrolled node enrolled again: %v", f.trace)
	}
	if f.trace[len(f.trace)-1] != "up" {
		t.Errorf("the re-run did not end with local up: %v", f.trace)
	}
}

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
	got := joinInitArgs(req, "dep-1")
	if !slices.Equal(got[len(got)-2:], []string{"--macos-image", "ghcr.io/x/y:1"}) {
		t.Errorf("init would be given %v, want the flags after -- at the end", got)
	}
}
