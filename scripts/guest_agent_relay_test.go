package scripts_test

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE BUILD CACHES NAME THE RELAY ONLY WHEN IT IS SERVING (#374). The block is
// extracted verbatim and executed against fake service managers: a relay that
// started repoints the cache endpoint at itself; one that could not be created,
// never became ready, has no docker bridge to bind, or would front an https
// endpoint leaves the node's own address in place, so the caches still work.
func TestTheBuildCachesNameTheRelayOnlyWhenItServes(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_CACHE_RELAY")
	fakes := writeFakeServiceManagers(t)

	withBridge := t.TempDir()
	if err := forkSafeWriteFile(filepath.Join(withBridge, "ip"), []byte("#!/bin/sh\n"+
		`echo "5: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0"`+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	noBridge := t.TempDir()
	if err := forkSafeWriteFile(filepath.Join(noBridge, "ip"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	const node = "http://172.31.0.1:7718"
	const relayed = "http://172.17.0.1:41322"

	for _, tc := range []struct {
		name                 string
		endpoint             string
		ipDir                string
		runFails, startFails bool
		want                 string
		wantRun              bool
	}{
		{name: "the relay starts and the caches name it", endpoint: node, ipDir: withBridge,
			want: relayed, wantRun: true},
		{name: "the unit could not be created", endpoint: node, ipDir: withBridge,
			runFails: true, want: node, wantRun: true},
		{name: "the relay never became ready", endpoint: node, ipDir: withBridge,
			startFails: true, want: node, wantRun: true},
		{name: "there is no docker bridge to bind", endpoint: node, ipDir: noBridge, want: node},
		{name: "an https endpoint is left direct", endpoint: "https://10.0.0.5:7718",
			ipDir: withBridge, want: "https://10.0.0.5:7718"},
		{name: "no cache session configures nothing", endpoint: "", ipDir: withBridge, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			record := filepath.Join(t.TempDir(), "record")
			script := strings.Join([]string{
				"set -euo pipefail",
				`PATH="` + tc.ipDir + ":" + fakes + `:$PATH"`,
				"export BILLET_FAKE_RECORD=" + shellQuote(record),
				"export BILLET_FAKE_RUN_STATUS=" + exitStatus(tc.runFails),
				"export BILLET_FAKE_START_STATUS=" + exitStatus(tc.startFails),
				`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
				"cache_endpoint=" + shellQuote(tc.endpoint),
				"docker_gateway=172.17.0.1",
				"cache_relay_port=41322",
				block,
				`printf 'endpoint=%s\n' "$cache_endpoint"`,
			}, "\n")

			output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the relay block failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "endpoint="+tc.want+"\n") {
				t.Errorf("the caches name %q, want %q\n%s", endpointOf(string(output)), tc.want, output)
			}

			run := readArgv(t, record+".systemd-run")
			if (len(run) > 0) != tc.wantRun {
				t.Fatalf("the relay unit was created=%v, want %v: %q", len(run) > 0, tc.wantRun, run)
			}
			if tc.wantRun {
				for _, want := range []string{
					"--unit=billet-cache-relay", "--socket-property=ListenStream=172.17.0.1:41322",
					"/usr/bin/python3", "/usr/local/bin/billet-actions-proxy", "--mode", "node-relay",
					"--upstream", node,
				} {
					if !slices.Contains(run, want) {
						t.Errorf("the relay unit's argv lacks %q: %q", want, run)
					}
				}
			}
			if tc.wantRun && tc.want != relayed {
				if stopped := readArgv(t, record+".systemctl"); !slices.Contains(stopped, "billet-cache-relay.socket") {
					t.Errorf("a relay that did not serve was not stopped: %q", stopped)
				}
			}
		})
	}
}

func endpointOf(output string) string {
	for line := range strings.Lines(output) {
		if value, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "endpoint="); ok {
			return value
		}
	}

	return ""
}
