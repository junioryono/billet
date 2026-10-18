package deploy_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A REMOVAL'S NODE STOP IS PROVED BY THE NODE'S OWN ENDING (#127): the package
// reads the node's invocation before the stop, and after it asks
// `billet node ending check --drain` about that invocation, refusing the
// removal unless the check exits 0. A stop systemd calls successful (which a
// stop killed at TimeoutStopSec is too), a check that refuses, a check that
// cannot be run, and an invocation systemd did not name each refuse it; an
// upgrade asks nothing. Stand-ins answer for systemctl and billet.
func TestPackageRemovalRequiresTheNodesDrain(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is required to execute the package script on Linux")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "ubuntu:24.04").Run(); err != nil {
		t.Skipf("a local ubuntu:24.04 image and Docker daemon are required: %v", err)
	}
	script, err := filepath.Abs("scripts/preremove.sh")
	if err != nil {
		t.Fatal(err)
	}
	name := "billet-package-preremove-" + filepath.Base(filepath.Dir(t.TempDir()))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer stop()
		out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container:") {
			t.Errorf("remove the package-test container: %v: %s", err, out)
		}
	})
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--network=none",
		"--name", name, "--mount", "type=bind,src="+script+",dst=/preremove.sh,readonly",
		"-i", "ubuntu:24.04", "sh", "-eu")
	cmd.Stdin = strings.NewReader(`
mkdir /shim /run/systemd/system
# systemctl: the node is active until stopped, the server never; the node's
# invocation is $INVOCATION.
cat >/shim/systemctl <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >>/calls
case "$1 $2" in
"is-active --quiet") [ "$3" = billet-node ] && [ ! -e /stopped ] && exit 0; exit 3 ;;
"show -p") printf '%s\n' "${INVOCATION-inv-1}"; exit 0 ;;
"stop billet-node") : >/stopped; exit 0 ;;
esac
exit 0
EOF
# billet: the ending check answers $CHECK_EXIT and records its argv.
cat >/usr/bin/billet <<'EOF'
#!/bin/sh
printf 'billet %s\n' "$*" >>/calls
printf '{"outcome":"stand-in"}\n'
exit "${CHECK_EXIT:-0}"
EOF
chmod 0755 /shim/systemctl /usr/bin/billet

run() { rm -f /calls /stopped; status=0; PATH="/shim:${PATH}" sh /preremove.sh remove 2>/err || status=$?; }

# A DRAIN PROVED: the removal goes on, and the check was asked about the
# invocation read before the stop, with a drain required.
run
[ "${status}" -eq 0 ] || { echo "a proved drain refused the removal: ${status}"; cat /err; exit 1; }
grep -qx 'billet node ending check --config /etc/billet/billet.yaml --invocation inv-1 --drain --json' /calls ||
    { echo 'the check was not asked about the stopped invocation with a drain:'; cat /calls; exit 1; }
show_at=$(grep -n '^systemctl show -p InvocationID --value billet-node$' /calls | cut -d: -f1)
stop_at=$(grep -n '^systemctl stop billet-node$' /calls | cut -d: -f1)
check_at=$(grep -n '^billet node ending check' /calls | cut -d: -f1)
[ "${show_at}" -lt "${stop_at}" ] && [ "${stop_at}" -lt "${check_at}" ] ||
    { echo 'not invocation, then stop, then check:'; cat /calls; exit 1; }

# AN UNPROVED STOP, A CHECK THAT CANNOT TELL, AND NO INVOCATION refuse it.
for exit_code in 2 3; do
    CHECK_EXIT=${exit_code} run
    [ "${status}" -eq 1 ] || { echo "check exit ${exit_code} let the removal go on: ${status}"; exit 1; }
    grep -qF 'did not prove it drained' /err || { echo "check exit ${exit_code}: no refusal said"; cat /err; exit 1; }
done
INVOCATION= CHECK_EXIT=2 run
[ "${status}" -eq 1 ] || { echo "an unnamed invocation let the removal go on: ${status}"; exit 1; }
grep -qF -- '--invocation <unknown>' /err || { echo 'the unnamed invocation is not said'; cat /err; exit 1; }

# A CHECK THAT CANNOT RUN refuses it too.
mv /usr/bin/billet /usr/bin/billet.away
run
mv /usr/bin/billet.away /usr/bin/billet
[ "${status}" -eq 1 ] || { echo "a missing billet let the removal go on: ${status}"; exit 1; }

# AN UPGRADE stops and asks nothing.
rm -f /calls
PATH="/shim:${PATH}" sh /preremove.sh upgrade
[ ! -e /calls ] || { echo 'an upgrade touched the services:'; cat /calls; exit 1; }
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package removal's drain proof: %v\n%s", err, out)
	}
}
