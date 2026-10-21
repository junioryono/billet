package host

import (
	"bytes"
	"os"
	"path/filepath"
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

	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains("\n"+string(body), "\nProtectSystem=strict\n") {
			strict = append(strict, filepath.Base(file))
		}
	}

	declared := make([]string, 0, len(unitDeclarations))
	for unit := range unitDeclarations {
		declared = append(declared, unit)
	}

	sort.Strings(strict)
	sort.Strings(declared)

	if strings.Join(strict, " ") != strings.Join(declared, " ") {
		t.Errorf("the strict units are %v and the declared ones %v", strict, declared)
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

	if err := unitpath.Exercise(t.Context(), before, ""); err != nil {
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
		{[]string{"--config", cfgPath, "--marker", "gate", deploy.NodeUnitName}, "only with --exercise"},
		{[]string{"--config", cfgPath, "--exercise", "--marker", "../gate", deploy.NodeUnitName}, "one plain file"},
	} {
		var out bytes.Buffer

		err := cmdLocalUnitPaths(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want a refusal saying %q", tc.args, err, tc.want)
		}
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
