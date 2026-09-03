package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// A valid trusted-pool policy for the docker trial: a non-default runner group
// and one exact workflow ref. Docker refuses anything less.
const (
	testTrialGroup    = "billet-trial"
	testTrialWorkflow = "acme/repo/.github/workflows/ci.yml@refs/heads/main"
)

// WHAT `billet init` WRITES HAS TO RUN, and that is the whole claim it makes.
//
// Copying the example did not: it describes the intended Firecracker deployment,
// so the provider had to be changed, every tier's image replaced with something
// pullable, and the state directories pointed somewhere writable — four edits
// before anything started, each silent when wrong. A generated config that also
// needs editing is the same trap with an extra step.
//
// The App ids are the one thing it cannot know, because the App does not exist
// yet. Everything else must be true of THIS machine — including that the tiers
// are trusted and bound to a runner group and workflow allowlist, without which
// the docker provider refuses the first job.
func TestInitWritesAConfigThatLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Filled in the way `github-app create --config` fills them, so what is under
	// test is everything init decided rather than the placeholder it leaves.
	cfg := loadWrittenConfig(t, path)

	if cfg.Server == nil || cfg.Node == nil {
		t.Fatal("a generated config describes both roles; this machine runs both")
	}

	vcpu, memory, err := config.DetectHostCapacity()
	if err != nil {
		t.Skipf("cannot detect this host's capacity: %v", err)
	}

	if cfg.Server.MaxVCPU > vcpu || cfg.Server.MaxMemory > memory {
		t.Errorf("the ceiling is %d vCPU / %s on a machine with %d vCPU / %s",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory, vcpu, memory)
	}

	if cfg.Server.MaxVCPU <= 0 || cfg.Server.MaxMemory <= 0 {
		t.Errorf("the ceiling is %d vCPU / %s, so nothing can ever be escrowed",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	// PULLABLE, TRUSTED, AND ALLOWLISTED. The image is handed straight to the
	// backend, and docker refuses an untrusted pool — so a tier that is not all
	// three is a config that loads and then refuses its first job.
	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]
		if !strings.Contains(tier.Image, "/") {
			t.Errorf("tier %q has image %q, which is not a pullable reference", tier.Label, tier.Image)
		}
		if tier.Trust != config.WorkloadTrusted {
			t.Errorf("tier %q is %q; docker refuses anything but trusted", tier.Label, tier.Trust)
		}
		if tier.RunnerGroup != testTrialGroup {
			t.Errorf("tier %q runner group is %q, want %q", tier.Label, tier.RunnerGroup, testTrialGroup)
		}
		if len(tier.Workflows) != 1 || tier.Workflows[0] != testTrialWorkflow {
			t.Errorf("tier %q workflows are %v, want [%q]", tier.Label, tier.Workflows, testTrialWorkflow)
		}
	}

	if cfg.Node.Name == "" {
		t.Error("the node has no name and no certificate to take one from")
	}
}

// FIRECRACKER IS UNTRUSTED BY DEFAULT AND NEEDS NO POLICY, and the config it
// writes names the host prep billet cannot do.
//
// A microVM has its own kernel and runs untrusted work on the untrusted bridge,
// so unlike docker it does not refuse a job for want of a trusted-pool policy.
// What it needs instead is on the host — a kernel, two bridges, a Ceph cluster —
// which the generated config names in node.firecracker and node.ceph so an
// operator (and `billet check`) sees exactly what to prepare.
func TestInitFirecrackerWritesAnUntrustedConfigThatLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--provider", "firecracker",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfg := loadWrittenConfig(t, path)

	if cfg.Node == nil || cfg.Node.Provider != config.ProviderFirecracker {
		t.Fatalf("the node is not a firecracker host: %+v", cfg.Node)
	}
	if cfg.Node.Firecracker == nil {
		t.Fatal("no node.firecracker block, so the host prep is unnamed")
	}
	if cfg.Node.Firecracker.UntrustedBridge == "" {
		t.Error("no untrusted bridge, so this host cannot run the untrusted tiers it declares")
	}
	if cfg.Node.Ceph == nil || cfg.Node.Ceph.ImagePool == "" || cfg.Node.Ceph.CachePool == "" {
		t.Errorf("the ceph storage block is incomplete: %+v", cfg.Node.Ceph)
	}

	if len(cfg.Tiers) == 0 {
		t.Fatal("no tiers")
	}
	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]
		if tier.Trust != config.WorkloadUntrusted {
			t.Errorf("tier %q is %q; firecracker defaults untrusted", tier.Label, tier.Trust)
		}
		if tier.RunnerGroup != "" || len(tier.Workflows) != 0 {
			t.Errorf("tier %q carries a trusted-pool policy it was not given: group=%q workflows=%v",
				tier.Label, tier.RunnerGroup, tier.Workflows)
		}
	}
}

// ON EVERY SIZE OF MACHINE, NOT JUST THIS ONE.
//
// The test above generates for whatever host runs it, so a laptop with plenty of
// cores proves nothing about the small machines billet is most likely to be
// tried on first. The ceiling is detected capacity minus headroom while the
// tiers are billet's own choice, so a generated config is only valid where the
// tiers happen to fit under the ceiling — and "happen to" is not a property.
func TestInitWritesAConfigThatLoadsOnAnySizeOfMachine(t *testing.T) {
	hosts := []struct {
		vcpu   int
		memory config.ByteSize
	}{
		{1, 2 * config.GiB},     // the smallest thing that can boot
		{2, 4 * config.GiB},     // a small cloud VM
		{4, 16 * config.GiB},    // a GitHub-hosted runner
		{8, 16 * config.GiB},    // cores without the memory to match
		{2, 64 * config.GiB},    // memory without the cores
		{16, 64 * config.GiB},   // a workstation
		{128, 512 * config.GiB}, // the reference host
	}

	// Both host backends billet init can write. Docker needs a trusted-pool
	// policy; firecracker is untrusted by default and needs none. The ceiling is
	// the same computation for both, so both must fit the tiers under it on every
	// machine.
	providers := []struct {
		kind   config.ProviderKind
		params func(vcpu int, memory config.ByteSize) initconfig.Params
	}{
		{
			kind: config.ProviderDocker,
			params: func(vcpu int, memory config.ByteSize) initconfig.Params {
				return initconfig.Params{
					Org: "acme", Provider: config.ProviderDocker,
					Image: initconfig.DefaultRunnerImage, VCPU: vcpu, Memory: memory,
					RunnerGroup: testTrialGroup, Workflows: []string{testTrialWorkflow},
				}
			},
		},
		{
			kind: config.ProviderFirecracker,
			params: func(vcpu int, memory config.ByteSize) initconfig.Params {
				return initconfig.Params{
					Org: "acme", Provider: config.ProviderFirecracker,
					Image: initconfig.DefaultFirecrackerImage, VCPU: vcpu, Memory: memory,
				}
			},
		},
	}

	for _, p := range providers {
		for _, h := range hosts {
			t.Run(fmt.Sprintf("%s-%dvcpu-%s", p.kind, h.vcpu, h.memory), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "billet.yaml")

				body, _, err := initconfig.Generate(p.params(h.vcpu, h.memory))
				if err != nil {
					t.Fatalf("a %d vCPU / %s machine gets no config: %v", h.vcpu, h.memory, err)
				}

				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}

				if err := writeGitHubBlock(path, githubBlock{
					Org: "acme", AppID: 1, InstallationID: 2,
					PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
				}); err != nil {
					t.Fatalf("filling in the app: %v", err)
				}

				cfg, err := config.Load(path)
				if err != nil {
					t.Fatalf("a %d vCPU / %s machine gets a config that does not load: %v\n\n%s",
						h.vcpu, h.memory, err, body)
				}

				if len(cfg.Tiers) == 0 {
					t.Fatal("the generated config has no tiers, so no job can ever be scheduled")
				}

				for i := range cfg.Tiers {
					tier := &cfg.Tiers[i]
					if tier.VCPU > cfg.Server.MaxVCPU || tier.Memory > cfg.Server.MaxMemory {
						t.Errorf("tier %q is %d vCPU / %s under a ceiling of %d vCPU / %s, so it can never be placed",
							tier.Label, tier.VCPU, tier.Memory, cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
					}
				}
			})
		}
	}
}

