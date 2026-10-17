package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE HYBRID ROOTS TAKE THE PLUGIN CACHE ONLY WITH LEAVE TO BREAK THEIR LOCK:
// they have no lock file, and terraform downloads a provider for a root with
// no lock entry rather than take it from the cache (measured with 1.14.0 and
// 1.14.1), so without the leave each of the four roots fetched the 194 MB aws
// provider again. Executed with a terraform and a billet that record what each
// init saw, with a cache set and without one.
func TestTheHybridRootsTakeThePluginCacheOnlyWhenOneIsSet(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		cache bool
		want  string
	}{
		"with a plugin cache":    {true, "true"},
		"without a plugin cache": {false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			bin := t.TempDir()
			calls := filepath.Join(t.TempDir(), "inits")

			// A billet that writes the root the script rewrites: one main.tf
			// pinning the module by ref.
			writeExecutable(t, filepath.Join(bin, "billet"), `#!/bin/sh
set -eu
out=
while [ $# -gt 0 ]; do
    case $1 in
        --out) out=$2; shift 2 ;;
        *) shift ;;
    esac
done
mkdir -p "$out/terraform"
printf 'module "billet" {\n  source = "github.com/junioryono/billet//terraform/modules/billet?ref=v0.0.0"\n}\n' >"$out/terraform/main.tf"
`)
			writeExecutable(t, filepath.Join(bin, "terraform"), `#!/bin/sh
case $1 in
    -chdir=*)
        if [ "$2" = init ]; then
            echo "init ${TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE:-}" >>"$HYBRID_TEST_CALLS"
        fi ;;
esac
exit 0
`)

			cmd := exec.CommandContext(t.Context(), "sh", "hybrid-root-check.sh")
			cmd.Env = append(environmentWithout("PATH", "TF_PLUGIN_CACHE_DIR",
				"TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE", "BILLET_HYBRID_CHECK_BINARY"),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"BILLET_HYBRID_CHECK_BINARY="+filepath.Join(bin, "billet"),
				"HYBRID_TEST_CALLS="+calls)
			if tc.cache {
				cmd.Env = append(cmd.Env, "TF_PLUGIN_CACHE_DIR="+t.TempDir())
			}

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("hybrid-root-check.sh: %v\n%s", err, out)
			}

			body, err := os.ReadFile(calls)
			if err != nil {
				t.Fatalf("no init was recorded: %v", err)
			}

			inits := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(inits) != 4 {
				t.Fatalf("recorded %d inits, want one for each of the four roots: %q", len(inits), inits)
			}

			for _, line := range inits {
				if got := strings.TrimSpace(strings.TrimPrefix(line, "init")); got != tc.want {
					t.Errorf("an init saw TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE=%q, want %q", got, tc.want)
				}
			}
		})
	}
}
