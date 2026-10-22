package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// BILLET CACHE GIT FORGET NAMES ONE REPOSITORY and runs on a node, and says
// what it dropped or that there was nothing to drop.
func TestCacheGitForgetDropsOneRepositorysMirrors(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	config := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(config, []byte("node:\n  server_addr: 127.0.0.1:7717\n  provider: docker\n"+
		"  state_dir: "+stateDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(stateDir, "git-mirrors", "untrusted", "acme", "api.git")
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := Run(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, args)

		return out.String(), err
	}

	for _, args := range [][]string{
		{"git"}, {"git", "remember"}, {"git", "forget", "--config", config},
		{"git", "forget", "--config", config, "--repository", "acme"},
		{"git", "forget", "--config", config, "--repository", "acme/api/extra"},
	} {
		if _, err := run(args...); err == nil {
			t.Errorf("billet cache %s was not refused", strings.Join(args, " "))
		}
	}

	out, err := run("git", "forget", "--config", config, "--repository", "acme/api")
	if err != nil || !strings.Contains(out, "forgot the git mirror untrusted/acme/api") {
		t.Fatalf("forget said %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "billet-broken")); err != nil {
		t.Fatalf("the mirror was not marked for the node to remove: %v", err)
	}
	out, err = run("git", "forget", "--config", config, "--repository", "acme/other")
	if err != nil || !strings.Contains(out, "keeps no git mirror of acme/other") {
		t.Fatalf("forgetting a repository with no mirror said %q, %v", out, err)
	}
}
