package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// ownHome redirects the user config dir into the test's tree, so generated
// state-dir paths — and the identity probes against them — can never touch
// the developer's real deployment.
func ownHome(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// A RE-RUN OVER PRISTINE INIT OUTPUT CONVERGES, and the App identity that
// `github-app create` filled in survives it — a regeneration that dropped it
// would send the operator back through onboarding against an App that exists.
func TestInitReRunConvergesAPristineConfig(t *testing.T) {
	ownHome(t)
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	// The real flow's key path follows the config's own (defaultKeyPath), so
	// the simulation must too — a foreign path IS operator divergence and
	// correctly lands on the write-beside path.
	keyPath, err := defaultKeyPath(path)
	if err != nil {
		t.Fatalf("resolve the key path: %v", err)
	}
	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: 7, InstallationID: 42, ClientID: "Iv1.abc",
		PrivateKeyPath: keyPath,
	}); err != nil {
		t.Fatalf("simulate github-app create: %v", err)
	}

	out := capture(t, func() {
		if err := cmdInit(t.Context(), []string{
			"--config", path, "--org", "acme",
			"--runner-group", testTrialGroup,
			"--workflow", testTrialWorkflow,
		}); err != nil {
			t.Fatalf("re-run: %v", err)
		}
	})

	if !strings.Contains(out, "Converged") {
		t.Errorf("a pristine re-run did not converge:\n%s", out)
	}
	if strings.Contains(out, "Create the GitHub App") {
		t.Errorf("a converged config was told to create an App that exists:\n%s", out)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, must := range []string{"app_id: 7", "installation_id: 42"} {
		if !strings.Contains(string(raw), must) {
			t.Errorf("the converged config lost the App identity (%s):\n%s", must, raw)
		}
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Error("a converged re-run also wrote a .new file")
	}
}

// AN EDITED CONFIG IS NEVER REWRITTEN: the fresh generation lands beside it
// and the original stays byte-identical — provider ordering, sites, a raised
// ceiling are the operator's only record of those decisions.
func TestInitReRunWritesBesideAnEditedConfig(t *testing.T) {
	ownHome(t)
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	edited, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// An operator's edit init cannot know is safe to drop.
	body := string(edited) + "\nsites:\n  - name: garage\n    store: ceph\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("edit: %v", err)
	}

	out := capture(t, func() {
		if err := cmdInit(t.Context(), []string{
			"--config", path, "--org", "acme",
			"--runner-group", testTrialGroup,
			"--workflow", testTrialWorkflow,
		}); err != nil {
			t.Fatalf("re-run: %v", err)
		}
	})

	if !strings.Contains(out, "was NOT touched") {
		t.Errorf("an edited re-run did not say it left the original alone:\n%s", out)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != body {
		t.Error("the edited original was modified")
	}
	if _, err := os.Stat(path + ".new"); err != nil {
		t.Errorf("no .new file beside the edited original: %v", err)
	}
}

// POINTING AWAY FROM A LIVE IDENTITY IS REFUSED, WITH NO OVERRIDE FLAG: the
// state directory's deployment-id is what every container this deployment
// started is labelled with, and moving the pointer orphans them all. The
// remedy is retirement, by name.
func TestInitRefusesPointingAwayFromALiveIdentity(t *testing.T) {
	ownHome(t)
	dir := t.TempDir()
	oldState := filepath.Join(dir, "old-state")
	if err := os.MkdirAll(oldState, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldState, "deployment-id"),
		[]byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("mint identity: %v", err)
	}

	path := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf("server:\n  listen: 127.0.0.1:7717\n  state_dir: %s\n", oldState)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	// WITHOUT --force this lands on the write-beside path, which moves no
	// pointer and so is NOT refused: the original stays byte-identical and the
	// identity stays live.
	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("the write-beside path was refused although it moves nothing: %v", err)
	}
	kept, err := os.ReadFile(path)
	if err != nil || string(kept) != body {
		t.Fatalf("the beside path modified the original (err %v)", err)
	}

	// --force REPLACES the file, and that is exactly what the refusal covers —
	// with deliberately no override flag.
	forceErr := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--force",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	})
	if forceErr == nil {
		t.Fatal("--force moved a pointer away from a live identity")
	}
	for _, must := range []string{"decommission", oldState} {
		if !strings.Contains(forceErr.Error(), must) {
			t.Errorf("the refusal does not name %s: %v", must, forceErr)
		}
	}

	// An unreadable directory is not an absent identity: make the state dir
	// unsearchable and the refusal must hold, fail-closed.
	if err := os.Chmod(oldState, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(oldState, 0o700); err != nil {
			t.Logf("restore state dir mode: %v", err)
		}
	})

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--force",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err == nil {
		t.Fatal("an unreadable state directory disarmed the refusal")
	}
}

