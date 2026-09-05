package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"

	"gopkg.in/yaml.v3"
)

// emitAnsibleArgs is the flag set every emission test shares: the docker trial's
// required trusted-pool policy, plus a --config path the test owns.
func emitAnsibleArgs(path string) []string {
	return []string{
		"--config", path, "--org", "acme", "--emit", "ansible",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}
}

// AN EMISSION WRITES NOTHING — not the config, and not the directory above it.
//
// The obvious way to get an inventory block for a host is to run this against
// the path the role will render, which on a fresh machine does not exist yet.
// Creating it as a side effect would leave /etc/billet behind on a machine
// nobody has converged, and worse, would make a later `billet init` believe a
// deployment lives there.
func TestEmitAnsibleWritesNothing(t *testing.T) {
	asLinux(t)

	dir := filepath.Join(t.TempDir(), "etc", "billet")
	path := filepath.Join(dir, "billet.yaml")

	var initErr error
	_ = capture(t, func() { initErr = cmdInit(t.Context(), emitAnsibleArgs(path)) })
	if initErr != nil {
		t.Fatalf("init --emit ansible: %v", initErr)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the emission wrote %s (stat err %v)", path, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the emission created %s (stat err %v)", dir, err)
	}
}

// AND NOTHING IS TOUCHED WHEN THE DESTINATION ALREADY EXISTS.
//
// The test above only proves nothing is CREATED. The paths that mutate a config
// — the converge, the .new beside it, --force, and the App-identity carry that
// reads the existing file — all run against a path that exists, so a truncation
// or a mode change there would have gone unnoticed. Snapshotted by bytes, mode
// and directory listing, across the carry and --force both.
func TestEmitAnsibleLeavesAnExistingConfigAlone(t *testing.T) {
	asLinux(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	// A config carrying a filled App identity, so the carry path runs rather
	// than being skipped for want of anything to carry.
	seed := []byte("github:\n  org: acme\n  app_id: 7\n  installation_id: 9\n" +
		"  private_key_path: /somewhere/else/key.pem\n")
	if err := os.WriteFile(path, seed, 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	entriesBefore, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	for _, extra := range [][]string{nil, {"--force"}} {
		var (
			out     string
			initErr error
		)

		out = capture(t, func() {
			initErr = cmdInit(t.Context(), append(emitAnsibleArgs(path), extra...))
		})
		if initErr != nil {
			t.Fatalf("init --emit ansible %v: %v", extra, initErr)
		}

		// THE CARRY PATH IS THE ONE THIS TEST IS ABOUT, so prove it ran. Without
		// this the test passes if carrying were skipped entirely — and a skipped
		// carry touches nothing, which is exactly what the assertions below check.
		var vars struct {
			Config struct {
				GitHub struct {
					AppID          int64 `yaml:"app_id"`
					InstallationID int64 `yaml:"installation_id"`
				} `yaml:"github"`
			} `yaml:"billet_config"`
		}
		if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
			t.Fatalf("parse the emitted block: %v\n%s", err, out)
		}
		if vars.Config.GitHub.AppID != 7 || vars.Config.GitHub.InstallationID != 9 {
			t.Fatalf("the seeded identity was not carried (app %d, installation %d), so this "+
				"test is not exercising the carry path",
				vars.Config.GitHub.AppID, vars.Config.GitHub.InstallationID)
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !bytes.Equal(after, seed) {
			t.Errorf("--emit ansible %v rewrote the existing config:\n%s", extra, after)
		}

		mode, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat back: %v", err)
		}
		if mode.Mode() != before.Mode() {
			t.Errorf("--emit ansible %v changed the mode from %v to %v",
				extra, before.Mode(), mode.Mode())
		}

		entriesAfter, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir back: %v", err)
		}
		if len(entriesAfter) != len(entriesBefore) {
			names := make([]string, 0, len(entriesAfter))
			for _, e := range entriesAfter {
				names = append(names, e.Name())
			}
			t.Errorf("--emit ansible %v left files behind: %v", extra, names)
		}
	}
}

// STDOUT CARRIES THE BLOCK AND NOTHING ELSE.
//
// `billet init --emit ansible >> inventory.yml` is what an operator will type.
// One line of prose on stdout is an inventory that no longer parses, and the
// cause is invisible in the file that broke — so every human-facing line goes
// to stderr, which this test does not capture.
func TestEmitAnsibleStdoutIsOnlyTheBlock(t *testing.T) {
	asLinux(t)

	path := filepath.Join(t.TempDir(), "billet.yaml")

	var initErr error
	out := capture(t, func() { initErr = cmdInit(t.Context(), emitAnsibleArgs(path)) })
	if initErr != nil {
		t.Fatalf("init --emit ansible: %v", initErr)
	}

	var vars map[string]any
	if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
		t.Fatalf("stdout is not parseable as inventory YAML: %v\n%s", err, out)
	}
	// The config, plus exactly the role flags this provider needs — no more.
	// A stray key here sets an unrelated Ansible variable on the host.
	want := map[string]bool{initconfig.AnsibleVar: true}
	for name := range initconfig.AnsibleCompanions(config.ProviderDocker) {
		want[name] = true
	}

	for name := range vars {
		if !want[name] {
			t.Errorf("stdout sets an unexpected variable %q:\n%s", name, out)
		}
	}
	if len(vars) != len(want) {
		t.Fatalf("stdout sets %d variables, want %d:\n%s", len(vars), len(want), out)
	}
	// A NON-NIL VALUE IS NOT A CONFIG. `billet_config: garbage` is valid YAML with
	// one key and a non-nil value, and satisfied the previous assertion — so the
	// test could not tell an emitted config from an emitted word.
	cfg, ok := vars[initconfig.AnsibleVar].(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, not a mapping:\n%s", initconfig.AnsibleVar,
			vars[initconfig.AnsibleVar], out)
	}
	for _, key := range []string{"server", "node", "tiers"} {
		if _, present := cfg[key]; !present {
			t.Errorf("the emitted config has no %q section:\n%s", key, out)
		}
	}
	tiers, ok := cfg["tiers"].([]any)
	if !ok || len(tiers) == 0 {
		t.Errorf("the emitted config carries no tiers (%T):\n%s", cfg["tiers"], out)
	}

	// The guidance is real and must exist — it just must not be here. Asserted
	// by its own words rather than by "no prose", so a reworded note does not
	// quietly stop being checked.
	for _, prose := range []string{"Nothing was written", "measured HERE", "App ids are zero"} {
		if strings.Contains(out, prose) {
			t.Errorf("guidance %q reached stdout; it belongs on stderr:\n%s", prose, out)
		}
	}
}

