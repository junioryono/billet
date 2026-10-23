package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// EACH INTERCEPTED ORIGIN IS TAKEN UP ONLY WITH THE AUTHORITY THAT SIGNS IT
// (ADR-020): the agent's decision is extracted verbatim and executed against a
// fake metadata fetch. A node names `actions-proxy` for the Actions cache,
// `npm-proxy` for the npm cache, and `actions-ca-pem` when either is named; a
// proxy without the authority is no interception at all, because a client could
// not trust what it would reach.
func TestTheAgentTakesUpEachInterceptedOriginOnlyWithItsAuthority(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_INTERCEPTION_METADATA")
	for _, tc := range []struct {
		name                      string
		actions, npm, ca          string
		wantNamed                 bool
		wantActions, wantNpmProxy string
	}{
		{name: "nothing named"},
		{name: "the Actions cache", actions: "http://billet:a@10.0.0.1:7718", ca: "PEM",
			wantNamed: true, wantActions: "http://billet:a@10.0.0.1:7718"},
		{name: "the npm cache alone", npm: "http://billet:a@10.0.0.1:7718", ca: "PEM",
			wantNamed: true, wantNpmProxy: "http://billet:a@10.0.0.1:7718"},
		{name: "both", actions: "http://billet:a@10.0.0.1:7718", npm: "http://billet:a@10.0.0.1:7718", ca: "PEM",
			wantNamed: true, wantActions: "http://billet:a@10.0.0.1:7718", wantNpmProxy: "http://billet:a@10.0.0.1:7718"},
		{name: "an npm proxy without its authority", npm: "http://billet:a@10.0.0.1:7718"},
		{name: "an authority with no proxy", ca: "PEM"},
	} {
		script := strings.Join([]string{
			"set -euo pipefail",
			"actions_proxy=",
			// The agent's fetch reads one metadata key and fails for an absent one.
			`fetch() { case "$1" in actions-proxy) v=$A;; npm-proxy) v=$N;; actions-ca-pem) v=$C;; *) return 1;; esac; ` +
				`[ -n "$v" ] || return 1; printf '%s' "$v"; }`,
			"A=" + shellQuote(tc.actions) + " N=" + shellQuote(tc.npm) + " C=" + shellQuote(tc.ca),
			block,
			`printf 'named=%s\nactions=%s\nnpm=%s\n' "$interception_named" "$actions_proxy" "$npm_proxy"`,
		}, "\n")
		output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: the metadata block failed: %v\n%s", tc.name, err, output)
		}
		named := "named="
		if tc.wantNamed {
			named += "1"
		}
		for _, want := range []string{named + "\n", "actions=" + tc.wantActions + "\n", "npm=" + tc.wantNpmProxy + "\n"} {
			if !strings.Contains(string(output), want) {
				t.Errorf("%s: want %q in\n%s", tc.name, want, output)
			}
		}
	}
}

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
	// a loopback answer the fallback must never name; any other question is an
	// error, so the block is held to asking about the registry, IPv4.
	for name, body := range map[string]string{
		"getent": "#!/bin/sh\n" +
			`if [ "$#" -ne 2 ] || [ "$1" != ahostsv4 ] || [ "$2" != registry.npmjs.org ]; then exit 3; fi` + "\n" +
			`if [ -n "${BILLET_FAKE_NO_REGISTRY:-}" ]; then exit 2; fi` + "\n" +
			`printf '104.16.6.34     STREAM registry.npmjs.org\n104.16.6.34     DGRAM\n` +
			`104.16.7.34     STREAM\n127.0.2.1       STREAM\n'` + "\n",
	} {
		if err := forkSafeWriteFile(filepath.Join(resolvers, name), []byte(body), 0o700); err != nil {
			t.Fatalf("write the fake %s: %v", name, err)
		}
	}

	const ca = "/home/runner/runner/_work/_billet/actions-cache-ca.pem"
	everyBundle := []string{"NODE_EXTRA_CA_CERTS=" + ca, "SSL_CERT_FILE=" + ca, "REQUESTS_CA_BUNDLE=" + ca,
		"CURL_CA_BUNDLE=" + ca}
	for _, tc := range []struct {
		name                    string
		npmProxy, actionsActive string
		runFails, startFails    bool
		noRegistry              bool
		wantRemap, wantLaunched bool
		wantStopped             bool
		wantEnv                 []string
	}{
		{name: "the passthrough serves and the registry is remapped",
			npmProxy: "http://billet:sesh@10.9.8.7:9000", wantRemap: true, wantLaunched: true, wantEnv: everyBundle},
		// THE ACTIONS CACHE'S OWN ENVIRONMENT AND HOOK ALREADY CARRY THE
		// AUTHORITY, so this block adds nothing beside it.
		{name: "with the Actions cache too", npmProxy: "http://billet:sesh@10.9.8.7:9000", actionsActive: "1",
			wantRemap: true, wantLaunched: true},
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
				"actions_ca_path=" + ca,
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
			var env []string
			for _, line := range strings.Split(string(output), "\n") {
				if value, ok := strings.CutPrefix(line, "env="); ok {
					env = append(env, value)
				}
			}
			if strings.Join(env, "\n") != strings.Join(tc.wantEnv, "\n") {
				t.Errorf("the runner's environment from the npm block = %q, want %q", env, tc.wantEnv)
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
// passthrough no container can reach or trust: the resolver's block is
// extracted and executed against a fake service manager, and the dnsmasq argv
// it starts is read back. The Actions remap is its one explicit address, and a
// guest whose container DNS was never pointed at it starts no resolver.
func TestTheContainerResolverNeverAnswersFromTheGuestsHostsFile(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_CONTAINER_DNS")
	fakes := writeFakeServiceManagers(t)
	for _, tc := range []struct {
		name                         string
		containerDNS, actionsActive  string
		wantLaunched, wantResultsMap bool
	}{
		{name: "with the Actions remap", containerDNS: "1", actionsActive: "1", wantLaunched: true,
			wantResultsMap: true},
		{name: "container DNS without the Actions remap", containerDNS: "1", wantLaunched: true},
		{name: "a guest whose containers were never pointed at it"},
	} {
		record := filepath.Join(t.TempDir(), "record")
		script := strings.Join([]string{
			"set -euo pipefail",
			`PATH="` + fakes + `:$PATH"`,
			"export BILLET_FAKE_RECORD=" + shellQuote(record),
			`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
			"container_dns_active=" + shellQuote(tc.containerDNS),
			"actions_cache_active=" + shellQuote(tc.actionsActive),
			"docker_gateway=172.17.0.1",
			block,
		}, "\n")
		if output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: the resolver block failed: %v\n%s", tc.name, err, output)
		}
		launched := readArgv(t, record+".systemd-run")
		if (len(launched) > 0) != tc.wantLaunched {
			t.Fatalf("%s: systemd-run invoked=%v, want %v", tc.name, len(launched) > 0, tc.wantLaunched)
		}
		if !tc.wantLaunched {
			continue
		}
		if !containsArgv(launched, []string{"/usr/sbin/dnsmasq"}) || !containsArgv(launched, []string{"--no-hosts"}) {
			t.Errorf("%s: the container resolver may answer from /etc/hosts:\n%s", tc.name,
				strings.Join(launched, " | "))
		}
		mapped := containsArgv(launched, []string{"--address=/results-receiver.actions.githubusercontent.com/172.17.0.1"})
		if mapped != tc.wantResultsMap {
			t.Errorf("%s: the results remap present=%v, want %v:\n%s", tc.name, mapped, tc.wantResultsMap,
				strings.Join(launched, " | "))
		}
		for _, argument := range launched {
			if strings.Contains(argument, "registry.npmjs.org") {
				t.Errorf("%s: the container resolver names the npm registry: %q", tc.name, argument)
			}
		}
	}
}