// THE PRINTED TRUST GUIDANCE MATCHES THE TIERS IT GENERATED.
//
// A firecracker host given a runner group and workflow allowlist emits TRUSTED
// tiers, so telling that operator their jobs run isolated on the untrusted
// bridge would be a dangerous falsehood — the exact case an earlier review caught.
// The guidance follows what Generate decided, not the provider default.
func TestInitFirecrackerGuidanceFollowsTheRealTrust(t *testing.T) {
	// The property, not the prose: the printed trust marker must match the trust
	// the tier actually carries, and never the opposite one. Derived from the
	// parsed tier so a legitimate reword still passes while a wrong-trust branch
	// fails — and asserted on the code-formatted `trust: X` marker rather than the
	// bare word, since "untrusted" contains "trusted".
	marker := func(trust config.WorkloadTrust) string { return "`trust: " + string(trust) + "`" }

	t.Run("untrusted by default", func(t *testing.T) {
		out, cfg := runInit(t, "--provider", "firecracker")
		if cfg.Tiers[0].Trust != config.WorkloadUntrusted {
			t.Fatalf("expected untrusted tiers, got %q", cfg.Tiers[0].Trust)
		}
		if !strings.Contains(out, marker(config.WorkloadUntrusted)) {
			t.Errorf("guidance lacks the tiers' %s marker:\n%s", marker(config.WorkloadUntrusted), out)
		}
		if strings.Contains(out, marker(config.WorkloadTrusted)) {
			t.Errorf("untrusted guidance also prints the trusted marker:\n%s", out)
		}
		if !strings.Contains(out, "untrusted bridge") {
			t.Errorf("untrusted guidance does not mention the isolating bridge:\n%s", out)
		}
	})

	t.Run("trusted with a policy", func(t *testing.T) {
		out, cfg := runInit(t, "--provider", "firecracker",
			"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow)
		if cfg.Tiers[0].Trust != config.WorkloadTrusted {
			t.Fatalf("expected trusted tiers, got %q", cfg.Tiers[0].Trust)
		}
		if !strings.Contains(out, marker(config.WorkloadTrusted)) {
			t.Errorf("guidance lacks the tiers' %s marker:\n%s", marker(config.WorkloadTrusted), out)
		}
		// The dangerous falsehoods: printing the untrusted marker, or telling a
		// trusted operator their jobs are isolated on the untrusted bridge.
		if strings.Contains(out, marker(config.WorkloadUntrusted)) {
			t.Errorf("trusted guidance also prints the untrusted marker:\n%s", out)
		}
		if strings.Contains(out, "untrusted bridge") {
			t.Errorf("trusted guidance falsely claims untrusted-bridge isolation:\n%s", out)
		}
	})
}

// A DOCKER TRIAL WITH NO TRUST POLICY IS REFUSED, not written and then broken.
//
// Docker shares the host kernel and refuses untrusted work, and a trusted pool
// needs a non-default runner group and a workflow allowlist. Generating an
// untrusted docker tier produces a config that loads and refuses its first job,
// so the missing policy is named up front instead.
func TestInitRefusesADockerTrialWithNoPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	err := cmdInit(t.Context(), []string{"--config", path, "--org", "acme"})
	if err == nil {
		t.Fatal("init wrote a docker config with no runner-group/workflow policy")
	}
	if !strings.Contains(err.Error(), "--runner-group") || !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("the refusal does not name the missing flags: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("a refused init still wrote a file")
	}
}

// THE POLICY IS VALIDATED BY THE FLAG THAT CARRIED IT.
//
// A malformed workflow ref or a runner group the scale-set client cannot carry
// would otherwise surface much later as a config-load error blaming the
// generated tier, on a file the operator did not hand-write.
func TestInitRefusesAMalformedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		wants string
	}{
		{
			name:  "bad workflow ref",
			args:  []string{"--runner-group", testTrialGroup, "--workflow", "not-a-ref"},
			wants: "--workflow",
		},
		{
			name: "default runner group",
			args: []string{"--runner-group", "default", "--workflow", testTrialWorkflow},
			// The default group is not a trusted pool; the refusal is the policy one.
			wants: "--runner-group",
		},
		{
			name: "default runner group, capitalized",
			// GitHub's built-in group is "Default" and resolves case-insensitively,
			// so this names the all-repositories group a trusted pool must never use.
			args:  []string{"--runner-group", "Default", "--workflow", testTrialWorkflow},
			wants: "--runner-group",
		},
		{
			name:  "unsafe runner group",
			args:  []string{"--runner-group", "Platform & Security", "--workflow", testTrialWorkflow},
			wants: "--runner-group",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "billet.yaml")
			err := cmdInit(t.Context(), append([]string{"--config", path, "--org", "acme"}, tc.args...))
			if err == nil {
				t.Fatalf("init accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the refusal does not name %q: %v", tc.wants, err)
			}
		})
	}
}

// IT DOES NOT REPLACE A CONFIG WITHOUT BEING ASKED. A file init cannot prove
// is its own pristine output — this one is not even init-shaped — is left
// byte-identical, and the fresh generation lands BESIDE it for the operator to
// merge deliberately.
func TestInitWillNotClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := os.WriteFile(path, []byte("server:\n  listen: 0.0.0.0:1\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := capture(t, func() {
		if err := cmdInit(t.Context(), []string{
			"--config", path, "--org", "acme",
			"--runner-group", testTrialGroup,
			"--workflow", testTrialWorkflow,
		}); err != nil {
			t.Fatalf("init over a foreign config errored instead of writing beside it: %v", err)
		}
	})

	if !strings.Contains(out, "was NOT touched") {
		t.Errorf("the run does not say it left the original alone:\n%s", out)
	}

	kept, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if !strings.Contains(string(kept), "0.0.0.0:1") {
		t.Error("the existing config was modified")
	}
	if _, err := os.Stat(path + ".new"); err != nil {
		t.Errorf("no fresh generation beside the original: %v", err)
	}

	// AND IT SAYS WHICH COMMAND DOES EDIT A CONFIG, because this branch is where
	// an operator learns that this one will not — after which the very next step
	// in the same sequence edits their file in place. It also has to say which of
	// the two files that command wants, since there are now two.
	if !strings.Contains(out, configEditRule) {
		t.Errorf("the .new guidance never says which command edits a config in place:\n%s", out)
	}
	if !strings.Contains(out, "point it at "+path+" ") {
		t.Errorf("the .new guidance does not point `github-app create` at the file the "+
			"deployment reads:\n%s", out)
	}
}

