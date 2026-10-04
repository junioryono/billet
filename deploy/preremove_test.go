package deploy_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A PACKAGE REMOVAL MARKS THIS HOST'S STOP FINAL BEFORE IT STOPS THE CONTROL PLANE.
// A server stopped while the deployment admits work hands over to the next control
// plane (#365), and a removal has none here, so the marker is what keeps its stop the
// drain it always was. It is a file on this host, never a seal of the deployment,
// which other controllers may be serving, and it is written before any service state
// is read. A marker that cannot be written refuses the removal before anything
// stops; an upgrade does none of it. Executed in a container against a fake
// systemctl and billet that record what they were asked, in order.
func TestPackageRemovalMarksTheStopFinalBeforeItStopsTheServer(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is required to execute the package script on Linux")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "ubuntu:24.04").Run(); err != nil {
		t.Skipf("a local ubuntu:24.04 image and Docker daemon are required: %v", err)
	}
	script, err := filepath.Abs("scripts/preremove.sh")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--network=none",
		"--mount", "type=bind,src="+script+",dst=/preremove.sh,readonly",
		"-i", "ubuntu:24.04", "sh", "-eu")
	cmd.Stdin = strings.NewReader(`
mkdir -p /run/systemd/system /state
cat >/usr/local/bin/systemctl <<'EOF'
#!/bin/sh
if [ "$1" = stop ] && [ "$2" = billet-server ]; then
    if [ -e /state/drain-on-stop ]; then
        echo "systemctl stop billet-server (final)" >>/calls
    else
        echo "systemctl stop billet-server (handoff)" >>/calls
    fi
    exit 0
fi
echo "systemctl $*" >>/calls
case "$*" in
"is-active --quiet billet-server") exit 0 ;;
"is-active --quiet billet-node") exit 3 ;;
esac
exit 0
EOF
cat >/usr/local/bin/billet <<'EOF'
#!/bin/sh
echo "billet $*" >>/calls
exit 0
EOF
chmod +x /usr/local/bin/systemctl /usr/local/bin/billet
export BILLET_SERVER_STATE_DIR=/state

: >/calls
sh /preremove.sh upgrade
if [ -s /calls ] || [ -e /state/drain-on-stop ]; then
    echo "an upgrade touched the services or marked the stop:" >&2
    cat /calls >&2
    exit 1
fi

: >/calls
sh /preremove.sh remove
if ! grep -qx 'systemctl stop billet-server (final)' /calls; then
    echo "a removal stopped the server without marking the stop final:" >&2
    cat /calls >&2
    exit 1
fi
if grep -q '^billet ' /calls; then
    echo "a removal sealed or drained the deployment instead of marking this host:" >&2
    cat /calls >&2
    exit 1
fi

: >/calls
rm -f /state/drain-on-stop
mkdir /state/drain-on-stop
if sh /preremove.sh remove 2>/refusal; then
    echo "a removal went on after the marker could not be written" >&2
    exit 1
fi
if grep -q 'stop' /calls; then
    echo "a removal stopped a service after the marker could not be written:" >&2
    cat /calls >&2
    exit 1
fi
grep -q 'Refusing to remove the package' /refusal
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package removal: %v\n%s", err, out)
	}
}
