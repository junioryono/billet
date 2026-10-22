package host

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// changeTestConfig is a node-only billet.yaml whose state lives under dir.
func changeTestConfig(dir string) string {
	return `node:
  server_addr: 10.0.0.1:7717
  provider: docker
  name: fc-1
  state_dir: ` + filepath.Join(dir, "node") + `
  lock_dir: ` + filepath.Join(dir, "lock") + `
  max_vcpu: 8
  max_memory: 32GiB
  tls:
    cert: ` + filepath.Join(dir, "tls", "node.crt") + `
    key: ` + filepath.Join(dir, "tls", "node.key") + `
    ca: ` + filepath.Join(dir, "tls", "ca.crt") + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
}

// runConfigChange runs `billet node config-change` on two bodies and returns
// its answer.
func runConfigChange(t *testing.T, installed, candidate string) configChangeAnswer {
	t.Helper()

	answer, _ := runConfigChangeRaw(t, installed, candidate)

	return answer
}

// runConfigChangeRaw is runConfigChange with the bytes the command printed,
// which are what the roles read.
func runConfigChangeRaw(t *testing.T, installed, candidate string) (configChangeAnswer, string) {
	t.Helper()

	dir := t.TempDir()
	a, b := filepath.Join(dir, "installed.yaml"), filepath.Join(dir, "candidate.yaml")
	mustOK(t, os.WriteFile(a, []byte(installed), 0o600))
	mustOK(t, os.WriteFile(b, []byte(candidate), 0o600))

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}
	if err := NodeConfigChange(t.Context(), env, []string{"--installed", a, "--candidate", b, "--json"}); err != nil {
		t.Fatalf("config-change: %v\n%s", err, out.String())
	}

	var answer configChangeAnswer
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, out.String())
	}
	if answer.Schema != configChangeSchema {
		t.Errorf("schema %d, want %d", answer.Schema, configChangeSchema)
	}

	return answer, out.String()
}

// A NODE KEEPS RUNNING ONLY WHEN WHAT IT READS IS UNCHANGED (#183): a tier of
// another provider added to a shared catalogue, or a nodes: policy entry, is
// keep; a change under node: or any other key it reads is restart; and so is
// everything billet cannot prove, a file that does not load and a host that
// runs the control plane too.
//
// Mutations: dropping a key from nodeIgnores turns a keep case into restart;
// skipping the server check or the load check turns its restart into keep.
func TestNodeConfigChangeKeepsANodeOnlyForWhatItDoesNotRead(t *testing.T) {
	dir := t.TempDir()
	base := changeTestConfig(dir)

	tartTier := base + `  - label: billet-macos
    provider: tart
    node: mac-1
    guest_os: macos
    vcpu: 4
    memory: 8GiB
    image: ghcr.io/example/macos:latest
`
	nodesPolicy := base + `nodes:
  - name: mac-1
    provider: tart
    guest_os: [macos]
`
	reordered := strings.Replace(base, "  max_vcpu: 8\n  max_memory: 32GiB\n", "  max_memory: 32GiB\n  max_vcpu: 8\n", 1)
	commented := "# Rendered by the role: change the inventory, not this file.\n" + base
	nodeChanged := strings.Replace(base, "max_vcpu: 8", "max_vcpu: 6", 1)
	siteAdded := strings.Replace(base, "node:\n", "images:\n  accept: [ubuntu]\nnode:\n", 1)
	// A COMBINED HOST THAT LOADS, so the control-plane cases are decided by the
	// control-plane rule and not by a refusal to load.
	withServer := `server:
  listen: 127.0.0.1:7717
  state_dir: ` + filepath.Join(dir, "server") + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + filepath.Join(dir, "app.pem") + `
node:
  server_addr: 127.0.0.1:7717
  provider: docker
  name: cp-1
  state_dir: ` + filepath.Join(dir, "node") + `
  lock_dir: ` + filepath.Join(dir, "lock") + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
	broken := strings.Replace(base, "max_vcpu: 8", "max_vcpu: eight", 1)
	// ONE BOOLEAN TO YAML, TWO NAMES TO billet: the untyped documents agree
	// and the loaded configurations do not.
	trueName := strings.Replace(base, "name: fc-1", "name: true", 1)
	trueNameUpper := strings.Replace(base, "name: fc-1", "name: TRUE", 1)

	for _, c := range []struct {
		name, installed, candidate, verdict, why string
	}{
		{"identical", base, base, configChangeKeep, ""},
		{"another provider's tier added", base, tartTier, configChangeKeep, "tiers"},
		{"a nodes: entry added", base, nodesPolicy, configChangeKeep, "nodes"},
		{"keys reordered", base, reordered, configChangeKeep, ""},
		{"a comment added", base, commented, configChangeKeep, ""},
		{"the node block changed", base, nodeChanged, configChangeRestart, "[node]"},
		{"another key the node reads", base, siteAdded, configChangeRestart, "images"},
		{"the candidate runs a control plane", base, withServer, configChangeRestart, "this host runs the control plane too"},
		{"the installed one ran a control plane", withServer, base, configChangeRestart, "this host runs the control plane too"},
		{"the candidate does not load", base, broken, configChangeRestart, "does not load"},
		{"a name YAML reads as the same boolean", trueName, trueNameUpper, configChangeRestart, "as billet loads it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := runConfigChange(t, c.installed, c.candidate)
			if got.Verdict != c.verdict {
				t.Fatalf("verdict %q (%s), want %q", got.Verdict, got.Why, c.verdict)
			}
			if c.why != "" && !strings.Contains(got.Why, c.why) {
				t.Errorf("why %q does not say %q", got.Why, c.why)
			}
		})
	}
}

