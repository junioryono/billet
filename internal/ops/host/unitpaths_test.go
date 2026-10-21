package host

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/unitpath"
)

// EVERY SHIPPED UNIT UNDER A STRICT SANDBOX DECLARES WHAT ITS COMMAND NEEDS
// (#174), and nothing else is declared: a strict unit with no declaration is
// one the sandbox gate never starts, and a declaration for a unit that is not
// strict proves a sandbox that does not exist.
func TestEveryStrictUnitDeclaresItsPaths(t *testing.T) {
	files, err := filepath.Glob("../../../deploy/*.service")
	if err != nil || len(files) == 0 {
		t.Fatalf("no shipped units found (%v)", err)
	}

	var strict []string

	root := map[string]bool{}

	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains("\n"+string(body), "\nProtectSystem=strict\n") {
			strict = append(strict, filepath.Base(file))
			// A UNIT THAT NAMES NO User= RUNS AS ROOT.
			text := "\n" + string(body)
			root[filepath.Base(file)] = strings.Contains(text, "\nUser=root\n") || !strings.Contains(text, "\nUser=")
		}
	}

	declared := make([]string, 0, len(unitDeclarations))
	for _, d := range unitDeclarations {
		declared = append(declared, d.unit)

		// THE ACCOUNT AN EXERCISE IS ALLOWED is the one the unit runs as.
		if d.root != root[d.unit] {
			t.Errorf("%s is declared root=%v and its unit says User=root is %v", d.unit, d.root, root[d.unit])
		}
	}

	sort.Strings(strict)
	sort.Strings(declared)

	if strings.Join(strict, " ") != strings.Join(declared, " ") {
		t.Errorf("the strict units are %v and the declared ones %v", strict, declared)
	}

	// THE SERVER BEFORE THE NODE, as `billet local up` starts them.
	if slices.IndexFunc(unitDeclarations, func(d unitDeclaration) bool { return d.unit == deploy.ServerUnitName }) >
		slices.IndexFunc(unitDeclarations, func(d unitDeclaration) bool { return d.unit == deploy.NodeUnitName }) {
		t.Error("the node is declared before the server, whose identity it claims")
	}
}

// THE NODE'S DECLARED LOCK IS THE FILE THE NODE LOCKS: its Take is
// app.ClaimNodeDeployment, the node's own claim, and the path reported once the
// identity exists is where that claim's lock is.
func TestTheNodeLockIsTheFileTheNodeLocks(t *testing.T) {
	root := t.TempDir()
	stateDir, lockDir := filepath.Join(root, "node"), filepath.Join(root, "locks")

	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(root, "billet.yaml")
	body := "github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: /etc/billet/app-private-key.pem\n" +
		"node:\n  name: n1\n  server_addr: 127.0.0.1:7717\n  provider: docker\n  state_dir: " + stateDir +
		"\n  lock_dir: " + lockDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	lockOf := func() unitpath.Need {
		for _, n := range nodePaths(processEnv(), cfg, cfgPath) {
			if n.Access == unitpath.Lock {
				return n
			}
		}

		t.Fatal("the node declares no lock")

		return unitpath.Need{}
	}

	before := lockOf()
	if before.Unknown == "" {
		t.Fatalf("a host with no identity reported its lock as %s", before.Path)
	}

	if _, err := unitpath.Exercise(t.Context(), before, ""); err != nil {
		t.Fatalf("the node's own claim could not be exercised: %v", err)
	}

	after := lockOf()
	if after.Unknown != "" || filepath.Dir(after.Path) != lockDir {
		t.Fatalf("after the claim minted an identity the lock reads %+v", after)
	}

	if _, err := os.Stat(after.Path); err != nil {
		t.Errorf("the declared lock %s is not the file the node's claim locked: %v", after.Path, err)
	}

	var registration bool
	for _, n := range nodePaths(processEnv(), cfg, cfgPath) {
		registration = registration ||
			(n.Path == "/run/"+deploy.NodeRegistrationDirectory && n.Access == unitpath.Write)
	}

	if !registration {
		t.Error("the node does not declare the directory it publishes its registration record in")
	}
}

// A LOCK THE CLAIM COULD NOT PLACE IS NOT A LOCK THE SANDBOX PERMITS, even where
// allow_unlocked_deployment lets the node start without one: the exercise says
// the path was refused.
func TestAnUnplacedNodeLockIsARefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a 0555 directory, so the refusal this stages needs an unprivileged run")
	}

	root := t.TempDir()
	lockDir := filepath.Join(root, "locks")
	if err := os.Mkdir(lockDir, 0o555); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(root, "billet.yaml")
	body := "github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: /etc/billet/app-private-key.pem\n" +
		"node:\n  name: n1\n  server_addr: 127.0.0.1:7717\n  provider: docker\n  state_dir: " +
		filepath.Join(root, "node") + "\n  lock_dir: " + lockDir + "\n  allow_unlocked_deployment: true\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range nodePaths(processEnv(), cfg, cfgPath) {
		if n.Access != unitpath.Lock {
			continue
		}

		if _, err := unitpath.Exercise(t.Context(), n, ""); err == nil || !strings.Contains(err.Error(), "could not be placed") {
			t.Errorf("an unplaced lock was exercised as %v", err)
		}

		return
	}

	t.Fatal("the node declares no lock")
}