// THE EMISSION FOLLOWS THE ROLE'S PATH SHAPE, NOT THE FLAG DEFAULT.
//
// `billet init`'s default profile is the two-terminal user-session shape, whose
// state lives under the user config directory. The role installs units that
// cannot read it (ProtectHome, and StateDirectory pins /var/lib/billet), so an
// emission carrying those paths converges a host whose services never start.
func TestEmitAnsibleDefaultsToTheServiceShape(t *testing.T) {
	asLinux(t)

	path := filepath.Join(t.TempDir(), "billet.yaml")

	var initErr error
	out := capture(t, func() { initErr = cmdInit(t.Context(), emitAnsibleArgs(path)) })
	if initErr != nil {
		t.Fatalf("init --emit ansible: %v", initErr)
	}

	var vars struct {
		Config struct {
			Server struct {
				StateDir string `yaml:"state_dir"`
			} `yaml:"server"`
			Node struct {
				StateDir string `yaml:"state_dir"`
				LockDir  string `yaml:"lock_dir"`
			} `yaml:"node"`
		} `yaml:"billet_config"`
	}
	if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
		t.Fatalf("parse the emitted block: %v\n%s", err, out)
	}

	for name, got := range map[string]string{
		"server.state_dir": vars.Config.Server.StateDir,
		"node.state_dir":   vars.Config.Node.StateDir,
	} {
		if !strings.HasPrefix(got, "/var/lib/billet") {
			t.Errorf("%s is %q, not the service shape the role installs", name, got)
		}
	}
	if vars.Config.Node.LockDir != "/run/billet/locks" {
		t.Errorf("node.lock_dir is %q, not the service shape's", vars.Config.Node.LockDir)
	}
}

// A USER-SESSION EMISSION IS REFUSED RATHER THAN EMITTED.
//
// Asking for it explicitly is asking for paths the role's units cannot read, so
// it fails here instead of converging a host whose server never starts.
func TestEmitAnsibleRefusesTheUserSessionShape(t *testing.T) {
	asLinux(t)

	err := cmdInit(t.Context(), append(emitAnsibleArgs(filepath.Join(t.TempDir(), "billet.yaml")),
		"--profile", "local"))
	if err == nil {
		t.Fatal("--emit ansible --profile local was not refused")
	}
	if !strings.Contains(err.Error(), "--profile local") {
		t.Errorf("the refusal does not name the profile: %v", err)
	}
}

// THE CEILING IS MEASURED HERE, so an emission for another machine is refused.
//
// A host-run backend's ceiling comes from DetectHostCapacity on the machine
// running the command. Emitting an inventory for a 128-thread server from a
// laptop writes the laptop's capacity under the server's name: a config that
// loads, starts, and advertises a fraction of the fleet forever. The refusal
// has to name the remedy, because "run it somewhere else" is not obvious when
// the command appears to have all the information it needs.
func TestEmitAnsibleOffLinuxNamesTheRemedy(t *testing.T) {
	prev := hostOS
	hostOS = "darwin"
	t.Cleanup(func() { hostOS = prev })

	err := cmdInit(t.Context(), emitAnsibleArgs(filepath.Join(t.TempDir(), "billet.yaml")))
	if err == nil {
		t.Fatal("an emission for another machine was not refused")
	}
	for _, want := range []string{"--emit ansible", "ssh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
}

// AN UNKNOWN --emit IS REFUSED BY NAME, before capacity detection or any live
// AWS fetch — the same contract --profile and --listen already have.
func TestEmitRefusesAnUnknownDestination(t *testing.T) {
	err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--emit", "inventory",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	})
	if err == nil {
		t.Fatal("--emit inventory was accepted")
	}
	if !strings.Contains(err.Error(), "--emit") {
		t.Errorf("the refusal does not name --emit: %v", err)
	}
}

// captureStderr runs fn with stderr redirected and returns what it wrote.
//
// Restored with t.Cleanup rather than a bare reassignment, so a t.Fatal inside
// fn cannot leave every later test in the package writing into a dead pipe.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	saved := os.Stderr

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stderr = w
	t.Cleanup(func() { os.Stderr = saved })

	done := make(chan string, 1)

	go func() {
		var b strings.Builder

		_, _ = io.Copy(&b, r) //nolint:errcheck // the write end is closed below, ending the copy

		done <- b.String()
	}()

	fn()

	os.Stderr = saved

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	return <-done
}

