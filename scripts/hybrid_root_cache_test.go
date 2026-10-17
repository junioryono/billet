package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE FIRST HYBRID ROOT'S LOCK IS EVERY LATER ROOT'S: the roots have no
// committed lock, and with no lock terraform neither takes a provider from the
// plugin cache nor trusts one (measured with 1.14.0 and 1.14.1), so without
// the hand-over each of the four roots downloaded the 194 MB aws provider. The
// first root's init runs with no lock, which authenticates what it downloads;
// each later root is given the lock it wrote; and nothing ever sets
// TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE, which would take whatever the
// cache held. Executed with a terraform and a billet that record each init.
func TestTheFirstHybridRootsLockIsEveryLaterRoots(t *testing.T) {
	t.Parallel()

	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "inits")

	// A billet that writes the root the script rewrites: one main.tf pinning
	// the module by ref, under the --out it is given.
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

	// A terraform whose init records the root, the lock it found and the
	// leave to break it, and writes a lock where there was none, as the real
	// one does once it has authenticated what it downloaded.
	writeExecutable(t, filepath.Join(bin, "terraform"), `#!/bin/sh
case $1 in
    -chdir=*)
        dir=${1#-chdir=}
        if [ "$2" = init ]; then
            found=none
            if [ -f "$dir/.terraform.lock.hcl" ]; then
                found=$(cat "$dir/.terraform.lock.hcl")
            fi
            printf '%s|%s|%s\n' "$(basename "$(dirname "$dir")")" "$found" \
                "${TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE:-unset}" >>"$HYBRID_TEST_CALLS"
            if [ "$found" = none ]; then
                echo "authenticated in $(basename "$(dirname "$dir")")" >"$dir/.terraform.lock.hcl"
            fi
        fi ;;
esac
exit 0
`)

	cmd := exec.CommandContext(t.Context(), "sh", "hybrid-root-check.sh")
	cmd.Env = append(environmentWithout("PATH", "TF_PLUGIN_CACHE_DIR",
		"TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE", "BILLET_HYBRID_CHECK_BINARY"),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BILLET_HYBRID_CHECK_BINARY="+filepath.Join(bin, "billet"),
		"HYBRID_TEST_CALLS="+calls,
		"TF_PLUGIN_CACHE_DIR="+t.TempDir())

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hybrid-root-check.sh: %v\n%s", err, out)
	}

	body, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("no init was recorded: %v", err)
	}

	type init struct{ root, lock, mayBreak string }

	var inits []init
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.SplitN(line, "|", 3)
		if len(fields) != 3 {
			t.Fatalf("unreadable init record %q", line)
		}
		inits = append(inits, init{fields[0], fields[1], fields[2]})
	}

	want := []string{"untrusted", "trusted", "builder", "cache"}
	if len(inits) != len(want) {
		t.Fatalf("recorded %d inits, want one for each of the four roots: %+v", len(inits), inits)
	}

	for i, in := range inits {
		if in.root != want[i] {
			t.Errorf("init %d ran in the %s root, want the %s root", i, in.root, want[i])
		}

		if in.mayBreak != "unset" {
			t.Errorf("the %s root's init ran with TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE=%s", in.root, in.mayBreak)
		}
	}

	if inits[0].lock != "none" {
		t.Errorf("the first root's init found a lock, %q, so nothing authenticated its providers", inits[0].lock)
	}

	for _, in := range inits[1:] {
		if in.lock != "authenticated in untrusted" {
			t.Errorf("the %s root's init found the lock %q, want the one the first root's init wrote", in.root, in.lock)
		}
	}
}
