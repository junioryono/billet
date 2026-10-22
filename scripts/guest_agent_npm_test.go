package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE AGENT'S NPM REMAP IS RUN HERE, not grepped for (ADR-020).
//
// The registry is remapped only where the guest's passthrough to the node is
// serving, and only in the guest's own /etc/hosts: a remap to a listener that
// never started would send every npm fetch to a refused connection, and one a
// container could see would fail its handshake against an authority it does not
// trust. So the block between the markers is extracted verbatim and executed
// against fake service-manager commands and a fake resolver, in every direction
// it can go, and so is the runner environment that carries the authority.
func TestTheAgentRemapsTheNpmRegistryOnlyWhenItsPassthroughServes(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_NPM_START")
	environment := agentBlock(t, "BILLET_NPM_ENV")
	fakes := writeFakeServiceManagers(t)
	resolvers := t.TempDir()
	// The registry's addresses as the guest resolved them before any remap, with
	// a loopback answer the fallback must never name.
	for name, body := range map[string]string{
		"getent": "#!/bin/sh\n" +
			`if [ -n "${BILLET_FAKE_NO_REGISTRY:-}" ]; then exit 2; fi` + "\n" +
			`printf '104.16.6.34     STREAM registry.npmjs.org\n104.16.6.34     DGRAM\n` +
			`104.16.7.34     STREAM\n127.0.2.1       STREAM\n'` + "\n",
	} {
		if err := forkSafeWriteFile(filepath.Join(resolvers, name), []byte(body), 0o700); err != nil {
			t.Fatalf("write the fake %s: %v", name, err)
		}
	}

	for _, tc := range []struct {
		name                      string
		npmProxy, actionsActive   string
		runFails, startFails      bool
		noRegistry                bool
		wantRemap, wantLaunched   bool
		wantStopped, wantCAInEnvs bool
	}{
		{name: "the passthrough serves and the registry is remapped",
			npmProxy: "http://billet:sesh@10.9.8.7:9000", wantRemap: true, wantLaunched: true,
			wantCAInEnvs: true},
		{name: "with the Actions cache too, its environment already carries the authority",
			npmProxy: "http://billet:sesh@10.9.8.7:9000", actionsActive: "1", wantRemap: true,
			wantLaunched: true},
		{name: "the unit could not be created", npmProxy: "http://billet:sesh@10.9.8.7:9000",
			runFails: true, wantLaunched: true, wantStopped: true},
		{name: "the service never became ready", npmProxy: "http://billet:sesh@10.9.8.7:9000",
			startFails: true, wantLaunched: true, wantStopped: true},
		// NOWHERE TO FAIL OPEN TO: a passthrough whose node is down would close
		// every npm connection, so without the registry's own addresses nothing is
		// started and npm goes direct.
		{name: "the registry did not resolve", npmProxy: "http://billet:sesh@10.9.8.7:9000",
			noRegistry: true, wantStopped: true},
		{name: "the node named no npm proxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			record := filepath.Join(t.TempDir(), "record")
			hosts := filepath.Join(t.TempDir(), "hosts")
			if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
				t.Fatalf("write hosts: %v", err)
			}
			noRegistry := ""
			if tc.noRegistry {
				noRegistry = "1"
			}
			script := strings.Join([]string{
				"set -euo pipefail",
				`PATH="` + resolvers + ":" + fakes + `:$PATH"`,
				"export BILLET_FAKE_RECORD=" + shellQuote(record),
				"export BILLET_FAKE_RUN_STATUS=" + exitStatus(tc.runFails),
				"export BILLET_FAKE_START_STATUS=" + exitStatus(tc.startFails),
				"export BILLET_FAKE_NO_REGISTRY=" + shellQuote(noRegistry),
				`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
				"python_runtime=/usr/bin/python3",
				"npm_proxy=" + shellQuote(tc.npmProxy),
				"actions_cache_active=" + shellQuote(tc.actionsActive),
				"actions_ca_path=/home/runner/runner/_work/_billet/actions-cache-ca.pem",
				"guest_hosts_file=" + shellQuote(hosts),
				"runner_env=()",
				block,
				environment,
				`for entry in ${runner_env[@]+"${runner_env[@]}"}; do printf 'env=%s\n' "$entry"; done`,
			}, "\n")

			output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the npm startup block failed: %v\n%s", err, output)
			}
			written, err := os.ReadFile(hosts)
			if err != nil {
				t.Fatalf("read hosts: %v", err)
			}
			remapped := strings.Contains(string(written), "127.0.2.1 registry.npmjs.org\n")
			if remapped != tc.wantRemap {
				t.Errorf("the registry remapped=%v, want %v:\n%s", remapped, tc.wantRemap, written)
			}
			ca := "env=NODE_EXTRA_CA_CERTS=/home/runner/runner/_work/_billet/actions-cache-ca.pem"
			if got := strings.Contains(string(output), ca); got != tc.wantCAInEnvs {
				t.Errorf("the npm block put the authority in the runner's environment=%v, want %v\n%s",
					got, tc.wantCAInEnvs, output)
			}

			launched := readArgv(t, record+".systemd-run")
			if (len(launched) > 0) != tc.wantLaunched {
				t.Fatalf("systemd-run invoked=%v, want %v", len(launched) > 0, tc.wantLaunched)
			}
			serviced := readArgv(t, record+".systemctl")
			if stopped := containsArgv(serviced, []string{"stop"}); stopped != tc.wantStopped {
				t.Errorf("the units were stopped=%v, want %v\n%s", stopped, tc.wantStopped,
					strings.Join(serviced, " | "))
			}
			if !tc.wantLaunched {
				return
			}
			// THE UNIT ITSELF: a passthrough of the registry's own origin, on the
			// loopback address no container reaches, failing open to the
			// registry's real addresses and never to a loopback one.
			for _, required := range [][]string{
				{"--unit=billet-npm-proxy"},
				{"/usr/bin/python3", "/usr/local/bin/billet-actions-proxy"},
				{"--origin", "registry.npmjs.org"},
				{"--systemd-socket"},
				{"--socket-property=ListenStream=127.0.2.1:443"},
				{"--property=Type=notify"},
				{"--fallback-addr", "104.16.6.34,104.16.7.34"},
				{"--upstream", "http://billet:sesh@10.9.8.7:9000"},
				{"--uid=runner"},
			} {
				if !containsArgv(launched, required) {
					t.Errorf("the npm unit does not carry %q:\n%s", required, strings.Join(launched, " | "))
				}
			}
			if tc.wantRemap && !containsArgv(serviced, []string{"start", "billet-npm-proxy.service"}) {
				t.Error("the registry was remapped without the service being started")
			}
		})
	}
}

// THE CONTAINER RESOLVER NEVER ANSWERS FROM /etc/hosts, which names the npm
// passthrough no container can reach or trust.
func TestTheContainerResolverDoesNotReadTheGuestsHostsFile(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatalf("read build-guest-image.sh: %v", err)
	}
	_, rest, found := strings.Cut(string(raw), "dnsmasq_args=(")
	if !found {
		t.Fatal("the agent no longer builds the container resolver's arguments")
	}
	arguments, _, _ := strings.Cut(rest, ")")
	if !strings.Contains(arguments, "--no-hosts") {
		t.Fatalf("the container resolver reads /etc/hosts: dnsmasq_args=(%s)", arguments)
	}
}
