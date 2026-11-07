package setup

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// THE STANZA client-key PRINTS IS ONE THE BROKER LOADS, AND IT GRANTS THE JOB RECORDS.
// Without `actions_read` a broker's token is refused GitHub's job records, every job
// keeps an unknown queue time, and the control plane warns about it each hour: found
// on a live run (2026-11-04), because a key's unscoped token never had that problem. A
// target whose tier re-runs a job billet disrupted is granted `actions_write` instead,
// which includes read.
func TestTheClientKeyStanzaIsOneTheBrokerLoadsAndGrantsJobRecords(t *testing.T) {
	for _, tc := range []struct {
		name, tier, grant string
		write             bool
	}{
		{"a tier that re-runs nothing", "", "actions_read: true", false},
		{"a tier that re-runs a disrupted job", "    retry: {disrupted: 1}\n", "actions_write: true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientKeyStanza(t, tc.tier, tc.grant, tc.write)
		})
	}
}

func clientKeyStanza(t *testing.T, tierExtra, grant string, write bool) {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(cfgPath, []byte(`server:
  listen: 127.0.0.1:7717
  state_dir: `+filepath.Join(dir, "state")+`
  max_vcpu: 4
  max_memory: 8GiB
  token_broker:
    url: https://broker.internal:8443
    client_key_path: `+filepath.Join(dir, "client.key")+`
release: {automatic: false}
github:
  org: acme
  app_id: 12345
  installation_id: 67890
tiers:
  - label: plain
    provider: docker
    trust: untrusted
    vcpu: 2
    memory: 4GiB
    disk: 20GiB
    image: ghcr.io/actions/actions-runner:latest
    command: ["./run.sh"]
`+tierExtra), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	var out, errs bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	if err := Broker(t.Context(), env, []string{"client-key", "--config", cfgPath, "--new-deployment"}); err != nil {
		t.Fatalf("billet broker client-key: %v\n%s", err, errs.String())
	}

	_, stanza, ok := strings.Cut(out.String(), "deployments:\n")
	if !ok {
		t.Fatalf("no stanza in:\n%s", out.String())
	}

	if !strings.Contains(stanza, grant) {
		t.Errorf("the stanza does not say %s:\n%s", grant, stanza)
	}

	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, []byte("not read by a load"), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	brokerPath := filepath.Join(dir, "broker.yaml")
	if err := os.WriteFile(brokerPath, []byte(`listen: 127.0.0.1:8443
cert_file: `+filepath.Join(dir, "broker.crt")+`
key_file: `+filepath.Join(dir, "broker.key")+`
apps:
  - app_id: 12345
    private_key_path: `+keyPath+`
deployments:
`+stanza), 0o600); err != nil {
		t.Fatalf("write the broker's file: %v", err)
	}

	loaded, err := config.LoadBroker(brokerPath)
	if err != nil {
		t.Fatalf("the broker refused the stanza client-key printed: %v\n%s", err, stanza)
	}

	got := loaded.Deployments[0].Installations[0]
	if got.Target != "acme" || got.ActionsRead == write || got.ActionsWrite != write || got.AppID != 12345 ||
		got.InstallationID != 67890 {
		t.Errorf("the stanza loaded as %+v", got)
	}
}

// brokerConfig is a deployment on a broker, with nothing yet on disk.
func brokerConfig(t *testing.T) (path, dir string) {
	t.Helper()

	dir = t.TempDir()
	path = filepath.Join(dir, "billet.yaml")

	if err := os.WriteFile(path, []byte(`server:
  listen: 127.0.0.1:7717
  state_dir: `+filepath.Join(dir, "state")+`
  max_vcpu: 4
  max_memory: 8GiB
  token_broker:
    url: https://broker.internal:8443
    client_key_path: `+filepath.Join(dir, "client.key")+`
release: {automatic: false}
github:
  org: acme
  app_id: 12345
  installation_id: 67890
tiers:
  - label: plain
    provider: docker
    trust: untrusted
    vcpu: 2
    memory: 4GiB
    disk: 20GiB
    image: ghcr.io/actions/actions-runner:latest
    command: ["./run.sh"]
`), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return path, dir
}

// AN IDENTITY MINTED BY ACCIDENT STOPS A RESTORE. A replacement host told to make its own
// key, before the restore that brings the real identity back, is refused and left as it
// was; and looking at a key never creates anything.
func TestClientKeyMintsNoIdentityUnlessAsked(t *testing.T) {
	cfgPath, dir := brokerConfig(t)
	stateDir := filepath.Join(dir, "state")

	for name, args := range map[string][]string{
		"making a key":       {"client-key", "--config", cfgPath},
		"looking at the key": {"client-key", "--config", cfgPath, "--show"},
	} {
		var out, errs bytes.Buffer

		env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}

		err := Broker(t.Context(), env, args)
		if err == nil || !strings.Contains(err.Error(), "holds no deployment identity") {
			t.Fatalf("%s: got %v, want a refusal naming the missing identity", name, err)
		}

		if _, found, err := state.PeekDeploymentID(stateDir); err != nil || found {
			t.Fatalf("%s: the identity is found=%v, %v; a refusal minted one", name, found, err)
		}

		if _, err := os.Stat(filepath.Join(dir, "client.key")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: a refusal left a key behind: %v", name, err)
		}
	}

	// A HOST THAT HAS ITS IDENTITY NEEDS NO FLAG, which is the one a restore leaves.
	if _, err := state.DeploymentID(stateDir); err != nil {
		t.Fatalf("mint the identity: %v", err)
	}

	var out, errs bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	if err := Broker(t.Context(), env, []string{"client-key", "--config", cfgPath}); err != nil {
		t.Fatalf("a host with its identity: %v\n%s", err, errs.String())
	}

	first := out.String()
	out.Reset()

	if err := Broker(t.Context(), env, []string{"client-key", "--config", cfgPath, "--show"}); err != nil {
		t.Fatalf("--show: %v", err)
	}

	if out.String() != first {
		t.Errorf("--show printed\n%s\nand the command that made the key printed\n%s", out.String(), first)
	}
}

// A HOST TOLD TO HOLD NO KEY IS REFUSED ONE, at both doors it could come in by: creating
// an App spends its one key, and the refusal has to come before that does.
func TestAKeyIsRefusedOnADeploymentOnABroker(t *testing.T) {
	cfgPath, dir := brokerConfig(t)

	var out, errs bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}

	err := GitHubAppCreate(t.Context(), env, []string{"--config", cfgPath, "--org", "acme"})
	if err == nil || !strings.Contains(err.Error(), "server.token_broker") {
		t.Errorf("github-app create: got %v, want the broker named", err)
	}

	from := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(from, []byte("not read"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err = githubAppStoreKey(t.Context(), env, []string{"--config", cfgPath, "--from", from})
	if err == nil || !strings.Contains(err.Error(), "server.token_broker") {
		t.Errorf("github-app store-key: got %v, want the broker named", err)
	}
}