// A FILE THAT CANNOT BE READ IS restart, never keep, and the command says so
// on stdout rather than failing: the role reads any answer but keep, and no
// answer at all, as the restart.
func TestNodeConfigChangeRestartsWhenAFileCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.yaml")
	mustOK(t, os.WriteFile(present, []byte(changeTestConfig(dir)), 0o600))

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}
	if err := NodeConfigChange(t.Context(), env, []string{
		"--installed", filepath.Join(dir, "absent.yaml"), "--candidate", present, "--json",
	}); err != nil {
		t.Fatalf("config-change: %v", err)
	}

	var answer configChangeAnswer
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, out.String())
	}
	if answer.Verdict != configChangeRestart || !strings.Contains(answer.Why, "installed") {
		t.Errorf("an unreadable installed file answered %+v, want restart naming it", answer)
	}

	for want, args := range map[string][]string{
		"pass --json":       {"--installed", present, "--candidate", present},
		"are both required": {"--candidate", present, "--json"},
	} {
		if err := NodeConfigChange(t.Context(), env, args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v answered %v, want a refusal saying %q", args, err, want)
		}
	}
}

// THE FIXTURES THE ROLES' SCENARIO TESTS ANSWER FROM, the bytes the command
// itself printed (BILLET_UPDATE_FIXTURES=1 rewrites them): re-encoding the
// decoded answer would let a change to how it prints pass here and fail on
// every host.
func TestNodeConfigChangeFixtures(t *testing.T) {
	for name, stdout := range configChangeOutputs(t) {
		compareFixture(t, "node-config-change", name, stdout)
	}
}

// configChangeOutputs is what the command prints for a keep and a restart.
func configChangeOutputs(t *testing.T) map[string]string {
	t.Helper()

	dir := t.TempDir()
	base := changeTestConfig(dir)

	out := map[string]string{}
	for name, candidate := range map[string]string{
		"keep":    base + "nodes:\n  - name: mac-1\n    provider: tart\n    guest_os: [macos]\n",
		"restart": strings.Replace(base, "max_vcpu: 8", "max_vcpu: 6", 1),
	} {
		_, out[name] = runConfigChangeRaw(t, base, candidate)
	}

	return out
}

// THE ROLES KEEP A NODE ONLY ON BILLET'S EXACT KEEP, and that literal is the
// command's own output: both roles compare the answer whole (a pattern let a
// half-formed reason through as keep), so a reworded keep would read as
// restart on every host until they follow it, and this fails first.
func TestTheRolesKeepOnlyOnBilletsExactKeep(t *testing.T) {
	keep := strings.TrimSpace(configChangeOutputs(t)["keep"])

	for _, path := range []string{
		"../../../ansible_collections/junioryono/billet/roles/host/tasks/node-config-answer.yml",
		"../../../ansible_collections/junioryono/billet/roles/macos_node/tasks/converge.yml",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "_keep_answer: '"+keep+"'") {
			t.Errorf("%s does not compare the answer with billet's exact keep %s", path, keep)
		}
	}
}
