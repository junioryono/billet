package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE BUILD CACHES NAME THE RELAY ONLY WHEN IT IS SERVING, AND ALL OF THEM DO
// (#374). The relay block runs, verbatim, before the two blocks that publish the
// cache to the job: the runner's environment and the Git, Bazel and Go
// configuration. A relay that started is what every one of them names; one that
// could not be created, never became ready, has no docker bridge to bind, or would
// front an https endpoint leaves the node's own address in all of them, so the
// caches still work and no credential helper is asked about a host it does not
// answer for.
func TestTheBuildCachesNameTheRelayOnlyWhenItServes(t *testing.T) {
	t.Parallel()

	relayBlock := agentBlock(t, "BILLET_CACHE_RELAY")
	envBlock := agentBlock(t, "BILLET_CACHE_ENV")
	cachesBlock := agentBlock(t, "BILLET_GUEST_CACHES")
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
	billet := filepath.Join(t.TempDir(), "billet")
	if err := forkSafeWriteFile(billet, []byte("#!/bin/sh\n"), 0o755); err != nil {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			record := filepath.Join(dir, "record")
			bazelrc := filepath.Join(dir, "bazel.bazelrc")
			gitconfig := filepath.Join(dir, "gitconfig")
			script := strings.Join([]string{
				"set -euo pipefail",
				`PATH="` + tc.ipDir + ":" + fakes + `:$PATH"`,
				"export BILLET_FAKE_RECORD=" + shellQuote(record),
				"export BILLET_FAKE_RUN_STATUS=" + exitStatus(tc.runFails),
				"export BILLET_FAKE_START_STATUS=" + exitStatus(tc.startFails),
				`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
				"cache_endpoint=" + shellQuote(tc.endpoint),
				"cache_token=token",
				"buildkit_cache_mount_limit_bytes=1073741824",
				"guest_caches=go,bazel,git",
				"GUEST_BILLET=" + shellQuote(billet),
				"BAZELRC_FILE=" + shellQuote(bazelrc),
				"GITCONFIG_FILE=" + shellQuote(gitconfig),
				"docker_gateway=172.17.0.1",
				"cache_relay_port=41322",
				"runner_env=()",
				relayBlock,
				envBlock,
				cachesBlock,
				`for entry in "${runner_env[@]}"; do printf 'env=%s\n' "$entry"; done`,
			}, "\n")

			output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the relay and cache blocks failed: %v\n%s", err, output)
			}

			// EVERY PLACE THAT NAMES THE CACHE NAMES THE SAME ONE.
			if !strings.Contains(string(output), "env=BILLET_CACHE_ENDPOINT="+tc.want+"\n") {
				t.Errorf("the runner's cache endpoint is not %q:\n%s", tc.want, output)
			}
			git, err := os.ReadFile(gitconfig)
			if err != nil {
				t.Fatalf("read gitconfig: %v", err)
			}
			for _, want := range []string{
				`[url "` + tc.want + `/v1/git/github.com/"]`,
				`[credential "` + tc.want + `"]`,
			} {
				if !strings.Contains(string(git), want) {
					t.Errorf("the git configuration lacks %q:\n%s", want, git)
				}
			}
			rc, err := os.ReadFile(bazelrc)
			if err != nil {
				t.Fatalf("read bazelrc: %v", err)
			}
			if !strings.Contains(string(rc), "build --remote_cache="+tc.want+"/v1/cas/bazel") {
				t.Errorf("bazel's remote cache is not %q:\n%s", tc.want, rc)
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