// AN EXPLICITLY EMPTY --profile= IS NOT A THIRD SHAPE.
//
// CheckProfile accepts an empty profile because Generate defaults an unset one,
// and fs.Visit reports `--profile=` as SET — so it skipped the emission's
// service-shape default, matched neither exact shape, and emitted user-session
// paths. Off Linux it also skipped the refusal that exists because the ceiling
// is measured on the machine running the command, so it would emit a laptop's
// capacity under a server's name.
func TestEmitAnsibleRefusesAnExplicitlyEmptyProfile(t *testing.T) {
	t.Run("on linux it is the user-session shape, and refused", func(t *testing.T) {
		asLinux(t)

		err := cmdInit(t.Context(), append(
			emitAnsibleArgs(filepath.Join(t.TempDir(), "billet.yaml")), "--profile="))
		if err == nil {
			t.Fatal("--profile= was accepted for an emission")
		}
		if !strings.Contains(err.Error(), "--profile local") {
			t.Errorf("the refusal does not name the profile: %v", err)
		}
	})

	t.Run("off linux it still measures the wrong machine", func(t *testing.T) {
		prev := hostOS
		hostOS = "darwin"
		t.Cleanup(func() { hostOS = prev })

		err := cmdInit(t.Context(), append(
			emitAnsibleArgs(filepath.Join(t.TempDir(), "billet.yaml")), "--profile="))
		if err == nil {
			t.Fatal("--profile= emitted for another machine")
		}
	})
}

// AN APP ID WITH NO RECORDED ORG IS NOT AN IDENTITY TO CARRY.
//
// The carry writes every github field, so a blank existing org overwrote the org
// this run was for — leaving real App credentials pointing at no organization,
// with `carried` suppressing the zero-ids remediation that would have said so.
func TestEmitAnsibleWillNotCarryAnIncompleteApp(t *testing.T) {
	// EVERY WAY THE BLOCK CAN BE INCOMPLETE, because the carry writes all of them
	// at once. A nonzero App id was once the whole test — so a missing
	// installation id, which renders `installation_id: 0` and fails to load,
	// passed straight through and the command reported success.
	for name, seed := range map[string]string{
		"no org":              "github:\n  app_id: 7\n  installation_id: 9\n",
		"a blank org":         "github:\n  org: \"   \"\n  app_id: 7\n  installation_id: 9\n",
		"no installation":     "github:\n  org: acme\n  app_id: 7\n",
		"a zero installation": "github:\n  org: acme\n  app_id: 7\n  installation_id: 0\n",
		// existingGitHubBlock admits any NONZERO app id, so a negative one reaches
		// the guard. Without this case the `AppID > 0` clause is unexercised: every
		// other row carries a positive id, so weakening that clause changes nothing
		// any of them can observe.
		"a negative app id": "github:\n  org: acme\n  app_id: -7\n  installation_id: 9\n",
	} {
		t.Run(name, func(t *testing.T) {
			asLinux(t)

			path := filepath.Join(t.TempDir(), "billet.yaml")
			if err := os.WriteFile(path, []byte(seed), 0o640); err != nil {
				t.Fatalf("seed: %v", err)
			}

			var (
				out     string
				initErr error
			)

			notes := captureStderr(t, func() {
				out = capture(t, func() { initErr = cmdInit(t.Context(), emitAnsibleArgs(path)) })
			})
			if initErr != nil {
				t.Fatalf("init --emit ansible: %v", initErr)
			}

			var vars struct {
				Config struct {
					GitHub struct {
						Org            string `yaml:"org"`
						AppID          int64  `yaml:"app_id"`
						InstallationID int64  `yaml:"installation_id"`
					} `yaml:"github"`
				} `yaml:"billet_config"`
			}
			if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
				t.Fatalf("parse the emitted block: %v\n%s", err, out)
			}

			if vars.Config.GitHub.Org != "acme" {
				t.Errorf("the requested org was overwritten: github.org is %q",
					vars.Config.GitHub.Org)
			}
			if vars.Config.GitHub.AppID != 0 || vars.Config.GitHub.InstallationID != 0 {
				t.Errorf("an incomplete App was carried anyway: app %d, installation %d",
					vars.Config.GitHub.AppID, vars.Config.GitHub.InstallationID)
			}
			if !strings.Contains(notes, "not a complete identity") {
				t.Errorf("nothing said why the identity was not carried:\n%s", notes)
			}
		})
	}
}

// THE KEY-MOVE GUIDANCE REACHES AN EMISSION.
//
// It sat after the file write, and an emission returns before that — so a
// carried identity whose key path the profile had just changed was emitted with
// nothing moving the key and nothing saying to. The deployed service then had
// App ids and no key at the path it was configured to read.
func TestEmitAnsibleSaysTheAppKeyHasToMove(t *testing.T) {
	asLinux(t)

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte("github:\n  org: acme\n  app_id: 7\n"+
		"  installation_id: 9\n  private_key_path: /home/someone/key.pem\n"), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var initErr error

	notes := captureStderr(t, func() {
		_ = capture(t, func() { initErr = cmdInit(t.Context(), emitAnsibleArgs(path)) })
	})
	if initErr != nil {
		t.Fatalf("init --emit ansible: %v", initErr)
	}

	if !strings.Contains(notes, "App key path moved") {
		t.Errorf("an emission carrying an identity said nothing about moving the key:\n%s", notes)
	}
	if !strings.Contains(notes, "/home/someone/key.pem") {
		t.Errorf("the guidance does not name the key to copy:\n%s", notes)
	}
}