// THE APP BLOCK IS WRITTEN INTO THE FILE, and the operator's own file survives
// it. Printing a block to paste was a step to get wrong, and getting it wrong is
// quiet: an app_id left at 0 surfaces only when `billet check` runs.
func TestWritingTheAppBlockKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	body := `# a comment the operator wrote
server:
  listen: 127.0.0.1:7717
  state_dir: /tmp/billet-server
  max_vcpu: 8
  max_memory: 32GiB

github:
  org: acme
  app_id: 0
  installation_id: 0
  private_key_path: /tmp/key.pem
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: 42, InstallationID: 99, ClientID: "Iv1.abc",
		PrivateKeyPath: "/etc/billet/key.pem",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	for _, want := range []string{"app_id: 42", "installation_id: 99", "client_id: Iv1.abc",
		"private_key_path: /etc/billet/key.pem"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the config does not contain %q after the write:\n%s", want, got)
		}
	}

	// Everything the operator wrote is still theirs.
	for _, keep := range []string{"# a comment the operator wrote", "max_vcpu: 8", "32GiB"} {
		if !strings.Contains(string(got), keep) {
			t.Errorf("writing the app block lost %q:\n%s", keep, got)
		}
	}
}

// runInit runs cmdInit into a temp config, fills the App block, loads it, and
// returns both the captured stdout and the parsed config so a test can assert
// the printed guidance against what was generated.
func runInit(t *testing.T, args ...string) (string, *config.Config) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet.yaml")
	full := append([]string{"--config", path, "--org", "acme"}, args...)

	var initErr error
	out := capture(t, func() { initErr = cmdInit(t.Context(), full) })
	if initErr != nil {
		t.Fatalf("init: %v", initErr)
	}

	cfg := loadWrittenConfig(t, path)
	if len(cfg.Tiers) == 0 {
		t.Fatal("no tiers")
	}

	return out, cfg
}

// THE PRINTED COMMANDS ARE COPY-PASTEABLE INTO A SHELL.
//
// printInitNext emits `billet ... --config <path>` lines an operator pastes, so
// a path with a space must be quoted into one argument, and the `<your-org>`
// placeholder must be single-quoted so a shell does not read its `<` as
// redirection.
func TestShellArgQuotesForPaste(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", "''"},
		{"acme", "acme"},
		{"your-org", "your-org"},
		{"<your-org>", "'<your-org>'"},
		{"/etc/billet/billet.yaml", "/etc/billet/billet.yaml"},
		{"/path with spaces/billet.yaml", "'/path with spaces/billet.yaml'"},
		{"Platform & Security", "'Platform & Security'"},
		{"a'b", `'a'\''b'`},
		{"o#rg", "'o#rg'"},
	} {
		if got := shellArg(tc.in); got != tc.want {
			t.Errorf("shellArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// THE GUIDANCE QUOTES A SPACED PATH AND NEVER PRINTS THE ANGLE-BRACKET
// PLACEHOLDER UNQUOTED, which a shell would misread as redirection. Driven
// through printInitNext with a path init itself would not produce (t.TempDir has
// no spaces), so the quoting is exercised rather than assumed.
func TestInitNextGuidanceIsShellSafe(t *testing.T) {
	out := capture(t, func() {
		printInitNext("/tmp/a b/billet.yaml", initconfig.Params{
			Provider: config.ProviderDocker, Profile: initconfig.ProfileLocal,
		}, true)
	})

	if !strings.Contains(out, "'/tmp/a b/billet.yaml'") {
		t.Errorf("the spaced config path is not quoted:\n%s", out)
	}
	// The placeholder is <your-org> — unmistakably a placeholder — but SINGLE
	// QUOTED so the shell does not read its `<` as redirection.
	if !strings.Contains(out, "--org '<your-org>'") {
		t.Errorf("the absent-org placeholder is not the shell-quoted <your-org>:\n%s", out)
	}
	if strings.Contains(out, "--org <your-org>") {
		t.Errorf("the placeholder is printed unquoted, so a shell reads its < as redirection:\n%s", out)
	}
}

// THE SERVICE SHAPE'S FILE IS GROUP-READABLE, because the packaged server unit
// runs as user billet and a 0600 root-owned config is a service that cannot
// start. The user-session shape stays 0600 — there is no second reader.
func TestInitLocalServiceWritesAGroupReadableConfig(t *testing.T) {
	asLinux(t)
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--profile", "local-service",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("a local-service config has mode %o, want 0640", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "lock_dir: /run/billet/locks") {
		t.Errorf("the written local-service config carries no lock_dir:\n%s", raw)
	}

	// The printed next step is `github-app create --config <this file>`, whose
	// key-path default follows THIS config — so the key must resolve under
	// /etc/billet, not into a home directory ProtectHome hides from the unit.
	if got, err := defaultKeyPath(path); err != nil || got != "/etc/billet/app-private-key.pem" {
		t.Errorf("github-app create would write the key to %q (err %v), breaking the service shape",
			got, err)
	}
}

// asLinux pins the generation to the systemd shape — /etc/billet, /var/lib, the
// billet group, mode 0640 — on the darwin machines billet is developed on. Not
// parallel-safe; none of these tests are parallel.
func asLinux(t *testing.T) {
	t.Helper()

	prev := hostOS
	hostOS = "linux"
	t.Cleanup(func() { hostOS = prev })
}

// THE MODE IS ENFORCED, NOT REQUESTED: a --force over a 0600 file must come out
// 0640 (WriteFile's create-only mode would keep 0600 and the unit could not
// read it), and the user-session shape must stay 0600 — there is no second
// reader, and widening it silently would be a regression the comment alone
// cannot prevent.
func TestInitEnforcesTheProfileMode(t *testing.T) {
	t.Run("the service shape is group-readable", func(t *testing.T) {
		asLinux(t)
		theServiceShapeIsGroupReadable(t)
	})

	t.Run("the user-session shape is not", func(t *testing.T) {
		// NO asLinux HERE, and that is the point rather than an omission: the
		// user-session paths come from THIS machine's user config directory, so a
		// generation naming another platform is one Generate refuses. Faking the
		// platform for this half was simulating something billet cannot do.
		theUserSessionShapeIsPrivate(t)
	})
}

func theServiceShapeIsGroupReadable(t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet.yaml")

	// A valid-YAML seed whose state dir matches the new generation's: --force
	// over unreadable YAML is refused fail-closed (its own test), and a
	// differing state dir would trip the identity machinery this test is not
	// about.
	if err := os.WriteFile(path, []byte("server:\n  state_dir: /var/lib/billet/server\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--profile", "local-service", "--force",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("init --force: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("--force over a 0600 file left mode %o, want the enforced 0640", got)
	}
}

func theUserSessionShapeIsPrivate(t *testing.T) {
	t.Helper()

	localPath := filepath.Join(t.TempDir(), "billet.yaml")
	if err := cmdInit(t.Context(), []string{
		"--config", localPath, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("a user-session config has mode %o, want 0600", got)
	}
}

// THE SERVICE PROFILE EXISTS WHERE BILLET SHIPS SERVICES, AND NOWHERE ELSE:
// systemd units on Linux, launch agents on macOS. Any other platform has neither,
// so the flag is refused there by name rather than writing a file whose every
// instruction is for a manager that is not there.
func TestInitRefusesLocalServiceWhereBilletShipsNoServices(t *testing.T) {
	prev := hostOS
	hostOS = "plan9"
	t.Cleanup(func() { hostOS = prev })

	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--profile", "local-service",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	})
	if err == nil {
		t.Fatal("local-service was not refused on a platform billet ships no services for")
	}
	if !strings.Contains(err.Error(), "--profile") {
		t.Errorf("the refusal does not name --profile: %v", err)
	}
}

// AND macOS IS NOT ONE OF THEM, which is the half that closed a loop.
//
// `billet local up` refuses a config that is not at the service path and tells
// the operator to "generate one there with `billet init --profile
// local-service`". While this command refused a Mac, that was two commands each
// pointing at the other and no way through — every guided macOS path ended
// there.
func TestInitAcceptsLocalServiceOnAMac(t *testing.T) {
	prev := hostOS
	hostOS = "darwin"
	t.Cleanup(func() { hostOS = prev })

	path := filepath.Join(t.TempDir(), "billet.yaml")

	err := cmdInit(t.Context(), []string{
		"--config", path,
		"--org", "acme", "--profile", "local-service",
		"--provider", "docker",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	})
	if err != nil {
		t.Fatalf("local-service was refused on a Mac, which is where the launch agents run: %v",
			err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the generated config: %v", err)
	}

	// AND IT WROTE THE macOS SHAPE, not the Linux one. Generating /var/lib paths
	// on a Mac would produce a config that loads and that the agents cannot use.
	if !strings.Contains(string(body), "/usr/local/var/lib/billet") {
		t.Errorf("the generated config does not use the macOS service paths:\n%s", body)
	}

	// EVERY /var/lib/billet MUST BE THE macOS ONE. A plain Contains check here
	// passes trivially and fails misleadingly, because the macOS path
	// /usr/local/var/lib/billet CONTAINS the Linux path as a substring -- the
	// same collision that made an assertion about `disable` match
	// `print-disabled` two files over.
	for i := 0; ; {
		j := strings.Index(string(body)[i:], "/var/lib/billet")
		if j < 0 {
			break
		}

		at := i + j
		if !strings.HasSuffix(string(body)[:at], "/usr/local") {
			t.Errorf("the generated config carries a Linux service path at offset %d:\n%s",
				at, body)
		}

		i = at + len("/var/lib/billet")
	}

	// AND IT IS 0600, NOT THE 0640 THE SERVICE SHAPE USES ON LINUX. That mode
	// exists so the `billet` group can read the file; on a Mac there is no such
	// group, so it widens the config for nobody.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("a macOS local-service config has mode %o, want 0600 — there is no service "+
			"group to read it", got)
	}

	// AND IT DESCRIBES THE MANAGER THIS MACHINE HAS. The paths were made
	// platform-derived before the comments around them were, so a Mac operator
	// read /usr/local paths introduced as "the packaged systemd units
	// (`systemctl enable --now billet-server billet-node`)" and a lock_dir
	// explained as a systemd RuntimeDirectory. The file is the reference an
	// operator reaches for, and every one of those sends them looking for
	// something that is not on their machine.
	//
	// Comments describing systemd's OWN behaviour are correct and are not
	// generated for a Mac at all, so a blanket check is the right one here.
	for _, absent := range []string{"systemd", "systemctl", "RuntimeDirectory"} {
		if strings.Contains(string(body), absent) {
			t.Errorf("the generated macOS config explains itself with %q:\n%s", absent, body)
		}
	}
}

// AND THE FIRST THING A MAC OPERATOR IS TOLD TO RUN MUST EXIST ON A MAC.
//
// The generation was made platform-derived before the sentences printed beside
// it were, so `init` wrote /usr/local paths for launchd and then said to
// `systemctl enable --now billet-server billet-node`, to install a package whose
// postinstall creates a `billet` group, and to chmod a key at
// /etc/billet/app-private-key.pem — a directory that is neither present on a Mac
// nor where the config it just wrote points. Every one of those is a first
// instruction that cannot be followed, and an operator who follows them anyway
// is being sent to look for a mistake that is not there.
//
// Asserted through printInitNext with the Linux case beside it, because the
// remedies that DO exist on Linux must survive: an implementation that simply
// stopped printing them would pass a macOS-only test.
func TestTheServiceNextStepsNameCommandsThatExistOnThisPlatform(t *testing.T) {
	const cfg = "/somewhere/else/billet.yaml"

	prev := hostOS
	t.Cleanup(func() { hostOS = prev })

	hostOS = "darwin"

	mac := capture(t, func() {
		printInitNext(cfg, initconfig.Params{
			Org: "acme", Provider: config.ProviderDocker,
			Profile: initconfig.ProfileLocalService,
		}, true)
	})

	hostOS = "linux"

	linux := capture(t, func() {
		printInitNext(cfg, initconfig.Params{
			Org: "acme", Provider: config.ProviderDocker,
			Profile: initconfig.ProfileLocalService,
		}, true)
	})

	// `billet local up` is what BOTH are told to run: it is the command that
	// proves the credential before starting a control plane on somebody's
	// organization and enables only what it proved, which neither manager's own
	// verb does.
	for name, out := range map[string]string{"macOS": mac, "Linux": linux} {
		if !strings.Contains(out, "billet local up") {
			t.Errorf("the %s next steps never name `billet local up`:\n%s", name, out)
		}
	}

	// THE macOS HALF NAMES NOTHING THAT IS NOT THERE.
	// "Install the billet package" is deliberately NOT in this list: it is
	// printed by serviceOwnership, never by printInitNext, so asserting it absent
	// here is decoration no production change can turn red. It is asserted where
	// it is actually emitted, in the ownership test below.
	for _, absent := range []string{
		"systemctl",
		"chown root:" + initconfig.ServiceGroup,
	} {
		if strings.Contains(mac, absent) {
			t.Errorf("the macOS next steps name %q, which does not exist on a Mac:\n%s",
				absent, mac)
		}
	}

	// AND EVERY /etc/billet IN IT IS THE macOS ONE. A plain Contains check here
	// fails on CORRECT output, because /usr/local/etc/billet contains the Linux
	// path -- the same collision as the /var/lib/billet scan above.
	for i := 0; ; {
		j := strings.Index(mac[i:], "/etc/billet")
		if j < 0 {
			break
		}

		at := i + j
		if !strings.HasSuffix(mac[:at], "/usr/local") {
			t.Errorf("the macOS next steps name a Linux service path at offset %d:\n%s", at, mac)
		}

		i = at + len("/etc/billet")
	}

	// AND IT NAMES THE KEY WHERE THIS CONFIG ACTUALLY POINTS. The old text was a
	// hardcoded literal, so it stayed correct for exactly one platform.
	if !strings.Contains(mac, initconfig.ServiceKeyPathFor("darwin")) {
		t.Errorf("the macOS next steps do not name the App key at %s:\n%s",
			initconfig.ServiceKeyPathFor("darwin"), mac)
	}

	// THE LINUX HALF STILL CARRIES ITS OWN REMEDIES. There is a service account
	// there, so the handover is real and dropping it would leave a config the
	// unit cannot read with nothing said about why.
	if !strings.Contains(linux, "chown root:"+initconfig.ServiceGroup) {
		t.Errorf("the Linux next steps lost the group handover:\n%s", linux)
	}

	// AND THE KEY IT NAMES IS THE LINUX ONE — asserted as a scan, because
	// `Contains("/etc/billet/app-private-key.pem")` is satisfied by the macOS
	// path, which contains it. That collision is the same one the macOS half
	// above is written around, and writing it into the opposite direction three
	// assertions later is how it keeps coming back: a ServiceKeyPathFor that
	// returned the macOS answer for every platform passed it.
	if !strings.Contains(linux, initconfig.ServiceKeyPathFor("linux")) {
		t.Errorf("the Linux next steps do not name the App key at %s:\n%s",
			initconfig.ServiceKeyPathFor("linux"), linux)
	}

	for i := 0; ; {
		j := strings.Index(linux[i:], "/etc/billet")
		if j < 0 {
			break
		}

		at := i + j
		if strings.HasSuffix(linux[:at], "/usr/local") {
			t.Errorf("the Linux next steps name a macOS service path at offset %d:\n%s",
				at, linux)
		}

		i = at + len("/etc/billet")
	}
}

// AND SO MUST THE THREE OTHER PLACES `init` SPEAKS ABOUT THE SERVICE SHAPE.
//
// The next-steps function was fixed and tested first, and that left three
// siblings printing systemd's vocabulary on a Mac — each on a path a real
// operator reaches:
//
//   - printInitNextFor with a CARRIED identity, which is every re-run after
//     `github-app create` has filled the App in. It said `systemctl restart`.
//   - the moved-key note, which is the instruction for moving the credential
//     GitHub issues exactly once. It said `install -o billet -g billet`, and
//     `install` fails with `invalid user` rather than doing anything.
//   - the beside-write note, which said `chown root:billet`. An operator who
//     "fixes" that with sudo makes the config unreadable to an agent that runs
//     as them.
//
// Driven through cmdInit rather than the printers, because what made these
// survive a fixed sibling is that nothing reached them at all.
func TestEveryServiceInstructionInitPrintsExistsOnItsPlatform(t *testing.T) {
	prev := hostOS
	t.Cleanup(func() { hostOS = prev })

	// A carried identity whose key path the profile is about to move, which is
	// what makes both the carried-identity guidance and the moved-key note fire.
	seed := "github:\n  org: acme\n  app_id: 7\n  installation_id: 9\n" +
		"  private_key_path: /home/someone/key.pem\n"

	run := func(t *testing.T, goos string) (string, string) {
		t.Helper()

		hostOS = goos

		path := filepath.Join(t.TempDir(), "billet.yaml")
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}

		var initErr error

		var out string

		notes := captureStderr(t, func() {
			out = capture(t, func() {
				initErr = cmdInit(t.Context(), []string{
					"--config", path, "--org", "acme", "--profile", "local-service",
					"--provider", "docker",
					"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
				})
			})
		})
		if initErr != nil {
			t.Fatalf("init: %v", initErr)
		}

		return out, notes
	}

	t.Run("darwin", func(t *testing.T) {
		out, notes := run(t, "darwin")
		all := out + notes

		for _, absent := range []string{
			"systemctl",
			"install -o " + initconfig.ServiceGroup,
			"chown root:" + initconfig.ServiceGroup,
		} {
			if strings.Contains(all, absent) {
				t.Errorf("a Mac operator is told to run %q, which cannot work there:\n%s",
					absent, all)
			}
		}

		// AND IS TOLD SOMETHING THAT DOES. An implementation that printed nothing
		// at all would satisfy every absence above.
		if !strings.Contains(all, "billet local") {
			t.Errorf("the macOS run names no lifecycle command at all:\n%s", all)
		}
	})

	t.Run("linux", func(t *testing.T) {
		out, notes := run(t, "linux")
		all := out + notes

		// THE REMEDIES THAT ARE REAL THERE SURVIVE, or a guard that dropped them
		// unconditionally would pass the darwin subtest and lose the handover the
		// packaged units depend on.
		if !strings.Contains(all, "install -o "+initconfig.ServiceGroup) {
			t.Errorf("the Linux moved-key note lost its ownership:\n%s", all)
		}
	})

	// AND THE CARRIED-IDENTITY GUIDANCE, WHICH THE RUN ABOVE DOES NOT REACH.
	//
	// A seeded config is not pristine init output, so cmdInit takes the
	// beside-write branch and returns before printInitNextFor — which the
	// mutation run is what proved: putting `systemctl restart` back survived
	// every assertion above. Driven directly, because the state that reaches it
	// is "the existing file IS pristine init output and carries an App", and
	// constructing that through the CLI makes the test about PlanReRun instead.
	t.Run("carried identity", func(t *testing.T) {
		for _, goos := range []string{"darwin", "linux"} {
			hostOS = goos

			out := capture(t, func() {
				printInitNextFor("/etc/billet/billet.yaml", initconfig.Params{
					Org: "acme", Provider: config.ProviderDocker,
					Profile: initconfig.ProfileLocalService,
				}, true, true)
			})

			if !strings.Contains(out, "billet local") {
				t.Errorf("the %s re-run guidance names no lifecycle command:\n%s", goos, out)
			}

			if goos == "darwin" && strings.Contains(out, "systemctl") {
				t.Errorf("a Mac re-run is told to run systemctl:\n%s", out)
			}
		}
	})
}

// NOTHING FROM THE ENVIRONMENT SHAPES A COMMAND THE OPERATOR WILL PASTE.
//
// This assertion replaced three, and the three are why it exists. Two versions
// of this remedy tried to NAME the operator from SUDO_USER, and each was wrong
// in its own way: shellArg closes command injection but not option injection,
// since `-` is an ordinary shell-safe character, so `--reference=/etc/shadow`
// was a valid chown OPTION rather than an owner; and validating the name still
// let a uid-0 account called something other than `root` through, while a direct
// root shell has nothing to name at all.
//
// The property that closes the whole class is not a better guess. It is that
// there is no guess: a uid needs no validation, cannot be an option, and is
// this process's own — which IS the operator whenever it is not root.
func TestNoEnvironmentValueReachesTheChownAdvice(t *testing.T) {
	prev := effectiveUID
	t.Cleanup(func() { effectiveUID = prev })

	const dir = "/usr/local/etc/billet"

	effectiveUID = func() int { return 4242 }

	for _, value := range []string{
		"-R",                      // a chown flag
		"--reference=/etc/shadow", // a chown flag that takes a value
		"; rm -rf /",              // a command separator
		"$(id -un)",               // a command substitution
		"root",                    // the account being refused
		"0",                       // its uid
		"a'b",                     // the quote shellArg itself uses
		// AND AN ORDINARY ONE, which is the case a list of hostile values
		// cannot cover: reintroducing the environment lookup for values that
		// merely LOOK fine is exactly how this came back twice, and every
		// hostile entry above would still have been rejected by it.
		"someone",
		"501",
	} {
		t.Setenv("SUDO_USER", value)
		t.Setenv("SUDO_UID", value)
		t.Setenv("USER", value)
		t.Setenv("LOGNAME", value)

		// EQUALITY, which proves both halves at once and cannot be tripped by
		// its own fixture. Asserting the value ABSENT and the command PRESENT
		// separately left a trap: a list entry that collided with the uid below
		// would fail the absence check against correct output, and the next
		// person to add "4242" to the list would be debugging the test. If the
		// advice IS exactly this, nothing else is in it.
		//
		// The whole command, not just the owner: the directory does not exist in
		// the case this remedy is for, so an advice that lost `mkdir -p` is one
		// the operator cannot follow.
		want := "sudo mkdir -p " + dir + " && sudo chown 4242 " + dir
		if advice := strings.TrimSpace(chownAdvice(dir)); advice != want {
			t.Errorf("with %q in the environment the advice is\n%s\nwant\n%s", value, advice, want)
		}
	}
}

// AND AS ROOT IT PRINTS PROSE, NOT A COMMAND TO PASTE WHERE IT IS READ.
//
// `$(id -un)` is correct in the operator's own shell and resolves to root in
// this one, so the sentence has to move them first. A previous version printed
// `sudo chown 0 …` here, which is the exact state the refusal exists to get out
// of, in the message explaining why it is wrong.
func TestTheRootAdviceMovesTheOperatorBeforeNamingAnybody(t *testing.T) {
	prev := effectiveUID
	t.Cleanup(func() { effectiveUID = prev })

	effectiveUID = func() int { return 0 }

	advice := chownAdvice("/usr/local/etc/billet")

	for _, wrong := range []string{"chown root", "chown 0 "} {
		if strings.Contains(advice, wrong) {
			t.Errorf("the root advice hands the directory back to root (%q):\n%s", wrong, advice)
		}
	}

	if !strings.Contains(advice, "Leave the root shell") {
		t.Errorf("the root advice does not say to stop being root first:\n%s", advice)
	}

	// AND THE WHOLE COMMAND, with `$(id -un)` specifically. Any other owner is
	// a guess about who the operator is, which is the thing this branch exists
	// to stop making — and asserting only "sudo mkdir -p" left that green.
	want := `  sudo mkdir -p /usr/local/etc/billet && sudo chown "$(id -un)" /usr/local/etc/billet`
	if !strings.Contains(advice, want) {
		t.Errorf("the root advice is not %q:\n%s", want, advice)
	}
}

// BOTH WRITE PATHS ROUTE THEIR PERMISSION FAILURE THROUGH THE REMEDY.
//
// Testing macServiceDirRemedy directly proves the function and nothing about
// whether anything calls it — and one branch did not. `writePath` becomes
// `<config>.new` for an existing file this command will not merge, and that
// branch is chosen by `writePath != *cfgPath`, so the remedy added to the other
// arm could never see it: on a stock Mac with an edited config in a directory
// that is not yours, mkdir succeeds (the directory is there), the exclusive
// create fails, and the bare permission error comes back exactly as before.
//
// Driven through cmdInit against a real unwritable directory, with the service
// directory pointed at it — which is the only way to reach the remedy, since it
// is scoped to that directory and a test cannot own /usr/local.
func TestBothWritePathsExplainAPermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this test makes a directory unwritable with")
	}

	prevOS, prevDir := hostOS, serviceConfigDir
	t.Cleanup(func() { hostOS, serviceConfigDir = prevOS, prevDir })

	hostOS = "darwin"

	for _, tc := range []struct {
		name string
		seed string
	}{
		// No existing file: the config is written in place, and the failure is
		// the staging create inside commitConfig.
		{name: "in place"},
		// An existing file this command will not merge: writePath becomes
		// <config>.new, and the failure is that branch's exclusive create.
		{name: "beside", seed: "server:\n  listen: 127.0.0.1:9999\n# edited by hand\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "billet.yaml")

			if tc.seed != "" {
				if err := os.WriteFile(path, []byte(tc.seed), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}

			serviceConfigDir = func() string { return dir }

			// UNWRITABLE ONLY NOW, so the seed above could be written.
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatalf("chmod: %v", err)
			}

			t.Cleanup(func() {
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Errorf("restore %s: %v", dir, err)
				}
			})

			err := cmdInit(t.Context(), []string{
				"--config", path, "--org", "acme", "--profile", "local-service",
				"--provider", "docker",
				"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
			})
			if err == nil {
				t.Fatal("a config was written into an unwritable directory")
			}

			// THE RELATIONSHIP, NOT THE PHRASE. "root-owned on a stock Mac"
			// appears in both the correct message and the wrong one it replaced
			// — which said the DIRECTORY is root-owned, when the common case is
			// that the directory does not exist and cannot be created because
			// /usr/local above it is. An operator can check that claim and find
			// it false.
			if !strings.Contains(err.Error(),
				dir+" is under /usr/local, which is root-owned on a stock Mac") {
				t.Errorf("the %s write failed with no remedy: %v", tc.name, err)
			}

			if strings.Contains(err.Error(), dir+" is root-owned") {
				t.Errorf("the %s remedy claims a directory's ownership rather than its "+
					"parent's: %v", tc.name, err)
			}
		})
	}
}

