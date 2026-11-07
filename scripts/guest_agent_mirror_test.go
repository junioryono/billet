package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A MIRROR'S AUTHORITY IS TRUSTED FOR THAT MIRROR ALONE, and only beside mirrors
// the agent took up. The agent's block is extracted verbatim and executed against
// a fake metadata fetch, in every direction it can go: nothing here may stop the
// agent, which runs under set -e before it has started a runner.
func TestTheAgentTrustsAMirrorsAuthorityForThatMirrorAlone(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_REGISTRY_MIRROR_TRUST")
	const mirrorJSON = `{"docker.io":"https://172.31.0.1:7740"}`

	for _, tc := range []struct {
		name, mirrors, authority string
		// certs is where the agent writes Docker's trust; "" is a directory that
		// can be made, and "file" one a regular file stands in the way of.
		certs       string
		wantTrusted bool
		wantLog     string
	}{
		{name: "mirrors with their authority", mirrors: mirrorJSON, authority: "PEM", wantTrusted: true},
		{name: "publicly trusted mirrors", mirrors: mirrorJSON},
		{name: "an authority with no mirror the agent took up", authority: "PEM"},
		{
			name: "an authority that cannot be installed", mirrors: mirrorJSON, authority: "PEM", certs: "file",
			wantLog: "could not be installed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			certs := filepath.Join(root, "certs.d")
			if tc.certs == "file" {
				if err := os.WriteFile(certs, nil, 0o600); err != nil {
					t.Fatalf("stand a file in the way: %v", err)
				}
			}
			path := filepath.Join(root, "mirrors-ca.pem")
			script := strings.Join([]string{
				"set -euo pipefail",
				"log() { printf 'log: %s\\n' \"$*\"; }",
				`fetch() { case "$1" in registry-mirrors-ca-pem) v=$C;; *) return 1;; esac; ` +
					`[ -n "$v" ] || return 1; printf '%s' "$v"; }`,
				"C=" + shellQuote(tc.authority),
				"registry_mirrors_json=" + shellQuote(tc.mirrors),
				"docker_mirror=https://172.31.0.1:7740",
				"registry_mirrors_ca_path=" + shellQuote(path),
				"docker_certs_dir=" + shellQuote(certs),
				block,
				`printf 'file=%s\n' "$registry_mirrors_ca_file"`,
			}, "\n")
			output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the block stopped the agent: %v\n%s", err, output)
			}

			want := "file=\n"
			if tc.wantTrusted {
				want = "file=" + path + "\n"
			}
			if !strings.Contains(string(output), want) {
				t.Errorf("want %q in\n%s", want, output)
			}
			if tc.wantLog != "" && !strings.Contains(string(output), tc.wantLog) {
				t.Errorf("want the log to say %q:\n%s", tc.wantLog, output)
			}

			body, err := os.ReadFile(filepath.Join(certs, "172.31.0.1:7740", "ca.crt"))
			switch {
			case tc.wantTrusted:
				if err != nil || string(body) != "PEM\n" {
					t.Errorf("Docker's trust = %q, %v, want the authority", body, err)
				}
				stat, err := os.Stat(path)
				if err != nil || stat.Mode().Perm() != 0o644 {
					t.Errorf("the runner's copy: %v, %v, want one a job's builder can read", stat, err)
				}
			case err == nil:
				t.Errorf("Docker trusts an authority it was not meant to: %q", body)
			}
		})
	}
}

// THE RUNNER IS TOLD OF THE FILE ONLY WHEN THE BLOCK SAID IT IS THERE, which is
// what the builder action reads.
func TestTheRunnerIsToldOfAMirrorsAuthorityOnlyWhenItWasInstalled(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatalf("read build-guest-image.sh: %v", err)
	}
	const want = `if [ -n "$registry_mirrors_ca_file" ]; then
	runner_env+=("BILLET_REGISTRY_MIRRORS_CA_FILE=$registry_mirrors_ca_file")
fi
`
	if !strings.Contains(string(raw), want) {
		t.Errorf("the agent no longer carries:\n%s", want)
	}
}
