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
// `billet node ending check --drain` about that invocation, under a bound,
// refusing the removal unless the check exits 0; a node already stopped is
// judged by what its last process left. A stop systemd calls successful (which
// a stop killed at TimeoutStopSec is too), a check that refuses, cannot tell,
// is ended by its bound or cannot be run, and a running node systemd names no
// invocation for each refuse it; an upgrade asks nothing. Stand-ins answer for
// systemctl, timeout and billet.
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
mkdir -p /shim /run/systemd/system
# systemctl: the node is active until stopped (/stopped), the server never; the
# node's invocation is $INVOCATION.
cat >/shim/systemctl <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >>/calls
case "$1 $2" in
"is-active --quiet") [ "$3" = billet-node ] && [ ! -e /stopped ] && exit 0
    [ "$3" = billet-server ] && [ "${SERVER:-}" = active ] && exit 0; exit 3 ;;
"show -p") printf '%s\n' "${INVOCATION-inv-1}"; exit 0 ;;
"stop billet-node") : >/stopped; exit 0 ;;
"stop billet-server") exit "${SERVER_STOP:-0}" ;;
esac
exit 0
EOF
# timeout: records the bound the check runs under, then runs it.
cat >/shim/timeout <<'EOF'
#!/bin/sh
printf 'timeout %s\n' "$*" >>/calls
shift 3
exec "$@"
EOF
# billet: the ending check answers $CHECK_EXIT and records its argv.
cat >/usr/bin/billet <<'EOF'
#!/bin/sh
printf 'billet %s\n' "$*" >>/calls
printf '{"outcome":"stand-in"}\n'
exit "${CHECK_EXIT:-0}"
EOF
chmod 0755 /shim/systemctl /shim/timeout /usr/bin/billet

# run starts from a running node; resume from one already stopped.
run() { rm -f /stopped; resume; }
resume() { rm -f /calls; status=0; PATH="/shim:${PATH}" sh /preremove.sh remove 2>/err || status=$?; }

# A DRAIN PROVED: the removal goes on, and the check was asked, under its
# bound, about the invocation read before the stop, with a drain required.
run
[ "${status}" -eq 0 ] || { echo "a proved drain refused the removal: ${status}"; cat /err; exit 1; }
grep -qx 'timeout -k 10 180 /usr/bin/billet node ending check --config /etc/billet/billet.yaml --invocation inv-1 --drain --json' /calls ||
    { echo 'the check was not asked, bounded, about the stopped invocation with a drain:'; cat /calls; exit 1; }
show_at=$(grep -n '^systemctl show -p InvocationID --value billet-node$' /calls | cut -d: -f1)
stop_at=$(grep -n '^systemctl stop billet-node$' /calls | cut -d: -f1)
check_at=$(grep -n '^billet node ending check' /calls | cut -d: -f1)
[ "${show_at}" -lt "${stop_at}" ] && [ "${stop_at}" -lt "${check_at}" ] ||
    { echo 'not invocation, then stop, then check:'; cat /calls; exit 1; }

# AN UNPROVED STOP, A CHECK THAT CANNOT TELL, AND ONE THE BOUND ENDED refuse it.
for exit_code in 2 3 124; do
    CHECK_EXIT=${exit_code} run
    [ "${status}" -eq 1 ] || { echo "check exit ${exit_code} let the removal go on: ${status}"; exit 1; }
    grep -qF "did not prove it drained (check exited ${exit_code})" /err ||
        { echo "check exit ${exit_code}: no refusal said"; cat /err; exit 1; }
done

# A RETRY OVER THE STOP A REFUSED REMOVAL LEFT is judged by what the stopped
# node's last process left, and refused again until that is proved.
CHECK_EXIT=2 resume
[ "${status}" -eq 1 ] || { echo "a retry over an unproved stop let the removal go on: ${status}"; exit 1; }
grep -qx 'timeout -k 10 180 /usr/bin/billet node ending check --config /etc/billet/billet.yaml --drain --json' /calls ||
    { echo 'the retry did not judge the stopped node standing, under the bound:'; cat /calls; exit 1; }
! grep -q '^systemctl stop billet-node$' /calls || { echo 'the retry stopped a stopped node'; cat /calls; exit 1; }
resume
[ "${status}" -eq 0 ] || { echo "a retry over a proved stop refused the removal: ${status}"; cat /err; exit 1; }

# A NODE NEVER STARTED is judged by what the package's node left, like any
# other stopped node: the check decides, not this script.
CHECK_EXIT=2 resume
[ "${status}" -eq 1 ] || { echo "a stopped node's unproved record let the removal go on: ${status}"; exit 1; }
grep -qx 'timeout -k 10 180 /usr/bin/billet node ending check --config /etc/billet/billet.yaml --drain --json' /calls ||
    { echo 'the stopped node was not judged standing, under the bound:'; cat /calls; exit 1; }

# THE SERVER stops after the node is judged, and a failed stop refuses.
SERVER=active run
[ "${status}" -eq 0 ] || { echo "a running server refused the removal: ${status}"; cat /err; exit 1; }
check_at=$(grep -n '^billet node ending check' /calls | cut -d: -f1)
server_at=$(grep -n '^systemctl stop billet-server$' /calls | cut -d: -f1)
[ -n "${server_at}" ] && [ "${check_at}" -lt "${server_at}" ] ||
    { echo 'the server was not stopped after the node was judged:'; cat /calls; exit 1; }
SERVER=active SERVER_STOP=1 run
[ "${status}" -eq 1 ] || { echo "a server that would not stop let the removal go on: ${status}"; exit 1; }
grep -qF 'could not stop billet-server' /err || { echo 'the server refusal is not said'; cat /err; exit 1; }

# A RUNNING NODE SYSTEMD NAMES NO INVOCATION FOR is refused before its stop.
INVOCATION= run
[ "${status}" -eq 1 ] || { echo "an unnamed invocation let the removal go on: ${status}"; exit 1; }
grep -qF 'names no invocation' /err || { echo 'the unnamed invocation is not said'; cat /err; exit 1; }
! grep -q '^systemctl stop billet-node$' /calls || { echo 'an unnamed invocation was stopped'; cat /calls; exit 1; }

# A CHECK THAT CANNOT RUN refuses it too.
mv /usr/bin/billet /usr/bin/billet.away
run
mv /usr/bin/billet.away /usr/bin/billet
[ "${status}" -eq 1 ] || { echo "a missing billet let the removal go on: ${status}"; exit 1; }
grep -qF 'did not prove it drained (check exited 127)' /err || { echo 'the missing billet is not said'; cat /err; exit 1; }

# AN UPGRADE stops and asks nothing.
rm -f /calls
PATH="/shim:${PATH}" sh /preremove.sh upgrade
[ ! -e /calls ] || { echo 'an upgrade touched the services:'; cat /calls; exit 1; }
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package removal's drain proof: %v\n%s", err, out)
	}
}