// THE /usr/local ADVICE IS GIVEN ONLY WHEN THE DIRECTORY IS ACTUALLY THE
// PROBLEM.
//
// commitConfig wraps staging, writing, syncing, closing AND renaming, so
// translating any permission error out of it into "this directory is root-owned"
// claims something the error does not say: a rename can return EPERM for an
// immutable destination or an ACL, having already proved the directory writable.
// The remedy asks the directory instead of inferring from which call failed.
func TestTheStockMacAdviceIsOnlyGivenWhenTheDirectoryIsTheProblem(t *testing.T) {
	prevOS, prevWritable := hostOS, dirWritable
	t.Cleanup(func() { hostOS, dirWritable = prevOS, prevWritable })

	hostOS = "darwin"

	service := initconfig.ServiceConfigPathFor("darwin")

	// NOT WRITABLE: this is the situation the advice describes.
	dirWritable = func(string) bool { return false }

	err := macServiceDirRemedy(service, initconfig.ProfileLocalService, os.ErrPermission)
	if err == nil {
		t.Fatal("no remedy for a permission failure in a service directory we cannot write")
	}

	if !strings.Contains(err.Error(), "stock Mac") {
		t.Errorf("the remedy does not explain the directory: %v", err)
	}

	// WRITABLE: the permission error came from something else — the rename's
	// destination, an ACL — and a chown of this directory fixes none of it.
	dirWritable = func(string) bool { return true }

	if err := macServiceDirRemedy(service, initconfig.ProfileLocalService,
		os.ErrPermission); err != nil {
		t.Errorf("a writable directory still collected the root-ownership advice: %v", err)
	}
}

