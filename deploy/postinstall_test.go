package deploy_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageKeepsPrivilegedStateOutsideTheServiceAccountsAuthority(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is required to execute the package installer on Linux")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "ubuntu:24.04").Run(); err != nil {
		t.Skipf("a local ubuntu:24.04 image and Docker daemon are required: %v", err)
	}
	script, err := filepath.Abs("scripts/postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	name := "billet-package-permissions-" + filepath.Base(filepath.Dir(t.TempDir()))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer stop()
		out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container:") {
			t.Errorf("remove the package-test container: %v: %s", err, out)
		}
	})
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--network=none",
		"--name", name, "--mount", "type=bind,src="+script+",dst=/postinstall.sh,readonly",
		"-i", "ubuntu:24.04", "sh", "-eu")
	cmd.Stdin = strings.NewReader(`
sh /postinstall.sh
mkdir -m 700 /var/lib/billet/upgrades /var/lib/billet/server
chown billet:billet /var/lib/billet/server
test "$(stat -c '%U:%G %a' /var/lib/billet)" = 'root:root 755'
runuser -u billet -- touch /var/lib/billet/server/ledger-fixture
if runuser -u billet -- mv /var/lib/billet/upgrades /var/lib/billet/replaced; then
    echo 'service account replaced privileged state' >&2
    exit 1
fi
if runuser -u billet -- mkdir /var/lib/billet/new-root-state; then
    echo 'service account planted privileged state' >&2
    exit 1
fi
sh /postinstall.sh
test "$(stat -c '%U:%G %a' /var/lib/billet)" = 'root:root 755'
test "$(stat -c '%U:%G %a' /var/lib/billet/server)" = 'billet:billet 700'
test -f /var/lib/billet/server/ledger-fixture
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package privilege boundary: %v\n%s", err, out)
	}
}
