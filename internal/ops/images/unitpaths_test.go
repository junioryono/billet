package images

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/unitpath"
	"github.com/junioryono/billet/internal/wirecert"
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
		dirs["server"]:                  unitpath.Write,
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
			if _, err := unitpath.Exercise(t.Context(), n, ""); err != nil {
				t.Fatalf("the pull's own lock could not be taken: %v", err)
			}
		}
	}

	if _, err := os.Stat(kernelLockPath(dirs["kernels"])); err != nil {
		t.Errorf("the kernel lock was not taken where it is declared: %v", err)
	}

	// AN ABSENT CHROOT BASE IS CREATED, as the firecracker provider creates it
	// (0700) before the probe boots, so a fresh custom one is not a refusal.
	for _, n := range needs {
		if n.Path != dirs["jailer"] {
			continue
		}

		if _, err := unitpath.Exercise(t.Context(), n, ""); err != nil {
			t.Fatalf("an absent chroot base was refused: %v", err)
		}

		info, err := os.Stat(dirs["jailer"])
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("the chroot base was made %v (%v), want the jailer's 0700", info.Mode().Perm(), err)
		}
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

	// IN THE DIRECTORY DECLARED FOR IT: beside a control plane, the node with no
	// certificate founds the identity in the server's directory, never its own.
	if _, err := os.Stat(state.DeploymentIDPath(dirs["server"])); err != nil {
		t.Errorf("the identity was not minted in the declared %s: %v", dirs["server"], err)
	}

	if _, err := os.Stat(dirs["node"]); !os.IsNotExist(err) {
		t.Errorf("the undeclared %s was written (%v)", dirs["node"], err)
	}
}

// THE IDENTITY'S DIRECTORY IS DECLARED WHERE THE VERIFICATION WRITES IT, in
// verifyDeploymentID's order: nowhere for a certificate that loads and names a
// deployment, the control plane's directory beside the node, or else the node's
// own state directory, which is also where a certificate that does not load
// falls back to; a certificate that loads writes nothing, even one the
// verification refuses.
func TestRefreshPathsDeclareWhereTheIdentityIsWritten(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "node")
	serverDir := filepath.Join(root, "server")

	body := strings.NewReplacer(
		"state_dir: /var/lib/billet/server", "state_dir: "+serverDir,
		"state_dir: /var/lib/billet/node", "state_dir: "+stateDir+"\n  lock_dir: "+filepath.Join(root, "locks"),
	).Replace(firecrackerNodeConfig)

	cfgPath := filepath.Join(root, "billet.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	load := func() *config.Config {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}

		return cfg
	}

	declares := func(cfg *config.Config, dir string) bool {
		return slices.ContainsFunc(RefreshPaths(processEnv(), cfg, cfgPath), func(n unitpath.Need) bool {
			return n.Path == dir && n.Access == unitpath.Write
		})
	}

	beside := load()
	if !declares(beside, serverDir) || declares(beside, stateDir) {
		t.Error("a node beside a control plane, with no certificate, does not declare the server's directory alone")
	}

	alone := load()
	alone.Server = nil

	if !declares(alone, stateDir) {
		t.Error("a node alone, with no certificate, does not declare the state directory its identity is minted in")
	}

	// A CERTIFICATE THAT DOES NOT LOAD IS PASSED OVER, as the verification
	// passes over it, so the directory past it is still written.
	broken := load()
	broken.Server = nil
	broken.Node.TLS = &config.NodeTLS{
		CertPath: filepath.Join(root, "absent.crt"), KeyPath: filepath.Join(root, "absent.key"), CAPath: filepath.Join(root, "absent-ca.crt"),
	}

	if !declares(broken, stateDir) {
		t.Error("a node whose certificate does not load does not declare the state directory it falls back to")
	}

	deployment, err := state.DeploymentID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A CERTIFICATE THAT LOADS IS THE ANSWER, whether it names a deployment the
	// verification accepts or one it refuses: either way it writes no identity,
	// so none is declared, and taking the locks, which resolves the identity as
	// the verification does, creates neither directory.
	for _, tc := range []struct {
		name, deployment string
		refused          bool
	}{
		{"a minted deployment", deployment, false},
		{"a deployment billet did not mint", "not-a-minted-identity", true},
		{"no deployment at all", "", true},
	} {
		ca, err := wirecert.LoadOrCreateCA(t.TempDir(), tc.deployment)
		if err != nil {
			t.Fatal(err)
		}

		bundle, err := ca.IssueNode("epyc-1")
		if err != nil {
			t.Fatal(err)
		}

		bundleDir := t.TempDir()
		if err := bundle.Write(bundleDir); err != nil {
			t.Fatal(err)
		}

		certified := load()
		certified.Node.TLS = &config.NodeTLS{
			CertPath: filepath.Join(bundleDir, "node.crt"), KeyPath: filepath.Join(bundleDir, "node.key"), CAPath: filepath.Join(bundleDir, "ca.crt"),
		}

		if loaded, err := app.NodeBundle(certified); err != nil || loaded == nil {
			t.Fatalf("%s: the bundle this case turns on does not load (%v)", tc.name, err)
		}

		if declares(certified, stateDir) || declares(certified, serverDir) {
			t.Errorf("%s: a node whose certificate loads declares a directory for an identity it never writes", tc.name)
		}

		// THE VERIFICATION'S OWN ANSWER, through the probe lock's Take: the
		// lock for a minted deployment, a refusal for the others.
		probed := false

		for _, n := range RefreshPaths(processEnv(), certified, cfgPath) {
			if n.Access == unitpath.Lock && strings.HasPrefix(n.Path, filepath.Join(root, "locks")) {
				_, err := unitpath.Exercise(t.Context(), n, "")
				if (err != nil) != tc.refused {
					t.Errorf("%s: resolving the identity answered %v, want refused=%v", tc.name, err, tc.refused)
				}

				probed = true
			}
		}

		if !probed {
			t.Fatalf("%s: no probe lock was declared to resolve the identity through", tc.name)
		}

		for _, dir := range []string{stateDir, serverDir} {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("%s: resolving the identity wrote %s, which the verification never does (%v)", tc.name, dir, err)
			}
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