// AND A ROOT-OWNED CONFIG IS REFUSED BEFORE IT EXISTS, NOT DIAGNOSED AFTER.
//
// /usr/local is root-owned on a stock Mac, so `billet init --profile
// local-service` fails at mkdir — and the obvious response is `sudo billet
// init`, which SUCCEEDS and writes a config the launch agents cannot read,
// because they run as the operator and `billet local up` refuses root. Nothing
// downstream would name the cause: `local up` fails on config.Load with a bare
// permission error about a file it can see.
//
// The refusal comes before the mkdir, so a root run does not first leave a
// root-owned directory the operator has to undo.
//
// THE UID IS BEHIND A SEAM so this can assert the refusal rather than only its
// complement. The first version of this test could not: `Geteuid` is not
// something a test becomes, so changing the production guard to `&& false` left
// the whole thing green, and on a root CI host it skipped entirely.
func TestTheServiceProfileRefusesToWriteAConfigAsRoot(t *testing.T) {
	prevOS, prevUID := hostOS, effectiveUID
	t.Cleanup(func() { hostOS, effectiveUID = prevOS, prevUID })

	hostOS = "darwin"
	effectiveUID = func() int { return 0 }

	dir := filepath.Join(t.TempDir(), "etc", "billet")

	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(dir, "billet.yaml"),
		"--org", "acme", "--profile", "local-service", "--provider", "docker",
		"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
	})
	if err == nil {
		t.Fatal("a root run wrote a service config the launch agents could not read")
	}

	for _, want := range []string{"as root", "billet local up", "sudo chown"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the root refusal does not mention %q: %v", want, err)
		}
	}

	// AND IT LEFT NOTHING BEHIND. Refusing after the mkdir would create a
	// root-owned directory the operator then has to undo — which is most of the
	// problem being refused.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("the refused root run created %s (stat %v)", dir, statErr)
	}

	// AND IT IS SCOPED. A Linux service profile has a service account to own the
	// file, an emission writes nothing at all, and the user-session profile is
	// not the launch agents' — a refusal that fired on any of those would break
	// package installs and CI.
	hostOS = "linux"

	linuxPath := filepath.Join(t.TempDir(), "billet.yaml")
	if err := cmdInit(t.Context(), []string{
		"--config", linuxPath, "--org", "acme", "--profile", "local-service",
		"--provider", "docker",
		"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
	}); err != nil {
		t.Errorf("a root Linux service-profile run was refused: %v", err)
	}

	hostOS = "darwin"

	userSession := filepath.Join(t.TempDir(), "billet.yaml")
	if err := cmdInit(t.Context(), []string{
		"--config", userSession, "--org", "acme", "--provider", "docker",
		"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
	}); err != nil {
		t.Errorf("a root macOS user-session run was refused: %v", err)
	}

	var emitErr error

	_ = captureStderr(t, func() {
		_ = capture(t, func() {
			emitErr = cmdInit(t.Context(), append(
				emitAnsibleArgs(filepath.Join(t.TempDir(), "billet.yaml")),
				"--max-vcpu", "8", "--max-memory", "32GiB"))
		})
	})
	if emitErr != nil {
		t.Errorf("a root ansible emission was refused, and it writes nothing: %v", emitErr)
	}
}

// AND AN ORDINARY RUN IS NOT REFUSED, nor does an unrelated permission failure
// collect advice about /usr/local.
//
// Kept beside the refusal because a guard that fired unconditionally would pass
// every assertion up there.
func TestTheServiceProfileAcceptsAnOrdinaryMacRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this asserts the non-root path; as root the 0500 directory below is writable")
	}

	prev := hostOS
	t.Cleanup(func() { hostOS = prev })

	hostOS = "darwin"

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--profile", "local-service",
		"--provider", "docker",
		"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("an ordinary macOS service-profile run was refused: %v", err)
	}

	// AND THE PERMISSION REMEDY IS SCOPED TO THE SERVICE DIRECTORY. It says
	// "/usr/local is root-owned on a stock Mac", which is a claim about
	// /usr/local and not about whatever an operator passed to --config — so an
	// EACCES on some other path must NOT collect that advice.
	//
	// Driven with a real unwritable directory rather than a fake error, because
	// the scoping compares the path and the fake would have to supply one.
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Restored so t.TempDir's own cleanup can remove it; a 0500 directory would
	// otherwise fail the removal and report it as a test error.
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o700); err != nil {
			t.Errorf("restore %s: %v", locked, err)
		}
	})

	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(locked, "sub", "billet.yaml"),
		"--org", "acme", "--profile", "local-service", "--provider", "docker",
		"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
	})
	if err == nil {
		t.Fatal("a config was written into an unwritable directory")
	}

	if strings.Contains(err.Error(), "stock Mac") {
		t.Errorf("an unrelated path collected the /usr/local advice: %v", err)
	}
}

// AND THE KEY THE GUIDANCE NAMES IS THE KEY THE CONFIG NAMES.
//
// Asserting the printed path against ServiceKeyPathFor proves the guidance reads
// the accessor; it does NOT prove the generated YAML does. Those were two
// platform branches over the same constants, so a one-line change to either would
// have left every other test green while init told an operator to secure a
// credential at a path the deployment does not read — for the one credential
// GitHub issues exactly once and never reissues.
//
// Both platforms, because a single-platform version of this passes against a
// generator that hardcodes that platform's answer.
func TestTheGuidanceAndTheConfigNameOneAppKey(t *testing.T) {
	prev := hostOS
	t.Cleanup(func() { hostOS = prev })

	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			hostOS = goos

			path := filepath.Join(t.TempDir(), "billet.yaml")
			if err := cmdInit(t.Context(), []string{
				"--config", path, "--org", "acme", "--profile", "local-service",
				"--provider", "docker",
				"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
			}); err != nil {
				t.Fatalf("init: %v", err)
			}

			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the generated config: %v", err)
			}

			configured := configuredKeyPathOf(string(body))

			// EQUALITY, NOT CONTAINMENT — and this is the FOURTH time the same
			// collision has been written into this tree. `Contains(out, configured)`
			// is satisfied on darwin by a regressed config: `configured` becomes
			// /etc/billet/app-private-key.pem, the guidance still says
			// /usr/local/etc/billet/app-private-key.pem, and the macOS path
			// CONTAINS the Linux one, so the assertion passes over exactly the
			// divergence it exists to catch.
			if want := initconfig.ServiceKeyPathFor(goos); configured != want {
				t.Errorf("the generated config points at %q, want %q", configured, want)
			}

			out := capture(t, func() {
				printInitNext(path, initconfig.Params{
					Org: "acme", Provider: config.ProviderDocker,
					Profile: initconfig.ProfileLocalService,
				}, true)
			})

			// ANCHORED TO THE WHOLE SENTENCE, so the guidance cannot satisfy this
			// with a longer path that happens to end in the configured one.
			if !strings.Contains(out, "The App key at "+configured+" must") {
				t.Errorf("the guidance does not name the key the config points at (%s):\n%s",
					configured, out)
			}
		})
	}
}

