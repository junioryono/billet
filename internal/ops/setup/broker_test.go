package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

// THE STANZA client-key PRINTS IS ONE THE BROKER LOADS, AND IT GRANTS THE JOB RECORDS.
// Without `actions_read` a broker's token is refused GitHub's job records, every job
// keeps an unknown queue time, and the control plane warns about it each hour: found
// on a live run (2026-11-04), because a key's unscoped token never had that problem.
func TestTheClientKeyStanzaIsOneTheBrokerLoadsAndGrantsJobRecords(t *testing.T) {
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
`), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	var out, errs bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	if err := Broker(t.Context(), env, []string{"client-key", "--config", cfgPath}); err != nil {
		t.Fatalf("billet broker client-key: %v\n%s", err, errs.String())
	}

	_, stanza, ok := strings.Cut(out.String(), "deployments:\n")
	if !ok {
		t.Fatalf("no stanza in:\n%s", out.String())
	}

	if !strings.Contains(stanza, "actions_read: true") {
		t.Errorf("the stanza does not grant the job records:\n%s", stanza)
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
	if got.Target != "acme" || !got.ActionsRead || got.AppID != 12345 || got.InstallationID != 67890 {
		t.Errorf("the stanza loaded as %+v", got)
	}
}
