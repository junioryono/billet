package deploy_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A PACKAGE REMOVAL SEALS THE DEPLOYMENT BEFORE IT STOPS THE CONTROL PLANE. An
// unsealed server stop is a handoff to the next control plane (#365), and a removal
// has none, so the seal is what keeps its stop the drain it always was; a seal that
// cannot be taken refuses the removal before anything stops, and an upgrade does
// neither. Executed in a container against fake systemctl and billet commands that
// record what they were asked, in order.
func TestPackageRemovalSealsBeforeItStopsTheServer(t *testing.T) {
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
mkdir -p /run/systemd/system
cat >/usr/local/bin/systemctl <<'EOF'
#!/bin/sh
echo "systemctl $*" >>/calls
case "$*" in
"is-active --quiet billet-server") exit 0 ;;
"is-active --quiet billet-node") exit 3 ;;
esac
exit 0
EOF
cat >/fake-billet <<'EOF'
#!/bin/sh
echo "billet $*" >>/calls
exit "${DRAIN_STATUS:-0}"
EOF
chmod +x /usr/local/bin/systemctl /fake-billet
export BILLET_BIN=/fake-billet

: >/calls
sh /preremove.sh upgrade
if [ -s /calls ]; then
    echo "an upgrade touched the services:" >&2
    cat /calls >&2
    exit 1
fi

: >/calls
sh /preremove.sh remove
drain=$(grep -n '^billet drain --config /etc/billet/billet.yaml --reason ' /calls | cut -d: -f1)
stop=$(grep -n '^systemctl stop billet-server$' /calls | cut -d: -f1)
if [ -z "$drain" ] || [ -z "$stop" ] || [ "$drain" -ge "$stop" ]; then
    echo "a removal did not seal before stopping the server:" >&2
    cat /calls >&2
    exit 1
fi

: >/calls
if DRAIN_STATUS=1 sh /preremove.sh remove 2>/refusal; then
    echo "a removal went on after the seal failed" >&2
    exit 1
fi
if grep -q '^systemctl stop' /calls; then
    echo "a removal stopped a service after the seal failed:" >&2
    cat /calls >&2
    exit 1
fi
grep -q 'Refusing to remove the package' /refusal
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package removal: %v\n%s", err, out)
	}
}