// AND THE OWNERSHIP NOTE IS SILENT WHERE THERE IS NO ACCOUNT TO HAND ANYTHING
// TO.
//
// serviceOwnership prints a NOTE naming `chown root:billet` and the billet
// package's postinstall when it cannot set the group itself -- which on a Mac is
// always, because no such group exists. Both halves of that note are
// unfollowable there: the package is a .deb and .rpm, and a launch agent reads
// the file as the operator who just wrote it.
//
// BOTH HALVES ARE DETERMINISTIC ON EVERY HOST, which the first version of this
// test was not: it skipped the Linux assertion where a `billet` group happened to
// exist, and on such a host a guard that returned unconditionally would satisfy
// the macOS half and skip the only assertion that would have caught it.
//
// The fix is the path. `serviceOwnership` reaches the note whenever it cannot set
// the group, and a name that does not exist makes the chown fail with ENOENT
// regardless of the account database or who is running the test — so the ONLY
// difference between the two calls below is the platform.
func TestTheOwnershipNoteIsSilentWhereTheServicesRunAsTheOperator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	prev := hostOS
	t.Cleanup(func() { hostOS = prev })

	hostOS = "darwin"

	if out := capture(t, func() { serviceOwnership(path) }); out != "" {
		t.Errorf("the ownership note speaks on a Mac, where there is no service account:\n%s", out)
	}

	// THE LINUX HALF STILL SPEAKS, or a guard that returned unconditionally would
	// pass the assertion above and silently drop the one remedy that is real.
	hostOS = "linux"

	out := capture(t, func() { serviceOwnership(path) })
	for _, want := range []string{"chown root:" + initconfig.ServiceGroup, "billet package"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Linux ownership note lost %q:\n%s", want, out)
		}
	}
}

// --listen REACHES THE FILE, at both ends, through the CLI path — not only
// through initconfig.Generate directly.
func TestInitListenFlagReachesBothEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--listen", "127.0.0.1:7901",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"listen: 127.0.0.1:7901", "server_addr: 127.0.0.1:7901"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the written config lacks %q", want)
		}
	}
}

// loadWrittenConfig fills in the App identity `github-app create --config`
// would have written and then LOADS the file, so a test reads what init decided
// rather than the placeholder it leaves and rather than its own YAML reader.
//
// The generated file leaves the App ids at zero and config.Load refuses those,
// so every test that wants the loaded config repeated this. One caller keeps its
// own copy deliberately: the ceiling table puts the whole generated body in its
// failure message, which is the only way to read what a machine of that size
// produced.
func loadWrittenConfig(t *testing.T, path string) *config.Config {
	t.Helper()

	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: 1, InstallationID: 2,
		PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
	}); err != nil {
		t.Fatalf("filling in the app: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the config billet generated does not load: %v", err)
	}

	return cfg
}

// `billet init --provider tart` WRITES A CONFIG A MAC CAN ACTUALLY RUN.
//
// It refused to for a long time, on the grounds that generating one meant naming
// a guest image and there was none billet could name — the published cirruslabs
// Ubuntu image was measured to carry neither the Actions runner nor Docker, so
// nothing would run in it. Two images have since been proved against real jobs
// (an Xcode build on macOS, a Docker build and a service container on arm64
// Linux), which is what made the refusal stale.
//
// The whole file is asserted through config.Load rather than by grepping, so
// this cannot pass against a generation that merely mentions the right words.
func TestInitTartWritesAConfigThatLoads(t *testing.T) {
	onAReferenceMac(t)

	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--provider", "tart", "--profile", "local-service",
		"--node-name", "mac-mini-1",
		"--guest-os", "macos", "--guest-os", "linux",
	}); err != nil {
		t.Fatalf("`billet init --provider tart`: %v", err)
	}

	cfg := loadWrittenConfig(t, path)

	if cfg.Node.Provider != config.ProviderTart {
		t.Fatalf("node.provider = %q, want tart", cfg.Node.Provider)
	}
	if cfg.Node.Name != "mac-mini-1" {
		t.Errorf("node.name = %q, want the name the macOS tier pins", cfg.Node.Name)
	}
	if len(cfg.Tiers) < 2 {
		t.Fatalf("both guest kinds were asked for and %d tier(s) were written", len(cfg.Tiers))
	}
}

// onAReferenceMac points every seam a tart generation reads at the machine such
// a config describes.
//
// THE PLATFORM, because CI is Linux and a tart config is refused anywhere but
// Apple silicon — without it every test here would assert that refusal instead of
// the thing it was written for. Both halves of the platform, since an Intel Mac
// is refused too.
//
// AND THE CAPACITY, which CI found and this machine could not. `billet init`
// MEASURES the host, so a generation asking for both guest kinds fits a 12-core
// Mac and does not fit GitHub's 4-vCPU runner — the macOS tier takes what there
// is and the Linux one is correctly refused. The numbers are the reference M2
// Max's, so these tests and internal/initconfig's reason about one ceiling.
func onAReferenceMac(t *testing.T) {
	t.Helper()

	prevOS, prevArch := hostOS, hostGOARCH
	prevCapacity := detectHostCapacity

	hostOS, hostGOARCH = "darwin", "arm64"
	detectHostCapacity = func() (int, config.ByteSize, error) { return 12, 32 * config.GiB, nil }

	t.Cleanup(func() {
		hostOS, hostGOARCH = prevOS, prevArch
		detectHostCapacity = prevCapacity
	})
}

// A TART CONFIG IS REFUSED ANYWHERE BUT THE MACHINE IT DESCRIBES.
//
// A NOTE would be the firecracker branch's answer, and the difference is that
// firecracker HAS a cross-machine path — `--emit ansible` renders a Linux host's
// config from anywhere — while the role is Linux-only, so there is none to a Mac.
// What is left is a file wrong in three ways at once: the ceiling is measured
// here, the node name comes from this hostname, and the paths are this
// platform's. A generation that must be edited in three places before it runs is
// the trap this generator exists to remove.
func TestInitRefusesATartConfigOffAppleSilicon(t *testing.T) {
	for _, host := range []struct {
		goos, arch string
		profile    string
	}{
		{goos: "linux", arch: "amd64", profile: "local-service"},
		{goos: "linux", arch: "arm64", profile: "local-service"},
		// An Intel Mac is the case a goos-only check would let through, and tart
		// does not run on one.
		{goos: "darwin", arch: "amd64", profile: "local-service"},
		// AND THE DEFAULT PROFILE, whose paths are neither this platform's service
		// ones nor a Mac's launch agents': they are under this account's config
		// directory, and it configures no lock at all.
		{goos: "linux", arch: "amd64", profile: "local"},
	} {
		t.Run(host.goos+"/"+host.arch+"/"+host.profile, func(t *testing.T) {
			prevOS, prevArch := hostOS, hostGOARCH
			hostOS, hostGOARCH = host.goos, host.arch
			t.Cleanup(func() { hostOS, hostGOARCH = prevOS, prevArch })

			path := filepath.Join(t.TempDir(), "billet.yaml")

			err := cmdInit(t.Context(), []string{
				"--config", path, "--org", "acme", "--provider", "tart",
				"--profile", host.profile,
			})
			if err == nil {
				t.Fatal("a tart config was written on a machine that cannot run one")
			}

			// THE PLATFORM AND THE REASON. "needs a Mac" alone would leave an
			// operator copying the file across and finding it wrong twice over.
			for _, want := range []string{
				host.goos + "/" + host.arch, "MEASURED here", "this hostname",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}

			// AND THE HOSTNAME CLAUSE GOES WHEN THE OPERATOR SUPPLIED THE NAME,
			// because it is then false: billet reads nothing off this machine for
			// it. The refusal is still correct; one of its reasons would not be.
			named := cmdInit(t.Context(), []string{
				"--config", filepath.Join(t.TempDir(), "billet.yaml"),
				"--org", "acme", "--provider", "tart", "--profile", "local-service",
				"--node-name", "mac-mini-1",
			})
			if named == nil {
				t.Fatal("a tart config was written on a machine that cannot run one")
			}
			if strings.Contains(named.Error(), "this hostname") {
				t.Errorf("the refusal credits this machine's hostname for a name the "+
					"operator gave: %v", named)
			}

			// AND THE LAUNCH-AGENT PATH CLAUSE ONLY WHERE IT IS TRUE, which is
			// narrower than "off darwin" in two directions: an Intel Mac's SERVICE
			// paths ARE the /usr/local ones, and a user-session generation has no
			// launch-agent paths on any platform. Both are refusals that are
			// correct for a reason the operator can check and find false.
			mentionsPaths := strings.Contains(err.Error(), "/usr/local")
			wantPaths := host.goos != "darwin" && host.profile == "local-service"

			if mentionsPaths != wantPaths {
				t.Errorf("the refusal on %s/%s %s %s the launch-agent path clause: %v",
					host.goos, host.arch, host.profile,
					map[bool]string{true: "carries", false: "omits"}[mentionsPaths], err)
			}

			if _, statErr := os.Stat(path); statErr == nil {
				t.Error("the refusal still wrote the file")
			}
		})
	}
}