// UNREADABLE YAML REFUSES --force: a file billet cannot read may still name
// live state, so wholesale replacement is not offered a way past it.
func TestInitForceRefusesUnreadableYAML(t *testing.T) {
	ownHome(t)
	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte("just a string, not a mapping"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--force",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	})
	if err == nil {
		t.Fatal("--force replaced a file whose pointer could not be read")
	}
	if !strings.Contains(err.Error(), "delete it yourself") {
		t.Errorf("the refusal does not name the deliberate path: %v", err)
	}
}

// A CONVERGING RE-RUN NEVER TOUCHES THE STATE DIRECTORY: the deployment
// identity and the node-wire authority must come out byte-identical, because
// a rotated authority strands every enrolled node at once.
func TestInitReRunNeverRotatesAuthority(t *testing.T) {
	ownHome(t)

	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stateDir := stateDirOf(t, string(raw))

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	deployment, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("mint identity: %v", err)
	}
	if _, err := wirecert.LoadOrCreateCA(stateDir, deployment); err != nil {
		t.Fatalf("mint authority: %v", err)
	}

	before := snapshotDir(t, stateDir)

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	}); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	after := snapshotDir(t, stateDir)
	if len(before) == 0 {
		t.Fatal("the snapshot is empty; the authority assertions would be vacuous")
	}
	for name, b := range before {
		if after[name] != b {
			t.Errorf("the re-run changed state file %s", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("the re-run created state file %s", name)
		}
	}
}

// The generated state dir points at the live user config dir; tests must read
// it from the file rather than assume it.
func stateDirOf(t *testing.T, body string) string {
	t.Helper()

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "state_dir:"); ok {
			return strings.TrimSpace(after)
		}
	}

	t.Fatal("no state_dir in the generated config")

	return ""
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(raw)

		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}

	return out
}

// THE BUSY-LISTEN NOTE: an operator re-running init while their server is up
// should hear that the address is held now, not at the next start.
func TestInitWarnsWhenTheListenAddressIsBusy(t *testing.T) {
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	path := filepath.Join(t.TempDir(), "billet.yaml")
	out := capture(t, func() {
		if err := cmdInit(t.Context(), []string{
			"--config", path, "--org", "acme", "--listen", l.Addr().String(),
			"--runner-group", testTrialGroup,
			"--workflow", testTrialWorkflow,
		}); err != nil {
			t.Fatalf("init: %v", err)
		}
	})

	if !strings.Contains(out, "already in use") {
		t.Errorf("a busy listen address was not noted:\n%s", out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the config was not written despite the note being advisory: %v", err)
	}
}

// THE PROBE COVERS THE DEFAULT ADDRESS TOO: Generate takes Params by value,
// so a listen default filled only inside it would leave the caller probing
// the empty string — which Go binds as a wildcard socket on a random port,
// making the note dead in the default case.
func TestInitWarnsWhenTheDefaultListenIsBusy(t *testing.T) {
	ownHome(t)

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:7717")
	if err != nil {
		t.Skipf("the default port is already held on this machine: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	path := filepath.Join(t.TempDir(), "billet.yaml")
	out := capture(t, func() {
		if err := cmdInit(t.Context(), []string{
			"--config", path, "--org", "acme",
			"--runner-group", testTrialGroup,
			"--workflow", testTrialWorkflow,
		}); err != nil {
			t.Fatalf("init: %v", err)
		}
	})

	if !strings.Contains(out, "already in use") {
		t.Errorf("a busy DEFAULT listen address was not noted:\n%s", out)
	}
}

// THE IDEMPOTENCE TABLE: every shape init can write converges on re-run with
// identical decisions — the proof that re-running setup is safe, per profile
// and provider.
func TestInitIdempotenceTable(t *testing.T) {
	ownHome(t)
	cases := map[string][]string{
		"docker trusted": {
			"--org", "acme",
			"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
		},
		"firecracker untrusted": {"--org", "acme", "--provider", "firecracker"},
		"docker with listen": {
			"--org", "acme", "--listen", "127.0.0.1:7911",
			"--runner-group", testTrialGroup, "--workflow", testTrialWorkflow,
		},
	}

	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "billet.yaml")
			args := append([]string{"--config", path}, flags...)

			if err := cmdInit(t.Context(), args); err != nil {
				t.Fatalf("first init: %v", err)
			}
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			out := capture(t, func() {
				if err := cmdInit(t.Context(), args); err != nil {
					t.Fatalf("re-run: %v", err)
				}
			})
			if !strings.Contains(out, "Converged") {
				t.Fatalf("the re-run did not converge:\n%s", out)
			}

			second, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Errorf("re-running with identical flags changed the file:\n--- first\n%s\n--- second\n%s",
					first, second)
			}
		})
	}
}

// A NODE-ONLY CONFIG STILL GETS THE ADOPTION HALF: replacing it with a config
// whose state dir holds someone else's identity mixes two deployments, and
// having no server section is not a way past that.
func TestInitForceRefusesAdoptingAForeignIdentity(t *testing.T) {
	ownHome(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")
	if err := os.WriteFile(path, []byte("node:\n  server_addr: 10.0.0.4:7717\n  provider: docker\n  state_dir: /tmp/n\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The NEW generation's state dir (under the redirected home) already holds
	// an identity that is not this config's.
	newState := initconfig.Params{}.ServerStateDir()
	if err := os.MkdirAll(newState, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newState, "deployment-id"),
		[]byte("fedcba9876543210fedcba9876543210\n"), 0o600); err != nil {
		t.Fatalf("mint foreign identity: %v", err)
	}

	err := cmdInit(t.Context(), []string{
		"--config", path, "--org", "acme", "--force",
		"--runner-group", testTrialGroup,
		"--workflow", testTrialWorkflow,
	})
	if err == nil {
		t.Fatal("a node-only config was replaced over a foreign identity")
	}
	if !strings.Contains(err.Error(), "decommission") {
		t.Errorf("the refusal does not name the remedy: %v", err)
	}
}
