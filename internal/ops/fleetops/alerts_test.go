package fleetops

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// writeAlertsConfig is a control plane with one alert rule, `red`, notifying one
// destination, `hook`, whose address is read from a file holding address.
func writeAlertsConfig(t *testing.T, stateDir, address string) string {
	t.Helper()

	seedLedger(t, stateDir)

	dir := t.TempDir()
	addressFile := filepath.Join(dir, "hook-url")
	if err := os.WriteFile(addressFile, []byte(address), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "billet.yaml")
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
  alerts:
    destinations:
      - {name: hook, kind: webhook, url_file: ` + addressFile + `}
    rules:
      - {name: red, on: {failed: true}, notify: [hook]}
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// runAlerts runs `billet alerts` with its output captured.
func runAlerts(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: io.Discard, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	err := Alerts(t.Context(), env, args)

	return out.String(), err
}

// A MUTE TAKES EFFECT IN THE LEDGER AND `list` SAYS SO, and unmuting lifts it: what an
// operator does at 3am, without a restart.
func TestAlertsMuteAndUnmuteARuleTheConfigNames(t *testing.T) {
	t.Parallel()

	cfg := writeAlertsConfig(t, t.TempDir(), "https://hooks.example.com/x")

	out, err := runAlerts(t, "mute", "red", "--for", "2h", "--config", cfg)
	if err != nil || !strings.Contains(out, "Muted red until") {
		t.Fatalf("mute = %q, %v", out, err)
	}

	out, err = runAlerts(t, "list", "--config", cfg)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "red") || !strings.Contains(out, "failed") || !strings.Contains(out, "hook") ||
		!strings.Contains(out, "until ") {
		t.Errorf("list while muted:\n%s", out)
	}

	out, err = runAlerts(t, "unmute", "red", "--config", cfg)
	if err != nil || !strings.Contains(out, "no longer muted") {
		t.Fatalf("unmute = %q, %v", out, err)
	}

	out, err = runAlerts(t, "list", "--config", cfg)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(out, "until ") {
		t.Errorf("list after unmuting still shows a mute:\n%s", out)
	}
}

// A MUTE OF A RULE THE CONFIG DOES NOT NAME IS REFUSED, so a typo cannot record a mute
// of nothing while the rule goes on firing; and a mute has to end.
func TestAlertsRefuseAMuteThatWouldSilenceNothingOrNeverEnd(t *testing.T) {
	t.Parallel()

	cfg := writeAlertsConfig(t, t.TempDir(), "https://hooks.example.com/x")

	if _, err := runAlerts(t, "mute", "rde", "--for", "2h", "--config", cfg); err == nil ||
		!strings.Contains(err.Error(), `names no rule "rde"`) {
		t.Errorf("a mute of an undeclared rule = %v", err)
	}
	if _, err := runAlerts(t, "unmute", "rde", "--config", cfg); err == nil {
		t.Error("an unmute of an undeclared rule was accepted")
	}
	if _, err := runAlerts(t, "mute", "red", "--config", cfg); err == nil ||
		!strings.Contains(err.Error(), "--for must be positive") {
		t.Errorf("a mute with no end = %v", err)
	}
}

// `test` SENDS ONE NOTICE TO THE DESTINATION IT NAMES, read with this shell's
// environment and files, and says so when the destination is not one the config has.
func TestAlertsTestSendsOneNoticeToTheDestination(t *testing.T) {
	t.Parallel()

	var got atomic.Int32
	bodies := make(chan string, 4)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the notice: %v", err)
		}
		got.Add(1)
		bodies <- string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(destination.Close)

	cfg := writeAlertsConfig(t, t.TempDir(), destination.URL+"/s3cret")

	out, err := runAlerts(t, "test", "--destination", "hook", "--config", cfg)
	if err != nil || !strings.Contains(out, "hook accepted a test notice") {
		t.Fatalf("test = %q, %v", out, err)
	}
	if got.Load() != 1 {
		t.Errorf("the destination received %d notices, want 1", got.Load())
	}
	select {
	case sent := <-bodies:
		if !strings.Contains(sent, `"billet_alert":1`) {
			t.Errorf("sent %q", sent)
		}
	default:
		t.Error("the destination kept no notice")
	}

	if _, err := runAlerts(t, "test", "--destination", "nobody", "--config", cfg); err == nil ||
		!strings.Contains(err.Error(), `names no destination "nobody"`) {
		t.Errorf("a destination the config does not have = %v", err)
	}
	if _, err := runAlerts(t, "test", "--config", cfg); err == nil ||
		!strings.Contains(err.Error(), "--destination is required") {
		t.Errorf("no destination = %v", err)
	}
}

// EVERY ONE OF THESE IS ABOUT A BLOCK, and a config without it is refused by name.
func TestAlertsRefuseAConfigWithNoAlertsBlock(t *testing.T) {
	t.Parallel()

	cfg := writeCAConfig(t, t.TempDir())

	for _, args := range [][]string{
		{"list", "--config", cfg},
		{"mute", "red", "--for", "1h", "--config", cfg},
		{"test", "--destination", "hook", "--config", cfg},
	} {
		if _, err := runAlerts(t, args...); err == nil || !strings.Contains(err.Error(), "has no server.alerts block") {
			t.Errorf("billet alerts %s = %v", args[0], err)
		}
	}
}