// THE FLAGS THAT ONLY MEAN SOMETHING TO A MAC ARE REFUSED ELSEWHERE.
//
// Silently ignored, they read as configured and do nothing — an operator who
// passed --node-name to a docker init would believe they had named the host.
// Refused by PRESENCE, so an empty value is caught the same as a real one.
func TestInitRefusesTartOnlyFlagsOnAnotherBackend(t *testing.T) {
	for _, flag := range []string{"--guest-os", "--node-name", "--macos-image", "--linux-image"} {
		t.Run(flag, func(t *testing.T) {
			err := cmdInit(t.Context(), []string{
				"--config", filepath.Join(t.TempDir(), "billet.yaml"),
				"--org", "acme", "--provider", "docker",
				"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
				flag, "",
			})
			if err == nil {
				t.Fatalf("%s was accepted on a docker config, where nothing reads it", flag)
			}
			if !strings.Contains(err.Error(), flag) {
				t.Errorf("the refusal does not name %s: %v", flag, err)
			}
		})
	}
}

// AN INVALID GUEST KIND IS REPORTED AS ONE, WHATEVER THIS MACHINE IS CALLED.
//
// The CLI resolves this host's name from the hostname, and doing that BEFORE
// establishing that the request is meaningful meant `--guest-os typo` on a Mac
// whose hostname is not a legal node name reported the node name instead: the
// same invalid input getting a different error depending on an unrelated property
// of the machine it was typed on.
func TestInitReportsAnInvalidGuestKindRatherThanTheHostname(t *testing.T) {
	onAReferenceMac(t)

	restore := hostName
	t.Cleanup(func() { hostName = restore })
	hostName = func() (string, error) { return "Junior's MacBook Pro.local", nil }

	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--provider", "tart", "--profile", "local-service",
		"--guest-os", "freebsd",
	})
	if err == nil {
		t.Fatal("a config was generated for a guest kind this backend cannot run")
	}

	if !strings.Contains(err.Error(), "--guest-os") {
		t.Errorf("the refusal does not name the flag that is wrong: %v", err)
	}
	if strings.Contains(err.Error(), "--node-name") {
		t.Errorf("an invalid guest kind was reported as a hostname problem: %v", err)
	}
}

// A HOSTNAME BILLET CANNOT READ IS THE SAME PROBLEM AS ONE IT CANNOT USE.
//
// Both end at --node-name, and only the macOS case earns Apple's reason for it —
// the branch was added without a test, and every hostname seam in this file
// returns a string successfully, so removing the clause again would go unnoticed.
func TestInitAsksForANameWhenItCannotReadTheHostname(t *testing.T) {
	for _, tc := range []struct {
		guest   string
		wantPin bool
	}{
		{guest: "macos", wantPin: true},
		{guest: "linux", wantPin: false},
	} {
		t.Run(tc.guest, func(t *testing.T) {
			onAReferenceMac(t)

			restore := hostName
			t.Cleanup(func() { hostName = restore })
			hostName = func() (string, error) { return "", errors.New("no hostname here") }

			err := cmdInit(t.Context(), []string{
				"--config", filepath.Join(t.TempDir(), "billet.yaml"),
				"--org", "acme", "--provider", "tart", "--profile", "local-service",
				"--guest-os", tc.guest,
			})
			if err == nil {
				t.Fatal("a config was generated without a name billet could resolve")
			}

			if !strings.Contains(err.Error(), "--node-name") {
				t.Errorf("the refusal does not name the flag that fixes it: %v", err)
			}

			// THE READ FAILURE'S OWN SENTENCE. Deleting that branch leaves `host`
			// empty, which falls into the invalid-hostname error a few lines
			// later — and that one carries both --node-name and the pin, so
			// everything else here would still pass over a deleted branch.
			if !strings.Contains(err.Error(), "could not read this machine's name") {
				t.Errorf("the refusal does not say the hostname could not be READ: %v", err)
			}

			if got := strings.Contains(err.Error(), "two-guest limit"); got != tc.wantPin {
				t.Errorf("the refusal %s Apple's pin for a %s generation: %v",
					map[bool]string{true: "gives", false: "omits"}[got], tc.guest, err)
			}
		})
	}
}

// AN EXPLICITLY EMPTY TART FLAG IS A MISUSE, NOT AN OMISSION.
//
// The rule refuseEC2OnlyFlags already follows for `--max-vcpu 0`: `set` is what
// the operator actually passed. Defaulting `--node-name ""` back to the hostname
// answers a question they had tried to answer themselves.
func TestInitRefusesAnEmptyTartFlag(t *testing.T) {
	onAReferenceMac(t)

	for _, flag := range []string{"--node-name", "--macos-image", "--linux-image"} {
		t.Run(flag, func(t *testing.T) {
			err := cmdInit(t.Context(), []string{
				"--config", filepath.Join(t.TempDir(), "billet.yaml"),
				"--org", "acme", "--provider", "tart", "--profile", "local-service",
				"--guest-os", "macos", "--guest-os", "linux",
				"--node-name", "mac-mini-1",
				flag, "",
			})
			if err == nil {
				t.Fatalf("%s was accepted with an empty value", flag)
			}
			if !strings.Contains(err.Error(), flag+" was given with an empty value") {
				t.Errorf("the refusal does not name %s: %v", flag, err)
			}
		})
	}
}

// AN EXPLICITLY EMPTY --state-dsn-env IS NOT AN ABSENT ONE.
//
// Only this layer can tell them apart: by the time the generator sees a
// StateParams, `--state-dsn-env=` and "no flag at all" are both the empty
// string — and under the sqlite default that is exactly what saying nothing
// looks like, so the flag was accepted and silently discarded. The same rule
// --runner-group and the tart flags already follow.
func TestInitRefusesAnEmptyStateDSNEnv(t *testing.T) {
	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--provider", "docker",
		"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
		"--state-dsn-env", "",
	})
	if err == nil {
		t.Fatal("--state-dsn-env was accepted with an empty value and silently discarded")
	}
	if !strings.Contains(err.Error(), "--state-dsn-env") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
}

// AND A NAME NOTHING COULD EXPORT IS REFUSED BY ITS FLAG, not by config.Parse.
//
// Both are refusals, so a test that only asserted "it failed" would pass either
// way — what separates them is WHICH sentence the operator reads, and only one
// of the two names something they typed.
func TestInitRefusesADSNEnvNameNothingCouldExport(t *testing.T) {
	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--provider", "docker",
		"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
		"--state-backend", "postgres", "--state-dsn-env", "9-lives",
	})
	if err == nil {
		t.Fatal("--state-dsn-env accepted a name no shell could export")
	}
	if !strings.Contains(err.Error(), "--state-dsn-env") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
	if strings.Contains(err.Error(), "the generated config is not valid") {
		t.Errorf("the refusal came from config.Parse rather than from the flag check ahead "+
			"of it, so it blames a generated block the operator never typed: %v", err)
	}
}

// THE PULL SIZES ARE CLAIMED ONLY FOR THE IMAGES THEY WERE MEASURED ON.
//
// They are measurements of two specific published images. An operator who passed
// --macos-image has an image billet has never seen, and telling them it is 87GB
// is a number they can check and find false; naming a guest kind this generation
// does not have is the same mistake one step smaller.
func TestInitClaimsAnImageSizeOnlyForTheImageItMeasured(t *testing.T) {
	onAReferenceMac(t)

	run := func(t *testing.T, extra ...string) string {
		t.Helper()

		args := append([]string{
			"--config", filepath.Join(t.TempDir(), "billet.yaml"),
			"--org", "acme", "--provider", "tart", "--profile", "local-service", "--node-name", "mac-mini-1",
		}, extra...)

		var err error

		out := capture(t, func() { err = cmdInit(t.Context(), args) })
		if err != nil {
			t.Fatalf("init: %v", err)
		}

		return out
	}

	t.Run("both defaults", func(t *testing.T) {
		out := run(t, "--guest-os", "macos", "--guest-os", "linux")
		for _, want := range []string{"87GB", "11GB"} {
			if !strings.Contains(out, want) {
				t.Errorf("the guidance omits %s:\n%s", want, out)
			}
		}
	})

	t.Run("only the guest kinds it wrote", func(t *testing.T) {
		out := run(t, "--guest-os", "linux")
		if strings.Contains(out, "87GB") {
			t.Errorf("a linux-only generation was told the macOS image's size:\n%s", out)
		}
		if !strings.Contains(out, "11GB") {
			t.Errorf("the guidance omits the size of the image it did name:\n%s", out)
		}
	})

	t.Run("nothing for an image billet has never seen", func(t *testing.T) {
		out := run(t, "--guest-os", "macos", "--macos-image", "ghcr.io/acme/macos:pinned")
		if strings.Contains(out, "87GB") {
			t.Errorf("an overridden image was given the default's measured size:\n%s", out)
		}
	})
}

// AN ANSIBLE EMISSION CANNOT DESCRIBE A MAC.
//
// The emission always targets Linux, because the junioryono.billet.host role is
// Linux-only: it installs systemd units, /etc/billet and a lock under
// /run/billet/locks. A tart node is Apple hardware. Emitting one would render a
// block whose every path is for a machine that cannot run the backend it names —
// the same defect as an emission from a Mac describing the Mac.
func TestInitRefusesAnAnsibleEmissionForTart(t *testing.T) {
	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--provider", "tart", "--profile", "local-service", "--node-name", "mac-mini-1",
		"--emit", "ansible",
	})
	if err == nil {
		t.Fatal("a tart block was emitted for a role that converges Linux hosts")
	}

	for _, want := range []string{"Linux-only", "billet local up"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A NODE NAME IS AN IDENTITY, SO AN UNUSABLE HOSTNAME IS REFUSED RATHER THAN
// SANITISED INTO ONE.
//
// A stock Mac's hostname carries the name from System Settings — spaces,
// apostrophes, a .local suffix — and none of that is a legal node name. Turning
// it into one silently would name a host the operator never chose, and they
// would meet it again the first time `billet ca issue` disagreed. Reached
// through the hostName seam because a test cannot rename the machine it runs on.
func TestInitRefusesAMacOSTierWhenTheHostnameCannotBeANodeName(t *testing.T) {
	onAReferenceMac(t)

	restore := hostName
	t.Cleanup(func() { hostName = restore })
	hostName = func() (string, error) { return "Junior's MacBook Pro.local", nil }

	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--provider", "tart", "--profile", "local-service",
	})
	if err == nil {
		t.Fatal("a macOS tier was pinned to a name nothing could authorise")
	}

	for _, want := range []string{"Junior's MacBook Pro.local", "--node-name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// AND A USABLE ONE IS USED, so an operator on a tidily-named Mac passes no flag.
func TestInitTakesTheNodeNameFromAUsableHostname(t *testing.T) {
	onAReferenceMac(t)

	restore := hostName
	t.Cleanup(func() { hostName = restore })
	hostName = func() (string, error) { return "mac-mini-1", nil }

	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--provider", "tart", "--profile", "local-service",
	}); err != nil {
		t.Fatalf("`billet init --provider tart`: %v", err)
	}

	if got := loadWrittenConfig(t, path).Node.Name; got != "mac-mini-1" {
		t.Errorf("node.name = %q, want the hostname billet found usable", got)
	}
}