// THE BLOCK MUST CONVERGE ON ITS OWN.
//
// billet_config alone does not. The role provisions a Firecracker host by
// default — guest bridges and Ceph — and ASSERTS those flags match the node's
// provider before it touches anything, so a docker or ec2 emission left at the
// defaults is a valid config the role declines before rendering it. The default
// provider of `billet init` is docker, so that was the default emission: paste
// it, converge, and stop at an assertion with nothing rendered.
//
// A firecracker emission carries none, and that is not an oversight: the role's
// defaults already describe it, and stating billet_ceph_enabled would claim this
// host should bootstrap a cluster — an operator's decision, not a generator's.
func TestEmitAnsibleCarriesTheRoleFlagsItsProviderNeeds(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want map[string]bool
	}{
		"docker needs both off": {
			args: nil,
			want: map[string]bool{
				"billet_firecracker_enabled": false,
				"billet_ceph_enabled":        false,
			},
		},
		"firecracker needs none": {
			args: []string{"--provider", "firecracker"},
			want: map[string]bool{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			asLinux(t)

			path := filepath.Join(t.TempDir(), "billet.yaml")

			var initErr error

			out := capture(t, func() {
				initErr = cmdInit(t.Context(), append(emitAnsibleArgs(path), tc.args...))
			})
			if initErr != nil {
				t.Fatalf("init --emit ansible %v: %v", tc.args, initErr)
			}

			var vars map[string]any
			if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
				t.Fatalf("stdout is not parseable as inventory YAML: %v\n%s", err, out)
			}

			for flag, expected := range tc.want {
				got, present := vars[flag]
				switch {
				case !present:
					t.Errorf("the block does not set %s, so the role refuses it:\n%s", flag, out)
				case got != expected:
					t.Errorf("%s is %v, want %v", flag, got, expected)
				}
			}

			// And nothing beyond the config and those flags.
			for flag := range vars {
				if flag != initconfig.AnsibleVar && !tc.want[flag] && flag != "billet_firecracker_enabled" &&
					flag != "billet_ceph_enabled" {
					t.Errorf("the block sets an unexpected variable %q:\n%s", flag, out)
				}
			}
			if len(tc.want) == 0 && len(vars) != 1 {
				t.Errorf("a firecracker block sets %d variables, want only the config:\n%s",
					len(vars), out)
			}
		})
	}
}

// STDOUT STAYS CLEAN EVEN WHEN THE FLAGS ARE WRONG.
//
// `billet init --emit ansible --typo >> inventory.yml` is the same keystroke as
// the working command with one mistake in it, and newFlagSet sends usage and
// parse errors to stdout deliberately so `-h` stays pipeable. For an emission
// stdout IS the artefact, so a diagnostic there appends prose to the operator's
// inventory and the file stops parsing — with nothing in it to say why.
func TestEmitAnsibleKeepsParseErrorsOffStdout(t *testing.T) {
	asLinux(t)

	for name, args := range map[string][]string{
		"an unknown flag":  append(emitAnsibleArgs(filepath.Join(t.TempDir(), "b.yaml")), "--typo"),
		"a stray argument": append(emitAnsibleArgs(filepath.Join(t.TempDir(), "b.yaml")), "extra"),
	} {
		t.Run(name, func(t *testing.T) {
			var initErr error

			out := capture(t, func() { initErr = cmdInit(t.Context(), args) })
			if initErr == nil {
				t.Fatal("the bad invocation was accepted")
			}
			if out != "" {
				t.Errorf("a refusal put %d bytes on stdout, which would corrupt an appended "+
					"inventory:\n%s", len(out), out)
			}
		})
	}
}

