package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE AGENT'S BUILD-CACHE BLOCK IS RUN HERE, not grepped for. What a job gets is
// its environment and the system bazelrc, and both are decided by this block from
// the node's metadata; the image gate can only see that the text is present.
func TestTheAgentConfiguresOnlyTheBuildCachesTheNodeOffers(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_GUEST_CACHES")
	billet := filepath.Join(t.TempDir(), "billet")
	if err := os.WriteFile(billet, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const endpoint = "http://172.31.0.1:7718"
	helper := billet + " cache gocacheprog"

	for _, tc := range []struct {
		name        string
		caches      string
		token       string
		binary      string
		wantEnv     []string
		wantNoEnv   []string
		wantBazelrc []string
		wantGit     []string
	}{
		{
			name: "go without test results runs every test", caches: "go", token: "token",
			binary: billet, wantEnv: []string{"GOCACHEPROG=" + helper, "GOFLAGS=-count=1"},
		},
		{
			name: "go with test results caches them", caches: "go,go-test-results", token: "token",
			binary: billet, wantEnv: []string{"GOCACHEPROG=" + helper}, wantNoEnv: []string{"GOFLAGS="},
		},
		{
			name: "bazel names the node and the helper, never the bearer", caches: "bazel",
			token: "token", binary: billet, wantNoEnv: []string{"GOCACHEPROG=", "GOFLAGS="},
			wantBazelrc: []string{
				"build --remote_cache=" + endpoint + "/v1/cas/bazel",
				"build --credential_helper=172.31.0.1=" + filepath.Dir(billet) + "/bazel-credential-helper",
			},
		},
		{
			name: "git rewrites github.com fetches to the node, and nothing else", caches: "git",
			token: "token", binary: billet, wantNoEnv: []string{"GOCACHEPROG="},
			wantGit: []string{
				`[url "` + endpoint + `/v1/git/github.com/"]`,
				"\tinsteadOf = https://github.com/",
				`[url "https://github.com/"]`,
				"\tpushInsteadOf = https://github.com/",
				`[credential "` + endpoint + `"]`,
				"\thelper = " + billet + " cache git-credential",
			},
		},
		{
			name: "an image without billet builds cold", caches: "go,bazel,git", token: "token",
			binary: filepath.Join(t.TempDir(), "absent"), wantNoEnv: []string{"GOCACHEPROG=", "GOFLAGS="},
		},
		{
			name: "no cache session configures nothing", caches: "go,bazel", token: "",
			binary: billet, wantNoEnv: []string{"GOCACHEPROG=", "GOFLAGS="},
		},
		{
			name: "an unknown cache is ignored", caches: "rust,go", token: "token",
			binary: billet, wantEnv: []string{"GOCACHEPROG=" + helper},
		},
		{
			name: "nothing offered", caches: "", token: "token",
			binary: billet, wantNoEnv: []string{"GOCACHEPROG=", "GOFLAGS="},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bazelrc := filepath.Join(t.TempDir(), "bazel.bazelrc")
			gitconfig := filepath.Join(t.TempDir(), "gitconfig")
			script := strings.Join([]string{
				"set -euo pipefail",
				`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
				"cache_endpoint=" + shellQuote(endpoint),
				"cache_token=" + shellQuote(tc.token),
				"guest_caches=" + shellQuote(tc.caches),
				"GUEST_BILLET=" + shellQuote(tc.binary),
				"BAZELRC_FILE=" + shellQuote(bazelrc),
				"GITCONFIG_FILE=" + shellQuote(gitconfig),
				"runner_env=()",
				block,
				`for entry in ${runner_env[@]+"${runner_env[@]}"}; do printf 'env=%s\n' "$entry"; done`,
			}, "\n")
			output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the build-cache block failed: %v\n%s", err, output)
			}
			var env []string
			for line := range strings.Lines(string(output)) {
				if value, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "env="); ok {
					env = append(env, value)
				}
			}
			for _, want := range tc.wantEnv {
				if !slices.Contains(env, want) {
					t.Errorf("the job's environment lacks %q: %q", want, env)
				}
			}
			for _, prefix := range tc.wantNoEnv {
				for _, entry := range env {
					if strings.HasPrefix(entry, prefix) {
						t.Errorf("the job's environment carries %q", entry)
					}
				}
			}

			rc, err := os.ReadFile(bazelrc)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(tc.wantBazelrc) == 0 && len(rc) != 0 {
				t.Errorf("a tier without the bazel cache got a bazelrc: %q", rc)
			}
			lines := strings.Split(strings.TrimSpace(string(rc)), "\n")
			if len(tc.wantBazelrc) > 0 && !slices.Equal(lines, tc.wantBazelrc) {
				t.Errorf("bazelrc = %q, want %q", lines, tc.wantBazelrc)
			}
			if strings.Contains(string(rc), "token") {
				t.Errorf("the bazelrc carries the bearer: %q", rc)
			}

			gc, err := os.ReadFile(gitconfig)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(tc.wantGit) == 0 && len(gc) != 0 {
				t.Errorf("a tier without the git cache got a gitconfig: %q", gc)
			}
			if got := strings.Split(strings.TrimSpace(string(gc)), "\n"); len(tc.wantGit) > 0 &&
				!slices.Equal(got, tc.wantGit) {
				t.Errorf("gitconfig = %q, want %q", got, tc.wantGit)
			}
			if strings.Contains(string(gc), "token") {
				t.Errorf("the gitconfig carries the bearer: %q", gc)
			}
		})
	}
}