// A LINUX-ONLY MAC IS ASKED FOR A NAME TOO, AND THE FIRST VERSION OF THIS TEST
// SAID OTHERWISE AND WAS GREEN.
//
// Skipping the name for a host that pins nothing reads as a kindness and is not:
// a config with no node.name gets one from the machine's hostname AT LOAD, so
// Generate's own config.Parse proof refuses the same unusable name a moment
// later — naming node.name and a file billet had just written, rather than the
// flag that fixes it. The test passed because it stubbed cmd/billet's hostName
// seam while config read the real machine's, which on this Mac is legal.
func TestInitAsksALinuxOnlyMacForANameItsHostnameCannotSupply(t *testing.T) {
	onAReferenceMac(t)

	restore := hostName
	t.Cleanup(func() { hostName = restore })
	hostName = func() (string, error) { return "Junior's MacBook Pro.local", nil }

	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--provider", "tart", "--profile", "local-service", "--guest-os", "linux",
	})
	if err == nil {
		t.Fatal("a config was written naming a host nothing could authorise")
	}

	// THE FLAG, AND NOT APPLE. The pin is a macOS requirement and this generation
	// has no macOS tier, so offering Apple's licence as the reason would explain a
	// rule that does not apply here.
	if !strings.Contains(err.Error(), "--node-name") {
		t.Errorf("the refusal does not name the flag that fixes it: %v", err)
	}
	if strings.Contains(err.Error(), "two-guest limit") {
		t.Errorf("a linux-only generation was given Apple's licence as the reason: %v", err)
	}
}

// AND A USABLE HOSTNAME IS WRITTEN FOR A LINUX-ONLY MAC, not left out.
//
// The name is what `billet ca issue` mints a certificate for and what the control
// plane authorises, so a tart node carries it whichever guests it serves; only
// the tier PIN is Apple's doing.
func TestInitWritesTheNodeNameForALinuxOnlyMac(t *testing.T) {
	onAReferenceMac(t)

	restore := hostName
	t.Cleanup(func() { hostName = restore })
	hostName = func() (string, error) { return "mac-mini-1", nil }

	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--provider", "tart", "--profile", "local-service", "--guest-os", "linux",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// ASSERTED AGAINST THE BYTES FIRST. config.Load fills an omitted node.name from
	// the REAL machine's hostname, so reading it back cannot tell a key billet
	// wrote from one it left out — and on a runner that happens to be called
	// mac-mini-1 the two are indistinguishable.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !strings.Contains(string(raw), "\n  name: mac-mini-1\n") {
		t.Errorf("the generation did not write the node name it resolved:\n%s", raw)
	}

	cfg := loadWrittenConfig(t, path)
	if cfg.Node.Name != "mac-mini-1" {
		t.Errorf("node.name = %q, want the name billet resolved", cfg.Node.Name)
	}

	// AND NO TIER PINS IT, which is the half that IS guest-kind specific.
	for i := range cfg.Tiers {
		if cfg.Tiers[i].Node != "" {
			t.Errorf("tier %q pins %q; only a macOS tier has to",
				cfg.Tiers[i].Label, cfg.Tiers[i].Node)
		}
	}
}

// A PADDED GUEST KIND IS THE SAME REQUEST AS AN UNPADDED ONE.
//
// Generate trims its own copy, so `--guest-os " macos "` generates a macOS tier —
// and everything cmd/billet decides BEFORE calling it compared the raw value. The
// hostname derivation concluded there was no macOS tier, derived no name, and the
// generation was then refused for the name the derivation would have supplied.
func TestInitTreatsAPaddedGuestKindAsTheKindItNames(t *testing.T) {
	onAReferenceMac(t)

	restore := hostName
	t.Cleanup(func() { hostName = restore })
	hostName = func() (string, error) { return "mac-mini-1", nil }

	path := filepath.Join(t.TempDir(), "billet.yaml")

	var initErr error

	out := capture(t, func() {
		initErr = cmdInit(t.Context(), []string{
			"--config", path, "--org", "acme", "--provider", "tart", "--profile", "local-service",
			"--guest-os", "  macos  ",
		})
	})
	if initErr != nil {
		t.Fatalf("a padded guest kind was refused: %v", initErr)
	}

	cfg := loadWrittenConfig(t, path)

	found := false

	for i := range cfg.Tiers {
		if cfg.Tiers[i].GuestOS == config.GuestMacOS {
			found = true
		}
	}

	if !found {
		t.Error("no macOS tier was generated for a padded --guest-os macos")
	}

	// AND THE GUIDANCE SAW IT TOO, which is the half a comparison of raw text
	// against a typed value silently drops.
	if !strings.Contains(out, "87GB") {
		t.Errorf("the guidance never mentioned the macOS image's pull:\n%s", out)
	}
}

// THE CEILING IN A GENERATED TART CONFIG COMES FROM THE SEAM, NOT THE RUNNER.
//
// `billet init` MEASURES the host, so without the seam these tests describe
// whatever machine ran them — which is how the same commit passed on a 12-core
// Mac and failed on GitHub's 4-vCPU runner.
//
// A MACHINE NOBODY HAS, deliberately, and not the reference Mac's 12 and 32GiB
// the other tests pin: those numbers ARE this development machine, so bypassing
// the seam would agree with them here and be caught only where the runner
// happened to differ — which is the failure mode this whole seam exists for,
// reproduced in the test written to prevent it. Seven vCPU and 21GiB is a
// machine, and it is not one anybody is running.
func TestInitSizesATartConfigFromTheSeamRatherThanTheRunner(t *testing.T) {
	onAReferenceMac(t)

	prev := detectHostCapacity
	t.Cleanup(func() { detectHostCapacity = prev })
	detectHostCapacity = func() (int, config.ByteSize, error) { return 7, 21 * config.GiB, nil }

	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--provider", "tart", "--profile", "local-service",
		"--node-name", "mac-mini-1",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfg := loadWrittenConfig(t, path)

	// The headroom rule applied to those numbers: a sixteenth or the floor,
	// whichever is larger, and the memory trimmed to a whole GiB.
	wantVCPU := initconfig.CeilingVCPU(7)
	wantMemory := initconfig.CeilingMemory(21 * config.GiB)

	if cfg.Server.MaxVCPU != wantVCPU || cfg.Server.MaxMemory != wantMemory {
		t.Errorf("ceiling = %d vCPU and %s, want %d and %s — the seam's machine, not this one",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory, wantVCPU, wantMemory)
	}
}

// AND THE THREE PROVIDERS THAT ARE WRITABLE STILL ARE.
//
// Kept beside the refusal because a `default` branch that grew to cover a
// provider it should not would fail nothing otherwise — the generator would
// simply stop working for it, and only an end-to-end run would notice.
func TestInitStillGeneratesForEveryWritableProvider(t *testing.T) {
	// EC2 IS ASSERTED SEPARATELY, and it has to be: its generation resolves shape
	// vcpu/memory/price against live AWS, so a unit test cannot complete one. What
	// it CAN prove offline is the thing this test is actually about — that ec2
	// does not fall through to the tart refusal — because every ec2 flag is
	// validated before the first signed request. Without this the list said
	// "every writable provider" and covered two of three, and returning
	// errNotImplemented for ec2 would have stayed green.
	t.Run("ec2", func(t *testing.T) {
		err := cmdInit(t.Context(), []string{
			"--config", filepath.Join(t.TempDir(), "billet.yaml"),
			"--org", "acme", "--provider", "ec2",
		})
		if err == nil {
			t.Fatal("an ec2 config was generated with no region, subnet, group or budget")
		}

		if errors.Is(err, errNotImplemented) {
			t.Fatalf("ec2 reached the not-implemented branch, which is tart's alone: %v", err)
		}

		// AND IT FAILED FOR THE RIGHT REASON — a missing flag, not a refusal to
		// write ec2 at all, and not an AWS call this test must never make.
		if !strings.Contains(err.Error(), "--region") {
			t.Errorf("ec2 failed for something other than its first missing input: %v", err)
		}
	})

	for _, provider := range []string{"docker", "firecracker", "tart"} {
		t.Run(provider, func(t *testing.T) {
			args := []string{
				"--config", filepath.Join(t.TempDir(), "billet.yaml"),
				"--org", "acme", "--provider", provider,
			}

			if provider == "docker" {
				args = append(args,
					"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow)
			}

			// The one input a Mac cannot supply from the machine: its hostname is
			// usually not a legal node name, and the macOS tier has to pin one. The
			// platform seams follow, because a tart config is refused anywhere but
			// the machine it describes and CI is Linux — and so does the SERVICE
			// shape, because a user-session generation's paths come from the running
			// process, which on CI is not the Mac these seams describe.
			if provider == "tart" {
				onAReferenceMac(t)

				args = append(args,
					"--node-name", "mac-mini-1", "--profile", "local-service")
			}

			if err := cmdInit(t.Context(), args); err != nil {
				t.Fatalf("`billet init --provider %s` no longer generates: %v", provider, err)
			}
		})
	}
}
