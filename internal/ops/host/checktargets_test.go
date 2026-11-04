package host

import (
	"os"
	"strings"
	"testing"
)

// A TIER A `targets` DECLARATION MADE SAYS WHICH ONE, in the line check prints
// for it, so the scale sets billet registers are never a surprise.
func TestCheckNamesTheDeclarationATierCameFrom(t *testing.T) {
	stubGitHubUnverifiable(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfgPath := writeUntrustedCheckConfig(t, "billet-first")

	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}

	declaration := `  - label: ${target}-small
    runs_on: billet-small
    targets: all
    provider: docker
    vcpu: 1
    memory: 4GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: untrusted
`
	if err := os.WriteFile(cfgPath, append(body, declaration...), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	var checkErr error

	out := capture(t, func() { checkErr = Check(t.Context(), processEnv(), []string{"--config", cfgPath}) })
	if checkErr != nil {
		t.Fatalf("check: %v\n%s", checkErr, out)
	}

	var line string

	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  default-small ") {
			line = l
		}
	}

	if !strings.Contains(line, "(runs-on billet-small)") || !strings.Contains(line, "from:${target}-small") {
		t.Errorf("the expanded tier's line is %q, want its runs-on name and its declaration:\n%s", line, out)
	}

	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  billet-first ") && strings.Contains(l, "from:") {
			t.Errorf("a hand-written tier names a declaration: %q", l)
		}
	}
}