func TestUnitPathsRefusesWhatItCannotAnswer(t *testing.T) {
	old := cli.HostOS
	cli.HostOS = "linux"
	t.Cleanup(func() { cli.HostOS = old })

	cfgPath := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(cfgPath, []byte("github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n"+
		"  private_key_path: /etc/billet/app-private-key.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--config", cfgPath}, "usage: billet local unit-paths"},
		{[]string{"--config", cfgPath, "billet-upgrade.service"}, `"billet-upgrade.service" is not one of them`},
		{[]string{"--config", cfgPath, "--marker", "billet-unit-paths-gate", deploy.NodeUnitName}, "only with --exercise"},
		{[]string{"--config", cfgPath, "--exercise", "--marker", "../billet-unit-paths-gate", deploy.NodeUnitName}, "one plain file"},
		// A MARKER NEVER NAMES A FILE A COMMAND KEEPS: the ledger here.
		{[]string{"--config", cfgPath, "--exercise", "--marker", "billet.db", deploy.ServerUnitName}, "one plain file beginning billet-unit-paths-"},
		{[]string{"--config", cfgPath, "--units", deploy.NodeUnitName}, "--units takes no unit"},
	} {
		var out bytes.Buffer

		err := cmdLocalUnitPaths(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want a refusal saying %q", tc.args, err, tc.want)
		}
	}
}

// AS ROOT, A UNIT THAT RUNS AS ITS SERVICE ACCOUNT IS NOT EXERCISED: the
// identity and lock it would create beside the ledger would be root's, and the
// server could not open them at its next start. Refused before anything exists.
func TestAnExerciseRunsAsTheUnitsOwnAccount(t *testing.T) {
	oldOS, oldEUID := cli.HostOS, geteuid
	cli.HostOS, geteuid = "linux", func() int { return 0 }
	t.Cleanup(func() { cli.HostOS, geteuid = oldOS, oldEUID })

	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	cfgPath := filepath.Join(root, "billet.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  listen: 127.0.0.1:7717\n  state_dir: "+serverDir+
		"\n  max_vcpu: 2\n  max_memory: 4GiB\n"+
		"github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n"+
		"  private_key_path: /etc/billet/app-private-key.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	err := cmdLocalUnitPaths(t.Context(), cli.Env{Stdout: &out, Stderr: &out},
		[]string{"--config", cfgPath, "--exercise", deploy.ServerUnitName})
	if err == nil || !strings.Contains(err.Error(), "runs its command as its service account, not root") {
		t.Fatalf("root exercised the server's paths: %v", err)
	}

	if _, err := os.Stat(serverDir); !os.IsNotExist(err) {
		t.Errorf("the refused exercise created %s (%v)", serverDir, err)
	}
}

func TestUnitPathsListsTheUnitsInStartOrder(t *testing.T) {
	old := cli.HostOS
	cli.HostOS = "linux"
	t.Cleanup(func() { cli.HostOS = old })

	var out bytes.Buffer
	if err := cmdLocalUnitPaths(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, []string{"--units"}); err != nil {
		t.Fatal(err)
	}

	want := deploy.ServerUnitName + "\n" + deploy.BackupUnitName + "\n" + deploy.ImagesRefreshUnitName + "\n" +
		deploy.NodeUnitName + "\n"
	if out.String() != want {
		t.Errorf("--units printed %q, want %q", out.String(), want)
	}
}

func TestUnitPathsReportsWhatTheCommandTouches(t *testing.T) {
	old := cli.HostOS
	cli.HostOS = "linux"
	t.Cleanup(func() { cli.HostOS = old })

	root := t.TempDir()
	cfgPath := filepath.Join(root, "billet.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  listen: 127.0.0.1:7717\n  state_dir: "+
		filepath.Join(root, "server")+"\n  max_vcpu: 2\n  max_memory: 4GiB\n"+
		"github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n"+
		"  private_key_path: /etc/billet/app-private-key.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := cmdLocalUnitPaths(t.Context(), cli.Env{Stdout: &out, Stderr: &out},
		[]string{"--config", cfgPath, "--json", deploy.BackupUnitName}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`"unit": "billet-backup.service"`,
		`"path": "/etc/billet/app-private-key.pem"`,
		`"path": "` + localBackupDir + `"`,
		`"path": "` + filepath.Join(root, "server") + `"`,
		`"access": "lock"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the backup's paths do not say %s:\n%s", want, out.String())
		}
	}
}
