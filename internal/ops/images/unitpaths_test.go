package images

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/unitpath"
)

// THE DECLARED LOCKS ARE THE FILES THE COMMAND LOCKS (#174): each Take is the
// command's own function, and the path reported for it is where that function
// leaves its file, so the declaration cannot drift from the pull it describes.
func TestRefreshPathsNameWhatThePullTouches(t *testing.T) {
	root := t.TempDir()
	dirs := map[string]string{}
	for _, name := range []string{"server", "node", "kernels", "jailer", "locks"} {
		dirs[name] = filepath.Join(root, name)
	}

	body := strings.NewReplacer(
		"state_dir: /var/lib/billet/server", "state_dir: "+dirs["server"],
		"state_dir: /var/lib/billet/node", "state_dir: "+dirs["node"]+"\n  lock_dir: "+dirs["locks"],
		"    bridge: billet0", "    bridge: billet0\n    kernel_dir: "+dirs["kernels"]+"\n    chroot_base: "+dirs["jailer"],
	).Replace(firecrackerNodeConfig)

	cfgPath := filepath.Join(root, "billet.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	needs := RefreshPaths(processEnv(), cfg, cfgPath)

	want := map[string]unitpath.Access{
		cfgPath:                         unitpath.Read,
		DefaultStagingDir:               unitpath.Private,
		dirs["kernels"]:                 unitpath.Write,
		kernelLockPath(dirs["kernels"]): unitpath.Lock,
		dirs["jailer"]:                  unitpath.Write,
		dirs["node"]:                    unitpath.Write,
	}

	var probe unitpath.Need

	for _, n := range needs {
		if n.Access == unitpath.Lock && strings.HasPrefix(n.Path, dirs["locks"]) {
			probe = n

			continue
		}

		if access, ok := want[n.Path]; !ok || access != n.Access {
			t.Errorf("declared %s %s, which the pull does not touch that way", n.Access, n.Path)
		}

		delete(want, n.Path)
	}

	for path, access := range want {
		t.Errorf("the pull's %s %s is not declared", access, path)
	}

	// THE PROBE'S LOCK IS NAMED FOR AN IDENTITY NOT YET MINTED, and says so.
	if probe.Unknown == "" || probe.Take == nil {
		t.Fatalf("the probe lock of a host with no identity is not reported unknown: %+v", probe)
	}

	for _, n := range needs {
		if n.Access == unitpath.Lock {
			if err := unitpath.Exercise(t.Context(), n, ""); err != nil {
				t.Fatalf("the pull's own lock could not be taken: %v", err)
			}
		}
	}

	if _, err := os.Stat(kernelLockPath(dirs["kernels"])); err != nil {
		t.Errorf("the kernel lock was not taken where it is declared: %v", err)
	}

	// THE EXERCISE MINTED THE IDENTITY WHERE THE VERIFICATION DOES, and the
	// lock it then took is the file the declaration now names.
	for _, n := range RefreshPaths(processEnv(), cfg, cfgPath) {
		if n.Access != unitpath.Lock || !strings.HasPrefix(n.Path, dirs["locks"]) {
			continue
		}

		if n.Unknown != "" {
			t.Fatalf("the probe lock is still unknown after the identity was minted: %s", n.Unknown)
		}

		if _, err := os.Stat(n.Path); err != nil {
			t.Errorf("the probe lock declared at %s is not the file the verification locked: %v", n.Path, err)
		}
	}
}

// A HOST THAT BOOTS NO FIRECRACKER GUEST asks the refresh for its config alone.
func TestRefreshPathsOfANodeThatBootsNoImage(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "billet.yaml")

	needs := RefreshPaths(processEnv(), &config.Config{}, cfgPath)
	if len(needs) != 1 || needs[0].Path != cfgPath || needs[0].Access != unitpath.Read {
		t.Errorf("a host with no node declared %+v", needs)
	}
}