// AND THE STREAM IS CHOSEN FROM THE RAW ARGS, in both spellings, because the
// flag set decides where to write a parse error before any value exists.
func TestWantsAnsibleEmissionReadsBothSpellings(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want bool
	}{
		"separate value": {[]string{"--emit", "ansible"}, true},
		"joined value":   {[]string{"--emit=ansible"}, true},
		"single dash":    {[]string{"-emit", "ansible"}, true},
		"joined single":  {[]string{"-emit=ansible"}, true},
		"the file sink":  {[]string{"--emit", "file"}, false},
		"no emit flag":   {[]string{"--org", "acme"}, false},
		"dangling flag":  {[]string{"--emit"}, false},
		"a value only":   {[]string{"ansible"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := wantsAnsibleEmission(tc.args); got != tc.want {
				t.Errorf("wantsAnsibleEmission(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// THE PRINTED BOOTSTRAP IS THIS INVOCATION'S OWN COMMANDS.
//
// A generic recipe was wrong three review rounds running, and each time the
// tests passed because they matched fragments of the surrounding prose rather
// than the commands. `billet init` with no flags defaults to docker and refuses
// to generate without a runner group and an exact workflow, so the printed first
// step did not run — and for a firecracker or ec2 operator it described
// bootstrapping a different deployment entirely. `github-app create` also needs
// --org, which it does not read from the config.
func TestEmitAnsibleBootstrapCarriesTheFlagsThatWereUsed(t *testing.T) {
	asLinux(t)

	path := filepath.Join(t.TempDir(), "billet.yaml") // no App identity to carry

	args := append(emitAnsibleArgs(path), "--provider", "firecracker")

	var initErr error

	notes := captureStderr(t, func() {
		_ = capture(t, func() { initErr = cmdInit(t.Context(), args) })
	})
	if initErr != nil {
		t.Fatalf("init --emit ansible: %v", initErr)
	}

	// The generation flags must survive into the printed first command, or it
	// bootstraps a deployment that is not the one being emitted.
	if !strings.Contains(notes, "--provider firecracker") {
		t.Errorf("the printed bootstrap dropped the provider that was used:\n%s", notes)
	}

	// And the App command must carry the org, which it cannot read from a config.
	if !strings.Contains(notes, "billet github-app create --org acme") {
		t.Errorf("the printed App command does not carry --org:\n%s", notes)
	}

	// The destination flags must NOT be echoed: they are replaced by the scratch
	// path, and repeating --emit ansible in the first step would print a block
	// instead of writing the config the second step needs.
	first, _, ok := strings.Cut(notes, "billet github-app create")
	if !ok {
		t.Fatalf("no bootstrap was printed:\n%s", notes)
	}
	if _, after, found := strings.Cut(first, "billet init --config"); found {
		line, _, _ := strings.Cut(after, "\n")
		if strings.Contains(line, "--emit") {
			t.Errorf("the printed first step re-emits instead of writing a config: %q", line)
		}
	}
}

// AND IT IS BUILT FROM PARSED VALUES, so a value that looks like a flag cannot
// be mistaken for one.
//
// The first version edited raw argv, dropping the destination flags and whatever
// followed them. `--runner-group --config` is a legal invocation whose runner
// group is the string "--config": the scan removed it as a destination flag and
// swallowed the next argument as its value, printing two commands that do not
// run. It also preserved --profile, and the service shape puts the App key under
// /etc/billet, which `github-app create` cannot create as an ordinary user.
func TestGenerationFlagsRebuildOnlyWhatDescribesTheDeployment(t *testing.T) {
	for name, tc := range map[string]struct {
		in   generationInputs
		want []string
	}{
		"a docker trial": {
			generationInputs{
				org: "acme", provider: "docker", runnerGroup: "billet-trial",
				workflows: []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
			},
			[]string{
				"--org", "acme", "--provider", "docker",
				"--runner-group", "billet-trial",
				"--workflow", "acme/repo/.github/workflows/ci.yml@refs/heads/main",
			},
		},
		"a runner group that looks like a flag": {
			generationInputs{org: "acme", provider: "docker", runnerGroup: "--config"},
			[]string{"--org", "acme", "--provider", "docker", "--runner-group", "--config"},
		},
		"repeated workflows keep their order": {
			generationInputs{
				org: "acme", provider: "docker", runnerGroup: "g",
				workflows: []string{"a/b/.github/workflows/1.yml@refs/heads/main",
					"a/b/.github/workflows/2.yml@refs/heads/main"},
			},
			[]string{
				"--org", "acme", "--provider", "docker", "--runner-group", "g",
				"--workflow", "a/b/.github/workflows/1.yml@refs/heads/main",
				"--workflow", "a/b/.github/workflows/2.yml@refs/heads/main",
			},
		},
		"the ec2 placement survives whole": {
			generationInputs{
				org: "acme", provider: "ec2", region: "us-west-2", subnet: "subnet-1",
				securityGroups: []string{"sg-a"}, untrustedGroups: []string{"sg-b"},
				instanceTypes: []string{"c7i.xlarge"}, priceOverrides: []string{"c7i.xlarge=0.17"},
				maxVCPU: 64, maxMemory: "128GiB",
			},
			[]string{
				"--org", "acme", "--provider", "ec2",
				"--region", "us-west-2", "--subnet", "subnet-1",
				"--security-group", "sg-a", "--untrusted-security-group", "sg-b",
				"--instance-type", "c7i.xlarge", "--price", "c7i.xlarge=0.17",
				"--max-vcpu", "64", "--max-memory", "128GiB",
			},
		},
		"an unset budget is omitted rather than zeroed": {
			generationInputs{org: "acme", provider: "firecracker"},
			[]string{"--org", "acme", "--provider", "firecracker"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := generationFlags(tc.in)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("generationFlags() = %q, want %q", got, tc.want)
			}

			// The destination and path flags must never appear: --config and
			// --emit are replaced by the bootstrap, and --profile would point the
			// App key at /etc/billet where it cannot be created.
			for _, flag := range []string{"--config", "--emit", "--profile"} {
				for i, a := range got {
					if a == flag && i%2 == 0 {
						t.Errorf("generationFlags() emitted %s, which the bootstrap replaces", flag)
					}
				}
			}
		})
	}
}

// THE CARRY GUARD IS COVERED, not merely present.
//
// Generate validates the config it RENDERED, and the App identity is written
// into those bytes afterwards — so an identity that renders a config
// config.Parse rejects would reach the operator with the command reporting
// success. `usable` refuses every shape able to do that today, which is exactly
// why this is reached directly: through the CLI the guard has no failing case,
// so without this its mutation survives and it reads as decoration.
//
// It exists for the field nobody has added yet. installation_id was carried
// unchecked until a review found it; the next field added to renderGitHubBlock
// arrives the same way.
func TestCheckCarriedRefusesBytesThatWillNotLoad(t *testing.T) {
	good, _, err := initconfig.Generate(initconfig.Params{
		Org: "acme", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
		RunnerGroup: testTrialGroup, Workflows: []string{testTrialWorkflow},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	withIdentity, err := renderGitHubBlock([]byte(good), githubBlock{
		Org: "acme", AppID: 1, InstallationID: 2,
		PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
	})
	if err != nil {
		t.Fatalf("render a good identity: %v", err)
	}
	if err := checkCarried(withIdentity); err != nil {
		t.Fatalf("a complete identity was refused: %v", err)
	}

	// The shapes renderGitHubBlock can produce that config.Parse rejects. Each
	// is what a future carried field looks like when it is written without
	// being checked.
	for name, bad := range map[string]githubBlock{
		"no key path":       {Org: "acme", AppID: 1, InstallationID: 2},
		"no installation":   {Org: "acme", AppID: 1, PrivateKeyPath: "/tmp/k.pem"},
		"no app id":         {Org: "acme", InstallationID: 2, PrivateKeyPath: "/tmp/k.pem"},
		"a whitespace org":  {Org: "   ", AppID: 1, InstallationID: 2, PrivateKeyPath: "/tmp/k.pem"},
		"a negative app id": {Org: "acme", AppID: -1, InstallationID: 2, PrivateKeyPath: "/tmp/k.pem"},
	} {
		t.Run(name, func(t *testing.T) {
			rendered, err := renderGitHubBlock([]byte(good), bad)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if err := checkCarried(rendered); err == nil {
				t.Error("a carried identity that does not load was accepted")
			}
		})
	}
}

// A WRITE THAT FAILED IS NOT A DELIVERY, and nothing downstream may act as if
// it were.
//
// Two claims at once. The emission's stdout write is checked, so a full disk
// after part of the block was accepted exits unsuccessfully instead of
// reporting success over a corrupted inventory. And the App key-move guidance
// runs only after a successful delivery — it was hoisted once to run before
// both destinations, which fixed an emission that never reached it and broke the
// file path instead: a failed write would tell the operator to move a credential
// for a config that does not exist.
//
// Provoked with a pipe whose read end is closed, which is the cheapest real
// write failure available; a full filesystem is the other and needs one.
func TestEmitAnsibleFailsLoudlyWhenStdoutCannotBeWritten(t *testing.T) {
	asLinux(t)

	// An identity to carry whose key path the profile will move, so the
	// guidance would fire if delivery had succeeded.
	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte("github:\n  org: acme\n  app_id: 7\n"+
		"  installation_id: 9\n  private_key_path: /home/someone/key.pem\n"), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := r.Close(); err != nil { // every write to w now fails
		t.Fatalf("close read end: %v", err)
	}

	saved := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = saved })

	var initErr error

	notes := captureStderr(t, func() { initErr = cmdInit(t.Context(), emitAnsibleArgs(path)) })

	os.Stdout = saved
	_ = w.Close()

	if initErr == nil {
		t.Fatal("a failed stdout write reported success")
	}
	if !strings.Contains(initErr.Error(), initconfig.AnsibleVar) {
		t.Errorf("the error does not name what could not be written: %v", initErr)
	}
	if strings.Contains(notes, "App key path moved") {
		t.Errorf("the key-move guidance was printed for a generation that was never "+
			"delivered:\n%s", notes)
	}
}

// WHAT AN OPERATOR WITH NO APP IS TOLD — AND THE RECIPE ACTUALLY WALKED.
//
// Three earlier versions of this text could not be followed, and the tests
// passed each time because they matched fragments of the surrounding prose. The
// commands are read here, and the middle one's EFFECT is then performed against
// a real file, so a recipe that cannot work fails this rather than an operator.
func TestNoIdentityGuidance(t *testing.T) {
	flags := []string{"--org", "acme", "--provider", "docker", "--runner-group", "g"}

	// ONE RECIPE, AND NO PROVIDER TO BRANCH ON. ec2 used to be refused a recipe
	// entirely, because the version then printed re-resolved --instance-type
	// against live AWS twice more. Minting an identity needs no generated config,
	// so noIdentityGuidance takes no provider at all — which is a stronger
	// statement than a loop over three identical outputs could make.
	t.Run("it mints an identity without generating anything", func(t *testing.T) {
		got := noIdentityGuidance("/etc/billet/billet.yaml", "acme", "", flags)

		for _, want := range []string{
			"refused when the config is LOADED",
			"not under BILLET_MAINTENANCE",
			"billet github-app create --org acme --config " + bootstrapIdentity,
			"billet init --emit ansible --config " + bootstrapIdentity,
			"--provider docker", // the flags actually used
			// NOCLOBBER. A plain `>` truncates the identity file on a second
			// run, before the key destination's own refusal fires — the key
			// survives and the only record of the App ids does not.
			"(set -C; printf",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the guidance does not carry %q:\n%s", want, got)
			}
		}

		// The step that made every earlier version unfollowable — and for ec2
		// unfollowable in a way no flag could fix.
		if strings.Contains(got, "billet init --config") {
			t.Errorf("the guidance still generates a scratch config:\n%s", got)
		}
	})

	t.Run("the org placeholder survives a shell", func(t *testing.T) {
		got := noIdentityGuidance("/etc/billet/billet.yaml", "", "", flags)

		// A bare <your-org> is input redirection: the shell removes it from
		// argv or fails before billet starts, and the App is never created.
		if strings.Contains(got, "--org <your-org>") {
			t.Errorf("the org placeholder is unquoted:\n%s", got)
		}
		if !strings.Contains(got, "--org '<your-org>'") {
			t.Errorf("the guidance does not name the org to supply:\n%s", got)
		}
	})

	// THE RECIPE'S MIDDLE STEP, PERFORMED. `github-app create --config` writes
	// into an existing file rather than creating one, and it needs a YAML
	// MAPPING — a bare `github:` parses as null and renderGitHubBlock declines
	// it silently, writing no block and returning no error. This walks the seed
	// the guidance prints through the exact call that command makes on success,
	// then proves the emission carries what it wrote.
	t.Run("the printed seed accepts an App identity", func(t *testing.T) {
		asLinux(t)

		dir := t.TempDir()
		identity := filepath.Join(dir, "billet-app.yaml")

		if err := os.WriteFile(identity, []byte(bootstrapSeed+"\n"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// Exactly what `github-app create --config` does once GitHub answers.
		if err := writeGitHubBlock(identity, githubBlock{
			Org: "acme", AppID: 7, InstallationID: 9,
			PrivateKeyPath: filepath.Join(dir, "app-private-key.pem"),
		}); err != nil {
			t.Fatalf("the printed seed does not accept an App identity: %v", err)
		}

		var initErr error

		out := capture(t, func() {
			initErr = cmdInit(t.Context(), emitAnsibleArgs(identity))
		})
		if initErr != nil {
			t.Fatalf("re-emitting against the identity file: %v", initErr)
		}

		var vars struct {
			Config struct {
				GitHub struct {
					Org            string `yaml:"org"`
					AppID          int64  `yaml:"app_id"`
					InstallationID int64  `yaml:"installation_id"`
				} `yaml:"github"`
			} `yaml:"billet_config"`
		}
		if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
			t.Fatalf("parse the emitted block: %v\n%s", err, out)
		}

		if vars.Config.GitHub.AppID != 7 || vars.Config.GitHub.InstallationID != 9 {
			t.Errorf("the recipe did not carry the identity it minted: app %d, installation %d",
				vars.Config.GitHub.AppID, vars.Config.GitHub.InstallationID)
		}
		if vars.Config.GitHub.Org != "acme" {
			t.Errorf("the carried org is %q", vars.Config.GitHub.Org)
		}
	})
}

// shellWords splits a printed command the way a shell would, for the quoting
// this code actually emits: bare words and single-quoted runs.
//
// Deliberately not a general shell parser. It exists so a test can EXECUTE what
// was printed instead of reconstructing arguments that merely resemble it — and
// it refuses anything it cannot split faithfully, so a future quoting change is
// a failed test rather than a silently different command.
func shellWords(t *testing.T, line string) []string {
	t.Helper()

	var (
		words   []string
		current strings.Builder
		inWord  bool
	)

	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				t.Fatalf("unterminated quote in printed command: %q", line)
			}

			current.WriteString(line[i+1 : i+1+end])
			inWord = true
			i += end + 1
		case ' ':
			if inWord {
				words = append(words, current.String())
				current.Reset()

				inWord = false
			}
		case '"', '\\':
			t.Fatalf("printed command uses quoting this test cannot split faithfully: %q", line)
		default:
			current.WriteByte(c)
			inWord = true
		}
	}

	if inWord {
		words = append(words, current.String())
	}

	return words
}

// THE PRINTED COMMAND IS RUN, NOT RECONSTRUCTED.
//
// Five versions of this recipe could not be followed, and the tests passed every
// time because they asserted on fragments of the prose or rebuilt arguments that
// merely resembled the printed ones. This lifts the third command out of the
// guidance, splits it as a shell would, and feeds those exact arguments to
// cmdInit. A recipe that cannot be followed now fails here.
func TestTheThirdPrintedCommandActuallyRuns(t *testing.T) {
	asLinux(t)

	dir := t.TempDir()
	identity := filepath.Join(dir, "billet-app.yaml")

	// Step one of the recipe, performed: the printed seed.
	if err := os.WriteFile(identity, []byte(bootstrapSeed+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Step two's effect: what `github-app create --config` writes on success.
	if err := writeGitHubBlock(identity, githubBlock{
		Org: "acme", AppID: 7, InstallationID: 9,
		PrivateKeyPath: filepath.Join(dir, "app-private-key.pem"),
	}); err != nil {
		t.Fatalf("the printed seed does not accept an App identity: %v", err)
	}

	// Step three, lifted from the guidance verbatim.
	guidance := noIdentityGuidance("/etc/billet/billet.yaml", "acme", "",
		[]string{"--org", "acme", "--provider", "docker",
			"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow})

	var printed string

	for line := range strings.SplitSeq(guidance, "\n") {
		if strings.Contains(line, "billet init --emit ansible") {
			printed = strings.TrimSpace(line)

			break
		}
	}

	if printed == "" {
		t.Fatalf("the guidance prints no re-emission command:\n%s", guidance)
	}

	words := shellWords(t, printed)
	if len(words) < 2 || words[0] != "billet" || words[1] != "init" {
		t.Fatalf("the printed command is not `billet init …`: %q", words)
	}

	// The only substitution: the recipe names a path in the operator's home,
	// and this test owns a directory instead. Everything else runs as printed.
	args := make([]string, 0, len(words)-2)

	for _, w := range words[2:] {
		if w == bootstrapIdentity {
			w = identity
		}

		args = append(args, w)
	}

	var initErr error

	out := capture(t, func() { initErr = cmdInit(t.Context(), args) })
	if initErr != nil {
		t.Fatalf("the printed command does not run: %v\n  %s", initErr, printed)
	}

	var vars struct {
		Config struct {
			GitHub struct {
				AppID          int64 `yaml:"app_id"`
				InstallationID int64 `yaml:"installation_id"`
			} `yaml:"github"`
		} `yaml:"billet_config"`
	}
	if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
		t.Fatalf("the printed command emitted unparseable YAML: %v\n%s", err, out)
	}

	if vars.Config.GitHub.AppID != 7 || vars.Config.GitHub.InstallationID != 9 {
		t.Errorf("the printed command did not carry the minted identity: app %d, installation %d",
			vars.Config.GitHub.AppID, vars.Config.GitHub.InstallationID)
	}
}

// AN EMISSION CAN DESCRIBE A MACHINE IT IS NOT RUNNING ON.
//
// The measured ceiling is why the emission was refused off Linux, and that
// refusal created a cycle: the target needed a billet binary before there was an
// inventory to install one from. Declaring the capacity breaks it — the operator
// takes responsibility for the numbers, exactly as the ec2 path already requires
// because no host exists under it to measure.
//
// The declared numbers are what the MACHINE HAS, not what billet may spend, so
// the host still keeps its headroom. That is the measured path's meaning, and it
// differs from ec2's, where the declared budget is itself the ceiling.
func TestEmitAnsibleCanDescribeAnotherMachine(t *testing.T) {
	prev := hostOS
	hostOS = "darwin"
	t.Cleanup(func() { hostOS = prev })

	path := filepath.Join(t.TempDir(), "billet.yaml")

	var initErr error

	var out string

	// STDERR TOO. The block is stdout, but the lines naming where the role
	// renders it are notes — so a test capturing only stdout stayed green when
	// those lines went back to describing the emitting machine.
	notes := captureStderr(t, func() {
		out = capture(t, func() {
			initErr = cmdInit(t.Context(), append(emitAnsibleArgs(path),
				"--max-vcpu", "8", "--max-memory", "32GiB"))
		})
	})
	if initErr != nil {
		t.Fatalf("a declaring emission was refused off linux: %v", initErr)
	}

	var vars struct {
		Config struct {
			Server struct {
				VCPU   int    `yaml:"max_vcpu"`
				Memory string `yaml:"max_memory"`
			} `yaml:"server"`
		} `yaml:"billet_config"`
	}
	if err := yaml.Unmarshal([]byte(out), &vars); err != nil {
		t.Fatalf("parse the emitted block: %v\n%s", err, out)
	}

	// 8 threads less the floor of 2; 32GiB less the 4GiB floor. Headroom is
	// withheld from a DECLARED reading exactly as from a measured one — a
	// declaration that became the ceiling outright would overcommit the target.
	if vars.Config.Server.VCPU != 6 {
		t.Errorf("max_vcpu is %d, want 6 — headroom was not withheld from the declaration",
			vars.Config.Server.VCPU)
	}
	if vars.Config.Server.Memory != "28GiB" {
		t.Errorf("max_memory is %q, want 28GiB", vars.Config.Server.Memory)
	}

	// AND IT DESCRIBES THE TARGET, NOT THE MACHINE THAT EMITTED IT.
	//
	// This is the one supported path where the generation is for a platform the
	// command is not running on, and the role that consumes it is Linux-only —
	// it installs the systemd units, creates /etc/billet and puts the App key
	// there. A generation that followed hostOS emitted /usr/local paths and a
	// private_key_path the role NEVER WRITES, so the converged host would have
	// had App ids and no key at the path it was configured to read: the
	// two-locations-for-one-credential hazard, reached through the platform seam
	// rather than through a literal.
	// `billet local up` is deliberately NOT in this list. The block's intro names
	// it, and on the Linux host the role converges it is a real command that
	// works — `lifeops` names a role-rendered unit rather than replacing it. It
	// is simply not how that host is managed, which is a different complaint from
	// the ones below: /usr/local and launch agents do not exist there at all.
	for _, absent := range []string{"/usr/local", "launch agent"} {
		if strings.Contains(out, absent) {
			t.Errorf("the emitted block carries %q, but the role converges a systemd host:\n%s",
				absent, out)
		}

		if strings.Contains(notes, absent) {
			t.Errorf("the emission's notes carry %q, but the role converges a systemd host:\n%s",
				absent, notes)
		}
	}

	// AND THE NOTES NAME THE ROLE'S OWN DESTINATION, which is the line an
	// operator acts on: "the role renders it to <path> on the target".
	if !strings.Contains(notes, initconfig.ServiceConfigPathFor("linux")) {
		t.Errorf("the emission does not name where the role renders it:\n%s", notes)
	}

	for _, want := range []string{
		"/etc/billet/app-private-key.pem",
		"/var/lib/billet",
		"/run/billet/locks",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the emitted block does not carry the Linux service path %q:\n%s", want, out)
		}
	}
}

// AND WITHOUT THE DECLARATION IT IS STILL REFUSED, naming both ways out.
func TestEmitAnsibleOffLinuxNamesBothRemedies(t *testing.T) {
	prev := hostOS
	hostOS = "darwin"
	t.Cleanup(func() { hostOS = prev })

	err := cmdInit(t.Context(), emitAnsibleArgs(filepath.Join(t.TempDir(), "billet.yaml")))
	if err == nil {
		t.Fatal("an emission for another machine was accepted with nothing declared")
	}

	for _, want := range []string{"ssh <host>", "--max-vcpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not offer %q: %v", want, err)
		}
	}
}

// HALF A READING IS NOT A READING.
//
// One flag alone would leave the other measured from whichever machine happens
// to be running the command — the same defect the off-Linux refusal exists to
// prevent, reached from the other side.
func TestEmitAnsibleRefusesHalfADeclaration(t *testing.T) {
	asLinux(t)

	for name, extra := range map[string][]string{
		"only vcpu":   {"--max-vcpu", "8"},
		"only memory": {"--max-memory", "32GiB"},
	} {
		t.Run(name, func(t *testing.T) {
			err := cmdInit(t.Context(), append(
				emitAnsibleArgs(filepath.Join(t.TempDir(), "billet.yaml")), extra...))
			if err == nil {
				t.Fatal("half a declaration was accepted")
			}
			if !strings.Contains(err.Error(), "together or not at all") {
				t.Errorf("the refusal does not say they go together: %v", err)
			}
		})
	}
}
